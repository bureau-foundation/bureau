// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestIsAuthError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "M_UNKNOWN_TOKEN",
			err:      &messaging.MatrixError{Code: messaging.ErrCodeUnknownToken, StatusCode: 401},
			expected: true,
		},
		{
			name:     "M_FORBIDDEN",
			err:      &messaging.MatrixError{Code: messaging.ErrCodeForbidden, StatusCode: 403},
			expected: true,
		},
		{
			name:     "M_NOT_FOUND",
			err:      &messaging.MatrixError{Code: messaging.ErrCodeNotFound, StatusCode: 404},
			expected: false,
		},
		{
			name:     "M_LIMIT_EXCEEDED",
			err:      &messaging.MatrixError{Code: messaging.ErrCodeLimitExceeded, StatusCode: 429},
			expected: false,
		},
		{
			name:     "generic error",
			err:      context.DeadlineExceeded,
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if isAuthError(test.err) != test.expected {
				t.Errorf("isAuthError(%v) = %v, want %v", test.err, !test.expected, test.expected)
			}
		})
	}
}

func TestEmergencyShutdown_DestroysAllSandboxes(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Set up a mock launcher socket that accepts destroy-sandbox requests.
	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")
	destroyedPrincipals := make(chan string, 10)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action == "destroy-sandbox" {
			destroyedPrincipals <- request.Principal
			return launcherIPCResponse{OK: true}
		}
		return launcherIPCResponse{OK: false, Error: "unexpected action: " + request.Action}
	})
	t.Cleanup(func() { listener.Close() })
	daemon.launcherSocket = launcherSocket

	// Pre-populate running state with 3 principals.
	alphaEntity := testEntity(t, daemon.fleet, "agent/alpha")
	betaEntity := testEntity(t, daemon.fleet, "agent/beta")
	gammaEntity := testEntity(t, daemon.fleet, "agent/gamma")
	daemon.running[alphaEntity] = true
	daemon.running[betaEntity] = true
	daemon.running[gammaEntity] = true
	daemon.lastSpecs[alphaEntity] = &schema.SandboxSpec{}
	daemon.lastSpecs[betaEntity] = &schema.SandboxSpec{}
	daemon.lastSpecs[gammaEntity] = &schema.SandboxSpec{}

	// Set up shutdownCancel to verify it's called.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	daemon.shutdownCtx = shutdownCtx
	daemon.shutdownCancel = shutdownCancel

	daemon.emergencyShutdown()

	// Verify all principals were destroyed.
	if len(daemon.running) != 0 {
		t.Errorf("running map should be empty, got %d entries", len(daemon.running))
	}

	// Verify shutdownCtx was cancelled.
	select {
	case <-shutdownCtx.Done():
		// expected
	default:
		t.Error("shutdownCtx should be cancelled after emergency shutdown")
	}

	// Collect destroyed principals. The IPC Principal field contains the
	// account localpart (e.g., "agent/alpha"), not the fleet-scoped localpart.
	close(destroyedPrincipals)
	destroyed := make(map[string]bool)
	for principal := range destroyedPrincipals {
		destroyed[principal] = true
	}
	for _, entity := range []ref.Entity{alphaEntity, betaEntity, gammaEntity} {
		if !destroyed[entity.AccountLocalpart()] {
			t.Errorf("expected destroy-sandbox IPC for %q", entity.AccountLocalpart())
		}
	}
}

func TestEmergencyShutdown_LauncherIPCFailure(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Point at a nonexistent socket — all IPC calls will fail.
	daemon.launcherSocket = "/nonexistent/launcher.sock"

	alphaEntity := testEntity(t, daemon.fleet, "agent/alpha")
	betaEntity := testEntity(t, daemon.fleet, "agent/beta")
	daemon.running[alphaEntity] = true
	daemon.running[betaEntity] = true
	daemon.lastSpecs[alphaEntity] = &schema.SandboxSpec{}
	daemon.lastSpecs[betaEntity] = &schema.SandboxSpec{}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	daemon.shutdownCtx = shutdownCtx
	daemon.shutdownCancel = shutdownCancel

	// Should not hang or panic even when launcher IPC fails.
	daemon.emergencyShutdown()

	// Maps should still be cleaned up despite IPC failure.
	if len(daemon.running) != 0 {
		t.Errorf("running map should be empty despite IPC failure, got %d entries", len(daemon.running))
	}

	// Shutdown should still be triggered.
	select {
	case <-shutdownCtx.Done():
		// expected
	default:
		t.Error("shutdownCtx should be cancelled even when IPC fails")
	}
}

func TestSyncLoop_AuthFailureTriggersShutdown(t *testing.T) {
	t.Parallel()

	// Mock homeserver that returns M_UNKNOWN_TOKEN on /sync.
	matrixServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_matrix/client/v3/sync" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUnknownToken,
				Message: "Unknown access token",
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	daemon, _ := newTestDaemon(t)

	session, err := matrixClient.SessionFromToken(daemon.machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon.session = session
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.launcherSocket = "/nonexistent/launcher.sock"

	// Pre-populate a running principal to verify it gets destroyed.
	testAgentEntity := testEntity(t, daemon.fleet, "agent/test")
	daemon.running[testAgentEntity] = true
	daemon.lastSpecs[testAgentEntity] = &schema.SandboxSpec{}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	daemon.shutdownCtx = shutdownCtx
	daemon.shutdownCancel = shutdownCancel

	// Run the shared sync loop in a goroutine — the daemon's
	// syncErrorHandler should detect the auth error and trigger
	// emergency shutdown.
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		service.RunSyncLoop(context.Background(), session, service.SyncConfig{
			Filter:      daemon.syncFilter,
			OnSyncError: daemon.syncErrorHandler,
		}, "batch_0", daemon.processSyncResponse, clock.Real(), daemon.logger)
	}()

	// Wait for sync loop to exit (should be fast — one failed /sync call).
	select {
	case <-syncDone:
		// expected
	case <-t.Context().Done():
		t.Fatal("syncLoop did not exit after auth failure")
	}

	// Verify emergency shutdown was triggered.
	select {
	case <-shutdownCtx.Done():
		// expected
	default:
		t.Error("shutdownCtx should be cancelled after auth failure in sync loop")
	}

	// Verify running map was cleaned up.
	daemon.reconcileMu.Lock()
	runningCount := len(daemon.running)
	daemon.reconcileMu.Unlock()
	if runningCount != 0 {
		t.Errorf("running map should be empty after auth failure, got %d entries", runningCount)
	}
}

func TestInitialSync_AuthFailureReturnsError(t *testing.T) {
	t.Parallel()

	matrixServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_matrix/client/v3/sync" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(messaging.MatrixError{
				Code:    messaging.ErrCodeUnknownToken,
				Message: "Unknown access token",
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	daemon, _ := newTestDaemon(t)

	session, err := matrixClient.SessionFromToken(daemon.machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon.session = session
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	_, syncErr := daemon.initialSync(context.Background())
	if syncErr == nil {
		t.Fatal("expected error from initialSync with deactivated account")
	}

	// The error should be an auth error (wrapping M_UNKNOWN_TOKEN).
	if !isAuthError(syncErr) {
		t.Errorf("expected auth error, got: %v", syncErr)
	}
}
