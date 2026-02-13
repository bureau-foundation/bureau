// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestPrefetchEnvironment_ExistingPath(t *testing.T) {
	t.Parallel()

	existingPath := t.TempDir()
	called := false
	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			called = true
			return fmt.Errorf("should not be called")
		},
	}

	err := daemon.prefetchEnvironment(context.Background(), existingPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("prefetchFunc should not be called when path exists on disk")
	}
}

func TestPrefetchEnvironment_MissingPathSuccess(t *testing.T) {
	t.Parallel()

	var capturedPath string
	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			capturedPath = storePath
			return nil
		},
	}

	const wantPath = "/nix/store/nonexistent-test-path"
	err := daemon.prefetchEnvironment(context.Background(), wantPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedPath != wantPath {
		t.Errorf("prefetchFunc called with %q, want %q", capturedPath, wantPath)
	}
}

func TestPrefetchEnvironment_MissingPathFailure(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return fmt.Errorf("substituter unreachable")
		},
	}

	err := daemon.prefetchEnvironment(context.Background(), "/nix/store/nonexistent-test-path")
	if err == nil {
		t.Fatal("expected error when prefetchFunc fails")
	}
	if !strings.Contains(err.Error(), "substituter unreachable") {
		t.Errorf("error = %v, want containing 'substituter unreachable'", err)
	}
}

func TestPrefetchEnvironment_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return ctx.Err()
		},
	}

	err := daemon.prefetchEnvironment(ctx, "/nix/store/nonexistent-test-path")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestPrefetchBureauVersion_AllPaths(t *testing.T) {
	t.Parallel()

	// All three store paths exist on disk, so no actual prefetch should
	// be needed (os.Stat fast path).
	daemonDir := t.TempDir()
	launcherDir := t.TempDir()
	proxyDir := t.TempDir()

	called := false
	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			called = true
			return fmt.Errorf("should not be called for existing paths")
		},
	}

	version := &schema.BureauVersion{
		DaemonStorePath:   daemonDir,
		LauncherStorePath: launcherDir,
		ProxyStorePath:    proxyDir,
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("prefetchFunc should not be called when all paths exist")
	}
}

func TestPrefetchBureauVersion_PartialPaths(t *testing.T) {
	t.Parallel()

	// Only DaemonStorePath is set; others are empty and should be skipped.
	existingDir := t.TempDir()

	var prefetchedPaths []string
	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			prefetchedPaths = append(prefetchedPaths, storePath)
			return nil
		},
	}

	version := &schema.BureauVersion{
		DaemonStorePath: existingDir,
		// LauncherStorePath and ProxyStorePath intentionally empty.
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prefetchedPaths) != 0 {
		t.Errorf("expected no prefetchFunc calls (path exists), got calls for %v", prefetchedPaths)
	}
}

func TestPrefetchBureauVersion_PrefetchFailure(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return fmt.Errorf("substituter unavailable")
		},
	}

	version := &schema.BureauVersion{
		DaemonStorePath: "/nix/store/nonexistent-daemon-binary",
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err == nil {
		t.Fatal("expected error when prefetchFunc fails")
	}
	if !strings.Contains(err.Error(), "daemon") || !strings.Contains(err.Error(), "substituter unavailable") {
		t.Errorf("error = %v, want error mentioning daemon and substituter unavailable", err)
	}
}

func TestPrefetchBureauVersion_MissingPathTriggersFetch(t *testing.T) {
	t.Parallel()

	var fetchedPaths []string
	daemon := &Daemon{
		clock:  clock.Real(),
		runDir: principal.DefaultRunDir,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			fetchedPaths = append(fetchedPaths, storePath)
			return nil
		},
	}

	version := &schema.BureauVersion{
		DaemonStorePath:   "/nix/store/nonexistent-daemon",
		LauncherStorePath: "/nix/store/nonexistent-launcher",
		ProxyStorePath:    "/nix/store/nonexistent-proxy",
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fetchedPaths) != 3 {
		t.Errorf("expected 3 prefetch calls, got %d: %v", len(fetchedPaths), fetchedPaths)
	}
}

func TestFindNixStore(t *testing.T) {
	t.Parallel()

	path, err := nix.FindBinary("nix-store")
	if err != nil {
		t.Skipf("nix-store not available: %v", err)
	}
	if !strings.Contains(path, "nix-store") {
		t.Errorf("nix.FindBinary(\"nix-store\") = %q, expected path containing 'nix-store'", path)
	}
}

// TestReconcile_PrefetchFailureSkipsPrincipal verifies that a failed
// Nix prefetch prevents sandbox creation and posts an error message to
// the config room. On the next reconcile (with prefetch succeeding),
// the principal starts normally.
func TestReconcile_PrefetchFailureSkipsPrincipal(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Track messages sent to the config room (prefetch error messages).
	var (
		messagesMu         sync.Mutex
		configRoomMessages []string
	)

	// Mock Matrix server with template and config state.
	matrixState := newPrefetchTestMatrixState(t, configRoomID, templateRoomID, machineName)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intercept message sends to the config room. SendEvent uses
		// PUT with url.PathEscape on room ID and event type.
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				configRoomMessages = append(configRoomMessages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		matrixState.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Mock launcher that tracks create-sandbox calls.
	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var (
		launcherMu        sync.Mutex
		createdPrincipals []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action == "create-sandbox" {
			launcherMu.Lock()
			createdPrincipals = append(createdPrincipals, request.Principal)
			launcherMu.Unlock()
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		}
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	// Start with prefetch that always fails.
	prefetchError := fmt.Errorf("connection to attic refused")
	daemon := &Daemon{
		clock:               clock.Real(),
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastCredentials:     make(map[string]string),
		lastVisibility:      make(map[string][]string),
		lastMatrixPolicy:    make(map[string]*schema.MatrixPolicy),
		lastObservePolicy:   make(map[string]*schema.ObservePolicy),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return prefetchError
		},
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// First reconcile: prefetch fails, principal should NOT start.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	if len(createdPrincipals) != 0 {
		t.Errorf("expected no create-sandbox calls after prefetch failure, got %v", createdPrincipals)
	}
	launcherMu.Unlock()

	if daemon.running["agent/test"] {
		t.Error("principal should not be marked as running after prefetch failure")
	}

	// Verify error message was posted to the config room.
	messagesMu.Lock()
	if len(configRoomMessages) == 0 {
		t.Error("expected error message in config room after prefetch failure")
	} else {
		found := false
		for _, message := range configRoomMessages {
			if strings.Contains(message, "agent/test") && strings.Contains(message, "attic refused") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("config room messages = %v, want message mentioning agent/test and attic refused", configRoomMessages)
		}
	}
	messagesMu.Unlock()

	// Second reconcile: prefetch succeeds, principal should start.
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	if len(createdPrincipals) != 1 || createdPrincipals[0] != "agent/test" {
		t.Errorf("expected create-sandbox for agent/test, got %v", createdPrincipals)
	}
	launcherMu.Unlock()

	if !daemon.running["agent/test"] {
		t.Error("principal should be marked as running after successful prefetch")
	}
}

// TestReconcile_NoPrefetchWithoutEnvironmentPath verifies that
// principals with templates that have no EnvironmentPath skip the
// prefetch step entirely.
func TestReconcile_NoPrefetchWithoutEnvironmentPath(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newPrefetchTestMatrixState(t, configRoomID, templateRoomID, machineName)
	// Override template to have no environment path.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	prefetchCalled := false
	daemon := &Daemon{
		clock:               clock.Real(),
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastCredentials:     make(map[string]string),
		lastVisibility:      make(map[string][]string),
		lastMatrixPolicy:    make(map[string]*schema.MatrixPolicy),
		lastObservePolicy:   make(map[string]*schema.ObservePolicy),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			prefetchCalled = true
			return fmt.Errorf("should not be called")
		},
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if prefetchCalled {
		t.Error("prefetchFunc should not be called when template has no EnvironmentPath")
	}

	if !daemon.running["agent/test"] {
		t.Error("principal should be running (no prefetch needed)")
	}
}

// --- Test helpers ---

// newPrefetchTestMatrixState creates a mock Matrix state with a
// template that has an EnvironmentPath and a MachineConfig that
// references it. Used by the reconcile prefetch tests.
func newPrefetchTestMatrixState(t *testing.T, configRoomID, templateRoomID, machineName string) *mockMatrixState {
	t.Helper()

	state := newMockMatrixState()

	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:     []string{"/bin/echo", "hello"},
		Environment: "/nix/store/abc123-test-env",
	})

	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})

	state.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	return state
}

// startMockLauncher starts a mock launcher that accepts CBOR IPC on a
// Unix socket and responds using the provided handler function. Returns
// the listener (caller should defer Close).
func startMockLauncher(t *testing.T, socketPath string, handler func(launcherIPCRequest) launcherIPCResponse) net.Listener {
	t.Helper()

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", socketPath, err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			var request launcherIPCRequest
			if err := codec.NewDecoder(conn).Decode(&request); err != nil {
				conn.Close()
				continue
			}

			response := handler(request)
			codec.NewEncoder(conn).Encode(response)
			conn.Close()
		}
	}()

	return listener
}
