// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// testRoomID parses a raw room ID string, panicking if it is invalid.
// Use in test setup where the input is a known-good constant.
func testRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("testRoomID(%q): %v", raw, err))
	}
	return roomID
}

// newTestService creates a TicketService suitable for unit testing sync
// logic. The session is nil, which is safe for code paths that don't
// make network calls (leave handling, tombstone detection, etc.).
func newTestService() *TicketService {
	return &TicketService{
		clock:            clock.Real(),
		startedAt:        time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:            make(map[ref.RoomID]*roomState),
		membersByRoom:    make(map[ref.RoomID]map[ref.UserID]roomMember),
		stewardshipIndex: stewardshipindex.NewIndex(),
		digestTimers:     make(map[digestKey]*digestEntry),
		subscribers:      make(map[ref.RoomID][]*subscriber),
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newTrackedRoom creates a roomState with a ticket_config and optional
// tickets pre-indexed.
func newTrackedRoom(tickets map[string]ticket.TicketContent) *roomState {
	state := &roomState{
		config:        &ticket.TicketConfigContent{Version: 1},
		index:         ticketindex.NewIndex(),
		pendingEchoes: make(map[string]ref.EventID),
	}
	for id, content := range tickets {
		state.index.Put(id, content)
	}
	return state
}

func TestHandleSyncLeaveRemovesTrackedRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!tracked:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "first", Status: ticket.StatusOpen},
		"tkt-2": {Version: 1, Title: "second", Status: ticket.StatusOpen},
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[ref.RoomID]messaging.LeftRoom{
				testRoomID("!tracked:local"): {},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	if _, exists := ts.rooms[testRoomID("!tracked:local")]; exists {
		t.Fatal("room should have been removed after leave")
	}
}

func TestHandleSyncLeaveIgnoresUntrackedRoom(t *testing.T) {
	ts := newTestService()

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[ref.RoomID]messaging.LeftRoom{
				testRoomID("!untracked:local"): {},
			},
		},
	}

	// Should not panic or error.
	ts.handleSync(context.Background(), response)

	if len(ts.rooms) != 0 {
		t.Fatalf("rooms map should be empty, got %d entries", len(ts.rooms))
	}
}

func TestHandleSyncLeavePreservesOtherRooms(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!keep:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "keep this", Status: ticket.StatusOpen},
	})
	ts.rooms[testRoomID("!remove:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-2": {Version: 1, Title: "remove this", Status: ticket.StatusOpen},
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[ref.RoomID]messaging.LeftRoom{
				testRoomID("!remove:local"): {},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	if _, exists := ts.rooms[testRoomID("!keep:local")]; !exists {
		t.Fatal("other room should not have been removed")
	}
	if _, exists := ts.rooms[testRoomID("!remove:local")]; exists {
		t.Fatal("left room should have been removed")
	}
}

// toContentMap converts a typed struct to the map[string]any form used
// by messaging.Event.Content. This round-trips through JSON, matching
// how the Matrix homeserver delivers events.
func toContentMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	return result
}

func stringPtr(s string) *string { return &s }

func TestProcessRoomStateIndexesTickets(t *testing.T) {
	ts := newTestService()

	configContent := toContentMap(t, ticket.TicketConfigContent{
		Version: 1,
		Prefix:  "tkt",
	})
	ticketContent := toContentMap(t, ticket.TicketContent{
		Version: 1,
		Title:   "test ticket",
		Status:  ticket.StatusOpen,
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeTicketConfig,
			StateKey: stringPtr(""),
			Content:  configContent,
		},
		{
			Type:     schema.EventTypeTicket,
			StateKey: stringPtr("tkt-1"),
			Content:  ticketContent,
		},
		{
			Type:     schema.EventTypeTicket,
			StateKey: stringPtr("tkt-2"),
			Content:  ticketContent,
		},
	}

	count := ts.processRoomState(context.Background(), testRoomID("!room:local"), stateEvents, nil)

	if count != 2 {
		t.Fatalf("processRoomState returned %d, want 2", count)
	}
	state, exists := ts.rooms[testRoomID("!room:local")]
	if !exists {
		t.Fatal("room should have been added to ts.rooms")
	}
	if state.index.Len() != 2 {
		t.Fatalf("index has %d tickets, want 2", state.index.Len())
	}
	if _, ok := state.index.Get("tkt-1"); !ok {
		t.Fatal("tkt-1 should be in the index")
	}
	if _, ok := state.index.Get("tkt-2"); !ok {
		t.Fatal("tkt-2 should be in the index")
	}
}

func TestProcessRoomStateSkipsRoomWithoutConfig(t *testing.T) {
	ts := newTestService()

	ticketContent := toContentMap(t, ticket.TicketContent{
		Version: 1,
		Title:   "orphan ticket",
		Status:  ticket.StatusOpen,
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeTicket,
			StateKey: stringPtr("tkt-1"),
			Content:  ticketContent,
		},
	}

	count := ts.processRoomState(context.Background(), testRoomID("!room:local"), stateEvents, nil)

	if count != 0 {
		t.Fatalf("processRoomState returned %d, want 0", count)
	}
	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("room without ticket_config should not be tracked")
	}
}

// --- Tombstone tests ---

func TestProcessRoomSyncTombstoneRemovesTrackedRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "doomed", Status: ticket.StatusOpen},
	})

	room := messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{
					Type:    schema.MatrixEventTypeTombstone,
					Content: map[string]any{"replacement_room": "!new:local"},
				},
			},
		},
	}

	ts.processRoomSync(context.Background(), testRoomID("!room:local"), room)

	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("tombstoned room should have been removed")
	}
}

func TestProcessRoomSyncTombstoneIgnoresUntrackedRoom(t *testing.T) {
	ts := newTestService()

	room := messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{
					Type:    schema.MatrixEventTypeTombstone,
					Content: map[string]any{"replacement_room": "!new:local"},
				},
			},
		},
	}

	// Should not panic.
	ts.processRoomSync(context.Background(), testRoomID("!untracked:local"), room)
}

func TestProcessRoomStateTombstoneSkipsRoom(t *testing.T) {
	ts := newTestService()

	configContent := toContentMap(t, ticket.TicketConfigContent{
		Version: 1,
		Prefix:  "tkt",
	})
	ticketContent := toContentMap(t, ticket.TicketContent{
		Version: 1,
		Title:   "should not be indexed",
		Status:  ticket.StatusOpen,
	})

	// Room has both ticket_config and a tombstone — tombstone wins.
	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeTicketConfig,
			StateKey: stringPtr(""),
			Content:  configContent,
		},
		{
			Type:    schema.MatrixEventTypeTombstone,
			Content: map[string]any{"body": "room replaced"},
		},
		{
			Type:     schema.EventTypeTicket,
			StateKey: stringPtr("tkt-1"),
			Content:  ticketContent,
		},
	}

	count := ts.processRoomState(context.Background(), testRoomID("!room:local"), stateEvents, nil)

	if count != 0 {
		t.Fatalf("processRoomState returned %d, want 0 for tombstoned room", count)
	}
	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("tombstoned room should not be tracked")
	}
}

func TestHandleRoomTombstoneExtractsReplacementRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: ticket.StatusOpen},
	})

	event := messaging.Event{
		Type: schema.MatrixEventTypeTombstone,
		Content: map[string]any{
			"body":             "this room has been replaced",
			"replacement_room": "!replacement:local",
		},
	}

	ts.handleRoomTombstone(context.Background(), testRoomID("!room:local"), event)

	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("room should have been removed after tombstone")
	}
}

func TestHandleRoomTombstoneNoReplacementRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: ticket.StatusOpen},
	})

	event := messaging.Event{
		Type:    schema.MatrixEventTypeTombstone,
		Content: map[string]any{"body": "room archived"},
	}

	// Should not panic when replacement_room is missing.
	ts.handleRoomTombstone(context.Background(), testRoomID("!room:local"), event)

	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("room should have been removed after tombstone")
	}
}

// TestConcurrentSyncAndReads exercises the race between handleSync
// (which writes to the index) and concurrent read handlers (which
// iterate the index). Before the addition of mu to TicketService,
// this test panicked with "concurrent map read and map write" under
// the race detector. Now it verifies the RWMutex serialization.
func TestConcurrentSyncAndReads(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "existing", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})

	ctx := context.Background()
	const iterations = 100

	var startBarrier sync.WaitGroup
	startBarrier.Add(1)

	var workers sync.WaitGroup

	// Reader goroutine: calls index.List via the locked path.
	workers.Add(1)
	go func() {
		defer workers.Done()
		startBarrier.Wait()
		for range iterations {
			ts.mu.RLock()
			state, exists := ts.rooms[testRoomID("!room:local")]
			if exists {
				state.index.List(ticketindex.Filter{})
				state.index.Ready()
				state.index.Stats()
			}
			ts.mu.RUnlock()
		}
	}()

	// Writer goroutine: simulates sync loop indexing new events.
	workers.Add(1)
	go func() {
		defer workers.Done()
		startBarrier.Wait()
		for i := range iterations {
			syncResponse := &messaging.SyncResponse{
				Rooms: messaging.RoomsSection{
					Join: map[ref.RoomID]messaging.JoinedRoom{
						testRoomID("!room:local"): {
							Timeline: messaging.TimelineSection{
								Events: []messaging.Event{{
									Type:     schema.EventTypeTicket,
									StateKey: stringPtr(ticketIDForIteration(i)),
									Content: map[string]any{
										"version":    float64(1),
										"title":      "synced ticket",
										"status":     "open",
										"type":       "task",
										"created_at": "2026-01-02T00:00:00Z",
										"updated_at": "2026-01-02T00:00:00Z",
									},
								}},
							},
						},
					},
				},
			}
			ts.handleSync(ctx, syncResponse)
		}
	}()

	// Release all goroutines simultaneously.
	startBarrier.Done()
	workers.Wait()

	// Verify the final state is consistent: should have the original
	// ticket plus all synced tickets.
	state := ts.rooms[testRoomID("!room:local")]
	if state == nil {
		t.Fatal("room state should exist")
	}
	// At least the original ticket plus some synced ones.
	if state.index.Len() < 2 {
		t.Fatalf("expected at least 2 tickets, got %d", state.index.Len())
	}
}

// ticketIDForIteration returns a unique ticket ID for the given
// iteration index in the concurrency test.
func ticketIDForIteration(iteration int) string {
	return fmt.Sprintf("tkt-sync-%04d", iteration)
}

// --- Pending echo tests ---
//
// These tests verify that the pending echo mechanism prevents the sync
// loop from overwriting optimistic local updates with stale events.

func TestPendingEchoSkipsStaleEvent(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Simulate a mutation handler writing to the ticket. The handler
	// called putWithEcho which stored the pending echo.
	closedContent := ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	}
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$mutation-event-id")
	state.index.Put("tkt-1", closedContent)

	// Now simulate the sync loop delivering a stale event (from a
	// /sync response that was in-flight when the mutation happened).
	staleContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusInProgress,
		Type: ticket.TypeTask, Assignee: ref.MustParseUserID("@someone:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T12:00:00Z",
	})
	staleEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$stale-event-id"), // Different from the pending echo
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  staleContent,
	}

	indexed := ts.indexTicketEvent(testRoomID("!room:local"), state, staleEvent)

	if indexed {
		t.Fatal("stale event should not have been indexed")
	}

	// The optimistic update should be preserved.
	current, exists := state.index.Get("tkt-1")
	if !exists {
		t.Fatal("ticket should still exist in index")
	}
	if current.Status != ticket.StatusClosed {
		t.Fatalf("ticket status should be 'closed' (optimistic update preserved), got %q", current.Status)
	}

	// The pending echo should still be there (not consumed).
	if _, pending := state.pendingEchoes["tkt-1"]; !pending {
		t.Fatal("pending echo should still be present after skipping stale event")
	}
}

func TestPendingEchoClearsOnEchoArrival(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Simulate a pending echo from a mutation handler.
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$echo-event-id")
	state.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})

	// The echo arrives: event ID matches the pending entry.
	echoContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})
	echoEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$echo-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  echoContent,
	}

	indexed := ts.indexTicketEvent(testRoomID("!room:local"), state, echoEvent)

	if !indexed {
		t.Fatal("echo event should have been indexed")
	}

	// The pending echo should be cleared.
	if _, pending := state.pendingEchoes["tkt-1"]; pending {
		t.Fatal("pending echo should have been cleared after echo arrival")
	}

	// The index should have the echo's content.
	current, _ := state.index.Get("tkt-1")
	if current.Status != ticket.StatusClosed {
		t.Fatalf("ticket status should be 'closed', got %q", current.Status)
	}
}

func TestPendingEchoAllowsEventsAfterEcho(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Set up and clear a pending echo (simulating echo arrival).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$echo-event-id")
	echoContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})
	ts.indexTicketEvent(testRoomID("!room:local"), state, messaging.Event{
		EventID:  ref.MustParseEventID("$echo-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  echoContent,
	})

	// Now a subsequent event arrives (e.g., someone reopened the ticket
	// after our close). With no pending echo, it should be applied.
	reopenContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusOpen,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-03T00:00:00Z",
	})
	reopenEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$reopen-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  reopenContent,
	}

	indexed := ts.indexTicketEvent(testRoomID("!room:local"), state, reopenEvent)

	if !indexed {
		t.Fatal("post-echo event should have been indexed")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != ticket.StatusOpen {
		t.Fatalf("ticket should have been reopened, got status %q", current.Status)
	}
}

func TestPendingEchoLatestWriteWins(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// First mutation: claim (in_progress).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$claim-event-id")
	state.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusInProgress,
		Type: ticket.TypeTask, Assignee: ref.MustParseUserID("@alice:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})

	// Second mutation: close (overwrites the pending echo).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$close-event-id")
	state.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T01:00:00Z", ClosedAt: "2026-01-02T01:00:00Z",
	})

	// Sync delivers the claim echo. It's NOT the expected echo
	// (we expect the close echo now), so it should be skipped.
	claimEchoContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusInProgress,
		Type: ticket.TypeTask, Assignee: ref.MustParseUserID("@alice:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})
	indexed := ts.indexTicketEvent(testRoomID("!room:local"), state, messaging.Event{
		EventID:  ref.MustParseEventID("$claim-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  claimEchoContent,
	})

	if indexed {
		t.Fatal("claim echo should have been skipped (close is the latest pending)")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != ticket.StatusClosed {
		t.Fatalf("ticket should remain closed, got %q", current.Status)
	}

	// The close echo arrives and should be applied.
	closeEchoContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "original", Status: ticket.StatusClosed,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T01:00:00Z", ClosedAt: "2026-01-02T01:00:00Z",
	})
	indexed = ts.indexTicketEvent(testRoomID("!room:local"), state, messaging.Event{
		EventID:  ref.MustParseEventID("$close-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  closeEchoContent,
	})

	if !indexed {
		t.Fatal("close echo should have been indexed")
	}

	if _, pending := state.pendingEchoes["tkt-1"]; pending {
		t.Fatal("pending echo should be cleared after close echo")
	}
}

func TestPendingEchoNoEffectWithoutPending(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: ticket.StatusOpen, Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// No pending echo — events should be indexed normally.
	newContent := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "updated", Status: ticket.StatusInProgress,
		Type: ticket.TypeTask, Assignee: ref.MustParseUserID("@bob:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})

	indexed := ts.indexTicketEvent(testRoomID("!room:local"), state, messaging.Event{
		EventID:  ref.MustParseEventID("$normal-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  newContent,
	})

	if !indexed {
		t.Fatal("event without pending echo should have been indexed")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != ticket.StatusInProgress {
		t.Fatalf("ticket should be in_progress, got %q", current.Status)
	}
}

// TestPutWithEchoRecordsEcho verifies that putWithEcho stores the
// event ID returned by SendStateEvent in the pending echoes map.
func TestPutWithEchoRecordsEcho(t *testing.T) {
	writer := &fakeWriterForEchoTest{}
	ts := &TicketService{
		writer:      writer,
		clock:       clock.Real(),
		startedAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:       make(map[ref.RoomID]*roomState),
		subscribers: make(map[ref.RoomID][]*subscriber),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	state := newTrackedRoom(nil)
	ts.rooms[testRoomID("!room:local")] = state

	content := ticket.TicketContent{
		Version: 1, Title: "test", Status: ticket.StatusOpen,
		Type: ticket.TypeTask, CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	writer.nextEventID = ref.MustParseEventID("$returned-event-42")

	err := ts.putWithEcho(context.Background(), testRoomID("!room:local"), state, "tkt-1", content)
	if err != nil {
		t.Fatalf("putWithEcho: %v", err)
	}

	// Verify the pending echo was recorded with the returned event ID.
	echoID, pending := state.pendingEchoes["tkt-1"]
	if !pending {
		t.Fatal("putWithEcho should record a pending echo")
	}
	if echoID != ref.MustParseEventID("$returned-event-42") {
		t.Fatalf("pending echo should be '$returned-event-42', got %q", echoID)
	}

	// Verify the index was updated.
	stored, exists := state.index.Get("tkt-1")
	if !exists {
		t.Fatal("ticket should be in index after putWithEcho")
	}
	if stored.Title != "test" {
		t.Fatalf("indexed ticket title should be 'test', got %q", stored.Title)
	}
}

// fakeWriterForEchoTest is a minimal matrixWriter that returns a
// configurable event ID.
type fakeWriterForEchoTest struct {
	nextEventID ref.EventID
}

func (f *fakeWriterForEchoTest) SendStateEvent(_ context.Context, _ ref.RoomID, _ ref.EventType, _ string, _ any) (ref.EventID, error) {
	return f.nextEventID, nil
}

// --- Sync filter tests ---

func TestSyncFilterIncludesStewardshipAndMember(t *testing.T) {
	filter := syncFilter
	if !strings.Contains(filter, schema.EventTypeStewardship.String()) {
		t.Errorf("sync filter should include %s", schema.EventTypeStewardship)
	}
	if !strings.Contains(filter, schema.MatrixEventTypeRoomMember.String()) {
		t.Errorf("sync filter should include %s", schema.MatrixEventTypeRoomMember)
	}
}

// --- Member indexing tests ---

func testUserID(raw string) ref.UserID {
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		panic(fmt.Sprintf("testUserID(%q): %v", raw, err))
	}
	return userID
}

func TestIndexMemberEventJoin(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership":  "join",
			"displayname": "Alice",
		},
	})

	members := ts.membersByRoom[roomID]
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	alice := members[testUserID("@alice:local")]
	if alice.DisplayName != "Alice" {
		t.Errorf("display name = %q, want %q", alice.DisplayName, "Alice")
	}
}

func TestIndexMemberEventLeave(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Add a member first.
	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership":  "join",
			"displayname": "Alice",
		},
	})

	// Leave removes the member.
	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership": "leave",
		},
	})

	members := ts.membersByRoom[roomID]
	if len(members) != 0 {
		t.Fatalf("expected 0 members after leave, got %d", len(members))
	}
}

func TestIndexMemberEventBan(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership":  "join",
			"displayname": "Alice",
		},
	})

	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership": "ban",
		},
	})

	if len(ts.membersByRoom[roomID]) != 0 {
		t.Fatal("ban should remove the member")
	}
}

func TestIndexMemberEventIgnoresInvite(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership":  "invite",
			"displayname": "Alice",
		},
	})

	if len(ts.membersByRoom[roomID]) != 0 {
		t.Fatal("invite events should not create member entries")
	}
}

func TestIndexMemberEventMissingDisplayName(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@bot:local"),
		Content: map[string]any{
			"membership": "join",
		},
	})

	member := ts.membersByRoom[roomID][testUserID("@bot:local")]
	if member.DisplayName != "" {
		t.Errorf("display name should be empty, got %q", member.DisplayName)
	}
}

func TestIndexMemberEventNilStateKey(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Should not panic.
	ts.indexMemberEvent(roomID, messaging.Event{
		Type: schema.MatrixEventTypeRoomMember,
		Content: map[string]any{
			"membership":  "join",
			"displayname": "Ghost",
		},
	})

	if len(ts.membersByRoom[roomID]) != 0 {
		t.Fatal("nil state key should be ignored")
	}
}

func TestIndexMemberEventLeaveFromEmptyRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Leave event for a room with no prior joins — should not panic.
	ts.indexMemberEvent(roomID, messaging.Event{
		Type:     schema.MatrixEventTypeRoomMember,
		StateKey: stringPtr("@alice:local"),
		Content: map[string]any{
			"membership": "leave",
		},
	})
}

// --- Stewardship indexing tests ---

func makeStewardshipContentMap(t *testing.T, patterns ...string) map[string]any {
	t.Helper()
	content := stewardship.StewardshipContent{
		Version:          stewardship.StewardshipContentVersion,
		ResourcePatterns: patterns,
		Tiers: []stewardship.StewardshipTier{
			{
				Principals: []string{"iree/amdgpu/pm:bureau.local"},
				Escalation: ticket.EscalationImmediate,
			},
		},
	}
	return toContentMap(t, content)
}

func TestIndexStewardshipEventPut(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	ts.indexStewardshipEvent(context.Background(), roomID, messaging.Event{
		Type:     schema.EventTypeStewardship,
		StateKey: stringPtr("fleet/gpu"),
		Content:  makeStewardshipContentMap(t, "fleet/gpu/**"),
	})

	if ts.stewardshipIndex.Len() != 1 {
		t.Fatalf("stewardship index length = %d, want 1", ts.stewardshipIndex.Len())
	}

	matches := ts.stewardshipIndex.Resolve([]string{"fleet/gpu/a100"})
	if len(matches) != 1 {
		t.Fatalf("resolve: got %d matches, want 1", len(matches))
	}
}

func TestIndexStewardshipEventRemoveOnEmpty(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Add a declaration.
	ts.indexStewardshipEvent(context.Background(), roomID, messaging.Event{
		Type:     schema.EventTypeStewardship,
		StateKey: stringPtr("fleet/gpu"),
		Content:  makeStewardshipContentMap(t, "fleet/gpu/**"),
	})

	// Empty content removes it.
	ts.indexStewardshipEvent(context.Background(), roomID, messaging.Event{
		Type:     schema.EventTypeStewardship,
		StateKey: stringPtr("fleet/gpu"),
		Content:  map[string]any{},
	})

	if ts.stewardshipIndex.Len() != 0 {
		t.Fatalf("stewardship index length = %d, want 0 after removal", ts.stewardshipIndex.Len())
	}
}

func TestIndexStewardshipEventNilStateKey(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Should not panic.
	ts.indexStewardshipEvent(context.Background(), roomID, messaging.Event{
		Type:    schema.EventTypeStewardship,
		Content: makeStewardshipContentMap(t, "fleet/**"),
	})

	if ts.stewardshipIndex.Len() != 0 {
		t.Fatal("nil state key should be ignored")
	}
}

// --- processRoomState member/stewardship indexing tests ---

func TestProcessRoomStateIndexesMembersForNonTicketRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!notickets:local")

	// Room with member events but no ticket_config.
	stateEvents := []messaging.Event{
		{
			Type:     schema.MatrixEventTypeRoomMember,
			StateKey: stringPtr("@alice:local"),
			Content: map[string]any{
				"membership":  "join",
				"displayname": "Alice",
			},
		},
		{
			Type:     schema.MatrixEventTypeRoomMember,
			StateKey: stringPtr("@bob:local"),
			Content: map[string]any{
				"membership":  "join",
				"displayname": "Bob",
			},
		},
	}

	count := ts.processRoomState(context.Background(), roomID, stateEvents, nil)

	// No tickets (no config).
	if count != 0 {
		t.Fatalf("ticket count = %d, want 0 (no ticket config)", count)
	}
	if _, tracked := ts.rooms[roomID]; tracked {
		t.Fatal("room should not be in ts.rooms (no ticket config)")
	}

	// But members should be indexed.
	members := ts.membersByRoom[roomID]
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
}

func TestProcessRoomStateIndexesStewardshipForNonTicketRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!notickets:local")

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeStewardship,
			StateKey: stringPtr("fleet/gpu"),
			Content:  makeStewardshipContentMap(t, "fleet/gpu/**"),
		},
	}

	count := ts.processRoomState(context.Background(), roomID, stateEvents, nil)

	if count != 0 {
		t.Fatalf("ticket count = %d, want 0 (no ticket config)", count)
	}
	if ts.stewardshipIndex.Len() != 1 {
		t.Fatalf("stewardship declarations = %d, want 1", ts.stewardshipIndex.Len())
	}
}

func TestProcessRoomStateSkipsMembersForTombstonedRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!doomed:local")

	stateEvents := []messaging.Event{
		{
			Type:     schema.MatrixEventTypeRoomMember,
			StateKey: stringPtr("@alice:local"),
			Content: map[string]any{
				"membership":  "join",
				"displayname": "Alice",
			},
		},
		{
			Type:    schema.MatrixEventTypeTombstone,
			Content: map[string]any{"replacement_room": "!new:local"},
		},
	}

	count := ts.processRoomState(context.Background(), roomID, stateEvents, nil)

	if count != 0 {
		t.Fatalf("ticket count = %d, want 0", count)
	}
	if len(ts.membersByRoom[roomID]) != 0 {
		t.Fatal("tombstoned room should not have indexed members")
	}
}

func TestProcessRoomStateSkipsStewardshipForTombstonedRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!doomed:local")

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeStewardship,
			StateKey: stringPtr("fleet/gpu"),
			Content:  makeStewardshipContentMap(t, "fleet/gpu/**"),
		},
		{
			Type:    schema.MatrixEventTypeTombstone,
			Content: map[string]any{"replacement_room": "!new:local"},
		},
	}

	count := ts.processRoomState(context.Background(), roomID, stateEvents, nil)

	if count != 0 {
		t.Fatalf("ticket count = %d, want 0", count)
	}
	if ts.stewardshipIndex.Len() != 0 {
		t.Fatal("tombstoned room should not have stewardship declarations")
	}
}

// --- handleSync leave cleanup tests ---

func TestHandleSyncLeaveCleansMembersAndStewardship(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!leaving:local")

	// Pre-populate member and stewardship data (non-ticket room).
	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		testUserID("@alice:local"): {DisplayName: "Alice"},
	}
	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          stewardship.StewardshipContentVersion,
		ResourcePatterns: []string{"fleet/gpu/**"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"pm:bureau.local"}, Escalation: ticket.EscalationImmediate},
		},
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[ref.RoomID]messaging.LeftRoom{
				roomID: {},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	if _, exists := ts.membersByRoom[roomID]; exists {
		t.Fatal("members should have been cleaned up after leave")
	}
	if ts.stewardshipIndex.Len() != 0 {
		t.Fatalf("stewardship declarations = %d, want 0 after leave", ts.stewardshipIndex.Len())
	}
}

// --- handleRoomTombstone cleanup tests ---

func TestHandleRoomTombstoneCleansMembersAndStewardship(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!doomed:local")

	// Pre-populate member and stewardship data.
	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		testUserID("@alice:local"): {DisplayName: "Alice"},
	}
	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          stewardship.StewardshipContentVersion,
		ResourcePatterns: []string{"fleet/gpu/**"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"pm:bureau.local"}, Escalation: ticket.EscalationImmediate},
		},
	})

	event := messaging.Event{
		Type: schema.MatrixEventTypeTombstone,
		Content: map[string]any{
			"replacement_room": "!new:local",
		},
	}

	ts.handleRoomTombstone(context.Background(), roomID, event)

	if _, exists := ts.membersByRoom[roomID]; exists {
		t.Fatal("members should have been cleaned up after tombstone")
	}
	if ts.stewardshipIndex.Len() != 0 {
		t.Fatalf("stewardship declarations = %d, want 0 after tombstone", ts.stewardshipIndex.Len())
	}
}

func TestHandleRoomTombstoneCleansMembersEvenForUntrackedRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!untracked:local")

	// Room has members but no ticket management.
	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		testUserID("@alice:local"): {DisplayName: "Alice"},
	}

	event := messaging.Event{
		Type: schema.MatrixEventTypeTombstone,
		Content: map[string]any{
			"replacement_room": "!new:local",
		},
	}

	ts.handleRoomTombstone(context.Background(), roomID, event)

	if _, exists := ts.membersByRoom[roomID]; exists {
		t.Fatal("members should have been cleaned up even for untracked room")
	}
}

// --- processRoomSync member/stewardship indexing tests ---

func TestProcessRoomSyncIndexesMembersAndStewardship(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	room := messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{
					Type:     schema.MatrixEventTypeRoomMember,
					StateKey: stringPtr("@alice:local"),
					Content: map[string]any{
						"membership":  "join",
						"displayname": "Alice",
					},
				},
				{
					Type:     schema.EventTypeStewardship,
					StateKey: stringPtr("fleet/gpu"),
					Content:  makeStewardshipContentMap(t, "fleet/gpu/**"),
				},
			},
		},
	}

	ts.processRoomSync(context.Background(), roomID, room)

	// Room is not ticket-configured, so no ticket state.
	if _, tracked := ts.rooms[roomID]; tracked {
		t.Fatal("room should not be tracked (no ticket config)")
	}

	// But members and stewardship should be indexed.
	if len(ts.membersByRoom[roomID]) != 1 {
		t.Fatalf("expected 1 member, got %d", len(ts.membersByRoom[roomID]))
	}
	if ts.stewardshipIndex.Len() != 1 {
		t.Fatalf("stewardship declarations = %d, want 1", ts.stewardshipIndex.Len())
	}
}
