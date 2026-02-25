// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

func TestGenerateMachineLayoutBasic(t *testing.T) {
	t.Parallel()

	principals := []string{
		"iree/amdgpu/test",
		"iree/amdgpu/codegen",
		"iree/amdgpu/pm",
	}

	layout := GenerateMachineLayout("machine/workstation", principals)

	if layout == nil {
		t.Fatal("GenerateMachineLayout returned nil")
	}
	if len(layout.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(layout.Windows))
	}

	window := layout.Windows[0]
	if window.Name != "machine/workstation" {
		t.Errorf("window name = %q, want %q", window.Name, "machine/workstation")
	}
	if len(window.Panes) != 3 {
		t.Fatalf("pane count = %d, want 3", len(window.Panes))
	}

	// Verify alphabetical sort order.
	if window.Panes[0].Observe != "iree/amdgpu/codegen" {
		t.Errorf("pane 0 observe = %q, want %q", window.Panes[0].Observe, "iree/amdgpu/codegen")
	}
	if window.Panes[1].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 1 observe = %q, want %q", window.Panes[1].Observe, "iree/amdgpu/pm")
	}
	if window.Panes[2].Observe != "iree/amdgpu/test" {
		t.Errorf("pane 2 observe = %q, want %q", window.Panes[2].Observe, "iree/amdgpu/test")
	}

	// First pane has no split (initial tmux pane).
	if window.Panes[0].Split != "" {
		t.Errorf("pane 0 split = %q, want empty", window.Panes[0].Split)
	}
	// Subsequent panes split vertically.
	if window.Panes[1].Split != observation.SplitVertical {
		t.Errorf("pane 1 split = %q, want %q", window.Panes[1].Split, observation.SplitVertical)
	}
	if window.Panes[2].Split != observation.SplitVertical {
		t.Errorf("pane 2 split = %q, want %q", window.Panes[2].Split, observation.SplitVertical)
	}
}

func TestGenerateMachineLayoutEmpty(t *testing.T) {
	t.Parallel()

	if layout := GenerateMachineLayout("machine/test", nil); layout != nil {
		t.Errorf("GenerateMachineLayout(nil) = %v, want nil", layout)
	}
	if layout := GenerateMachineLayout("machine/test", []string{}); layout != nil {
		t.Errorf("GenerateMachineLayout([]) = %v, want nil", layout)
	}
}

func TestGenerateMachineLayoutSinglePrincipal(t *testing.T) {
	t.Parallel()

	layout := GenerateMachineLayout("machine/box", []string{"service/stt/whisper"})

	if layout == nil {
		t.Fatal("GenerateMachineLayout returned nil")
	}
	if len(layout.Windows[0].Panes) != 1 {
		t.Fatalf("pane count = %d, want 1", len(layout.Windows[0].Panes))
	}

	pane := layout.Windows[0].Panes[0]
	if pane.Observe != "service/stt/whisper" {
		t.Errorf("observe = %q, want %q", pane.Observe, "service/stt/whisper")
	}
	if pane.Split != "" {
		t.Errorf("split = %q, want empty (single pane)", pane.Split)
	}
}

func TestGenerateMachineLayoutDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	principals := []string{"z/agent", "a/agent", "m/agent"}
	originalFirst := principals[0]

	GenerateMachineLayout("machine/test", principals)

	// The input slice should not be sorted in place.
	if principals[0] != originalFirst {
		t.Errorf("input slice was mutated: first element = %q, want %q", principals[0], originalFirst)
	}
}
