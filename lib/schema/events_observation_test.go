// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"
)

func TestLayoutContentRoundTrip(t *testing.T) {
	// A channel layout with two windows: agents (two observe panes) and
	// tools (a command pane and an observe pane). Exercises all pane modes
	// except ObserveMembers (tested separately).
	original := LayoutContent{
		Prefix: "C-a",
		Windows: []LayoutWindow{
			{
				Name: "agents",
				Panes: []LayoutPane{
					{Observe: "iree/amdgpu/pm", Split: "horizontal", Size: 50},
					{Observe: "iree/amdgpu/codegen", Size: 50},
				},
			},
			{
				Name: "tools",
				Panes: []LayoutPane{
					{Command: "beads-tui --project iree/amdgpu", Split: "horizontal", Size: 30},
					{Observe: "iree/amdgpu/ci-runner", Size: 70},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format from OBSERVATION.md.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "prefix", "C-a")
	windows, ok := raw["windows"].([]any)
	if !ok {
		t.Fatal("windows field missing or wrong type")
	}
	if len(windows) != 2 {
		t.Fatalf("windows count = %d, want 2", len(windows))
	}

	agentsWindow := windows[0].(map[string]any)
	assertField(t, agentsWindow, "name", "agents")
	agentsPanes := agentsWindow["panes"].([]any)
	if len(agentsPanes) != 2 {
		t.Fatalf("agents panes count = %d, want 2", len(agentsPanes))
	}
	firstPane := agentsPanes[0].(map[string]any)
	assertField(t, firstPane, "observe", "iree/amdgpu/pm")
	assertField(t, firstPane, "split", "horizontal")
	assertField(t, firstPane, "size", float64(50))

	toolsWindow := windows[1].(map[string]any)
	assertField(t, toolsWindow, "name", "tools")
	toolsPanes := toolsWindow["panes"].([]any)
	firstToolPane := toolsPanes[0].(map[string]any)
	assertField(t, firstToolPane, "command", "beads-tui --project iree/amdgpu")
	assertField(t, firstToolPane, "size", float64(30))

	// Round-trip back to struct.
	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != original.Prefix {
		t.Errorf("Prefix: got %q, want %q", decoded.Prefix, original.Prefix)
	}
	if len(decoded.Windows) != len(original.Windows) {
		t.Fatalf("windows count: got %d, want %d", len(decoded.Windows), len(original.Windows))
	}
	for windowIndex, window := range original.Windows {
		decodedWindow := decoded.Windows[windowIndex]
		if decodedWindow.Name != window.Name {
			t.Errorf("window[%d].Name: got %q, want %q", windowIndex, decodedWindow.Name, window.Name)
		}
		if len(decodedWindow.Panes) != len(window.Panes) {
			t.Fatalf("window[%d] panes count: got %d, want %d", windowIndex, len(decodedWindow.Panes), len(window.Panes))
		}
		for paneIndex, pane := range window.Panes {
			decodedPane := decodedWindow.Panes[paneIndex]
			if decodedPane.Observe != pane.Observe {
				t.Errorf("window[%d].pane[%d].Observe: got %q, want %q", windowIndex, paneIndex, decodedPane.Observe, pane.Observe)
			}
			if decodedPane.Command != pane.Command {
				t.Errorf("window[%d].pane[%d].Command: got %q, want %q", windowIndex, paneIndex, decodedPane.Command, pane.Command)
			}
			if decodedPane.Split != pane.Split {
				t.Errorf("window[%d].pane[%d].Split: got %q, want %q", windowIndex, paneIndex, decodedPane.Split, pane.Split)
			}
			if decodedPane.Size != pane.Size {
				t.Errorf("window[%d].pane[%d].Size: got %d, want %d", windowIndex, paneIndex, decodedPane.Size, pane.Size)
			}
		}
	}
}

func TestLayoutContentPrincipalLayout(t *testing.T) {
	// A principal layout uses "role" instead of "observe" or "command".
	// The launcher resolves roles to concrete commands.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent", Split: "horizontal", Size: 65},
					{Role: "shell", Size: 35},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Prefix should be omitted when empty (uses Bureau default).
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	windows := raw["windows"].([]any)
	mainWindow := windows[0].(map[string]any)
	panes := mainWindow["panes"].([]any)
	agentPane := panes[0].(map[string]any)
	assertField(t, agentPane, "role", "agent")
	assertField(t, agentPane, "size", float64(65))

	// Observe and command should not appear in principal layouts.
	if _, exists := agentPane["observe"]; exists {
		t.Error("observe should be omitted when empty")
	}
	if _, exists := agentPane["command"]; exists {
		t.Error("command should be omitted when empty")
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != "" {
		t.Errorf("Prefix should be empty, got %q", decoded.Prefix)
	}
	if decoded.Windows[0].Panes[0].Role != "agent" {
		t.Errorf("Role: got %q, want %q", decoded.Windows[0].Panes[0].Role, "agent")
	}
}

func TestLayoutContentObserveMembers(t *testing.T) {
	// Dynamic pane creation from room membership. The daemon expands
	// ObserveMembers into concrete observe panes at runtime.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "team",
				Panes: []LayoutPane{
					{
						ObserveMembers: &LayoutMemberFilter{Labels: map[string]string{"role": "agent"}},
						Split:          "horizontal",
					},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	windows := raw["windows"].([]any)
	panes := windows[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	observeMembers, ok := pane["observe_members"].(map[string]any)
	if !ok {
		t.Fatal("observe_members field missing or wrong type")
	}
	labels, ok := observeMembers["labels"].(map[string]any)
	if !ok {
		t.Fatal("observe_members.labels field missing or wrong type")
	}
	assertField(t, labels, "role", "agent")

	// Other pane mode fields should be absent.
	for _, field := range []string{"observe", "command", "role"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when ObserveMembers is set", field)
		}
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	decodedPane := decoded.Windows[0].Panes[0]
	if decodedPane.ObserveMembers == nil {
		t.Fatal("ObserveMembers should not be nil after round-trip")
	}
	if decodedPane.ObserveMembers.Labels["role"] != "agent" {
		t.Errorf("ObserveMembers.Labels[role]: got %q, want %q", decodedPane.ObserveMembers.Labels["role"], "agent")
	}
}

func TestLayoutContentSourceMachineRoundTrip(t *testing.T) {
	// SourceMachine and SealedMetadata are set by the daemon before
	// publishing; verify they survive JSON serialization.
	original := LayoutContent{
		SourceMachine:  "@machine/workstation:bureau.local",
		SealedMetadata: "age-encrypted-blob-base64",
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent"},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "source_machine", "@machine/workstation:bureau.local")
	assertField(t, raw, "sealed_metadata", "age-encrypted-blob-base64")

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.SourceMachine != original.SourceMachine {
		t.Errorf("SourceMachine: got %q, want %q", decoded.SourceMachine, original.SourceMachine)
	}
	if decoded.SealedMetadata != original.SealedMetadata {
		t.Errorf("SealedMetadata: got %q, want %q", decoded.SealedMetadata, original.SealedMetadata)
	}
}

func TestLayoutContentOmitsEmptySourceMachine(t *testing.T) {
	// When SourceMachine and SealedMetadata are empty, they should be
	// omitted from the JSON to keep the wire format clean.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{Name: "main", Panes: []LayoutPane{{Role: "agent"}}},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"source_machine", "sealed_metadata", "prefix"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestLayoutContentOmitsEmptyFields(t *testing.T) {
	// Verify that zero-value optional fields are omitted from JSON.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "minimal",
				Panes: []LayoutPane{
					{Observe: "test/agent"},
				},
			},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Top-level prefix should be omitted.
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	panes := raw["windows"].([]any)[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	for _, field := range []string{"command", "role", "observe_members", "split", "size"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when zero-value", field)
		}
	}
}
