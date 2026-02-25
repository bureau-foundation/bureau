// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

func TestExpandMembersNilLayout(t *testing.T) {
	t.Parallel()
	result := ExpandMembers(nil, []RoomMember{{Localpart: "a/b"}})
	if result != nil {
		t.Errorf("ExpandMembers(nil, ...) = %v, want nil", result)
	}
}

func TestExpandMembersNoObserveMembers(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Prefix: "C-a",
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Command: "htop"},
					{Observe: "iree/amdgpu/pm", Split: observation.SplitHorizontal, Size: 50},
				},
			},
		},
	}

	result := ExpandMembers(layout, []RoomMember{{Localpart: "test/agent"}})

	if result.Prefix != "C-a" {
		t.Errorf("Prefix = %q, want %q", result.Prefix, "C-a")
	}
	if len(result.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(result.Windows))
	}
	if len(result.Windows[0].Panes) != 2 {
		t.Fatalf("pane count = %d, want 2", len(result.Windows[0].Panes))
	}
	if result.Windows[0].Panes[0].Command != "htop" {
		t.Errorf("pane 0 command = %q, want %q", result.Windows[0].Panes[0].Command, "htop")
	}
	if result.Windows[0].Panes[1].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 1 observe = %q, want %q", result.Windows[0].Panes[1].Observe, "iree/amdgpu/pm")
	}
}

func TestExpandMembersAllMembers(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{ObserveMembers: &MemberFilter{}},
				},
			},
		},
	}

	members := []RoomMember{
		{Localpart: "iree/amdgpu/pm"},
		{Localpart: "iree/amdgpu/codegen"},
		{Localpart: "iree/amdgpu/test"},
	}

	result := ExpandMembers(layout, members)

	if len(result.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(result.Windows))
	}
	panes := result.Windows[0].Panes
	if len(panes) != 3 {
		t.Fatalf("pane count = %d, want 3", len(panes))
	}

	// First pane inherits the original position (no split, since the
	// ObserveMembers pane was the first pane in the window).
	if panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q", panes[0].Observe, "iree/amdgpu/pm")
	}
	if panes[0].Split != "" {
		t.Errorf("pane 0 split = %q, want empty (first pane)", panes[0].Split)
	}

	// Subsequent panes default to vertical stacking since the original
	// had no split direction.
	if panes[1].Observe != "iree/amdgpu/codegen" {
		t.Errorf("pane 1 observe = %q, want %q", panes[1].Observe, "iree/amdgpu/codegen")
	}
	if panes[1].Split != observation.SplitVertical {
		t.Errorf("pane 1 split = %q, want %q", panes[1].Split, observation.SplitVertical)
	}

	if panes[2].Observe != "iree/amdgpu/test" {
		t.Errorf("pane 2 observe = %q, want %q", panes[2].Observe, "iree/amdgpu/test")
	}
	if panes[2].Split != observation.SplitVertical {
		t.Errorf("pane 2 split = %q, want %q", panes[2].Split, observation.SplitVertical)
	}
}

func TestExpandMembersFilterByLabel(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name: "agents-only",
				Panes: []Pane{
					{
						ObserveMembers: &MemberFilter{Labels: map[string]string{"role": "agent"}},
						Split:          observation.SplitHorizontal,
						Size:           60,
					},
				},
			},
		},
	}

	members := []RoomMember{
		{Localpart: "iree/amdgpu/pm", Labels: map[string]string{"role": "agent", "team": "iree"}},
		{Localpart: "service/stt/whisper", Labels: map[string]string{"role": "service"}},
		{Localpart: "iree/amdgpu/codegen", Labels: map[string]string{"role": "agent", "team": "iree"}},
		{Localpart: "machine/workstation", Labels: map[string]string{"role": "machine"}},
	}

	result := ExpandMembers(layout, members)

	if len(result.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(result.Windows))
	}
	panes := result.Windows[0].Panes
	if len(panes) != 2 {
		t.Fatalf("pane count = %d, want 2 (only agents)", len(panes))
	}

	if panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q", panes[0].Observe, "iree/amdgpu/pm")
	}
	// First pane inherits the original split and size.
	if panes[0].Split != observation.SplitHorizontal {
		t.Errorf("pane 0 split = %q, want %q", panes[0].Split, observation.SplitHorizontal)
	}
	if panes[0].Size != 60 {
		t.Errorf("pane 0 size = %d, want %d", panes[0].Size, 60)
	}

	if panes[1].Observe != "iree/amdgpu/codegen" {
		t.Errorf("pane 1 observe = %q, want %q", panes[1].Observe, "iree/amdgpu/codegen")
	}
	// Subsequent panes get the split direction but no size.
	if panes[1].Split != observation.SplitHorizontal {
		t.Errorf("pane 1 split = %q, want %q", panes[1].Split, observation.SplitHorizontal)
	}
	if panes[1].Size != 0 {
		t.Errorf("pane 1 size = %d, want 0 (even split)", panes[1].Size)
	}
}

func TestExpandMembersNoMatchRemovesPane(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{Command: "htop"},
					{
						ObserveMembers: &MemberFilter{Labels: map[string]string{"role": "nonexistent"}},
						Split:          observation.SplitHorizontal,
					},
				},
			},
		},
	}

	members := []RoomMember{
		{Localpart: "iree/amdgpu/pm", Labels: map[string]string{"role": "agent"}},
	}

	result := ExpandMembers(layout, members)

	if len(result.Windows) != 1 {
		t.Fatalf("window count = %d, want 1", len(result.Windows))
	}
	// Only the htop pane should remain.
	if len(result.Windows[0].Panes) != 1 {
		t.Fatalf("pane count = %d, want 1 (observe_members removed)", len(result.Windows[0].Panes))
	}
	if result.Windows[0].Panes[0].Command != "htop" {
		t.Errorf("remaining pane command = %q, want %q", result.Windows[0].Panes[0].Command, "htop")
	}
}

func TestExpandMembersEmptyWindowRemoved(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name: "static",
				Panes: []Pane{
					{Command: "htop"},
				},
			},
			{
				Name: "dynamic",
				Panes: []Pane{
					{ObserveMembers: &MemberFilter{Labels: map[string]string{"role": "nonexistent"}}},
				},
			},
		},
	}

	// No members match the "nonexistent" label, so the dynamic window
	// should be removed entirely.
	result := ExpandMembers(layout, []RoomMember{
		{Localpart: "test/agent", Labels: map[string]string{"role": "agent"}},
	})

	if len(result.Windows) != 1 {
		t.Fatalf("window count = %d, want 1 (empty window removed)", len(result.Windows))
	}
	if result.Windows[0].Name != "static" {
		t.Errorf("remaining window name = %q, want %q", result.Windows[0].Name, "static")
	}
}

func TestExpandMembersMixedPanes(t *testing.T) {
	t.Parallel()
	// Layout with a static command pane followed by an observe_members pane.
	layout := &Layout{
		Windows: []Window{
			{
				Name: "workspace",
				Panes: []Pane{
					{Command: "htop"},
					{
						ObserveMembers: &MemberFilter{},
						Split:          observation.SplitHorizontal,
						Size:           70,
					},
				},
			},
		},
	}

	members := []RoomMember{
		{Localpart: "a/one"},
		{Localpart: "a/two"},
	}

	result := ExpandMembers(layout, members)

	panes := result.Windows[0].Panes
	if len(panes) != 3 {
		t.Fatalf("pane count = %d, want 3 (htop + 2 expanded)", len(panes))
	}

	if panes[0].Command != "htop" {
		t.Errorf("pane 0 = command %q, want htop", panes[0].Command)
	}
	if panes[1].Observe != "a/one" {
		t.Errorf("pane 1 observe = %q, want %q", panes[1].Observe, "a/one")
	}
	if panes[1].Split != observation.SplitHorizontal || panes[1].Size != 70 {
		t.Errorf("pane 1 position = split=%q size=%d, want horizontal/70", panes[1].Split, panes[1].Size)
	}
	if panes[2].Observe != "a/two" {
		t.Errorf("pane 2 observe = %q, want %q", panes[2].Observe, "a/two")
	}
	if panes[2].Split != observation.SplitHorizontal || panes[2].Size != 0 {
		t.Errorf("pane 2 position = split=%q size=%d, want horizontal/0", panes[2].Split, panes[2].Size)
	}
}

func TestExpandMembersPreservesOriginal(t *testing.T) {
	t.Parallel()
	original := &Layout{
		Prefix: "C-b",
		Windows: []Window{
			{
				Name: "main",
				Panes: []Pane{
					{ObserveMembers: &MemberFilter{Labels: map[string]string{"role": "agent"}}},
				},
			},
		},
	}

	result := ExpandMembers(original, []RoomMember{
		{Localpart: "test/a", Labels: map[string]string{"role": "agent"}},
	})

	// Original should be unmodified.
	if original.Windows[0].Panes[0].ObserveMembers == nil {
		t.Error("original ObserveMembers was cleared")
	}
	if original.Windows[0].Panes[0].Observe != "" {
		t.Error("original pane gained an Observe field")
	}

	// Result should have the expanded pane.
	if result.Prefix != "C-b" {
		t.Errorf("result prefix = %q, want %q", result.Prefix, "C-b")
	}
	if len(result.Windows[0].Panes) != 1 {
		t.Fatalf("result pane count = %d, want 1", len(result.Windows[0].Panes))
	}
	if result.Windows[0].Panes[0].Observe != "test/a" {
		t.Errorf("result pane observe = %q, want %q", result.Windows[0].Panes[0].Observe, "test/a")
	}
}

func TestExpandMembersEmptyMembersList(t *testing.T) {
	t.Parallel()
	layout := &Layout{
		Windows: []Window{
			{
				Name: "dynamic",
				Panes: []Pane{
					{ObserveMembers: &MemberFilter{}},
				},
			},
		},
	}

	// No members at all — the window should be removed.
	result := ExpandMembers(layout, nil)

	if len(result.Windows) != 0 {
		t.Errorf("window count = %d, want 0 (no members to expand)", len(result.Windows))
	}
}

func TestFilterMembersEmptyLabels(t *testing.T) {
	t.Parallel()
	members := []RoomMember{
		{Localpart: "a", Labels: map[string]string{"role": "agent"}},
		{Localpart: "b", Labels: map[string]string{"role": "service"}},
		{Localpart: "c"},
	}

	// Empty labels matches all.
	result := filterMembers(members, &MemberFilter{})
	if len(result) != 3 {
		t.Errorf("filterMembers with empty labels returned %d, want 3", len(result))
	}
}

func TestFilterMembersSingleLabel(t *testing.T) {
	t.Parallel()
	members := []RoomMember{
		{Localpart: "a", Labels: map[string]string{"role": "agent"}},
		{Localpart: "b", Labels: map[string]string{"role": "service"}},
		{Localpart: "c", Labels: map[string]string{"role": "agent", "team": "iree"}},
	}

	result := filterMembers(members, &MemberFilter{Labels: map[string]string{"role": "agent"}})
	if len(result) != 2 {
		t.Errorf("filterMembers with role=agent returned %d, want 2", len(result))
	}
	if result[0].Localpart != "a" || result[1].Localpart != "c" {
		t.Errorf("filterMembers returned wrong members: %v", result)
	}
}

func TestFilterMembersMultipleLabels(t *testing.T) {
	t.Parallel()
	members := []RoomMember{
		{Localpart: "a", Labels: map[string]string{"role": "agent", "team": "iree"}},
		{Localpart: "b", Labels: map[string]string{"role": "agent", "team": "infra"}},
		{Localpart: "c", Labels: map[string]string{"role": "service", "team": "iree"}},
		{Localpart: "d", Labels: map[string]string{"role": "agent", "team": "iree", "tier": "gpu"}},
	}

	// Both labels must match.
	result := filterMembers(members, &MemberFilter{Labels: map[string]string{"role": "agent", "team": "iree"}})
	if len(result) != 2 {
		t.Errorf("filterMembers with role=agent,team=iree returned %d, want 2", len(result))
	}
	if result[0].Localpart != "a" || result[1].Localpart != "d" {
		t.Errorf("filterMembers returned wrong members: %v", result)
	}
}

func TestFilterMembersNilMemberLabels(t *testing.T) {
	t.Parallel()
	// Members with nil labels should not match label-specific filters
	// but should match empty filters.
	members := []RoomMember{
		{Localpart: "a"},
		{Localpart: "b", Labels: map[string]string{"role": "agent"}},
	}

	// With filter: only "b" matches.
	result := filterMembers(members, &MemberFilter{Labels: map[string]string{"role": "agent"}})
	if len(result) != 1 {
		t.Fatalf("filterMembers returned %d, want 1", len(result))
	}
	if result[0].Localpart != "b" {
		t.Errorf("filterMembers returned %q, want %q", result[0].Localpart, "b")
	}

	// Without filter: both match.
	result = filterMembers(members, &MemberFilter{})
	if len(result) != 2 {
		t.Errorf("filterMembers with empty filter returned %d, want 2", len(result))
	}
}
