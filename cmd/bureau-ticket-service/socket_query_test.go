// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
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
		if entry.Content.Status != ticket.StatusOpen {
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
			"tkt-1": {Version: 1, Title: "blocker", Status: ticket.StatusOpen, Priority: 1, CreatedAt: "2026-01-01T00:00:00Z"},
			"tkt-3": {Version: 1, Title: "blocked", Status: ticket.StatusOpen, Priority: 2, BlockedBy: []string{"tkt-1"}, CreatedAt: "2026-01-02T00:00:00Z"},
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
				Status:    ticket.StatusClosed,
				Priority:  2,
				Type:      ticket.TypeTask,
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-10T00:00:00Z",
			},
			"tkt-ready-a": {
				Version:   1,
				Title:     "ready ticket A",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeTask,
				BlockedBy: []string{"tkt-blocker"},
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-ready-b": {
				Version:   1,
				Title:     "ready ticket B unblocks downstream",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeTask,
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-blocked": {
				Version:   1,
				Title:     "blocked by B",
				Status:    ticket.StatusOpen,
				Priority:  0,
				Type:      ticket.TypeBug,
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
				Status:    ticket.StatusClosed,
				Priority:  2,
				Type:      ticket.TypeTask,
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
				Status:    ticket.StatusOpen,
				Priority:  1,
				Type:      ticket.TypeEpic,
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-done": {
				Version:   1,
				Title:     "done child",
				Status:    ticket.StatusClosed,
				Priority:  2,
				Type:      ticket.TypeTask,
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-05T00:00:00Z",
			},
			"tkt-ready": {
				Version:   1,
				Title:     "ready child",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeTask,
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-02T00:00:00Z",
			},
			"tkt-blocked-child": {
				Version:   1,
				Title:     "blocked child",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeTask,
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

// --- Stewardship context tests ---

func TestComputeTierProgress(t *testing.T) {
	threshold := 1
	review := &ticket.TicketReview{
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
			{Tier: 1, Threshold: nil},
		},
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:local"), Tier: 0, Disposition: "approved"},
			{UserID: ref.MustParseUserID("@bob:local"), Tier: 0, Disposition: "pending"},
			{UserID: ref.MustParseUserID("@carol:local"), Tier: 1, Disposition: "pending"},
		},
	}

	progress := computeTierProgress(review)

	if len(progress) != 2 {
		t.Fatalf("tier count = %d, want 2", len(progress))
	}

	// Tier 0: threshold=1, 1 of 2 approved → satisfied.
	tier0 := progress[0]
	if tier0.Tier != 0 {
		t.Errorf("tier[0].Tier = %d, want 0", tier0.Tier)
	}
	if tier0.Total != 2 {
		t.Errorf("tier[0].Total = %d, want 2", tier0.Total)
	}
	if tier0.Approved != 1 {
		t.Errorf("tier[0].Approved = %d, want 1", tier0.Approved)
	}
	if tier0.Threshold == nil || *tier0.Threshold != 1 {
		t.Errorf("tier[0].Threshold = %v, want 1", tier0.Threshold)
	}
	if !tier0.Satisfied {
		t.Error("tier[0] should be satisfied (threshold=1, approved=1)")
	}

	// Tier 1: threshold=nil (all must approve), 0 of 1 → not satisfied.
	tier1 := progress[1]
	if tier1.Tier != 1 {
		t.Errorf("tier[1].Tier = %d, want 1", tier1.Tier)
	}
	if tier1.Total != 1 {
		t.Errorf("tier[1].Total = %d, want 1", tier1.Total)
	}
	if tier1.Approved != 0 {
		t.Errorf("tier[1].Approved = %d, want 0", tier1.Approved)
	}
	if tier1.Threshold != nil {
		t.Errorf("tier[1].Threshold = %v, want nil", tier1.Threshold)
	}
	if tier1.Satisfied {
		t.Error("tier[1] should not be satisfied (0 of 1 approved)")
	}
}

func TestComputeTierProgressEmptyReview(t *testing.T) {
	review := &ticket.TicketReview{}
	progress := computeTierProgress(review)
	if len(progress) != 0 {
		t.Errorf("empty review should produce 0 tiers, got %d", len(progress))
	}
}

func TestComputeStewardshipSummary(t *testing.T) {
	content := ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "stewardship:fleet/gpu", Type: ticket.GateReview, Status: ticket.GateSatisfied},
			{ID: "stewardship:cooperative", Type: ticket.GateReview, Status: ticket.GatePending},
			{ID: "pipeline:ci", Type: ticket.GatePipeline, Status: ticket.GatePending},
		},
	}

	gateCount, satisfied := computeStewardshipSummary(content)

	if gateCount != 2 {
		t.Errorf("gate count = %d, want 2", gateCount)
	}
	if satisfied != 1 {
		t.Errorf("satisfied = %d, want 1", satisfied)
	}
}

func TestComputeStewardshipSummaryNoGates(t *testing.T) {
	content := ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "pipeline:ci", Type: ticket.GatePipeline, Status: ticket.GatePending},
		},
	}

	gateCount, satisfied := computeStewardshipSummary(content)

	if gateCount != 0 {
		t.Errorf("gate count = %d, want 0", gateCount)
	}
	if satisfied != 0 {
		t.Errorf("satisfied = %d, want 0", satisfied)
	}
}

func TestHandleShowIncludesStewardshipContext(t *testing.T) {
	// Create a room with a ticket that has stewardship gates.
	threshold := 1
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!roomS:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-s1": {
				Version:  1,
				Title:    "GPU quota increase",
				Status:   ticket.StatusReview,
				Priority: 2,
				Type:     ticket.TypeTask,
				Affects:  []string{"fleet/gpu/a100"},
				Gates: []ticket.TicketGate{
					{ID: "stewardship:fleet/gpu", Type: ticket.GateReview, Status: ticket.GatePending},
				},
				Review: &ticket.TicketReview{
					TierThresholds: []ticket.TierThreshold{
						{Tier: 0, Threshold: &threshold},
					},
					Reviewers: []ticket.ReviewerEntry{
						{UserID: ref.MustParseUserID("@admin:local"), Tier: 0, Disposition: "approved"},
					},
				},
			},
		}),
	}

	env := newTestServer(t, rooms, testServerOpts{noWriter: true})
	defer env.cleanup()

	// Add a stewardship declaration to the index for the room.
	env.service.stewardshipIndex.Put(
		testRoomID("!roomS:local"),
		"fleet/gpu",
		stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"fleet/gpu/**"},
			GateTypes:        []ticket.TicketType{ticket.TypeTask},
			Description:      "GPU fleet governance",
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"admin-*:local"}, Threshold: &threshold},
			},
		},
	)

	var result showResponse
	err := env.client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-s1",
		"room":   "!roomS:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Stewardship == nil {
		t.Fatal("stewardship context is nil, want populated")
	}

	// Declarations should include the matched declaration.
	if len(result.Stewardship.Declarations) != 1 {
		t.Fatalf("declarations count = %d, want 1", len(result.Stewardship.Declarations))
	}
	declaration := result.Stewardship.Declarations[0]
	if declaration.StateKey != "fleet/gpu" {
		t.Errorf("declaration state_key = %q, want fleet/gpu", declaration.StateKey)
	}
	if declaration.MatchedResource != "fleet/gpu/a100" {
		t.Errorf("matched_resource = %q, want fleet/gpu/a100", declaration.MatchedResource)
	}
	if declaration.Description != "GPU fleet governance" {
		t.Errorf("description = %q, want 'GPU fleet governance'", declaration.Description)
	}

	// Gates should show tier progress.
	if len(result.Stewardship.Gates) != 1 {
		t.Fatalf("gates count = %d, want 1", len(result.Stewardship.Gates))
	}
	gate := result.Stewardship.Gates[0]
	if gate.GateID != "stewardship:fleet/gpu" {
		t.Errorf("gate_id = %q, want stewardship:fleet/gpu", gate.GateID)
	}
	if gate.Status != ticket.GatePending {
		t.Errorf("gate status = %q, want pending", gate.Status)
	}
	if len(gate.Tiers) != 1 {
		t.Fatalf("tier count = %d, want 1", len(gate.Tiers))
	}
	if gate.Tiers[0].Approved != 1 || gate.Tiers[0].Total != 1 {
		t.Errorf("tier[0] approved=%d total=%d, want 1/1",
			gate.Tiers[0].Approved, gate.Tiers[0].Total)
	}
	if !gate.Tiers[0].Satisfied {
		t.Error("tier[0] should be satisfied (1/1 approved with threshold=1)")
	}
}

func TestHandleShowNoStewardshipWithoutAffects(t *testing.T) {
	// A ticket without Affects should have nil stewardship context.
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

	if result.Stewardship != nil {
		t.Errorf("stewardship should be nil for ticket without affects, got %+v", result.Stewardship)
	}
}

func TestListIncludesStewardshipSummary(t *testing.T) {
	threshold := 1
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!roomS:local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-s1": {
				Version:  1,
				Title:    "with stewardship",
				Status:   ticket.StatusOpen,
				Priority: 2,
				Type:     ticket.TypeTask,
				Affects:  []string{"fleet/gpu/a100"},
				Gates: []ticket.TicketGate{
					{ID: "stewardship:fleet/gpu", Type: ticket.GateReview, Status: ticket.GateSatisfied},
					{ID: "stewardship:cooperative", Type: ticket.GateReview, Status: ticket.GatePending},
				},
				Review: &ticket.TicketReview{
					TierThresholds: []ticket.TierThreshold{
						{Tier: 0, Threshold: &threshold},
					},
					Reviewers: []ticket.ReviewerEntry{
						{UserID: ref.MustParseUserID("@admin:local"), Tier: 0, Disposition: "approved"},
					},
				},
			},
			"tkt-s2": {
				Version:  1,
				Title:    "without stewardship",
				Status:   ticket.StatusOpen,
				Priority: 2,
				Type:     ticket.TypeTask,
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var results []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room": "!roomS:local",
	}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	for _, entry := range results {
		switch entry.ID {
		case "tkt-s1":
			if entry.StewardshipGates != 2 {
				t.Errorf("tkt-s1 stewardship_gates = %d, want 2", entry.StewardshipGates)
			}
			if entry.StewardshipSatisfied != 1 {
				t.Errorf("tkt-s1 stewardship_satisfied = %d, want 1", entry.StewardshipSatisfied)
			}
		case "tkt-s2":
			if entry.StewardshipGates != 0 {
				t.Errorf("tkt-s2 stewardship_gates = %d, want 0", entry.StewardshipGates)
			}
		}
	}
}
