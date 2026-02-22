// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// --- List rooms tests ---

func TestHandleListRoomsReturnsSummary(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []roomInfo
	err := client.Call(context.Background(), "list-rooms", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("got %d rooms, want 2", len(result))
	}

	// Build a map for order-independent assertion.
	byRoom := make(map[string]roomInfo)
	for _, room := range result {
		byRoom[room.RoomID] = room
	}

	roomA, exists := byRoom["!roomA:local"]
	if !exists {
		t.Fatal("missing roomA in response")
	}
	if roomA.Stats.Total != 3 {
		t.Errorf("roomA total: got %d, want 3", roomA.Stats.Total)
	}
	// newTrackedRoom uses empty prefix → handler should default to "tkt".
	if roomA.Prefix != "tkt" {
		t.Errorf("roomA prefix: got %q, want 'tkt'", roomA.Prefix)
	}

	roomB, exists := byRoom["!roomB:local"]
	if !exists {
		t.Fatal("missing roomB in response")
	}
	if roomB.Stats.Total != 1 {
		t.Errorf("roomB total: got %d, want 1", roomB.Stats.Total)
	}
}

func TestHandleListRoomsIncludesAlias(t *testing.T) {
	rooms := sampleRooms()
	rooms[testRoomID("!roomA:local")].alias = "#general:bureau.local"

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []roomInfo
	err := client.Call(context.Background(), "list-rooms", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	byRoom := make(map[string]roomInfo)
	for _, room := range result {
		byRoom[room.RoomID] = room
	}

	roomA := byRoom["!roomA:local"]
	if roomA.Alias != "#general:bureau.local" {
		t.Errorf("alias: got %q, want '#general:bureau.local'", roomA.Alias)
	}
}

func TestHandleListRoomsCustomPrefix(t *testing.T) {
	rooms := sampleRooms()
	rooms[testRoomID("!roomA:local")].config.Prefix = "iree"

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []roomInfo
	err := client.Call(context.Background(), "list-rooms", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	byRoom := make(map[string]roomInfo)
	for _, room := range result {
		byRoom[room.RoomID] = room
	}

	if byRoom["!roomA:local"].Prefix != "iree" {
		t.Errorf("prefix: got %q, want 'iree'", byRoom["!roomA:local"].Prefix)
	}
}

func TestHandleListRoomsEmptyReturnsEmptySlice(t *testing.T) {
	client, cleanup := testServer(t, map[ref.RoomID]*roomState{})
	defer cleanup()

	var result []roomInfo
	err := client.Call(context.Background(), "list-rooms", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result == nil {
		t.Fatal("expected empty slice, got nil")
	}
	if len(result) != 0 {
		t.Fatalf("got %d rooms, want 0", len(result))
	}
}

// --- List tests ---

func TestHandleListReturnsAllTickets(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("got %d entries, want 3", len(result))
	}
}

func TestHandleListFilterByStatus(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":   "!roomA:local",
		"status": "open",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("got %d entries, want 2 open tickets", len(result))
	}
	for _, entry := range result {
		if entry.Content.Status != "open" {
			t.Errorf("ticket %s has status %q, want open", entry.ID, entry.Content.Status)
		}
	}
}

func TestHandleListFilterByPriority(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	priority := 0
	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":     "!roomA:local",
		"priority": priority,
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d entries, want 1 critical ticket", len(result))
	}
	if result[0].ID != "tkt-2" {
		t.Errorf("got ticket %s, want tkt-2", result[0].ID)
	}
}

func TestHandleListFilterByLabel(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":  "!roomA:local",
		"label": "backend",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d entries, want 1", len(result))
	}
	if result[0].ID != "tkt-2" {
		t.Errorf("got ticket %s, want tkt-2", result[0].ID)
	}
}

func TestHandleListMissingRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "list", nil, nil)
	requireServiceError(t, err)
}

func TestHandleListUnknownRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "list", map[string]any{
		"room": "!nonexistent:local",
	}, nil)
	requireServiceError(t, err)
}

// --- Ready tests ---

func TestHandleReady(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "ready", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-1 is open with no blockers → ready.
	// tkt-2 is closed → not ready.
	// tkt-3 is open but blocked by tkt-2 (which is closed) → ready.
	if len(result) != 2 {
		t.Fatalf("got %d ready tickets, want 2", len(result))
	}
}

// --- Blocked tests ---

func TestHandleBlocked(t *testing.T) {
	// Create a room where tkt-3 is blocked by tkt-1 (which is open).
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-1": {Version: 1, Title: "blocker", Status: "open", Priority: 1, CreatedAt: "2026-01-01T00:00:00Z"},
			"tkt-3": {Version: 1, Title: "blocked", Status: "open", Priority: 2, BlockedBy: []string{"tkt-1"}, CreatedAt: "2026-01-02T00:00:00Z"},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "blocked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d blocked tickets, want 1", len(result))
	}
	if result[0].ID != "tkt-3" {
		t.Errorf("got ticket %s, want tkt-3", result[0].ID)
	}
}

// --- Show tests ---

func TestHandleShow(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-1" {
		t.Errorf("id: got %q, want tkt-1", result.ID)
	}
	if result.Room != "!roomA:local" {
		t.Errorf("room: got %q, want !roomA:local", result.Room)
	}
	if result.Content.Title != "implement login" {
		t.Errorf("title: got %q, want 'implement login'", result.Content.Title)
	}
	if result.Content.Body != "add OAuth support" {
		t.Errorf("body: got %q, want 'add OAuth support'", result.Content.Body)
	}
	if result.Content.Assignee != ref.MustParseUserID("@agent/coder:bureau.local") {
		t.Errorf("assignee: got %q", result.Content.Assignee)
	}
}

func TestHandleShowComputedFields(t *testing.T) {
	// tkt-3 is a child of tkt-1 and blocked by tkt-2.
	// tkt-1 should show: blocks=[] (nothing blocked by it in blocked_by),
	// child_total=1, child_closed=0.
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-1 has one child (tkt-3).
	if result.ChildTotal != 1 {
		t.Errorf("child_total: got %d, want 1", result.ChildTotal)
	}
	if result.ChildClosed != 0 {
		t.Errorf("child_closed: got %d, want 0", result.ChildClosed)
	}

	// Show tkt-2 — tkt-3 is blocked by it, so Blocks should include tkt-3.
	var result2 showResponse
	err = client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result2)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(result2.Blocks) != 1 || result2.Blocks[0] != "tkt-3" {
		t.Errorf("blocks: got %v, want [tkt-3]", result2.Blocks)
	}
}

func TestHandleShowNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleShowMissingTicketField(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "show", nil, nil)
	requireServiceError(t, err)
}

// --- Children tests ---

func TestHandleChildren(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result childrenResponse
	err := client.Call(context.Background(), "children", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Parent != "tkt-1" {
		t.Errorf("parent: got %q, want tkt-1", result.Parent)
	}
	if len(result.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(result.Children))
	}
	if result.Children[0].ID != "tkt-3" {
		t.Errorf("child: got %q, want tkt-3", result.Children[0].ID)
	}
	if result.ChildTotal != 1 {
		t.Errorf("child_total: got %d, want 1", result.ChildTotal)
	}
	if result.ChildClosed != 0 {
		t.Errorf("child_closed: got %d, want 0", result.ChildClosed)
	}
}

func TestHandleChildrenNoChildren(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result childrenResponse
	err := client.Call(context.Background(), "children", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Children) != 0 {
		t.Fatalf("got %d children, want 0", len(result.Children))
	}
}

// --- Grep tests ---

func TestHandleGrepRoomScoped(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "login",
		"room":    "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-1" {
		t.Errorf("got ticket %s, want tkt-1", result[0].ID)
	}
}

func TestHandleGrepCrossRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// "deploy" only appears in room B.
	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "deploy",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-10" {
		t.Errorf("got ticket %s, want tkt-10", result[0].ID)
	}
	if result[0].Room != "!roomB:local" {
		t.Errorf("room: got %q, want !roomB:local", result[0].Room)
	}
}

func TestHandleGrepBodySearch(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// "OAuth" is in tkt-1's body, not title.
	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "OAuth",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-1" {
		t.Errorf("got ticket %s, want tkt-1", result[0].ID)
	}
}

func TestHandleGrepNoResults(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "xyzzy",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 0 {
		t.Fatalf("got %d results, want 0", len(result))
	}
}

func TestHandleGrepInvalidRegex(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "[invalid",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleGrepMissingPattern(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "grep", nil, nil)
	requireServiceError(t, err)
}

// --- Stats tests ---

func TestHandleStats(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result ticketindex.Stats
	err := client.Call(context.Background(), "stats", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Total != 3 {
		t.Errorf("total: got %d, want 3", result.Total)
	}
	if result.ByStatus["open"] != 2 {
		t.Errorf("open: got %d, want 2", result.ByStatus["open"])
	}
	if result.ByStatus["closed"] != 1 {
		t.Errorf("closed: got %d, want 1", result.ByStatus["closed"])
	}
}

// --- Deps tests ---

func TestHandleDeps(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result depsResponse
	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "tkt-3",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Ticket != "tkt-3" {
		t.Errorf("ticket: got %q, want tkt-3", result.Ticket)
	}
	if len(result.Deps) != 1 || result.Deps[0] != "tkt-2" {
		t.Errorf("deps: got %v, want [tkt-2]", result.Deps)
	}
}

func TestHandleDepsNoDependencies(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result depsResponse
	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Deps) != 0 {
		t.Errorf("deps: got %v, want empty", result.Deps)
	}
}

func TestHandleDepsNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

// --- Ranked tests ---

func TestHandleRanked(t *testing.T) {
	// Set up a room with scoring-relevant structure: one ticket
	// that unblocks another (high leverage) and one that doesn't.
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-blocker": {
				Version:   1,
				Title:     "high leverage blocker",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-10T00:00:00Z",
			},
			"tkt-ready-a": {
				Version:   1,
				Title:     "ready ticket A",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				BlockedBy: []string{"tkt-blocker"},
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-ready-b": {
				Version:   1,
				Title:     "ready ticket B unblocks downstream",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-blocked": {
				Version:   1,
				Title:     "blocked by B",
				Status:    "open",
				Priority:  0,
				Type:      "bug",
				BlockedBy: []string{"tkt-ready-b"},
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []rankedEntryResponse
	err := client.Call(context.Background(), "ranked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-ready-a and tkt-ready-b are ready. tkt-ready-b unblocks
	// a P0 (high leverage + borrowed priority) so it should rank first.
	if len(result) != 2 {
		t.Fatalf("got %d ranked entries, want 2", len(result))
	}
	if result[0].ID != "tkt-ready-b" {
		t.Errorf("first ranked entry: got %q, want tkt-ready-b", result[0].ID)
	}
	if result[0].Score.UnblockCount != 1 {
		t.Errorf("tkt-ready-b UnblockCount: got %d, want 1", result[0].Score.UnblockCount)
	}
	if result[0].Score.BorrowedPriority != 0 {
		t.Errorf("tkt-ready-b BorrowedPriority: got %d, want 0", result[0].Score.BorrowedPriority)
	}
	if result[1].ID != "tkt-ready-a" {
		t.Errorf("second ranked entry: got %q, want tkt-ready-a", result[1].ID)
	}
}

func TestHandleRankedEmpty(t *testing.T) {
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-closed": {
				Version:   1,
				Title:     "all done",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []rankedEntryResponse
	err := client.Call(context.Background(), "ranked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("got %d ranked entries, want 0", len(result))
	}
}

func TestHandleRankedMissingRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "ranked", nil, nil)
	requireServiceError(t, err)
}

// --- Epic health tests ---

func TestHandleEpicHealth(t *testing.T) {
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-epic": {
				Version:   1,
				Title:     "the epic",
				Status:    "open",
				Priority:  1,
				Type:      "epic",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-done": {
				Version:   1,
				Title:     "done child",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-05T00:00:00Z",
			},
			"tkt-ready": {
				Version:   1,
				Title:     "ready child",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-02T00:00:00Z",
			},
			"tkt-blocked-child": {
				Version:   1,
				Title:     "blocked child",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				BlockedBy: []string{"tkt-ready"},
				CreatedAt: "2026-01-02T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result epicHealthResponse
	err := client.Call(context.Background(), "epic-health", map[string]any{
		"ticket": "tkt-epic",
		"room":   "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Ticket != "tkt-epic" {
		t.Errorf("ticket: got %q, want tkt-epic", result.Ticket)
	}
	if result.Health.TotalChildren != 3 {
		t.Errorf("TotalChildren: got %d, want 3", result.Health.TotalChildren)
	}
	if result.Health.ClosedChildren != 1 {
		t.Errorf("ClosedChildren: got %d, want 1", result.Health.ClosedChildren)
	}
	if result.Health.ReadyChildren != 1 {
		t.Errorf("ReadyChildren: got %d, want 1", result.Health.ReadyChildren)
	}
	if result.Health.CriticalDepth != 1 {
		t.Errorf("CriticalDepth: got %d, want 1", result.Health.CriticalDepth)
	}
}

func TestHandleEpicHealthNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "epic-health", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleEpicHealthMissingTicketField(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "epic-health", nil, nil)
	requireServiceError(t, err)
}

// --- Show with score tests ---

func TestHandleShowIncludesScore(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-1 is open — should include a score.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Score == nil {
		t.Fatal("expected non-nil Score for open ticket")
	}
}

func TestHandleShowClosedTicketOmitsScore(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-2 is closed — score should be nil.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Score != nil {
		t.Errorf("expected nil Score for closed ticket, got %+v", result.Score)
	}
}

// --- Cross-room show test ---

func TestHandleShowCrossRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-10 is in room B.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-10",
		"room":   "!roomB:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!roomB:local" {
		t.Errorf("room: got %q, want !roomB:local", result.Room)
	}
	if result.Content.Title != "deploy service" {
		t.Errorf("title: got %q, want 'deploy service'", result.Content.Title)
	}
}
