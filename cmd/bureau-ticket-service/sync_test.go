// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
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
		clock:     clock.Real(),
		startedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:     make(map[ref.RoomID]*roomState),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newTrackedRoom creates a roomState with a ticket_config and optional
// tickets pre-indexed.
func newTrackedRoom(tickets map[string]schema.TicketContent) *roomState {
	state := &roomState{
		config:        &schema.TicketConfigContent{Version: 1},
		index:         ticket.NewIndex(),
		pendingEchoes: make(map[string]ref.EventID),
	}
	for id, content := range tickets {
		state.index.Put(id, content)
	}
	return state
}

func TestHandleSyncLeaveRemovesTrackedRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!tracked:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "first", Status: "open"},
		"tkt-2": {Version: 1, Title: "second", Status: "open"},
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
	ts.rooms[testRoomID("!keep:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "keep this", Status: "open"},
	})
	ts.rooms[testRoomID("!remove:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-2": {Version: 1, Title: "remove this", Status: "open"},
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

	configContent := toContentMap(t, schema.TicketConfigContent{
		Version: 1,
		Prefix:  "tkt",
	})
	ticketContent := toContentMap(t, schema.TicketContent{
		Version: 1,
		Title:   "test ticket",
		Status:  "open",
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

	ticketContent := toContentMap(t, schema.TicketContent{
		Version: 1,
		Title:   "orphan ticket",
		Status:  "open",
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
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "doomed", Status: "open"},
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

	configContent := toContentMap(t, schema.TicketConfigContent{
		Version: 1,
		Prefix:  "tkt",
	})
	ticketContent := toContentMap(t, schema.TicketContent{
		Version: 1,
		Title:   "should not be indexed",
		Status:  "open",
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
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: "open"},
	})

	event := messaging.Event{
		Type: schema.MatrixEventTypeTombstone,
		Content: map[string]any{
			"body":             "this room has been replaced",
			"replacement_room": "!replacement:local",
		},
	}

	ts.handleRoomTombstone(testRoomID("!room:local"), event)

	if _, exists := ts.rooms[testRoomID("!room:local")]; exists {
		t.Fatal("room should have been removed after tombstone")
	}
}

func TestHandleRoomTombstoneNoReplacementRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: "open"},
	})

	event := messaging.Event{
		Type:    schema.MatrixEventTypeTombstone,
		Content: map[string]any{"body": "room archived"},
	}

	// Should not panic when replacement_room is missing.
	ts.handleRoomTombstone(testRoomID("!room:local"), event)

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
	ts.rooms[testRoomID("!room:local")] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "existing", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
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
				state.index.List(ticket.Filter{})
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
	state := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Simulate a mutation handler writing to the ticket. The handler
	// called putWithEcho which stored the pending echo.
	closedContent := schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	}
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$mutation-event-id")
	state.index.Put("tkt-1", closedContent)

	// Now simulate the sync loop delivering a stale event (from a
	// /sync response that was in-flight when the mutation happened).
	staleContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "in_progress",
		Type: "task", Assignee: ref.MustParseUserID("@someone:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T12:00:00Z",
	})
	staleEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$stale-event-id"), // Different from the pending echo
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  staleContent,
	}

	indexed := ts.indexTicketEvent(state, staleEvent)

	if indexed {
		t.Fatal("stale event should not have been indexed")
	}

	// The optimistic update should be preserved.
	current, exists := state.index.Get("tkt-1")
	if !exists {
		t.Fatal("ticket should still exist in index")
	}
	if current.Status != "closed" {
		t.Fatalf("ticket status should be 'closed' (optimistic update preserved), got %q", current.Status)
	}

	// The pending echo should still be there (not consumed).
	if _, pending := state.pendingEchoes["tkt-1"]; !pending {
		t.Fatal("pending echo should still be present after skipping stale event")
	}
}

func TestPendingEchoClearsOnEchoArrival(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Simulate a pending echo from a mutation handler.
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$echo-event-id")
	state.index.Put("tkt-1", schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})

	// The echo arrives: event ID matches the pending entry.
	echoContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})
	echoEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$echo-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  echoContent,
	}

	indexed := ts.indexTicketEvent(state, echoEvent)

	if !indexed {
		t.Fatal("echo event should have been indexed")
	}

	// The pending echo should be cleared.
	if _, pending := state.pendingEchoes["tkt-1"]; pending {
		t.Fatal("pending echo should have been cleared after echo arrival")
	}

	// The index should have the echo's content.
	current, _ := state.index.Get("tkt-1")
	if current.Status != "closed" {
		t.Fatalf("ticket status should be 'closed', got %q", current.Status)
	}
}

func TestPendingEchoAllowsEventsAfterEcho(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// Set up and clear a pending echo (simulating echo arrival).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$echo-event-id")
	echoContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z", ClosedAt: "2026-01-02T00:00:00Z",
	})
	ts.indexTicketEvent(state, messaging.Event{
		EventID:  ref.MustParseEventID("$echo-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  echoContent,
	})

	// Now a subsequent event arrives (e.g., someone reopened the ticket
	// after our close). With no pending echo, it should be applied.
	reopenContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "open",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-03T00:00:00Z",
	})
	reopenEvent := messaging.Event{
		EventID:  ref.MustParseEventID("$reopen-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  reopenContent,
	}

	indexed := ts.indexTicketEvent(state, reopenEvent)

	if !indexed {
		t.Fatal("post-echo event should have been indexed")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != "open" {
		t.Fatalf("ticket should have been reopened, got status %q", current.Status)
	}
}

func TestPendingEchoLatestWriteWins(t *testing.T) {
	ts := newTestService()
	state := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// First mutation: claim (in_progress).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$claim-event-id")
	state.index.Put("tkt-1", schema.TicketContent{
		Version: 1, Title: "original", Status: "in_progress",
		Type: "task", Assignee: ref.MustParseUserID("@alice:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})

	// Second mutation: close (overwrites the pending echo).
	state.pendingEchoes["tkt-1"] = ref.MustParseEventID("$close-event-id")
	state.index.Put("tkt-1", schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T01:00:00Z", ClosedAt: "2026-01-02T01:00:00Z",
	})

	// Sync delivers the claim echo. It's NOT the expected echo
	// (we expect the close echo now), so it should be skipped.
	claimEchoContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "in_progress",
		Type: "task", Assignee: ref.MustParseUserID("@alice:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})
	indexed := ts.indexTicketEvent(state, messaging.Event{
		EventID:  ref.MustParseEventID("$claim-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  claimEchoContent,
	})

	if indexed {
		t.Fatal("claim echo should have been skipped (close is the latest pending)")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != "closed" {
		t.Fatalf("ticket should remain closed, got %q", current.Status)
	}

	// The close echo arrives and should be applied.
	closeEchoContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "original", Status: "closed",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T01:00:00Z", ClosedAt: "2026-01-02T01:00:00Z",
	})
	indexed = ts.indexTicketEvent(state, messaging.Event{
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
	state := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "original", Status: "open", Type: "task", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[testRoomID("!room:local")] = state

	// No pending echo — events should be indexed normally.
	newContent := toContentMap(t, schema.TicketContent{
		Version: 1, Title: "updated", Status: "in_progress",
		Type: "task", Assignee: ref.MustParseUserID("@bob:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})

	indexed := ts.indexTicketEvent(state, messaging.Event{
		EventID:  ref.MustParseEventID("$normal-event-id"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  newContent,
	})

	if !indexed {
		t.Fatal("event without pending echo should have been indexed")
	}

	current, _ := state.index.Get("tkt-1")
	if current.Status != "in_progress" {
		t.Fatalf("ticket should be in_progress, got %q", current.Status)
	}
}

// TestPutWithEchoRecordsEcho verifies that putWithEcho stores the
// event ID returned by SendStateEvent in the pending echoes map.
func TestPutWithEchoRecordsEcho(t *testing.T) {
	writer := &fakeWriterForEchoTest{}
	ts := &TicketService{
		writer:    writer,
		clock:     clock.Real(),
		startedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:     make(map[ref.RoomID]*roomState),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	state := newTrackedRoom(nil)
	ts.rooms[testRoomID("!room:local")] = state

	content := schema.TicketContent{
		Version: 1, Title: "test", Status: "open",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
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
