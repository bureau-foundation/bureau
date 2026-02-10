// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"strings"
	"testing"
	"time"
)

func TestReadTmuxLayoutSinglePane(t *testing.T) {
	serverSocket := TmuxServer(t)
	TmuxSession(t, serverSocket, "test-single", "sleep 3600")

	// Rename the default window so we have a known name.
	mustTmux(t, serverSocket, "rename-window", "-t", "test-single", "main")

	layout, err := ReadTmuxLayout(serverSocket, "test-single")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	if len(layout.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(layout.Windows))
	}

	window := layout.Windows[0]
	if window.Name != "main" {
		t.Errorf("window name = %q, want %q", window.Name, "main")
	}
	if len(window.Panes) != 1 {
		t.Fatalf("pane count = %d, want 1", len(window.Panes))
	}

	pane := window.Panes[0]
	if pane.Split != "" {
		t.Errorf("first pane split = %q, want empty", pane.Split)
	}
	if pane.Command != "sleep" {
		t.Errorf("first pane command = %q, want %q", pane.Command, "sleep")
	}
}

func TestReadTmuxLayoutHorizontalSplit(t *testing.T) {
	serverSocket := TmuxServer(t)
	TmuxSession(t, serverSocket, "test-hsplit", "sleep 3600")
	mustTmux(t, serverSocket, "rename-window", "-t", "test-hsplit", "agents")

	// Split horizontally (side by side). The new pane gets 30%.
	mustTmux(t, serverSocket, "split-window", "-t", "test-hsplit", "-h", "-l", "30%", "sleep 3600")

	layout, err := ReadTmuxLayout(serverSocket, "test-hsplit")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	window := layout.Windows[0]
	if len(window.Panes) != 2 {
		t.Fatalf("pane count = %d, want 2", len(window.Panes))
	}

	// First pane: root, no split.
	if window.Panes[0].Split != "" {
		t.Errorf("pane 0 split = %q, want empty", window.Panes[0].Split)
	}

	// Second pane: horizontal split.
	if window.Panes[1].Split != "horizontal" {
		t.Errorf("pane 1 split = %q, want %q", window.Panes[1].Split, "horizontal")
	}

	// The 30% split should be approximately 30% (tmux rounds to cell boundaries).
	if window.Panes[1].Size < 25 || window.Panes[1].Size > 35 {
		t.Errorf("pane 1 size = %d, want approximately 30", window.Panes[1].Size)
	}
}

func TestReadTmuxLayoutVerticalSplit(t *testing.T) {
	serverSocket := TmuxServer(t)
	TmuxSession(t, serverSocket, "test-vsplit", "sleep 3600")
	mustTmux(t, serverSocket, "rename-window", "-t", "test-vsplit", "stack")

	// Split vertically (top/bottom). The new pane gets 40%.
	mustTmux(t, serverSocket, "split-window", "-t", "test-vsplit", "-v", "-l", "40%", "sleep 3600")

	layout, err := ReadTmuxLayout(serverSocket, "test-vsplit")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	window := layout.Windows[0]
	if len(window.Panes) != 2 {
		t.Fatalf("pane count = %d, want 2", len(window.Panes))
	}

	if window.Panes[1].Split != "vertical" {
		t.Errorf("pane 1 split = %q, want %q", window.Panes[1].Split, "vertical")
	}

	if window.Panes[1].Size < 35 || window.Panes[1].Size > 45 {
		t.Errorf("pane 1 size = %d, want approximately 40", window.Panes[1].Size)
	}
}

func TestReadTmuxLayoutMultipleWindows(t *testing.T) {
	serverSocket := TmuxServer(t)
	TmuxSession(t, serverSocket, "test-multiwin", "sleep 3600")
	mustTmux(t, serverSocket, "rename-window", "-t", "test-multiwin", "agents")

	// Add a second window.
	mustTmux(t, serverSocket, "new-window", "-t", "test-multiwin", "-n", "tools", "sleep 3600")

	layout, err := ReadTmuxLayout(serverSocket, "test-multiwin")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	if len(layout.Windows) != 2 {
		t.Fatalf("window count = %d, want 2", len(layout.Windows))
	}

	if layout.Windows[0].Name != "agents" {
		t.Errorf("window 0 name = %q, want %q", layout.Windows[0].Name, "agents")
	}
	if layout.Windows[1].Name != "tools" {
		t.Errorf("window 1 name = %q, want %q", layout.Windows[1].Name, "tools")
	}
}

func TestReadTmuxLayoutSessionNotFound(t *testing.T) {
	serverSocket := TmuxServer(t)
	// Start the server with a dummy session so it's running.
	TmuxSession(t, serverSocket, "dummy", "sleep 3600")

	_, err := ReadTmuxLayout(serverSocket, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
}

func TestApplyLayoutCreatesSession(t *testing.T) {
	serverSocket := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Command: "sleep 3600"},
				},
			},
		},
	}

	if err := ApplyLayout(serverSocket, "test-apply", layout); err != nil {
		t.Fatalf("ApplyLayout: %v", err)
	}

	// Verify the session exists.
	if _, err := tmuxCommand(serverSocket, "has-session", "-t", "test-apply"); err != nil {
		t.Fatalf("session not created: %v", err)
	}

	// Verify window name.
	windowName := mustTmuxTrimmed(t, serverSocket, "list-windows",
		"-t", "test-apply", "-F", "#{window_name}")
	if windowName != "main" {
		t.Errorf("window name = %q, want %q", windowName, "main")
	}

	// Verify pane count.
	paneCount := countPanes(t, serverSocket, "test-apply")
	if paneCount != 1 {
		t.Errorf("pane count = %d, want 1", paneCount)
	}
}

func TestApplyLayoutWithSplits(t *testing.T) {
	serverSocket := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{Command: "sleep 3600"},
					{Command: "sleep 3600", Split: "horizontal", Size: 40},
				},
			},
		},
	}

	if err := ApplyLayout(serverSocket, "test-splits", layout); err != nil {
		t.Fatalf("ApplyLayout: %v", err)
	}

	paneCount := countPanes(t, serverSocket, "test-splits")
	if paneCount != 2 {
		t.Fatalf("pane count = %d, want 2", paneCount)
	}

	// Verify pane arrangement: two panes side by side (horizontal split).
	paneOutput := mustTmuxTrimmed(t, serverSocket, "list-panes",
		"-t", "test-splits", "-F", "#{pane_left},#{pane_top}")
	lines := splitLines(paneOutput)
	if len(lines) != 2 {
		t.Fatalf("expected 2 pane lines, got %d", len(lines))
	}

	// First pane at (0,0), second pane at (>0, 0) — same row, different column.
	if !strings.HasPrefix(lines[0], "0,0") {
		t.Errorf("pane 0 position = %q, want to start at 0,0", lines[0])
	}
	if strings.HasPrefix(lines[1], "0,") {
		t.Errorf("pane 1 should be to the right of pane 0, but left=0")
	}
}

func TestApplyLayoutMultipleWindows(t *testing.T) {
	serverSocket := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{Command: "sleep 3600"},
				},
			},
			{
				Name: "tools",
				Panes: []Pane{
					{Command: "sleep 3600"},
					{Command: "sleep 3600", Split: "vertical", Size: 50},
				},
			},
		},
	}

	if err := ApplyLayout(serverSocket, "test-multiwin-apply", layout); err != nil {
		t.Fatalf("ApplyLayout: %v", err)
	}

	// Verify window count and names.
	windowLines := mustTmuxTrimmed(t, serverSocket, "list-windows",
		"-t", "test-multiwin-apply", "-F", "#{window_name}")
	names := splitLines(windowLines)
	if len(names) != 2 {
		t.Fatalf("window count = %d, want 2", len(names))
	}
	if names[0] != "agents" {
		t.Errorf("window 0 name = %q, want %q", names[0], "agents")
	}
	if names[1] != "tools" {
		t.Errorf("window 1 name = %q, want %q", names[1], "tools")
	}
}

func TestApplyLayoutEmptyLayoutError(t *testing.T) {
	serverSocket := TmuxServer(t)
	// Need a running server for has-session check.
	TmuxSession(t, serverSocket, "dummy", "sleep 3600")

	err := ApplyLayout(serverSocket, "test-empty", &Layout{})
	if err == nil {
		t.Fatal("expected error for empty layout, got nil")
	}
	if !strings.Contains(err.Error(), "no windows") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "no windows")
	}
}

func TestApplyLayoutEmptyWindowError(t *testing.T) {
	serverSocket := TmuxServer(t)
	TmuxSession(t, serverSocket, "dummy", "sleep 3600")

	layout := &Layout{
		Windows: []Window{
			{Name: "empty", Panes: []Pane{}},
		},
	}

	err := ApplyLayout(serverSocket, "test-empty-win", layout)
	if err == nil {
		t.Fatal("expected error for empty window, got nil")
	}
	if !strings.Contains(err.Error(), "no panes") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "no panes")
	}
}

func TestApplyThenReadRoundTrip(t *testing.T) {
	serverSocket := TmuxServer(t)

	original := &Layout{
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Command: "sleep 3600"},
					{Command: "sleep 3600", Split: "horizontal", Size: 50},
				},
			},
			{
				Name: "secondary",
				Panes: []Pane{
					{Command: "sleep 3600"},
					{Command: "sleep 3600", Split: "vertical", Size: 40},
				},
			},
		},
	}

	if err := ApplyLayout(serverSocket, "test-roundtrip", original); err != nil {
		t.Fatalf("ApplyLayout: %v", err)
	}

	// Give tmux a moment to settle (process creation, pane rendering).
	time.Sleep(200 * time.Millisecond)

	readBack, err := ReadTmuxLayout(serverSocket, "test-roundtrip")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	// Verify structural equivalence.
	if len(readBack.Windows) != len(original.Windows) {
		t.Fatalf("window count: got %d, want %d", len(readBack.Windows), len(original.Windows))
	}

	for windowIndex, window := range original.Windows {
		readWindow := readBack.Windows[windowIndex]
		if readWindow.Name != window.Name {
			t.Errorf("window[%d] name = %q, want %q", windowIndex, readWindow.Name, window.Name)
		}
		if len(readWindow.Panes) != len(window.Panes) {
			t.Fatalf("window[%d] pane count = %d, want %d", windowIndex, len(readWindow.Panes), len(window.Panes))
		}

		for paneIndex, pane := range window.Panes {
			readPane := readWindow.Panes[paneIndex]

			// First pane has no split direction.
			if paneIndex == 0 {
				if readPane.Split != "" {
					t.Errorf("window[%d].pane[0] split = %q, want empty", windowIndex, readPane.Split)
				}
				continue
			}

			if readPane.Split != pane.Split {
				t.Errorf("window[%d].pane[%d] split = %q, want %q",
					windowIndex, paneIndex, readPane.Split, pane.Split)
			}

			// Size should be approximately correct (within 5% due to
			// tmux rounding to cell boundaries).
			sizeDelta := readPane.Size - pane.Size
			if sizeDelta < 0 {
				sizeDelta = -sizeDelta
			}
			if sizeDelta > 5 {
				t.Errorf("window[%d].pane[%d] size = %d, want approximately %d",
					windowIndex, paneIndex, readPane.Size, pane.Size)
			}
		}
	}
}

func TestApplyLayoutWithSessionSlashes(t *testing.T) {
	// Bureau session names use slashes: "bureau/iree/amdgpu/pm".
	// Verify tmux handles these correctly.
	serverSocket := TmuxServer(t)

	layout := &Layout{
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Command: "sleep 3600"},
				},
			},
		},
	}

	if err := ApplyLayout(serverSocket, "bureau/iree/amdgpu/pm", layout); err != nil {
		t.Fatalf("ApplyLayout: %v", err)
	}

	readBack, err := ReadTmuxLayout(serverSocket, "bureau/iree/amdgpu/pm")
	if err != nil {
		t.Fatalf("ReadTmuxLayout: %v", err)
	}

	if len(readBack.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(readBack.Windows))
	}
	if readBack.Windows[0].Name != "main" {
		t.Errorf("window name = %q, want %q", readBack.Windows[0].Name, "main")
	}
}

func TestResolveCommand(t *testing.T) {
	tests := []struct {
		name string
		pane Pane
		want string
	}{
		{
			name: "command pane",
			pane: Pane{Command: "beads-tui --project iree/amdgpu"},
			want: "beads-tui --project iree/amdgpu",
		},
		{
			name: "observe pane",
			pane: Pane{Observe: "iree/amdgpu/pm"},
			want: "bureau observe iree/amdgpu/pm",
		},
		{
			name: "role pane returns empty",
			pane: Pane{Role: "agent"},
			want: "",
		},
		{
			name: "empty pane returns empty",
			pane: Pane{},
			want: "",
		},
		{
			name: "command takes priority over observe",
			pane: Pane{Command: "explicit-cmd", Observe: "principal"},
			want: "explicit-cmd",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolveCommand(test.pane)
			if got != test.want {
				t.Errorf("resolveCommand() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestInferPaneSplitsEmpty(t *testing.T) {
	result := inferPaneSplits(nil)
	if result != nil {
		t.Errorf("inferPaneSplits(nil) = %v, want nil", result)
	}
}

func TestInferPaneSplitsSinglePane(t *testing.T) {
	panes := []tmuxPane{
		{index: 0, width: 160, height: 48, left: 0, top: 0, command: "sleep"},
	}
	result := inferPaneSplits(panes)
	if len(result) != 1 {
		t.Fatalf("pane count = %d, want 1", len(result))
	}
	if result[0].Split != "" {
		t.Errorf("split = %q, want empty", result[0].Split)
	}
	if result[0].Command != "sleep" {
		t.Errorf("command = %q, want %q", result[0].Command, "sleep")
	}
}

func TestInferPaneSplitsHorizontal(t *testing.T) {
	// Two panes side by side (same top, different left).
	panes := []tmuxPane{
		{index: 0, width: 79, height: 48, left: 0, top: 0, command: "vim"},
		{index: 1, width: 80, height: 48, left: 80, top: 0, command: "zsh"},
	}
	result := inferPaneSplits(panes)
	if len(result) != 2 {
		t.Fatalf("pane count = %d, want 2", len(result))
	}
	if result[1].Split != "horizontal" {
		t.Errorf("split = %q, want %q", result[1].Split, "horizontal")
	}
	// 80 / (79+80+1) = 50%
	if result[1].Size != 50 {
		t.Errorf("size = %d, want 50", result[1].Size)
	}
}

func TestInferPaneSplitsVertical(t *testing.T) {
	// Two panes stacked (same left, different top).
	panes := []tmuxPane{
		{index: 0, width: 160, height: 23, left: 0, top: 0, command: "vim"},
		{index: 1, width: 160, height: 24, left: 0, top: 24, command: "zsh"},
	}
	result := inferPaneSplits(panes)
	if len(result) != 2 {
		t.Fatalf("pane count = %d, want 2", len(result))
	}
	if result[1].Split != "vertical" {
		t.Errorf("split = %q, want %q", result[1].Split, "vertical")
	}
	// 24 / (23+24+1) = 50%
	if result[1].Size != 50 {
		t.Errorf("size = %d, want 50", result[1].Size)
	}
}

