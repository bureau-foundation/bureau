// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tmux_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/tmux"
)

func TestNewSession(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.NewSession("test-session", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if !server.HasSession("test-session") {
		t.Fatal("HasSession returned false for a session that was just created")
	}
}

func TestNewSessionWithCommand(t *testing.T) {
	server := tmux.NewTestServer(t)

	// Run a command that exits immediately. The session should disappear
	// after the command completes.
	if err := server.NewSession("ephemeral", "true"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Wait for tmux to notice the command exited, bounded by the
	// test context timeout.
	for server.HasSession("ephemeral") {
		if t.Context().Err() != nil {
			break
		}
		runtime.Gosched()
	}

	if server.HasSession("ephemeral") {
		t.Fatal("session still exists after command exited")
	}
}

func TestHasSessionReturnsFalseForMissing(t *testing.T) {
	server := tmux.NewTestServer(t)

	if server.HasSession("nonexistent") {
		t.Fatal("HasSession returned true for a session that does not exist")
	}
}

func TestKillSession(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.NewSession("doomed", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !server.HasSession("doomed") {
		t.Fatal("session not created")
	}

	if err := server.KillSession("doomed"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if server.HasSession("doomed") {
		t.Fatal("session still exists after KillSession")
	}
}

func TestKillSessionBenignWhenMissing(t *testing.T) {
	server := tmux.NewTestServer(t)

	// Killing a nonexistent session should not return an error.
	if err := server.KillSession("never-existed"); err != nil {
		t.Fatalf("KillSession on missing session returned error: %v", err)
	}
}

func TestKillServer(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.NewSession("session-a", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession a: %v", err)
	}
	if err := server.NewSession("session-b", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession b: %v", err)
	}

	if err := server.KillServer(); err != nil {
		t.Fatalf("KillServer: %v", err)
	}

	if server.HasSession("session-a") || server.HasSession("session-b") || server.HasSession("_guard") {
		t.Fatal("sessions still exist after KillServer")
	}
}

func TestKillServerBenignWhenStopped(t *testing.T) {
	server := tmux.NewTestServer(t)
	// Kill once to stop the server.
	server.KillServer()

	// Kill again — should not error.
	if err := server.KillServer(); err != nil {
		t.Fatalf("KillServer on stopped server returned error: %v", err)
	}
}

func TestSetOptionGlobal(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.SetOption("", "prefix", "C-a"); err != nil {
		t.Fatalf("SetOption global: %v", err)
	}

	// Read the option back.
	output, err := server.Run("show-option", "-gv", "prefix")
	if err != nil {
		t.Fatalf("show-option: %v", err)
	}
	if got := strings.TrimSpace(output); got != "C-a" {
		t.Fatalf("global prefix = %q, want %q", got, "C-a")
	}
}

func TestSetOptionPerSession(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.NewSession("opt-test", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := server.SetOption("opt-test", "status-left", "hello"); err != nil {
		t.Fatalf("SetOption per-session: %v", err)
	}

	output, err := server.Run("show-option", "-t", "opt-test", "-v", "status-left")
	if err != nil {
		t.Fatalf("show-option: %v", err)
	}
	if got := strings.TrimSpace(output); got != "hello" {
		t.Fatalf("status-left = %q, want %q", got, "hello")
	}
}

func TestRun(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.NewSession("run-test", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	output, err := server.Run("list-windows", "-t", "run-test", "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("Run list-windows: %v", err)
	}
	// The default window name is usually "sleep" (the running command).
	if strings.TrimSpace(output) == "" {
		t.Fatal("list-windows returned empty output")
	}
}

func TestSocketPath(t *testing.T) {
	socketPath := "/tmp/test-tmux.sock"
	server := tmux.NewServer(socketPath, "/dev/null")

	if got := server.SocketPath(); got != socketPath {
		t.Fatalf("SocketPath() = %q, want %q", got, socketPath)
	}
}

func TestNewTestServerIsolation(t *testing.T) {
	serverA := tmux.NewTestServer(t)
	serverB := tmux.NewTestServer(t)

	if err := serverA.NewSession("only-on-a", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession on A: %v", err)
	}

	if serverB.HasSession("only-on-a") {
		t.Fatal("server B can see a session from server A — servers are not isolated")
	}
}

func TestCapturePane(t *testing.T) {
	server := tmux.NewTestServer(t)

	// Enable remain-on-exit so the pane stays alive after the command exits.
	// This mirrors how the launcher configures Bureau's tmux server.
	if err := server.SetOption("", "remain-on-exit", "on"); err != nil {
		t.Fatalf("SetOption remain-on-exit: %v", err)
	}

	// Run a command that prints known output and exits.
	if err := server.NewSession("capture-test", "sh", "-c", "echo 'hello from sandbox'; echo 'error: something broke' >&2"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Wait for the pane to become dead (command exited, but session persists).
	for {
		output, err := server.Run("list-panes", "-t", "capture-test", "-F", "#{pane_dead}")
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		if strings.TrimSpace(output) == "1" {
			break
		}
		if t.Context().Err() != nil {
			t.Fatal("timed out waiting for pane to become dead")
		}
		runtime.Gosched()
	}

	// Session should still exist (remain-on-exit).
	if !server.HasSession("capture-test") {
		t.Fatal("session disappeared despite remain-on-exit")
	}

	// Capture the pane content.
	captured, err := server.CapturePane("capture-test", 0)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}

	if !strings.Contains(captured, "hello from sandbox") {
		t.Errorf("captured output missing stdout content, got: %q", captured)
	}
	if !strings.Contains(captured, "error: something broke") {
		t.Errorf("captured output missing stderr content, got: %q", captured)
	}
}

func TestCapturePaneWithMaxLines(t *testing.T) {
	server := tmux.NewTestServer(t)

	if err := server.SetOption("", "remain-on-exit", "on"); err != nil {
		t.Fatalf("SetOption remain-on-exit: %v", err)
	}

	// Print 10 numbered lines.
	if err := server.NewSession("capture-limit", "sh", "-c",
		"for i in 1 2 3 4 5 6 7 8 9 10; do echo \"line $i\"; done"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Wait for pane to become dead.
	for {
		output, err := server.Run("list-panes", "-t", "capture-limit", "-F", "#{pane_dead}")
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		if strings.TrimSpace(output) == "1" {
			break
		}
		if t.Context().Err() != nil {
			t.Fatal("timed out waiting for pane to become dead")
		}
		runtime.Gosched()
	}

	// Capture with limit of 3 lines.
	captured, err := server.CapturePane("capture-limit", 3)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}

	// Should contain the last lines but not the first ones.
	lines := strings.Split(strings.TrimRight(captured, "\n"), "\n")
	if len(lines) > 3 {
		t.Errorf("expected at most 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestConfigIsolation(t *testing.T) {
	// Create a custom tmux.conf that sets a distinctive option.
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "tmux.conf")
	if err := os.WriteFile(configPath, []byte("set-option -g history-limit 99999\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Server with custom config — should have history-limit 99999.
	socketA := filepath.Join(testutil.SocketDir(t), "a.sock")
	serverA := tmux.NewServer(socketA, configPath)
	if err := serverA.NewSession("_guard", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession on A: %v", err)
	}
	t.Cleanup(func() { serverA.KillServer() })

	outputA, err := serverA.Run("show-option", "-gv", "history-limit")
	if err != nil {
		t.Fatalf("show-option on A: %v", err)
	}
	if got := strings.TrimSpace(outputA); got != "99999" {
		t.Fatalf("server A history-limit = %q, want 99999 (custom config not loaded)", got)
	}

	// Server with /dev/null config — should have the tmux default (2000).
	socketB := filepath.Join(testutil.SocketDir(t), "b.sock")
	serverB := tmux.NewServer(socketB, "/dev/null")
	if err := serverB.NewSession("_guard", "sleep", "infinity"); err != nil {
		t.Fatalf("NewSession on B: %v", err)
	}
	t.Cleanup(func() { serverB.KillServer() })

	outputB, err := serverB.Run("show-option", "-gv", "history-limit")
	if err != nil {
		t.Fatalf("show-option on B: %v", err)
	}
	if got := strings.TrimSpace(outputB); got == "99999" {
		t.Fatal("server B has history-limit 99999 — /dev/null config did not prevent custom config loading")
	}
}
