// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"context"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TestControlClientDebounce verifies that rapid layout notifications
// coalesce into a single LayoutChanged event after the debounce interval.
func TestControlClientDebounce(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/debounce", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a 1 second debounce so the sequential split-window commands
	// (each spawns a tmux subprocess taking ~50-100ms) all land within
	// a single debounce window.
	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(1*time.Second))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	// Wait for the control client to attach before triggering events.
	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")

	// Split the window three times in rapid succession. Each split
	// produces a %layout-change notification. With 1s debounce,
	// they should coalesce into one event.
	for range 3 {
		mustTmux(t, server, "split-window", "-t", sessionName, "-v")
	}

	// Wait for exactly one event.
	event := testutil.RequireReceive(t, client.Events(), 5*time.Second, "waiting for layout change event")
	if event.SessionName != sessionName {
		t.Errorf("event session = %q, want %q", event.SessionName, sessionName)
	}

	// Verify no second event arrives within a reasonable window.
	assertNoEvent(t, client, 2*time.Second)
}

// TestControlClientWindowAdd verifies that creating a new window
// triggers a layout change event.
func TestControlClientWindowAdd(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/winadd", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")

	// Create a new window — triggers %window-add.
	mustTmux(t, server, "new-window", "-t", sessionName)
	testutil.RequireReceive(t, client.Events(), 3*time.Second, "waiting for window-add event")
}

// TestControlClientWindowRename verifies that renaming a window
// triggers a layout change event.
func TestControlClientWindowRename(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/winrename", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")

	mustTmux(t, server, "rename-window", "-t", sessionName, "renamed")
	testutil.RequireReceive(t, client.Events(), 3*time.Second, "waiting for window-rename event")
}

// TestControlClientWindowClose verifies that closing a window triggers
// a layout change event. When the window is not the currently attached
// one, tmux sends %unlinked-window-close instead of %window-close.
func TestControlClientWindowClose(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/winclose", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a second window so we can close it.
	mustTmux(t, server, "new-window", "-t", sessionName)

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")

	// Find the index of the second window. We can't hardcode it because
	// the starting index depends on base-index (0 by default).
	windowList := mustTmuxTrimmed(t, server, "list-windows",
		"-t", sessionName, "-F", "#{window_index}")
	windowIndices := splitLines(windowList)
	if len(windowIndices) < 2 {
		t.Fatalf("expected 2 windows, got %d", len(windowIndices))
	}
	secondWindowIndex := windowIndices[len(windowIndices)-1]

	// Close the second window. The control client is attached to the
	// session; when we kill a non-current window, tmux sends
	// %unlinked-window-close.
	mustTmux(t, server, "kill-window", "-t", sessionName+":"+secondWindowIndex)
	testutil.RequireReceive(t, client.Events(), 3*time.Second, "waiting for window-close event")
}

// TestControlClientIgnoresNonLayoutEvents verifies that notifications
// unrelated to layout (like %output) do not trigger events.
func TestControlClientIgnoresNonLayoutEvents(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/ignore", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	// Wait for attach, then send keystrokes. This generates
	// %output notifications but no layout changes.
	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")
	TmuxSendKeys(t, server, sessionName, "hello")

	assertNoEvent(t, client, 500*time.Millisecond)
}

// TestControlClientCleanShutdown verifies that cancelling the context
// stops the control client cleanly: the events channel is closed and
// Stop returns promptly.
func TestControlClientCleanShutdown(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/shutdown", "")

	ctx, cancel := context.WithCancel(context.Background())

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	// Cancel the context and verify Stop returns within 5 seconds.
	cancel()

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()

	testutil.RequireClosed(t, stopped, 5*time.Second, "Stop should return after context cancel")

	// Verify the events channel is closed.
	_, open := <-client.Events()
	if open {
		t.Error("events channel still open after Stop")
	}
}

// TestControlClientSessionExit verifies that killing the tmux session
// causes the control client to shut down cleanly.
func TestControlClientSessionExit(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/exit", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	// Kill the session. The control mode client should detect the
	// exit and shut down.
	mustTmux(t, server, "kill-session", "-t", sessionName)

	stopped := make(chan struct{})
	go func() {
		client.Stop()
		close(stopped)
	}()

	testutil.RequireClosed(t, stopped, 5*time.Second, "Stop should return after session kill")
}

// TestControlClientNoSizeConstraint verifies that the control mode
// client does not constrain the terminal dimensions of a session
// created with a specific size.
func TestControlClientNoSizeConstraint(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)

	// Create a session with a known size.
	mustTmux(t, server, "new-session", "-d", "-s", "control/size",
		"-x", "120", "-y", "40")
	t.Cleanup(func() {
		server.Run("kill-session", "-t", "control/size")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, "control/size",
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	// Wait for the control client to attach.
	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")

	// Verify the window dimensions are unchanged.
	dimensions := mustTmuxTrimmed(t, server, "display-message",
		"-t", "control/size", "-p", "#{window_width} #{window_height}")
	if dimensions != "120 40" {
		t.Errorf("window dimensions = %q, want %q (control client constrained the size)",
			dimensions, "120 40")
	}
}

// TestControlClientResponseBlockFiltering verifies that lines inside
// %begin/%end response blocks are not treated as notifications.
func TestControlClientResponseBlockFiltering(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	sessionName := TmuxSession(t, server, "control/blocks", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewControlClient(ctx, server, sessionName,
		WithDebounceInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	defer client.Stop()

	// Wait for initial attach to complete. No layout events should
	// fire from the control mode attach sequence.
	testutil.RequireClosed(t, client.Ready(), 5*time.Second, "control client ready")
	assertNoEvent(t, client, 300*time.Millisecond)

	// Now trigger a real layout change to prove events still work.
	mustTmux(t, server, "split-window", "-t", sessionName, "-v")
	testutil.RequireReceive(t, client.Events(), 3*time.Second, "waiting for layout change event")
}

// TestIsLayoutNotification exercises the notification classifier.
func TestIsLayoutNotification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line     string
		expected bool
	}{
		{"%layout-change @0 b25d,80x24,0,0,0 b25d,80x24,0,0,0", true},
		{"%layout-change @1 c195,80x24,0,0[80x12,0,0,0,80x11,0,13,1]", true},
		{"%window-add @1", true},
		{"%window-close @1", true},
		{"%window-renamed @0 my-window", true},
		{"%unlinked-window-close @1", true},
		{"%output %0 hello world", false},
		{"%session-changed $1 my-session", false},
		{"%session-window-changed $0 @1", false},
		{"%window-pane-changed @0 %1", false},
		{"%pane-mode-changed %0", false},
		{"%client-detached /dev/pts/3", false},
		{"%begin 1363006971 2 1", false},
		{"%end 1363006971 2 1", false},
		{"%error 1363006971 2 1", false},
		{"%exit", false},
		{"", false},
		{"some random line", false},
	}

	for _, test := range tests {
		result := isLayoutNotification(test.line)
		if result != test.expected {
			t.Errorf("isLayoutNotification(%q) = %v, want %v",
				test.line, result, test.expected)
		}
	}
}

// receiveEvent waits for a LayoutChanged event from the client within
// the timeout. Fails the test if no event arrives.
func receiveEvent(t *testing.T, client *ControlClient, timeout time.Duration) LayoutChanged {
	t.Helper()
	return testutil.RequireReceive(t, client.Events(), timeout, "waiting for layout change event")
}

// assertNoEvent verifies that no LayoutChanged event arrives within
// the timeout. This is a negative assertion: we genuinely need to wait
// a wall-clock interval to confirm nothing happens, so we use
// testutil.RequireReceive on a timer channel for the duration.
func assertNoEvent(t *testing.T, client *ControlClient, timeout time.Duration) {
	t.Helper()
	timer := make(chan struct{})
	go func() {
		<-time.After(timeout) //nolint:realclock negative assertion requires wall-clock wait
		close(timer)
	}()
	select {
	case event, ok := <-client.Events():
		if ok {
			t.Fatalf("unexpected layout change event: %+v", event)
		}
	case <-timer:
		// Good — no event.
	}
}
