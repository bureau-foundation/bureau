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
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/messaging"
)

// newTestDaemonWithLayout creates a Daemon with a mock Matrix server and an
// isolated tmux server for layout watcher tests. Returns the daemon, mock
// state (for verifying published events), and the tmux server.
func newTestDaemonWithLayout(t *testing.T) (*Daemon, *mockMatrixState, *tmux.Server) {
	t.Helper()

	tmuxServer := tmux.NewTestServer(t)

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:bureau.local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		clock:             clock.Real(),
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      "!config:test",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastMatrixPolicy:  make(map[string]*schema.MatrixPolicy),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		tmuxServer:        tmuxServer,
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	return daemon, matrixState, tmuxServer
}

// createTestTmuxSession creates a tmux session on the test-isolated server.
// Blocks until the session is ready. The session runs "cat" so it stays alive
// and accepts input.
func createTestTmuxSession(t *testing.T, server *tmux.Server, sessionName string) {
	t.Helper()
	_, err := server.Run("new-session", "-d",
		"-s", sessionName, "-x", "160", "-y", "48", "cat")
	if err != nil {
		t.Fatalf("create tmux session %q: %v", sessionName, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if server.HasSession(sessionName) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tmux session %q not ready after 5 seconds", sessionName)
}

// TestLayoutWatcherPublishOnChange verifies that splitting a pane triggers the
// layout watcher to publish the new layout to Matrix.
func TestLayoutWatcherPublishOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, matrixState, tmuxServer := newTestDaemonWithLayout(t)

	localpart := "test/layout"
	sessionName := "bureau/" + localpart
	createTestTmuxSession(t, tmuxServer, sessionName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startLayoutWatcher(ctx, localpart)
	defer daemon.stopLayoutWatcher(localpart)

	// Wait for the ControlClient to attach.
	time.Sleep(300 * time.Millisecond)

	// Split a pane to trigger a layout change.
	if _, err := tmuxServer.Run("split-window", "-t", sessionName, "-h", "cat"); err != nil {
		t.Fatalf("split-window: %v", err)
	}

	// Wait for debounce (500ms) + publish.
	time.Sleep(2 * time.Second)

	// Verify the layout was published to the mock Matrix.
	key := "!config:test\x00" + schema.EventTypeLayout + "\x00" + localpart
	matrixState.mu.Lock()
	raw, ok := matrixState.stateEvents[key]
	matrixState.mu.Unlock()

	if !ok {
		t.Fatal("layout event was not published to Matrix")
	}

	var content schema.LayoutContent
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("unmarshal published layout: %v", err)
	}

	if content.SourceMachine != "@machine/test:bureau.local" {
		t.Errorf("SourceMachine = %q, want %q",
			content.SourceMachine, "@machine/test:bureau.local")
	}

	if len(content.Windows) == 0 {
		t.Fatal("published layout has no windows")
	}

	totalPanes := 0
	for _, window := range content.Windows {
		totalPanes += len(window.Panes)
	}
	if totalPanes < 2 {
		t.Errorf("published layout has %d panes, want at least 2", totalPanes)
	}
}

// TestLayoutWatcherRestoreOnCreate verifies that when a layout event already
// exists in Matrix, starting the watcher applies it to the tmux session.
func TestLayoutWatcherRestoreOnCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, matrixState, tmuxServer := newTestDaemonWithLayout(t)

	localpart := "test/restore"
	sessionName := "bureau/" + localpart

	// Pre-populate Matrix with a 2-window layout.
	matrixState.setStateEvent("!config:test", schema.EventTypeLayout, localpart, schema.LayoutContent{
		Windows: []schema.LayoutWindow{
			{
				Name:  "main",
				Panes: []schema.LayoutPane{{Command: "cat"}},
			},
			{
				Name:  "shell",
				Panes: []schema.LayoutPane{{Command: "cat"}},
			},
		},
		SourceMachine: "@machine/other:bureau.local",
	})

	// Create the tmux session (starts with 1 window).
	createTestTmuxSession(t, tmuxServer, sessionName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startLayoutWatcher(ctx, localpart)
	defer daemon.stopLayoutWatcher(localpart)

	// Wait for the restore to complete.
	time.Sleep(2 * time.Second)

	// Verify the tmux session now has 2 windows.
	output, err := tmuxServer.Run("list-windows",
		"-t", sessionName, "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}

	windowNames := strings.Split(strings.TrimSpace(output), "\n")
	if len(windowNames) != 2 {
		t.Errorf("tmux session has %d windows, want 2\nwindow names: %v",
			len(windowNames), windowNames)
	}
}

// TestLayoutWatcherStopCleanup verifies that stopping a layout watcher removes
// it from the map and doesn't hang.
func TestLayoutWatcherStopCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, _, tmuxServer := newTestDaemonWithLayout(t)

	localpart := "test/stop"
	sessionName := "bureau/" + localpart
	createTestTmuxSession(t, tmuxServer, sessionName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startLayoutWatcher(ctx, localpart)
	time.Sleep(300 * time.Millisecond)

	// Stop should return promptly and remove from map.
	done := make(chan struct{})
	go func() {
		daemon.stopLayoutWatcher(localpart)
		close(done)
	}()

	select {
	case <-done:
		// Good.
	case <-time.After(5 * time.Second):
		t.Fatal("stopLayoutWatcher hung for 5 seconds")
	}

	daemon.layoutWatchersMu.Lock()
	_, exists := daemon.layoutWatchers[localpart]
	daemon.layoutWatchersMu.Unlock()

	if exists {
		t.Error("watcher still in map after stop")
	}
}

// TestLayoutWatcherIdempotentStart verifies that calling startLayoutWatcher
// twice for the same principal is a no-op (only one watcher runs).
func TestLayoutWatcherIdempotentStart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, _, tmuxServer := newTestDaemonWithLayout(t)

	localpart := "test/idempotent"
	sessionName := "bureau/" + localpart
	createTestTmuxSession(t, tmuxServer, sessionName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startLayoutWatcher(ctx, localpart)
	defer daemon.stopLayoutWatcher(localpart)
	time.Sleep(300 * time.Millisecond)

	// Second start should be a no-op.
	daemon.startLayoutWatcher(ctx, localpart)

	daemon.layoutWatchersMu.Lock()
	count := len(daemon.layoutWatchers)
	daemon.layoutWatchersMu.Unlock()

	if count != 1 {
		t.Errorf("watcher count = %d, want 1", count)
	}
}

// TestLayoutWatcherStopAll verifies that stopAllLayoutWatchers shuts down
// all watchers cleanly.
func TestLayoutWatcherStopAll(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, _, tmuxServer := newTestDaemonWithLayout(t)

	principals := []string{"a/one", "a/two", "a/three"}
	for _, name := range principals {
		createTestTmuxSession(t, tmuxServer, "bureau/"+name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, name := range principals {
		daemon.startLayoutWatcher(ctx, name)
	}
	time.Sleep(300 * time.Millisecond)

	// Stop all should complete promptly.
	done := make(chan struct{})
	go func() {
		daemon.stopAllLayoutWatchers()
		close(done)
	}()

	select {
	case <-done:
		// Good.
	case <-time.After(10 * time.Second):
		t.Fatal("stopAllLayoutWatchers hung for 10 seconds")
	}

	daemon.layoutWatchersMu.Lock()
	count := len(daemon.layoutWatchers)
	daemon.layoutWatchersMu.Unlock()

	if count != 0 {
		t.Errorf("watcher count after stopAll = %d, want 0", count)
	}
}

// TestLayoutWatcherNoTmuxSession verifies that starting a layout watcher for
// a session that doesn't exist exits cleanly without hanging.
func TestLayoutWatcherNoTmuxSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	daemon, _, _ := newTestDaemonWithLayout(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a watcher for a session that doesn't exist. The ControlClient
	// will fail to attach, and the goroutine should exit.
	daemon.startLayoutWatcher(ctx, "test/nonexistent")

	// The watcher should exit quickly since the tmux session doesn't exist.
	time.Sleep(2 * time.Second)

	// stopLayoutWatcher should not hang.
	done := make(chan struct{})
	go func() {
		daemon.stopLayoutWatcher("test/nonexistent")
		close(done)
	}()

	select {
	case <-done:
		// Good — the goroutine already exited.
	case <-time.After(5 * time.Second):
		t.Fatal("stopLayoutWatcher hung for a watcher with no tmux session")
	}
}
