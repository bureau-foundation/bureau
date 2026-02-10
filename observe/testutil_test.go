// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TmuxServer creates an isolated tmux server for testing. Returns the
// server socket path, which is inside t.TempDir() and automatically
// cleaned up. The test cleanup kills the tmux server on completion.
//
// CRITICAL: all tmux commands in tests MUST use the returned socket path
// via the -S flag. A bare tmux command without -S will hit the default
// tmux server, which may be the session the test agent is running in.
// Killing the default server kills the agent.
func TmuxServer(t *testing.T) string {
	t.Helper()

	socketPath := filepath.Join(testutil.SocketDir(t), "tmux.sock")

	// Create a guard session with -f /dev/null to prevent loading the
	// user's ~/.tmux.conf. The server starts when the first session is
	// created, and the config is only read at that point. Without this,
	// tests inherit whatever settings the user has (base-index, prefix,
	// etc.), making window indices and key bindings unpredictable.
	//
	// The guard session runs "sleep infinity" so it never exits, keeping
	// the server alive until our cleanup kills it.
	cmd := exec.Command("tmux", "-S", socketPath, "-f", "/dev/null",
		"new-session", "-d", "-s", "_guard", "sleep", "infinity")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start tmux test server: %v\n%s", err, output)
	}

	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socketPath, "kill-server").Run()
	})
	return socketPath
}

// TmuxSession creates a tmux session on the given test server. The
// session runs the specified command (or "cat" if empty, which blocks
// and accepts input — useful for testing terminal I/O). Returns the
// session name.
//
// The session is automatically killed when the test completes (via the
// TmuxServer cleanup).
func TmuxSession(t *testing.T, serverSocket, sessionName, command string) string {
	t.Helper()
	if command == "" {
		command = "cat"
	}
	cmd := exec.Command("tmux", "-S", serverSocket, "new-session", "-d",
		"-s", sessionName,
		"-x", "80", "-y", "24",
		command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create tmux session %q: %v\n%s", sessionName, err, output)
	}

	// Wait for the session to be ready. tmux -d returns immediately but
	// the session may not be queryable yet.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		check := exec.Command("tmux", "-S", serverSocket, "has-session", "-t", sessionName)
		if check.Run() == nil {
			return sessionName
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tmux session %q not ready after 5 seconds", sessionName)
	return sessionName
}

// TmuxSendKeys sends keystrokes to a tmux pane on the test server.
// Useful for driving terminal interactions in tests.
func TmuxSendKeys(t *testing.T, serverSocket, target, keys string) {
	t.Helper()
	cmd := exec.Command("tmux", "-S", serverSocket, "send-keys", "-t", target, keys)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("send-keys to %q: %v\n%s", target, err, output)
	}
}

// TmuxCapturePane captures the visible content of a tmux pane on the
// test server. Returns the pane content as a string.
func TmuxCapturePane(t *testing.T, serverSocket, target string) string {
	t.Helper()
	cmd := exec.Command("tmux", "-S", serverSocket, "capture-pane", "-t", target, "-p")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capture-pane %q: %v\n%s", target, err, output)
	}
	return string(output)
}

// mustTmux runs a tmux command via the production tmuxCommand function
// and fails the test on error.
func mustTmux(t *testing.T, serverSocket string, args ...string) {
	t.Helper()
	if _, err := tmuxCommand(serverSocket, args...); err != nil {
		t.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
}

// mustTmuxTrimmed runs a tmux command via the production tmuxCommand
// function and returns the trimmed output. Fails the test on error.
func mustTmuxTrimmed(t *testing.T, serverSocket string, args ...string) string {
	t.Helper()
	output, err := tmuxCommand(serverSocket, args...)
	if err != nil {
		t.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(output)
}

// countPanes returns the number of panes in the first window of a session.
func countPanes(t *testing.T, serverSocket, sessionName string) int {
	t.Helper()
	output := mustTmuxTrimmed(t, serverSocket, "list-panes", "-t", sessionName)
	return len(splitLines(output))
}
