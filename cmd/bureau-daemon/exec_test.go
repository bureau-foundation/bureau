// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/watchdog"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- checkDaemonWatchdog tests ---

func TestCheckDaemonWatchdog_NoWatchdog(t *testing.T) {
	t.Parallel()

	watchdogPath := filepath.Join(t.TempDir(), "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	failedPath := checkDaemonWatchdog(watchdogPath, "/bin/daemon", nil, "", logger)
	if failedPath != "" {
		t.Errorf("failedPath = %q, want empty (no watchdog)", failedPath)
	}
}

func TestCheckDaemonWatchdog_SuccessfulExec(t *testing.T) {
	t.Parallel()

	watchdogDir := t.TempDir()
	watchdogPath := filepath.Join(watchdogDir, "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Write a watchdog as if the previous process wrote it before exec.
	err := watchdog.Write(watchdogPath, watchdog.State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-daemon/bin/bureau-daemon",
		Timestamp:      time.Now(),
	})
	if err != nil {
		t.Fatalf("writing watchdog: %v", err)
	}

	// Current binary matches NewBinary → successful transition.
	failedPath := checkDaemonWatchdog(
		watchdogPath,
		"/nix/store/new-daemon/bin/bureau-daemon",
		nil, "", logger,
	)
	if failedPath != "" {
		t.Errorf("failedPath = %q, want empty (successful exec)", failedPath)
	}

	// Watchdog should be cleared.
	if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
		t.Error("watchdog file should be removed after successful exec detection")
	}
}

func TestCheckDaemonWatchdog_FailedExec(t *testing.T) {
	t.Parallel()

	watchdogDir := t.TempDir()
	watchdogPath := filepath.Join(watchdogDir, "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	err := watchdog.Write(watchdogPath, watchdog.State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-daemon/bin/bureau-daemon",
		Timestamp:      time.Now(),
	})
	if err != nil {
		t.Fatalf("writing watchdog: %v", err)
	}

	// Current binary matches PreviousBinary → the new binary failed,
	// old binary was restarted.
	failedPath := checkDaemonWatchdog(
		watchdogPath,
		"/nix/store/old-daemon/bin/bureau-daemon",
		nil, "", logger,
	)
	if failedPath != "/nix/store/new-daemon/bin/bureau-daemon" {
		t.Errorf("failedPath = %q, want %q", failedPath, "/nix/store/new-daemon/bin/bureau-daemon")
	}

	// Watchdog should be cleared.
	if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
		t.Error("watchdog file should be removed after failed exec detection")
	}
}

func TestCheckDaemonWatchdog_StaleWatchdog(t *testing.T) {
	t.Parallel()

	watchdogDir := t.TempDir()
	watchdogPath := filepath.Join(watchdogDir, "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Write a watchdog with an old timestamp (beyond watchdogMaxAge).
	err := watchdog.Write(watchdogPath, watchdog.State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-daemon/bin/bureau-daemon",
		Timestamp:      time.Now().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("writing watchdog: %v", err)
	}

	// Stale watchdog should be ignored.
	failedPath := checkDaemonWatchdog(
		watchdogPath,
		"/nix/store/old-daemon/bin/bureau-daemon",
		nil, "", logger,
	)
	if failedPath != "" {
		t.Errorf("failedPath = %q, want empty (stale watchdog)", failedPath)
	}
}

func TestCheckDaemonWatchdog_NeitherMatch(t *testing.T) {
	t.Parallel()

	watchdogDir := t.TempDir()
	watchdogPath := filepath.Join(watchdogDir, "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	err := watchdog.Write(watchdogPath, watchdog.State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-daemon/bin/bureau-daemon",
		Timestamp:      time.Now(),
	})
	if err != nil {
		t.Fatalf("writing watchdog: %v", err)
	}

	// Current binary is a third path (manual deployment, etc.).
	failedPath := checkDaemonWatchdog(
		watchdogPath,
		"/nix/store/third-daemon/bin/bureau-daemon",
		nil, "", logger,
	)
	if failedPath != "" {
		t.Errorf("failedPath = %q, want empty (neither match)", failedPath)
	}

	// Watchdog should be cleared.
	if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
		t.Error("watchdog file should be removed when neither path matches")
	}
}

func TestCheckDaemonWatchdog_ReportsToMatrix(t *testing.T) {
	t.Parallel()

	watchdogDir := t.TempDir()
	watchdogPath := filepath.Join(watchdogDir, "daemon-watchdog.json")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	err := watchdog.Write(watchdogPath, watchdog.State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-daemon/bin/bureau-daemon",
		Timestamp:      time.Now(),
	})
	if err != nil {
		t.Fatalf("writing watchdog: %v", err)
	}

	// Set up a mock Matrix server that captures sent messages.
	var (
		messageMu   sync.Mutex
		sentMessage string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/send/") {
			var body map[string]any
			json.NewDecoder(request.Body).Decode(&body)
			messageMu.Lock()
			sentMessage, _ = body["body"].(string)
			messageMu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"event_id":"$test"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{}`)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Successful exec — should report success.
	failedPath := checkDaemonWatchdog(
		watchdogPath,
		"/nix/store/new-daemon/bin/bureau-daemon",
		session,
		"!config:test.local",
		logger,
	)
	if failedPath != "" {
		t.Errorf("failedPath = %q, want empty", failedPath)
	}

	messageMu.Lock()
	defer messageMu.Unlock()
	if !strings.Contains(sentMessage, "succeeded") {
		t.Errorf("matrix message = %q, want substring 'succeeded'", sentMessage)
	}
}

// --- execDaemon tests ---

func TestExecDaemon_WritesWatchdogAndCallsExec(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	watchdogPath := filepath.Join(stateDir, "daemon-watchdog.json")

	var (
		execMu     sync.Mutex
		execCalled bool
		execBinary string
		execArgv   []string
	)

	daemon := &Daemon{
		clock:            clock.Real(),
		runDir:           principal.DefaultRunDir,
		daemonBinaryPath: "/nix/store/old-daemon/bin/bureau-daemon",
		daemonBinaryHash: "abcd1234",
		stateDir:         stateDir,
		failedExecPaths:  make(map[string]bool),
		session:          newNoopSession(t),
		configRoomID:     "!config:test.local",
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(binary string, argv []string, env []string) error {
			execMu.Lock()
			defer execMu.Unlock()
			execCalled = true
			execBinary = binary
			execArgv = argv
			// Return an error to simulate exec failure (so the test
			// process isn't actually replaced).
			return errors.New("simulated exec failure")
		},
	}

	err := daemon.execDaemon(context.Background(), "/nix/store/new-daemon/bin/bureau-daemon")
	if err == nil {
		t.Fatal("expected error from simulated exec failure")
	}

	// Verify exec was called with the correct binary and argv.
	execMu.Lock()
	defer execMu.Unlock()
	if !execCalled {
		t.Fatal("execFunc was not called")
	}
	if execBinary != "/nix/store/new-daemon/bin/bureau-daemon" {
		t.Errorf("exec binary = %q, want %q", execBinary, "/nix/store/new-daemon/bin/bureau-daemon")
	}
	if len(execArgv) == 0 || execArgv[0] != "/nix/store/new-daemon/bin/bureau-daemon" {
		t.Errorf("exec argv[0] = %q, want %q", execArgv[0], "/nix/store/new-daemon/bin/bureau-daemon")
	}

	// Watchdog should be cleared (exec failed).
	if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
		t.Error("watchdog should be cleared after exec failure")
	}

	// Path should be in failedExecPaths.
	if !daemon.failedExecPaths["/nix/store/new-daemon/bin/bureau-daemon"] {
		t.Error("failedExecPaths should contain the failed path")
	}
}

func TestExecDaemon_WatchdogWrittenBeforeExec(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	watchdogPath := filepath.Join(stateDir, "daemon-watchdog.json")

	// Capture whether the watchdog exists when exec is called.
	var watchdogExistedAtExecTime bool

	daemon := &Daemon{
		clock:            clock.Real(),
		runDir:           principal.DefaultRunDir,
		daemonBinaryPath: "/nix/store/old-daemon/bin/bureau-daemon",
		stateDir:         stateDir,
		failedExecPaths:  make(map[string]bool),
		session:          newNoopSession(t),
		configRoomID:     "!config:test.local",
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(binary string, argv []string, env []string) error {
			// Check if the watchdog was written before we got called.
			state, err := watchdog.Read(watchdogPath)
			watchdogExistedAtExecTime = err == nil && state.NewBinary == binary
			return errors.New("simulated")
		},
	}

	daemon.execDaemon(context.Background(), "/nix/store/new-daemon/bin/bureau-daemon")

	if !watchdogExistedAtExecTime {
		t.Error("watchdog should be written before exec() is called")
	}
}

func TestExecDaemon_ExecFailure(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	var (
		messageMu sync.Mutex
		messages  []string
	)

	matrixServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/send/") {
			var body map[string]any
			json.NewDecoder(request.Body).Decode(&body)
			messageMu.Lock()
			if bodyText, ok := body["body"].(string); ok {
				messages = append(messages, bodyText)
			}
			messageMu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"event_id":"$test"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{}`)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		clock:            clock.Real(),
		runDir:           principal.DefaultRunDir,
		daemonBinaryPath: "/nix/store/old-daemon/bin/bureau-daemon",
		stateDir:         stateDir,
		failedExecPaths:  make(map[string]bool),
		session:          session,
		configRoomID:     "!config:test.local",
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(binary string, argv []string, env []string) error {
			return errors.New("permission denied")
		},
	}

	err = daemon.execDaemon(context.Background(), "/nix/store/new-daemon/bin/bureau-daemon")
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want substring 'permission denied'", err.Error())
	}

	// Should have sent two messages: pre-exec and failure.
	messageMu.Lock()
	defer messageMu.Unlock()
	if len(messages) < 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0], "self-updating") {
		t.Errorf("first message = %q, want substring 'self-updating'", messages[0])
	}
	if !strings.Contains(messages[1], "failed") {
		t.Errorf("second message = %q, want substring 'failed'", messages[1])
	}
}

func TestExecDaemon_RetryProtection(t *testing.T) {
	t.Parallel()

	execCalled := false
	daemon := &Daemon{
		clock:            clock.Real(),
		runDir:           principal.DefaultRunDir,
		daemonBinaryPath: "/nix/store/old-daemon/bin/bureau-daemon",
		stateDir:         t.TempDir(),
		failedExecPaths: map[string]bool{
			"/nix/store/broken-daemon/bin/bureau-daemon": true,
		},
		session:      newNoopSession(t),
		configRoomID: "!config:test.local",
		logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(binary string, argv []string, env []string) error {
			execCalled = true
			return errors.New("should not be called")
		},
	}

	// Attempt exec for the already-failed path.
	err := daemon.execDaemon(context.Background(), "/nix/store/broken-daemon/bin/bureau-daemon")
	if err != nil {
		t.Errorf("expected nil error for skipped exec, got: %v", err)
	}
	if execCalled {
		t.Error("exec should not be called for a path in failedExecPaths")
	}
}

func TestExecDaemon_EmptyBinaryPath(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		clock:            clock.Real(),
		runDir:           principal.DefaultRunDir,
		daemonBinaryPath: "", // unknown
		stateDir:         t.TempDir(),
		failedExecPaths:  make(map[string]bool),
		session:          newNoopSession(t),
		configRoomID:     "!config:test.local",
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(binary string, argv []string, env []string) error {
			t.Fatal("exec should not be called when binary path is unknown")
			return nil
		},
	}

	err := daemon.execDaemon(context.Background(), "/nix/store/new-daemon/bin/bureau-daemon")
	if err == nil {
		t.Fatal("expected error when daemon binary path is empty")
	}
	if !strings.Contains(err.Error(), "binary path unknown") {
		t.Errorf("error = %q, want substring 'binary path unknown'", err.Error())
	}
}

// --- Integration test: reconcileBureauVersion triggers exec ---

func TestReconcileBureauVersion_DaemonChanged_TriggersExec(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	// Create a real binary to hash (reuse the test binary itself).
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	testHash, err := binhash.HashFile(testBinary)
	if err != nil {
		t.Fatalf("hashing test binary: %v", err)
	}

	// Create a "new daemon" binary (different content → different hash).
	newDaemonDir := t.TempDir()
	newDaemonPath := filepath.Join(newDaemonDir, "bureau-daemon-new")
	if err := os.WriteFile(newDaemonPath, []byte("different-binary-content"), 0755); err != nil {
		t.Fatalf("writing new daemon binary: %v", err)
	}

	// Mock Matrix: BureauVersion points to the new daemon binary.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		BureauVersion: &schema.BureauVersion{
			DaemonStorePath: newDaemonPath,
		},
	})

	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Mock launcher that responds to status queries.
	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action == "status" {
			return launcherIPCResponse{
				OK:         true,
				BinaryHash: "launcher-hash-irrelevant",
			}
		}
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	var (
		execMu     sync.Mutex
		execCalled bool
		execBinary string
	)

	daemon := &Daemon{
		clock:             clock.Real(),
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       machineName,
		serverName:        serverName,
		configRoomID:      configRoomID,
		launcherSocket:    launcherSocket,
		daemonBinaryHash:  binhash.FormatDigest(testHash),
		daemonBinaryPath:  testBinary,
		stateDir:          t.TempDir(),
		failedExecPaths:   make(map[string]bool),
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastMatrixPolicy:  make(map[string]*schema.MatrixPolicy),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		lastSpecs:         make(map[string]*schema.SandboxSpec),
		previousSpecs:     make(map[string]*schema.SandboxSpec),
		lastTemplates:     make(map[string]*schema.TemplateContent),
		healthMonitors:    make(map[string]*healthMonitor),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(socketDir, localpart+".admin.sock")
		},
		layoutWatchers: make(map[string]*layoutWatcher),
		logger:         slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return nil // Skip real Nix operations.
		},
		execFunc: func(binary string, argv []string, env []string) error {
			execMu.Lock()
			defer execMu.Unlock()
			execCalled = true
			execBinary = binary
			return errors.New("simulated exec for testing")
		},
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// Run reconcile — should detect daemon change and call exec.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	execMu.Lock()
	defer execMu.Unlock()
	if !execCalled {
		t.Error("exec should have been called for daemon binary change")
	}
	if execBinary != newDaemonPath {
		t.Errorf("exec binary = %q, want %q", execBinary, newDaemonPath)
	}
}

// --- Helpers ---

// newNoopSession creates a messaging.Session connected to a mock server
// that accepts all requests. Used for tests that need a valid session
// but don't care about Matrix interactions.
func newNoopSession(t *testing.T) *messaging.Session {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"event_id":"$noop"}`)
	}))
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("newNoopSession: creating client: %v", err)
	}
	session, err := client.SessionFromToken("@noop:test.local", "noop-token")
	if err != nil {
		t.Fatalf("newNoopSession: creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}
