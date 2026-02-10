// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
	socketPath := filepath.Join(t.TempDir(), "tmux.sock")
	t.Cleanup(func() {
		// Kill the test tmux server and all its sessions. Ignore errors
		// because the server may not have been started (e.g., if the test
		// failed before creating any sessions).
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
