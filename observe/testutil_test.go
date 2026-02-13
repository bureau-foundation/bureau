// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/tmux"
)

// TmuxServer creates an isolated tmux server for testing. Returns the
// server, which is automatically cleaned up when the test completes.
//
// This is a thin wrapper around tmux.NewTestServer for observe package
// tests. External packages should use tmux.NewTestServer directly.
func TmuxServer(t *testing.T) *tmux.Server {
	t.Helper()
	return tmux.NewTestServer(t)
}

// TmuxSession creates a tmux session on the given test server. The
// session runs the specified command (or "cat" if empty, which blocks
// and accepts input — useful for testing terminal I/O). Returns the
// session name.
//
// The session is automatically killed when the test completes (via the
// server's cleanup).
func TmuxSession(t *testing.T, server *tmux.Server, sessionName, command string) string {
	t.Helper()
	if command == "" {
		command = "cat"
	}
	_, err := server.Run("new-session", "-d",
		"-s", sessionName,
		"-x", "80", "-y", "24",
		command)
	if err != nil {
		t.Fatalf("create tmux session %q: %v", sessionName, err)
	}

	// Wait for the session to be ready. tmux -d returns immediately but
	// the session may not be queryable yet. Use the test context for the
	// overall deadline and yield between polls.
	for {
		if server.HasSession(sessionName) {
			return sessionName
		}
		if t.Context().Err() != nil {
			t.Fatalf("tmux session %q not ready before test deadline", sessionName)
		}
		runtime.Gosched()
	}
}

// TmuxSendKeys sends keystrokes to a tmux pane on the test server.
// Useful for driving terminal interactions in tests.
func TmuxSendKeys(t *testing.T, server *tmux.Server, target, keys string) {
	t.Helper()
	if _, err := server.Run("send-keys", "-t", target, keys); err != nil {
		t.Fatalf("send-keys to %q: %v", target, err)
	}
}

// TmuxCapturePane captures the visible content of a tmux pane on the
// test server. Returns the pane content as a string.
func TmuxCapturePane(t *testing.T, server *tmux.Server, target string) string {
	t.Helper()
	output, err := server.Run("capture-pane", "-t", target, "-p")
	if err != nil {
		t.Fatalf("capture-pane %q: %v", target, err)
	}
	return output
}

// mustTmux runs a tmux command via the Server.Run method and fails the
// test on error.
func mustTmux(t *testing.T, server *tmux.Server, args ...string) {
	t.Helper()
	if _, err := server.Run(args...); err != nil {
		t.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
}

// mustTmuxTrimmed runs a tmux command via Server.Run and returns the
// trimmed output. Fails the test on error.
func mustTmuxTrimmed(t *testing.T, server *tmux.Server, args ...string) string {
	t.Helper()
	output, err := server.Run(args...)
	if err != nil {
		t.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(output)
}

// countPanes returns the number of panes in the first window of a session.
func countPanes(t *testing.T, server *tmux.Server, sessionName string) int {
	t.Helper()
	output := mustTmuxTrimmed(t, server, "list-panes", "-t", sessionName)
	return len(splitLines(output))
}

// pollTmuxContent polls a tmux pane until the expected substring appears
// in the captured content, or the test deadline expires. This is for
// waiting on external tmux process output that has no event-based
// notification mechanism.
func pollTmuxContent(t *testing.T, server *tmux.Server, target, expected string) {
	t.Helper()
	var lastContent string
	for {
		lastContent = TmuxCapturePane(t, server, target)
		if strings.Contains(lastContent, expected) {
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("timed out waiting for %q in tmux pane %s (last capture: %q)",
				expected, target, lastContent)
		}
		runtime.Gosched()
	}
}
