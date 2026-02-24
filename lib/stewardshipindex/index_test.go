// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package stewardshipindex

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
)

// testRoomID returns a ref.RoomID for use in tests.
func testRoomID(id string) ref.RoomID {
	roomID, err := ref.ParseRoomID(id)
	if err != nil {
		panic("invalid test room ID: " + err.Error())
	}
	return roomID
}

// makeDeclarationContent returns a StewardshipContent with the given
// resource patterns and sensible defaults.
func makeDeclarationContent(patterns ...string) stewardship.StewardshipContent {
	return stewardship.StewardshipContent{
		Version:          stewardship.StewardshipContentVersion,
		ResourcePatterns: patterns,
		Tiers: []stewardship.StewardshipTier{
			{
				Principals: []string{"iree/amdgpu/pm:bureau.local"},
				Escalation: "immediate",
			},
		},
	}
}

func TestNewIndex(t *testing.T) {
	idx := NewIndex()
	if idx.Len() != 0 {
		t.Errorf("Len() = %d, want 0", idx.Len())
	}
}

func TestPutAndLen(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	if idx.Len() != 1 {
		t.Errorf("Len() = %d, want 1", idx.Len())
	}

	idx.Put(roomA, "workspace/lib", makeDeclarationContent("workspace/lib/**"))
	if idx.Len() != 2 {
		t.Errorf("Len() = %d, want 2", idx.Len())
	}
}

func TestPutOverwritesExisting(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	if idx.Len() != 1 {
		t.Errorf("after first Put: Len() = %d, want 1", idx.Len())
	}

	// Overwrite with different patterns.
	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/a100"))
	if idx.Len() != 1 {
		t.Errorf("after second Put: Len() = %d, want 1 (should overwrite)", idx.Len())
	}

	// Verify the updated content is used for resolution.
	matches := idx.Resolve([]string{"fleet/gpu/v100"})
	if len(matches) != 0 {
		t.Errorf("Resolve(fleet/gpu/v100) = %d matches, want 0 (old pattern replaced)", len(matches))
	}
	matches = idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Errorf("Resolve(fleet/gpu/a100) = %d matches, want 1", len(matches))
	}
}

func TestRemove(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	idx.Remove(roomA, "fleet/gpu")
	if idx.Len() != 0 {
		t.Errorf("after Remove: Len() = %d, want 0", idx.Len())
	}

	// Resolve should find nothing.
	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 0 {
		t.Errorf("Resolve after Remove: %d matches, want 0", len(matches))
	}
}

func TestRemoveNonexistent(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")

	// Should not panic.
	idx.Remove(roomA, "nonexistent")
}

func TestResolveExactMatch(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/a100"))

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("Resolve: got %d matches, want 1", len(matches))
	}
	if matches[0].MatchedPattern != "fleet/gpu/a100" {
		t.Errorf("MatchedPattern = %q, want %q", matches[0].MatchedPattern, "fleet/gpu/a100")
	}
	if matches[0].MatchedResource != "fleet/gpu/a100" {
		t.Errorf("MatchedResource = %q, want %q", matches[0].MatchedResource, "fleet/gpu/a100")
	}
	if matches[0].Declaration.StateKey != "fleet/gpu" {
		t.Errorf("Declaration.StateKey = %q, want %q", matches[0].Declaration.StateKey, "fleet/gpu")
	}
}

func TestResolveSingleWildcard(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/*"))

	// Should match — single-segment wildcard.
	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("Resolve(fleet/gpu/a100): got %d matches, want 1", len(matches))
	}

	// Should not match — wildcard doesn't cross boundaries.
	matches = idx.Resolve([]string{"fleet/gpu/a100/batch"})
	if len(matches) != 0 {
		t.Errorf("Resolve(fleet/gpu/a100/batch): got %d matches, want 0 (single wildcard doesn't cross /)", len(matches))
	}
}

func TestResolveDoubleWildcard(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet", makeDeclarationContent("fleet/**"))

	// Matches at various depths.
	for _, resource := range []string{
		"fleet/gpu",
		"fleet/gpu/a100",
		"fleet/gpu/a100/batch/queue",
	} {
		matches := idx.Resolve([]string{resource})
		if len(matches) != 1 {
			t.Errorf("Resolve(%q): got %d matches, want 1", resource, len(matches))
		}
	}

	// Does not match different top-level.
	matches := idx.Resolve([]string{"workspace/lib/schema"})
	if len(matches) != 0 {
		t.Errorf("Resolve(workspace/lib/schema): got %d matches, want 0", len(matches))
	}
}

func TestResolveInteriorWildcard(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet-agents", makeDeclarationContent("fleet/**/agent"))

	matches := idx.Resolve([]string{"fleet/gpu/a100/agent"})
	if len(matches) != 1 {
		t.Fatalf("Resolve(fleet/gpu/a100/agent): got %d matches, want 1", len(matches))
	}

	// Zero-segment case: "fleet/agent" matches (** matches nothing).
	matches = idx.Resolve([]string{"fleet/agent"})
	if len(matches) != 1 {
		t.Errorf("Resolve(fleet/agent): got %d matches, want 1 (zero-segment ** match)", len(matches))
	}

	// Does not match — missing trailing "agent".
	matches = idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 0 {
		t.Errorf("Resolve(fleet/gpu/a100): got %d matches, want 0", len(matches))
	}
}

func TestResolveNoMatch(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))

	matches := idx.Resolve([]string{"workspace/lib/schema"})
	if len(matches) != 0 {
		t.Errorf("Resolve: got %d matches, want 0", len(matches))
	}
}

func TestResolveEmptyAffects(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))

	matches := idx.Resolve(nil)
	if matches != nil {
		t.Errorf("Resolve(nil): got %v, want nil", matches)
	}

	matches = idx.Resolve([]string{})
	if matches != nil {
		t.Errorf("Resolve([]): got %v, want nil", matches)
	}
}

func TestResolveEmptyIndex(t *testing.T) {
	idx := NewIndex()

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if matches != nil {
		t.Errorf("Resolve on empty index: got %v, want nil", matches)
	}
}

func TestResolveCrossRoom(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	roomB := testRoomID("!roomB:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	idx.Put(roomB, "fleet/all", makeDeclarationContent("fleet/**"))

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 2 {
		t.Fatalf("Resolve: got %d matches, want 2 (one from each room)", len(matches))
	}

	// Verify both rooms are represented.
	rooms := make(map[string]bool)
	for _, match := range matches {
		rooms[match.Declaration.RoomID.String()] = true
	}
	if !rooms[roomA.String()] {
		t.Error("missing match from roomA")
	}
	if !rooms[roomB.String()] {
		t.Error("missing match from roomB")
	}
}

func TestResolveForRoom(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	roomB := testRoomID("!roomB:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	idx.Put(roomB, "fleet/all", makeDeclarationContent("fleet/**"))

	// Scoped to roomA.
	matches := idx.ResolveForRoom(roomA, []string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("ResolveForRoom(roomA): got %d matches, want 1", len(matches))
	}
	if matches[0].Declaration.RoomID != roomA {
		t.Errorf("ResolveForRoom(roomA): match from wrong room %s", matches[0].Declaration.RoomID)
	}

	// Empty affects.
	matches = idx.ResolveForRoom(roomA, nil)
	if matches != nil {
		t.Errorf("ResolveForRoom(nil affects): got %v, want nil", matches)
	}
}

func TestResolveMultiplePatterns(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "mixed", makeDeclarationContent("fleet/**", "workspace/**"))

	// Matches the first pattern.
	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("Resolve(fleet/gpu/a100): got %d matches, want 1", len(matches))
	}
	if matches[0].MatchedPattern != "fleet/**" {
		t.Errorf("MatchedPattern = %q, want %q", matches[0].MatchedPattern, "fleet/**")
	}

	// Matches the second pattern.
	matches = idx.Resolve([]string{"workspace/lib/schema"})
	if len(matches) != 1 {
		t.Fatalf("Resolve(workspace/lib/schema): got %d matches, want 1", len(matches))
	}
	if matches[0].MatchedPattern != "workspace/**" {
		t.Errorf("MatchedPattern = %q, want %q", matches[0].MatchedPattern, "workspace/**")
	}
}

func TestResolveMultipleAffects(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "fleet", makeDeclarationContent("fleet/**"))

	matches := idx.Resolve([]string{"fleet/gpu/a100", "fleet/cpu/batch"})
	if len(matches) != 2 {
		t.Fatalf("Resolve(two affects): got %d matches, want 2", len(matches))
	}

	// Verify both resources are present.
	resources := make(map[string]bool)
	for _, match := range matches {
		resources[match.MatchedResource] = true
	}
	if !resources["fleet/gpu/a100"] {
		t.Error("missing match for fleet/gpu/a100")
	}
	if !resources["fleet/cpu/batch"] {
		t.Error("missing match for fleet/cpu/batch")
	}
}

func TestResolveMultiplePatternsMatchSameResource(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	// Both patterns match fleet/gpu/a100.
	idx.Put(roomA, "fleet", makeDeclarationContent("fleet/**", "fleet/gpu/**"))

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 2 {
		t.Fatalf("Resolve: got %d matches, want 2 (one per matching pattern)", len(matches))
	}

	patterns := make(map[string]bool)
	for _, match := range matches {
		patterns[match.MatchedPattern] = true
	}
	if !patterns["fleet/**"] {
		t.Error("missing match for pattern fleet/**")
	}
	if !patterns["fleet/gpu/**"] {
		t.Error("missing match for pattern fleet/gpu/**")
	}
}

func TestDeclarationsInRoom(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	roomB := testRoomID("!roomB:bureau.local")

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	idx.Put(roomA, "workspace", makeDeclarationContent("workspace/**"))
	idx.Put(roomB, "fleet/cpu", makeDeclarationContent("fleet/cpu/**"))

	declarations := idx.DeclarationsInRoom(roomA)
	if len(declarations) != 2 {
		t.Fatalf("DeclarationsInRoom(roomA): got %d, want 2", len(declarations))
	}

	declarations = idx.DeclarationsInRoom(roomB)
	if len(declarations) != 1 {
		t.Fatalf("DeclarationsInRoom(roomB): got %d, want 1", len(declarations))
	}

	roomC := testRoomID("!roomC:bureau.local")
	declarations = idx.DeclarationsInRoom(roomC)
	if len(declarations) != 0 {
		t.Errorf("DeclarationsInRoom(roomC): got %d, want 0", len(declarations))
	}
}

func TestResolveUniversalPattern(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	idx.Put(roomA, "everything", makeDeclarationContent("**"))

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("Resolve with ** pattern: got %d matches, want 1", len(matches))
	}

	matches = idx.Resolve([]string{"any/arbitrary/path"})
	if len(matches) != 1 {
		t.Errorf("Resolve with ** pattern: got %d matches, want 1", len(matches))
	}
}

func TestAll(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")
	roomB := testRoomID("!roomB:bureau.local")

	// Empty index returns nil.
	if all := idx.All(); all != nil {
		t.Errorf("All() on empty index: got %v, want nil", all)
	}

	idx.Put(roomA, "fleet/gpu", makeDeclarationContent("fleet/gpu/**"))
	idx.Put(roomA, "workspace", makeDeclarationContent("workspace/**"))
	idx.Put(roomB, "fleet/cpu", makeDeclarationContent("fleet/cpu/**"))

	all := idx.All()
	if len(all) != 3 {
		t.Fatalf("All(): got %d declarations, want 3", len(all))
	}

	// Verify all state keys are present.
	stateKeys := make(map[string]bool)
	for _, declaration := range all {
		stateKeys[declaration.StateKey] = true
	}
	for _, expected := range []string{"fleet/gpu", "workspace", "fleet/cpu"} {
		if !stateKeys[expected] {
			t.Errorf("All() missing declaration with state key %q", expected)
		}
	}
}

func TestDeclarationRoomIDPreserved(t *testing.T) {
	idx := NewIndex()
	roomA := testRoomID("!roomA:bureau.local")

	content := makeDeclarationContent("fleet/gpu/**")
	content.Description = "GPU fleet governance"
	idx.Put(roomA, "fleet/gpu", content)

	matches := idx.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("Resolve: got %d matches, want 1", len(matches))
	}

	declaration := matches[0].Declaration
	if declaration.RoomID != roomA {
		t.Errorf("Declaration.RoomID = %s, want %s", declaration.RoomID, roomA)
	}
	if declaration.StateKey != "fleet/gpu" {
		t.Errorf("Declaration.StateKey = %q, want %q", declaration.StateKey, "fleet/gpu")
	}
	if declaration.Content.Description != "GPU fleet governance" {
		t.Errorf("Declaration.Content.Description = %q, want %q",
			declaration.Content.Description, "GPU fleet governance")
	}
}
