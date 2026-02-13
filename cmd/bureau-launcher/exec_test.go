// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/lib/watchdog"
)

func TestWriteAndReadStateFile(t *testing.T) {
	runDir := testutil.SocketDir(t)
	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		proxyBinaryPath: "/nix/store/abc-bureau-proxy/bin/bureau-proxy",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Add some sandbox entries.
	launcher.sandboxes["test/echo"] = &managedSandbox{
		localpart:    "test/echo",
		proxyProcess: &os.Process{Pid: 12345},
		configDir:    "/tmp/bureau-proxy-test-echo-abc",
		done:         make(chan struct{}),
		roles: map[string][]string{
			"worker": {"python", "worker.py"},
		},
	}
	launcher.sandboxes["service/stt"] = &managedSandbox{
		localpart:    "service/stt",
		proxyProcess: &os.Process{Pid: 67890},
		configDir:    "/tmp/bureau-proxy-service-stt-def",
		done:         make(chan struct{}),
	}

	// Also add an already-exited sandbox — it should be skipped.
	exitedDone := make(chan struct{})
	close(exitedDone)
	launcher.sandboxes["dead/process"] = &managedSandbox{
		localpart:    "dead/process",
		proxyProcess: &os.Process{Pid: 99999},
		configDir:    "/tmp/bureau-proxy-dead",
		done:         exitedDone,
	}

	// Write the state file.
	if err := launcher.writeStateFile(); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}

	// Read it back and verify.
	data, err := os.ReadFile(launcher.launcherStatePath())
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	var state launcherState
	if err := codec.Unmarshal(data, &state); err != nil {
		t.Fatalf("parsing state file: %v", err)
	}

	if state.ProxyBinaryPath != "/nix/store/abc-bureau-proxy/bin/bureau-proxy" {
		t.Errorf("ProxyBinaryPath = %q, want /nix/store/abc-bureau-proxy/bin/bureau-proxy", state.ProxyBinaryPath)
	}

	// Only the two alive sandboxes should be present.
	if len(state.Sandboxes) != 2 {
		t.Fatalf("len(Sandboxes) = %d, want 2", len(state.Sandboxes))
	}

	echo := state.Sandboxes["test/echo"]
	if echo == nil {
		t.Fatal("missing sandbox entry for test/echo")
	}
	if echo.ProxyPID != 12345 {
		t.Errorf("test/echo ProxyPID = %d, want 12345", echo.ProxyPID)
	}
	if echo.ConfigDir != "/tmp/bureau-proxy-test-echo-abc" {
		t.Errorf("test/echo ConfigDir = %q, want /tmp/bureau-proxy-test-echo-abc", echo.ConfigDir)
	}
	if len(echo.Roles) != 1 || echo.Roles["worker"][0] != "python" {
		t.Errorf("test/echo Roles = %v, want worker: [python worker.py]", echo.Roles)
	}

	stt := state.Sandboxes["service/stt"]
	if stt == nil {
		t.Fatal("missing sandbox entry for service/stt")
	}
	if stt.ProxyPID != 67890 {
		t.Errorf("service/stt ProxyPID = %d, want 67890", stt.ProxyPID)
	}

	// Verify dead/process was excluded.
	if _, exists := state.Sandboxes["dead/process"]; exists {
		t.Error("dead/process should have been excluded from state file (already exited)")
	}
}

func TestClearStateFile(t *testing.T) {
	runDir := testutil.SocketDir(t)
	launcher := &Launcher{
		runDir:   runDir,
		stateDir: t.TempDir(),
		logger:   slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Write something to clear.
	statePath := launcher.launcherStatePath()
	if err := os.WriteFile(statePath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	launcher.clearStateFile()

	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should not exist after clear, got err: %v", err)
	}

	// Clearing a non-existent file should be harmless.
	launcher.clearStateFile()
}

func TestReconnectSandboxes(t *testing.T) {
	runDir := testutil.SocketDir(t)

	// Start a real subprocess that we can reconnect to.
	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	sleepPID := sleepCmd.Process.Pid
	t.Cleanup(func() {
		sleepCmd.Process.Kill()
		sleepCmd.Wait()
	})

	// Create the admin socket so the reconnect check passes.
	adminSocketDir := filepath.Dir(principal.RunDirAdminSocketPath(runDir, "test/reconnect"))
	if err := os.MkdirAll(adminSocketDir, 0755); err != nil {
		t.Fatal(err)
	}
	adminSocketPath := principal.RunDirAdminSocketPath(runDir, "test/reconnect")
	// Create a dummy socket file (just needs to exist for the stat check).
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: adminSocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	configDir := t.TempDir()

	// Write state file with the sleep process as the proxy.
	state := launcherState{
		Sandboxes: map[string]*sandboxEntry{
			"test/reconnect": {
				ProxyPID:  sleepPID,
				ConfigDir: configDir,
				Roles:     map[string][]string{"worker": {"sleep", "60"}},
			},
		},
		ProxyBinaryPath: "/test/proxy/binary",
	}
	stateData, _ := codec.Marshal(state)
	statePath := filepath.Join(runDir, "launcher-state.cbor")
	if err := os.WriteFile(statePath, stateData, 0600); err != nil {
		t.Fatal(err)
	}

	tmuxServer := tmux.NewTestServer(t)

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		proxyBinaryPath: "",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		tmuxServer:      tmuxServer,
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	if err := launcher.reconnectSandboxes(); err != nil {
		t.Fatalf("reconnectSandboxes: %v", err)
	}

	// Verify the sandbox was reconnected.
	sandbox, exists := launcher.sandboxes["test/reconnect"]
	if !exists {
		t.Fatal("sandbox test/reconnect not found after reconnect")
	}
	if sandbox.proxyProcess.Pid != sleepPID {
		t.Errorf("proxy PID = %d, want %d", sandbox.proxyProcess.Pid, sleepPID)
	}
	if sandbox.configDir != configDir {
		t.Errorf("configDir = %q, want %q", sandbox.configDir, configDir)
	}
	if len(sandbox.roles) != 1 {
		t.Errorf("roles = %v, want 1 entry", sandbox.roles)
	}

	// Verify the proxy binary path was restored.
	if launcher.proxyBinaryPath != "/test/proxy/binary" {
		t.Errorf("proxyBinaryPath = %q, want /test/proxy/binary", launcher.proxyBinaryPath)
	}

	// Verify the state file was deleted.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should be deleted after reconnect")
	}

	// The session watcher should detect that there is no tmux session
	// (we didn't create one in this test) and close the done channel.
	// This also exercises the "sandbox exits because session is gone"
	// path that happens after exec() reconnection when the tmux server
	// has died.
	select {
	case <-sandbox.done:
		// Session watcher detected no tmux session.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session watcher to close done channel")
	}
}

func TestReconnectDeadProcess(t *testing.T) {
	runDir := testutil.SocketDir(t)

	// Start and immediately kill a process to get a dead PID.
	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := sleepCmd.Process.Pid
	sleepCmd.Process.Kill()
	sleepCmd.Wait()

	configDir := t.TempDir()
	// Create a marker file to verify cleanup.
	markerPath := filepath.Join(configDir, "marker")
	os.WriteFile(markerPath, []byte("test"), 0600)

	// Write state file with the dead PID.
	state := launcherState{
		Sandboxes: map[string]*sandboxEntry{
			"test/dead": {
				ProxyPID:  deadPID,
				ConfigDir: configDir,
			},
		},
	}
	stateData, _ := codec.Marshal(state)
	statePath := filepath.Join(runDir, "launcher-state.cbor")
	os.WriteFile(statePath, stateData, 0600)

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	if err := launcher.reconnectSandboxes(); err != nil {
		t.Fatalf("reconnectSandboxes: %v", err)
	}

	// Dead process should not be in sandboxes.
	if _, exists := launcher.sandboxes["test/dead"]; exists {
		t.Error("dead process should not be reconnected")
	}

	// Config dir should have been cleaned up.
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Error("config dir should be cleaned up for dead process")
	}
}

func TestReconnectMissingSocket(t *testing.T) {
	runDir := testutil.SocketDir(t)

	// Start a real process (alive, but no admin socket).
	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	sleepPID := sleepCmd.Process.Pid
	// Don't defer cleanup — the reconnect code should kill it.

	configDir := t.TempDir()

	state := launcherState{
		Sandboxes: map[string]*sandboxEntry{
			"test/nosocket": {
				ProxyPID:  sleepPID,
				ConfigDir: configDir,
			},
		},
	}
	stateData, _ := codec.Marshal(state)
	statePath := filepath.Join(runDir, "launcher-state.cbor")
	os.WriteFile(statePath, stateData, 0600)

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	if err := launcher.reconnectSandboxes(); err != nil {
		t.Fatalf("reconnectSandboxes: %v", err)
	}

	// Process should have been killed and not reconnected.
	if _, exists := launcher.sandboxes["test/nosocket"]; exists {
		t.Error("process without admin socket should not be reconnected")
	}

	// Verify the process is actually dead (signal 0 should fail).
	process, _ := os.FindProcess(sleepPID)
	if err := process.Signal(syscall.Signal(0)); err == nil {
		// Process is still alive — kill it for cleanup.
		process.Kill()
		var status syscall.WaitStatus
		syscall.Wait4(sleepPID, &status, 0, nil)
		t.Error("process should have been killed when admin socket was missing")
	}
}

func TestReconnectNoStateFile(t *testing.T) {
	runDir := testutil.SocketDir(t)
	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// No state file — should be a no-op.
	if err := launcher.reconnectSandboxes(); err != nil {
		t.Fatalf("reconnectSandboxes with no state file: %v", err)
	}

	if len(launcher.sandboxes) != 0 {
		t.Error("sandboxes should be empty when no state file exists")
	}
}

func TestReconnectPreservesProxyBinaryPath(t *testing.T) {
	runDir := testutil.SocketDir(t)

	state := launcherState{
		Sandboxes:       make(map[string]*sandboxEntry),
		ProxyBinaryPath: "/nix/store/updated-proxy/bin/bureau-proxy",
	}
	stateData, _ := codec.Marshal(state)
	statePath := filepath.Join(runDir, "launcher-state.cbor")
	os.WriteFile(statePath, stateData, 0600)

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		proxyBinaryPath: "/nix/store/old-proxy/bin/bureau-proxy",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	if err := launcher.reconnectSandboxes(); err != nil {
		t.Fatalf("reconnectSandboxes: %v", err)
	}

	if launcher.proxyBinaryPath != "/nix/store/updated-proxy/bin/bureau-proxy" {
		t.Errorf("proxyBinaryPath = %q, want /nix/store/updated-proxy/bin/bureau-proxy",
			launcher.proxyBinaryPath)
	}
}

func TestHandleExecUpdate(t *testing.T) {
	runDir := testutil.SocketDir(t)
	stateDir := t.TempDir()

	// Create a temporary "binary" to validate against.
	binaryPath := filepath.Join(t.TempDir(), "bureau-launcher-new")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        stateDir,
		binaryPath:      "/current/launcher",
		proxyBinaryPath: "/proxy/binary",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	request := &IPCRequest{
		BinaryPath: binaryPath,
	}

	response := launcher.handleExecUpdate(context.Background(), request)

	if !response.OK {
		t.Fatalf("handleExecUpdate returned error: %s", response.Error)
	}

	// Verify state file was written.
	if _, err := os.Stat(launcher.launcherStatePath()); err != nil {
		t.Errorf("state file should exist after handleExecUpdate: %v", err)
	}

	// Verify watchdog was written.
	state, err := watchdog.Read(launcher.launcherWatchdogPath())
	if err != nil {
		t.Fatalf("reading watchdog: %v", err)
	}
	if state.Component != "launcher" {
		t.Errorf("watchdog component = %q, want launcher", state.Component)
	}
	if state.PreviousBinary != "/current/launcher" {
		t.Errorf("watchdog PreviousBinary = %q, want /current/launcher", state.PreviousBinary)
	}
	if state.NewBinary != binaryPath {
		t.Errorf("watchdog NewBinary = %q, want %q", state.NewBinary, binaryPath)
	}
}

func TestHandleExecUpdateFailedPath(t *testing.T) {
	runDir := testutil.SocketDir(t)
	binaryPath := "/some/failed/binary"

	launcher := &Launcher{
		runDir:     runDir,
		stateDir:   t.TempDir(),
		binaryPath: "/current/launcher",
		sandboxes:  make(map[string]*managedSandbox),
		failedExecPaths: map[string]bool{
			binaryPath: true,
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	request := &IPCRequest{BinaryPath: binaryPath}
	response := launcher.handleExecUpdate(context.Background(), request)

	if response.OK {
		t.Fatal("should reject previously failed exec path")
	}
	if response.Error == "" {
		t.Fatal("error message should not be empty")
	}

	// State file and watchdog should NOT have been written.
	if _, err := os.Stat(launcher.launcherStatePath()); !os.IsNotExist(err) {
		t.Error("state file should not exist for rejected exec")
	}
	if _, err := os.Stat(launcher.launcherWatchdogPath()); !os.IsNotExist(err) {
		t.Error("watchdog should not exist for rejected exec")
	}
}

func TestHandleExecUpdateBadBinary(t *testing.T) {
	runDir := testutil.SocketDir(t)
	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		binaryPath:      "/current/launcher",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("nonexistent binary", func(t *testing.T) {
		request := &IPCRequest{BinaryPath: "/nonexistent/binary"}
		response := launcher.handleExecUpdate(context.Background(), request)
		if response.OK {
			t.Fatal("should reject nonexistent binary")
		}
	})

	t.Run("empty binary path", func(t *testing.T) {
		request := &IPCRequest{BinaryPath: ""}
		response := launcher.handleExecUpdate(context.Background(), request)
		if response.OK {
			t.Fatal("should reject empty binary path")
		}
	})

	t.Run("unknown launcher path", func(t *testing.T) {
		launcher.binaryPath = ""
		binaryPath := filepath.Join(t.TempDir(), "binary")
		os.WriteFile(binaryPath, []byte("test"), 0755)
		request := &IPCRequest{BinaryPath: binaryPath}
		response := launcher.handleExecUpdate(context.Background(), request)
		if response.OK {
			t.Fatal("should reject when launcher binary path is unknown")
		}
	})
}

func TestPerformExecFailure(t *testing.T) {
	runDir := testutil.SocketDir(t)
	stateDir := t.TempDir()

	// Pre-create the state file and watchdog so we can verify they are
	// cleaned up after exec failure.
	statePath := filepath.Join(runDir, "launcher-state.cbor")
	os.WriteFile(statePath, []byte("{}"), 0600)

	watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
	watchdog.Write(watchdogPath, watchdog.State{
		Component: "launcher",
		Timestamp: time.Now(),
	})

	execCallCount := 0
	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        stateDir,
		binaryPath:      "/current/launcher",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(argv0 string, argv []string, envv []string) error {
			execCallCount++
			return fmt.Errorf("exec not permitted in test")
		},
	}

	launcher.performExec("/new/launcher")

	// execFunc should have been called.
	if execCallCount != 1 {
		t.Errorf("execFunc called %d times, want 1", execCallCount)
	}

	// State file should be cleaned up.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should be cleaned up after exec failure")
	}

	// Watchdog should be cleaned up.
	if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
		t.Error("watchdog should be cleaned up after exec failure")
	}

	// Path should be recorded as failed.
	if !launcher.failedExecPaths["/new/launcher"] {
		t.Error("failed path should be recorded in failedExecPaths")
	}
}

func TestPerformExecCallsExecWithCorrectArgs(t *testing.T) {
	runDir := testutil.SocketDir(t)

	var capturedArgv0 string
	var capturedArgv []string

	launcher := &Launcher{
		runDir:          runDir,
		stateDir:        t.TempDir(),
		binaryPath:      "/current/launcher",
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		execFunc: func(argv0 string, argv []string, envv []string) error {
			capturedArgv0 = argv0
			capturedArgv = argv
			return fmt.Errorf("exec not permitted in test")
		},
	}

	launcher.performExec("/nix/store/new-launcher/bin/bureau-launcher")

	if capturedArgv0 != "/nix/store/new-launcher/bin/bureau-launcher" {
		t.Errorf("argv0 = %q, want /nix/store/new-launcher/bin/bureau-launcher", capturedArgv0)
	}
	// argv[0] should be the new binary, followed by os.Args[1:]
	if len(capturedArgv) < 1 || capturedArgv[0] != "/nix/store/new-launcher/bin/bureau-launcher" {
		t.Errorf("argv[0] = %q, want /nix/store/new-launcher/bin/bureau-launcher", capturedArgv[0])
	}
}

func TestCheckLauncherWatchdog(t *testing.T) {
	t.Run("no watchdog", func(t *testing.T) {
		stateDir := t.TempDir()
		watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		failedPath := checkLauncherWatchdog(watchdogPath, "/current/binary", logger)
		if failedPath != "" {
			t.Errorf("failedPath = %q, want empty (no watchdog)", failedPath)
		}
	})

	t.Run("exec succeeded", func(t *testing.T) {
		stateDir := t.TempDir()
		watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		watchdog.Write(watchdogPath, watchdog.State{
			Component:      "launcher",
			PreviousBinary: "/old/launcher",
			NewBinary:      "/new/launcher",
			Timestamp:      time.Now(),
		})

		failedPath := checkLauncherWatchdog(watchdogPath, "/new/launcher", logger)
		if failedPath != "" {
			t.Errorf("failedPath = %q, want empty (exec succeeded)", failedPath)
		}

		// Watchdog should be cleared.
		if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
			t.Error("watchdog should be cleared after success")
		}
	})

	t.Run("exec failed - old binary restarted", func(t *testing.T) {
		stateDir := t.TempDir()
		watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		watchdog.Write(watchdogPath, watchdog.State{
			Component:      "launcher",
			PreviousBinary: "/old/launcher",
			NewBinary:      "/new/launcher",
			Timestamp:      time.Now(),
		})

		failedPath := checkLauncherWatchdog(watchdogPath, "/old/launcher", logger)
		if failedPath != "/new/launcher" {
			t.Errorf("failedPath = %q, want /new/launcher", failedPath)
		}

		// Watchdog should be cleared.
		if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
			t.Error("watchdog should be cleared after failure detection")
		}
	})

	t.Run("stale watchdog - neither match", func(t *testing.T) {
		stateDir := t.TempDir()
		watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		watchdog.Write(watchdogPath, watchdog.State{
			Component:      "launcher",
			PreviousBinary: "/old/launcher",
			NewBinary:      "/new/launcher",
			Timestamp:      time.Now(),
		})

		failedPath := checkLauncherWatchdog(watchdogPath, "/completely/different", logger)
		if failedPath != "" {
			t.Errorf("failedPath = %q, want empty (stale watchdog)", failedPath)
		}

		// Stale watchdog should be cleared.
		if _, err := os.Stat(watchdogPath); !os.IsNotExist(err) {
			t.Error("stale watchdog should be cleared")
		}
	})

	t.Run("expired watchdog", func(t *testing.T) {
		stateDir := t.TempDir()
		watchdogPath := filepath.Join(stateDir, "launcher-watchdog.cbor")
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		watchdog.Write(watchdogPath, watchdog.State{
			Component:      "launcher",
			PreviousBinary: "/old/launcher",
			NewBinary:      "/new/launcher",
			Timestamp:      time.Now().Add(-10 * time.Minute), // Expired.
		})

		failedPath := checkLauncherWatchdog(watchdogPath, "/old/launcher", logger)
		if failedPath != "" {
			t.Errorf("failedPath = %q, want empty (expired watchdog)", failedPath)
		}
	})
}
