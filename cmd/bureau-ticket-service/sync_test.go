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
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// newTestService creates a TicketService suitable for unit testing sync
// logic. The session is nil, which is safe for code paths that don't
// make network calls (leave handling, tombstone detection, etc.).
func newTestService() *TicketService {
	return &TicketService{
		clock:     clock.Real(),
		startedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:     make(map[string]*roomState),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newTrackedRoom creates a roomState with a ticket_config and optional
// tickets pre-indexed.
func newTrackedRoom(tickets map[string]schema.TicketContent) *roomState {
	state := &roomState{
		config: &schema.TicketConfigContent{Version: 1},
		index:  ticket.NewIndex(),
	}
	for id, content := range tickets {
		state.index.Put(id, content)
	}
	return state
}

func TestHandleSyncLeaveRemovesTrackedRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms["!tracked:local"] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "first", Status: "open"},
		"tkt-2": {Version: 1, Title: "second", Status: "open"},
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!tracked:local": {},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	if _, exists := ts.rooms["!tracked:local"]; exists {
		t.Fatal("room should have been removed after leave")
	}
}

func TestHandleSyncLeaveIgnoresUntrackedRoom(t *testing.T) {
	ts := newTestService()

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!untracked:local": {},
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
	ts.rooms["!keep:local"] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "keep this", Status: "open"},
	})
	ts.rooms["!remove:local"] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-2": {Version: 1, Title: "remove this", Status: "open"},
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!remove:local": {},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	if _, exists := ts.rooms["!keep:local"]; !exists {
		t.Fatal("other room should not have been removed")
	}
	if _, exists := ts.rooms["!remove:local"]; exists {
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

	count := ts.processRoomState(context.Background(), "!room:local", stateEvents, nil)

	if count != 2 {
		t.Fatalf("processRoomState returned %d, want 2", count)
	}
	state, exists := ts.rooms["!room:local"]
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

	count := ts.processRoomState(context.Background(), "!room:local", stateEvents, nil)

	if count != 0 {
		t.Fatalf("processRoomState returned %d, want 0", count)
	}
	if _, exists := ts.rooms["!room:local"]; exists {
		t.Fatal("room without ticket_config should not be tracked")
	}
}

// --- Tombstone tests ---

func TestProcessRoomSyncTombstoneRemovesTrackedRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms["!room:local"] = newTrackedRoom(map[string]schema.TicketContent{
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

	ts.processRoomSync(context.Background(), "!room:local", room)

	if _, exists := ts.rooms["!room:local"]; exists {
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
	ts.processRoomSync(context.Background(), "!untracked:local", room)
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

	count := ts.processRoomState(context.Background(), "!room:local", stateEvents, nil)

	if count != 0 {
		t.Fatalf("processRoomState returned %d, want 0 for tombstoned room", count)
	}
	if _, exists := ts.rooms["!room:local"]; exists {
		t.Fatal("tombstoned room should not be tracked")
	}
}

func TestHandleRoomTombstoneExtractsReplacementRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms["!room:local"] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: "open"},
	})

	event := messaging.Event{
		Type: schema.MatrixEventTypeTombstone,
		Content: map[string]any{
			"body":             "this room has been replaced",
			"replacement_room": "!replacement:local",
		},
	}

	ts.handleRoomTombstone("!room:local", event)

	if _, exists := ts.rooms["!room:local"]; exists {
		t.Fatal("room should have been removed after tombstone")
	}
}

func TestHandleRoomTombstoneNoReplacementRoom(t *testing.T) {
	ts := newTestService()
	ts.rooms["!room:local"] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {Version: 1, Title: "test", Status: "open"},
	})

	event := messaging.Event{
		Type:    schema.MatrixEventTypeTombstone,
		Content: map[string]any{"body": "room archived"},
	}

	// Should not panic when replacement_room is missing.
	ts.handleRoomTombstone("!room:local", event)

	if _, exists := ts.rooms["!room:local"]; exists {
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
	ts.rooms["!room:local"] = newTrackedRoom(map[string]schema.TicketContent{
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
			state, exists := ts.rooms["!room:local"]
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
					Join: map[string]messaging.JoinedRoom{
						"!room:local": {
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
	state := ts.rooms["!room:local"]
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
