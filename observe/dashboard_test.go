// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/tmux"
)

func TestDashboardCommandPanes(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "tools",
				Panes: []Pane{
					{Command: "sleep 3600"},
					{Command: "sleep 3600", Split: "horizontal", Size: 50},
				},
			},
		},
	}

	err := Dashboard(server, "observe/test/tools", "/tmp/fake-daemon.sock", layout)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	// Verify the session was created with the correct name.
	if !server.HasSession("observe/test/tools") {
		t.Fatal("session not created")
	}

	// Verify window name.
	windowName := mustTmuxTrimmed(t, server, "list-windows",
		"-t", "observe/test/tools", "-F", "#{window_name}")
	if windowName != "tools" {
		t.Errorf("window name = %q, want %q", windowName, "tools")
	}

	// Verify two panes exist.
	paneOutput := mustTmuxTrimmed(t, server, "list-panes",
		"-t", "observe/test/tools", "-F", "#{pane_index}")
	paneLines := splitLines(paneOutput)
	if len(paneLines) != 2 {
		t.Errorf("pane count = %d, want 2", len(paneLines))
	}
}

func TestDashboardObservePanes(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	daemonSocket := "/run/bureau/observe.sock"

	// The bureau observe commands will fail immediately (no daemon
	// running, binary may not be in PATH). Set remain-on-exit so that
	// panes stay around for inspection after the command exits.
	initTmuxServerWithRemainOnExit(t, server)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{Observe: "iree/amdgpu/pm"},
					{Observe: "iree/amdgpu/codegen", Split: "horizontal", Size: 50},
				},
			},
		},
	}

	err := Dashboard(server, "observe/iree/amdgpu/general", daemonSocket, layout)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	// Verify the session exists.
	if !server.HasSession("observe/iree/amdgpu/general") {
		t.Fatal("session not created")
	}

	// Give processes a moment to start so pane_start_command is populated.
	time.Sleep(200 * time.Millisecond)

	// Verify panes were created. The bureau observe commands will fail
	// (no daemon running), but the panes survive (remain-on-exit).
	paneOutput := mustTmuxTrimmed(t, server, "list-panes",
		"-t", "observe/iree/amdgpu/general",
		"-F", "#{pane_start_command}")
	paneLines := splitLines(paneOutput)
	if len(paneLines) != 2 {
		t.Fatalf("pane count = %d, want 2", len(paneLines))
	}

	// First pane should have the bureau observe command for iree/amdgpu/pm.
	expectedFirst := "bureau observe iree/amdgpu/pm --socket " + daemonSocket
	if !strings.Contains(paneLines[0], "bureau observe iree/amdgpu/pm") {
		t.Errorf("pane 0 start command = %q, want to contain %q", paneLines[0], expectedFirst)
	}

	// Second pane should have the bureau observe command for iree/amdgpu/codegen.
	if !strings.Contains(paneLines[1], "bureau observe iree/amdgpu/codegen") {
		t.Errorf("pane 1 start command = %q, want to contain %q",
			paneLines[1], "bureau observe iree/amdgpu/codegen")
	}
}

func TestDashboardMixedPaneTypes(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	initTmuxServerWithRemainOnExit(t, server)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "workspace",
				Panes: []Pane{
					{Observe: "iree/amdgpu/pm"},
					{Command: "sleep 3600", Split: "horizontal", Size: 30},
				},
			},
			{
				Name: "monitoring",
				Panes: []Pane{
					{Role: "dashboard"},
				},
			},
		},
	}

	err := Dashboard(server, "observe/mixed", "/tmp/daemon.sock", layout)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	// Verify two windows.
	windowOutput := mustTmuxTrimmed(t, server, "list-windows",
		"-t", "observe/mixed", "-F", "#{window_name}")
	windows := splitLines(windowOutput)
	if len(windows) != 2 {
		t.Fatalf("window count = %d, want 2", len(windows))
	}
	if windows[0] != "workspace" {
		t.Errorf("window 0 name = %q, want %q", windows[0], "workspace")
	}
	if windows[1] != "monitoring" {
		t.Errorf("window 1 name = %q, want %q", windows[1], "monitoring")
	}
}

func TestDashboardRolePaneShowsIdentity(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Role: "agent"},
				},
			},
		},
	}

	err := Dashboard(server, "observe/role-test", "/tmp/daemon.sock", layout)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	// Give the echo command time to execute.
	time.Sleep(300 * time.Millisecond)

	// Capture the pane content — it should show the role name.
	content := TmuxCapturePane(t, server, "observe/role-test")
	if !strings.Contains(content, "role: agent") {
		t.Errorf("pane content should contain 'role: agent', got:\n%s", content)
	}
}

func TestDashboardNilLayoutError(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	// Need a running server.
	TmuxSession(t, server, "dummy", "sleep 3600")

	err := Dashboard(server, "test", "/tmp/daemon.sock", nil)
	if err == nil {
		t.Fatal("expected error for nil layout")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error = %q, want to contain 'nil'", err.Error())
	}
}

func TestDashboardEmptyDaemonSocketError(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	TmuxSession(t, server, "dummy", "sleep 3600")

	layout := &Layout{
		Windows: []Window{
			{Name: "main", Panes: []Pane{{Command: "sleep 3600"}}},
		},
	}

	err := Dashboard(server, "test", "", layout)
	if err == nil {
		t.Fatal("expected error for empty daemon socket")
	}
	if !strings.Contains(err.Error(), "daemon socket") {
		t.Errorf("error = %q, want to contain 'daemon socket'", err.Error())
	}
}

func TestDashboardEmptyLayoutError(t *testing.T) {
	t.Parallel()
	server := TmuxServer(t)
	TmuxSession(t, server, "dummy", "sleep 3600")

	err := Dashboard(server, "test", "/tmp/daemon.sock", &Layout{})
	if err == nil {
		t.Fatal("expected error for empty layout")
	}
	if !strings.Contains(err.Error(), "no windows") {
		t.Errorf("error = %q, want to contain 'no windows'", err.Error())
	}
}

func TestResolveLayoutPreservesOriginal(t *testing.T) {
	t.Parallel()
	original := &Layout{
		Prefix: "C-a",
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{Observe: "iree/amdgpu/pm"},
					{Command: "htop", Split: "horizontal", Size: 30},
					{Role: "shell", Split: "vertical", Size: 50},
				},
			},
		},
	}

	resolved := resolveLayout(original, "/run/bureau/observe.sock")

	// Original should be unchanged.
	if original.Windows[0].Panes[0].Observe != "iree/amdgpu/pm" {
		t.Error("original observe pane was modified")
	}
	if original.Windows[0].Panes[0].Command != "" {
		t.Error("original observe pane gained a command")
	}

	// Resolved should have concrete commands.
	if resolved.Prefix != "C-a" {
		t.Errorf("prefix = %q, want %q", resolved.Prefix, "C-a")
	}

	resolvedPanes := resolved.Windows[0].Panes

	// Observe → bureau observe with daemon socket.
	if !strings.Contains(resolvedPanes[0].Command, "bureau observe iree/amdgpu/pm") {
		t.Errorf("observe pane command = %q, want bureau observe invocation", resolvedPanes[0].Command)
	}
	if !strings.Contains(resolvedPanes[0].Command, "/run/bureau/observe.sock") {
		t.Errorf("observe pane command = %q, want daemon socket in args", resolvedPanes[0].Command)
	}

	// Command → passes through unchanged.
	if resolvedPanes[1].Command != "htop" {
		t.Errorf("command pane = %q, want %q", resolvedPanes[1].Command, "htop")
	}
	if resolvedPanes[1].Split != "horizontal" || resolvedPanes[1].Size != 30 {
		t.Error("command pane lost its split/size")
	}

	// Role → informational echo.
	if !strings.Contains(resolvedPanes[2].Command, "role: shell") {
		t.Errorf("role pane command = %q, want role echo", resolvedPanes[2].Command)
	}
	if resolvedPanes[2].Split != "vertical" || resolvedPanes[2].Size != 50 {
		t.Error("role pane lost its split/size")
	}
}

func TestResolveLayoutEmptyPaneGetsNoCommand(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name:  "main",
				Panes: []Pane{{}},
			},
		},
	}

	resolved := resolveLayout(layout, "/tmp/daemon.sock")
	if resolved.Windows[0].Panes[0].Command != "" {
		t.Errorf("empty pane should have no command, got %q",
			resolved.Windows[0].Panes[0].Command)
	}
}

// initTmuxServerWithRemainOnExit starts a tmux server and sets
// remain-on-exit on so that panes survive when their commands exit.
// This is needed for tests where pane commands will fail (e.g.,
// bureau observe with no running daemon). Without remain-on-exit,
// the session collapses before we can inspect it.
//
// The helper creates a long-lived "keepalive" session that keeps the
// tmux server running while Dashboard creates its sessions with
// potentially short-lived commands. TmuxServer's cleanup handles
// final teardown.
func initTmuxServerWithRemainOnExit(t *testing.T, server *tmux.Server) {
	t.Helper()
	TmuxSession(t, server, "keepalive", "sleep 3600")
	mustTmux(t, server, "set-option", "-g", "remain-on-exit", "on")
}
