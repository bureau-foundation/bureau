// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// testTicket returns a minimal valid TicketContent for use in unit
// tests. Defaults to type "task", status "open", priority 2.
func testTicket(title string) ticket.TicketContent {
	return ticket.TicketContent{
		Version:   1,
		Title:     title,
		Status:    "open",
		Priority:  2,
		Type:      "task",
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-02-12T10:00:00Z",
		UpdatedAt: "2026-02-12T10:00:00Z",
	}
}

// --- matchGateEvent unit tests ---

func TestMatchPipelineGateMatches(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:          "ci-pass",
		Type:        "pipeline",
		Status:      "pending",
		PipelineRef: "dev-workspace-init",
		Conclusion:  "success",
	}

	event := messaging.Event{
		Type:     schema.EventTypePipelineResult,
		StateKey: stringPtr("dev-workspace-init"),
		Content: map[string]any{
			"pipeline_ref": "dev-workspace-init",
			"conclusion":   "success",
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("pipeline gate should match on matching pipeline_ref and conclusion")
	}
}

func TestMatchPipelineGateWrongRef(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:          "ci-pass",
		Type:        "pipeline",
		Status:      "pending",
		PipelineRef: "dev-workspace-init",
		Conclusion:  "success",
	}

	event := messaging.Event{
		Type:     schema.EventTypePipelineResult,
		StateKey: stringPtr("other-pipeline"),
		Content: map[string]any{
			"pipeline_ref": "other-pipeline",
			"conclusion":   "success",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("pipeline gate should not match on different pipeline_ref")
	}
}

func TestMatchPipelineGateWrongConclusion(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:          "ci-pass",
		Type:        "pipeline",
		Status:      "pending",
		PipelineRef: "dev-workspace-init",
		Conclusion:  "success",
	}

	event := messaging.Event{
		Type:     schema.EventTypePipelineResult,
		StateKey: stringPtr("dev-workspace-init"),
		Content: map[string]any{
			"pipeline_ref": "dev-workspace-init",
			"conclusion":   "failure",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("pipeline gate should not match on wrong conclusion")
	}
}

func TestMatchPipelineGateAnyConclusionMatches(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:          "ci-done",
		Type:        "pipeline",
		Status:      "pending",
		PipelineRef: "dev-workspace-init",
		Conclusion:  "", // Empty = any completed result.
	}

	event := messaging.Event{
		Type:     schema.EventTypePipelineResult,
		StateKey: stringPtr("dev-workspace-init"),
		Content: map[string]any{
			"pipeline_ref": "dev-workspace-init",
			"conclusion":   "failure",
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("pipeline gate with empty conclusion should match any completed result")
	}
}

func TestMatchPipelineGateWrongEventType(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:          "ci-pass",
		Type:        "pipeline",
		Status:      "pending",
		PipelineRef: "dev-workspace-init",
	}

	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content: map[string]any{
			"pipeline_ref": "dev-workspace-init",
			"conclusion":   "success",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("pipeline gate should not match on wrong event type")
	}
}

// --- Ticket gate tests ---

func TestMatchTicketGateClosedMatches(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "blocker-done",
		Type:     "ticket",
		Status:   "pending",
		TicketID: "tkt-abc",
	}

	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-abc"),
		Content: map[string]any{
			"status": "closed",
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("ticket gate should match when watched ticket reaches closed")
	}
}

func TestMatchTicketGateNotClosedDoesNotMatch(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "blocker-done",
		Type:     "ticket",
		Status:   "pending",
		TicketID: "tkt-abc",
	}

	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-abc"),
		Content: map[string]any{
			"status": "in_progress",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("ticket gate should not match when ticket is not closed")
	}
}

func TestMatchTicketGateWrongTicketID(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "blocker-done",
		Type:     "ticket",
		Status:   "pending",
		TicketID: "tkt-abc",
	}

	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-xyz"),
		Content: map[string]any{
			"status": "closed",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("ticket gate should not match for a different ticket ID")
	}
}

// --- State event gate tests ---

func TestMatchStateEventGateBasicMatch(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "deploy-ready",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		StateKey:  "/workspace/proj",
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("/workspace/proj"),
		Content: map[string]any{
			"status": "active",
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should match on event_type + state_key")
	}
}

func TestMatchStateEventGateWrongEventType(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "deploy-ready",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
	}

	event := messaging.Event{
		Type:     "m.bureau.worktree",
		StateKey: stringPtr(""),
		Content:  map[string]any{},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should not match on wrong event type")
	}
}

func TestMatchStateEventGateWrongStateKey(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "deploy-ready",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		StateKey:  "/workspace/proj-a",
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("/workspace/proj-b"),
		Content:  map[string]any{},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should not match on wrong state key")
	}
}

func TestMatchStateEventGateNoStateKeyMatchesAny(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "any-workspace",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		// StateKey empty — matches any state key.
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("anything-here"),
		Content:  map[string]any{},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate with no state_key should match any state key")
	}
}

func TestMatchStateEventGateWithContentMatch(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "status-active",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		ContentMatch: schema.ContentMatch{
			"status": schema.Eq("active"),
		},
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr(""),
		Content: map[string]any{
			"status": "active",
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should match when content_match criteria are satisfied")
	}
}

func TestMatchStateEventGateContentMatchFails(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "status-active",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		ContentMatch: schema.ContentMatch{
			"status": schema.Eq("active"),
		},
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr(""),
		Content: map[string]any{
			"status": "removing",
		},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should not match when content_match criteria fail")
	}
}

func TestMatchStateEventGateWithNumericContentMatch(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "high-priority",
		Type:      "state_event",
		Status:    "pending",
		EventType: schema.EventTypeTicket,
		ContentMatch: schema.ContentMatch{
			"priority": schema.Lte(1),
		},
	}

	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-99"),
		Content: map[string]any{
			"priority": float64(0), // JSON numbers are float64.
		},
	}

	if !matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate should match on numeric content_match")
	}
}

func TestMatchStateEventGateSkipsCrossRoom(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "cross-room",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		RoomAlias: ref.MustParseRoomAlias("#other/room:bureau.local"),
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr(""),
		Content:  map[string]any{},
	}

	if matchGateEvent(gate, event, nil, "") {
		t.Fatal("state_event gate with RoomAlias should be skipped (cross-room not yet supported)")
	}
}

// --- Human gate is never auto-matched ---

func TestHumanGateNotAutoMatched(t *testing.T) {
	ts := newGateTestService()
	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version: 1,
			Title:   "gated ticket",
			Status:  "open",
			Type:    "task",
			Gates: []ticket.TicketGate{
				{
					ID:     "approval",
					Type:   "human",
					Status: "pending",
				},
			},
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})

	// Send an event that matches nothing specific — human gates
	// should never auto-satisfy regardless.
	event := messaging.Event{
		EventID:  ref.MustParseEventID("$ev1"),
		Type:     "m.bureau.workspace",
		StateKey: stringPtr(""),
		Content:  map[string]any{"status": "active"},
	}
	ts.evaluateGatesForEvent(context.Background(), roomID, ts.rooms[roomID], event)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("human gate should remain pending, got %q", content.Gates[0].Status)
	}
}

// --- Timer gate tests ---

func TestTimerExpiredTrue(t *testing.T) {
	gate := &ticket.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "1h",
		Target:    "2026-01-01T01:00:00Z",
	}
	now := time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC) // 1s after target
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !expired {
		t.Fatal("timer should be expired")
	}
}

func TestTimerExpiredExactDeadline(t *testing.T) {
	gate := &ticket.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "1h",
		Target:    "2026-01-01T01:00:00Z",
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) // Exactly at target.
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !expired {
		t.Fatal("timer should be expired at exact deadline")
	}
}

func TestTimerNotExpired(t *testing.T) {
	gate := &ticket.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "24h",
		Target:    "2026-01-02T00:00:00Z",
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // 12h before target.
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Fatal("timer should not be expired before deadline")
	}
}

func TestTimerEmptyTargetNotExpired(t *testing.T) {
	// Timer with no Target (e.g., base="unblocked" with open
	// blockers) should never fire.
	gate := &ticket.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "1h",
		Base:      "unblocked",
		// Target intentionally empty.
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Fatal("timer with empty target should never fire")
	}
}

func TestTimerInvalidTarget(t *testing.T) {
	gate := &ticket.TicketGate{
		Target: "not-a-timestamp",
	}
	_, err := timerExpired(gate, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error for invalid target")
	}
}

// --- fireExpiredTimersLocked integration tests ---

func TestFireExpiredTimersSatisfiesExpired(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock set to 2h after gate creation — timer has expired.
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "timer gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Target:    "2026-01-01T01:00:00Z",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.rebuildTimerHeap()
	ts.fireExpiredTimersLocked(context.Background())

	content, exists := ts.rooms[roomID].index.Get("tkt-timer")
	if !exists {
		t.Fatal("ticket should still exist")
	}
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("timer gate should be satisfied, got %q", content.Gates[0].Status)
	}
	if content.Gates[0].SatisfiedBy != "timer" {
		t.Fatalf("timer gate satisfied_by should be 'timer', got %q", content.Gates[0].SatisfiedBy)
	}
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write, got %d", len(writer.events))
	}
	if ts.timers.Len() != 0 {
		t.Fatalf("heap should be empty after firing, got %d entries", ts.timers.Len())
	}
}

func TestFireExpiredTimersSkipsUnexpired(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock set to 30m after gate creation — timer has NOT expired (1h target).
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "timer gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Target:    "2026-01-01T01:00:00Z",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.rebuildTimerHeap()
	ts.fireExpiredTimersLocked(context.Background())

	content, _ := ts.rooms[roomID].index.Get("tkt-timer")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("unexpired timer gate should remain pending, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no Matrix writes, got %d", len(writer.events))
	}
	if ts.timers.Len() != 1 {
		t.Fatalf("heap should still have 1 entry, got %d", ts.timers.Len())
	}
}

func TestFireExpiredTimersSkipsEmptyTarget(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock far in the future — but target is empty so the gate is
	// never added to the heap (rebuildTimerHeap skips empty targets).
	fakeClock := clock.Fake(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-unblocked-pending": {
			Version:   1,
			Title:     "waiting for unblock",
			Status:    "open",
			Type:      "task",
			BlockedBy: []string{"tkt-blocker"},
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Base:      "unblocked",
					CreatedAt: "2026-01-01T00:00:00Z",
					// Target intentionally empty — blocker not yet closed.
				},
			},
		},
		"tkt-blocker": {
			Version:   1,
			Title:     "blocker",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})

	ts.rebuildTimerHeap()
	if ts.timers.Len() != 0 {
		t.Fatalf("heap should be empty (empty target not added), got %d entries", ts.timers.Len())
	}

	ts.fireExpiredTimersLocked(context.Background())

	content, _ := ts.rooms[roomID].index.Get("tkt-unblocked-pending")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("timer gate with empty target should remain pending, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no Matrix writes, got %d", len(writer.events))
	}
}

func TestFireExpiredTimersLazyDeletion(t *testing.T) {
	// Verify that stale heap entries (gate already satisfied) are
	// silently skipped rather than causing errors or double-fires.
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "already satisfied",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "satisfied", // Already satisfied.
					Duration:  "1h",
					Target:    "2026-01-01T01:00:00Z",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Manually push a heap entry for the gate (simulating a push
	// that happened before the gate was satisfied externally).
	ts.pushTimerGates(roomID, "tkt-timer", &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:     "soak",
				Type:   "timer",
				Status: "pending", // Push sees "pending" but index has "satisfied".
				Target: "2026-01-01T01:00:00Z",
			},
		},
	})
	// pushTimerGates pushes based on the content arg, not the index.
	// But fireExpiredTimersLocked re-reads from the index.
	// The index has status="satisfied", so the entry is skipped.

	ts.fireExpiredTimersLocked(context.Background())

	if len(writer.events) != 0 {
		t.Fatalf("stale heap entry should not fire, got %d writes", len(writer.events))
	}
}

func TestTimerHeapOrdering(t *testing.T) {
	// Verify the heap pops entries in target-time order.
	ts := &TicketService{
		clock:      clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Push entries in non-chronological order.
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "late", Type: "timer", Status: "pending", Target: "2026-01-01T03:00:00Z"},
			{ID: "early", Type: "timer", Status: "pending", Target: "2026-01-01T01:00:00Z"},
			{ID: "mid", Type: "timer", Status: "pending", Target: "2026-01-01T02:00:00Z"},
		},
	}
	ts.pushTimerGates(testRoomID("!room:local"), "tkt-1", content)

	if ts.timers.Len() != 3 {
		t.Fatalf("expected 3 heap entries, got %d", ts.timers.Len())
	}

	// Heap minimum should be "early".
	if ts.timers[0].gateID != "early" {
		t.Fatalf("heap minimum should be 'early', got %q", ts.timers[0].gateID)
	}
}

func TestTimerLoopEndToEnd(t *testing.T) {
	// Full integration: create a service with a timer gate, start
	// the timer loop, advance the fake clock past the target, and
	// verify the gate fires.
	writer := &fakeWriterForGates{notify: make(chan struct{}, 16)}
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:      writer,
		clock:       fakeClock,
		rooms:       make(map[ref.RoomID]*roomState),
		aliasCache:  make(map[ref.RoomAlias]ref.RoomID),
		timerNotify: make(chan struct{}, 1),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "timer gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Target:    "2026-01-01T01:00:00Z",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.rebuildTimerHeap()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ts.startTimerLoop(ctx)

	// Wait for the AfterFunc waiter to be registered by
	// scheduleNextTimerLocked (called at the start of the loop).
	fakeClock.WaitForTimers(1)

	// Advance past the target. AfterFunc fires synchronously,
	// sending on timerNotify. The timer loop goroutine picks it up.
	fakeClock.Advance(1*time.Hour + time.Second)

	// Block until the timer loop goroutine writes the gate
	// satisfaction event.
	<-writer.notify

	ts.mu.Lock()
	defer ts.mu.Unlock()

	content, exists := ts.rooms[roomID].index.Get("tkt-timer")
	if !exists {
		t.Fatal("ticket should still exist")
	}
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("timer gate should be satisfied via timer loop, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write, got %d", len(writer.events))
	}
}

func TestTimerLoopRescheduleOnNewEntry(t *testing.T) {
	// Verify that pushing a closer timer to the heap reschedules
	// the wake-up so it fires at the new minimum.
	writer := &fakeWriterForGates{notify: make(chan struct{}, 16)}
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:      writer,
		clock:       fakeClock,
		rooms:       make(map[ref.RoomID]*roomState),
		aliasCache:  make(map[ref.RoomAlias]ref.RoomID),
		timerNotify: make(chan struct{}, 1),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-far": {
			Version:   1,
			Title:     "far timer",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "far-gate",
					Type:      "timer",
					Status:    "pending",
					Duration:  "24h",
					Target:    "2026-01-02T00:00:00Z",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.rebuildTimerHeap()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ts.startTimerLoop(ctx)
	fakeClock.WaitForTimers(1) // Wait for the 24h AfterFunc.

	// Now push a closer timer (1h) while holding the lock.
	ts.mu.Lock()
	ts.rooms[roomID].index.Put("tkt-near", ticket.TicketContent{
		Version:   1,
		Title:     "near timer",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "near-gate",
				Type:      "timer",
				Status:    "pending",
				Duration:  "1h",
				Target:    "2026-01-01T01:00:00Z",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	})
	ts.pushTimerGates(roomID, "tkt-near", &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:     "near-gate",
				Type:   "timer",
				Status: "pending",
				Target: "2026-01-01T01:00:00Z",
			},
		},
	})
	ts.mu.Unlock()

	// Wait for the rescheduled AfterFunc (1h instead of 24h).
	fakeClock.WaitForTimers(1)

	// Advance past the near timer but not the far one.
	fakeClock.Advance(1*time.Hour + time.Second)

	// Block until the timer loop goroutine writes the near gate
	// satisfaction event.
	<-writer.notify

	ts.mu.Lock()
	defer ts.mu.Unlock()

	// The near timer should have fired.
	nearContent, _ := ts.rooms[roomID].index.Get("tkt-near")
	if nearContent.Gates[0].Status != "satisfied" {
		t.Fatalf("near timer should be satisfied, got %q", nearContent.Gates[0].Status)
	}

	// The far timer should still be pending.
	farContent, _ := ts.rooms[roomID].index.Get("tkt-far")
	if farContent.Gates[0].Status != "pending" {
		t.Fatalf("far timer should still be pending, got %q", farContent.Gates[0].Status)
	}
}

// --- satisfyGate integration test ---

func TestSatisfyGateWritesToMatrixAndUpdatesIndex(t *testing.T) {
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	content := ticket.TicketContent{
		Version:   1,
		Title:     "gated",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "ci-pass",
				Type:      "pipeline",
				Status:    "pending",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": content,
	})

	err := ts.satisfyGate(context.Background(), roomID, ts.rooms[roomID], "tkt-1", content, 0, "$event-id")
	if err != nil {
		t.Fatalf("satisfyGate: %v", err)
	}

	// Verify the index was updated.
	updated, exists := ts.rooms[roomID].index.Get("tkt-1")
	if !exists {
		t.Fatal("ticket should exist in index")
	}
	if updated.Gates[0].Status != "satisfied" {
		t.Fatalf("gate should be satisfied in index, got %q", updated.Gates[0].Status)
	}
	if updated.Gates[0].SatisfiedBy != "$event-id" {
		t.Fatalf("gate satisfied_by should be '$event-id', got %q", updated.Gates[0].SatisfiedBy)
	}
	if updated.Gates[0].SatisfiedAt != "2026-02-01T12:00:00Z" {
		t.Fatalf("gate satisfied_at should be clock time, got %q", updated.Gates[0].SatisfiedAt)
	}
	if updated.UpdatedAt != "2026-02-01T12:00:00Z" {
		t.Fatalf("ticket updated_at should be clock time, got %q", updated.UpdatedAt)
	}

	// Verify a state event was written.
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write, got %d", len(writer.events))
	}
	if writer.events[0].RoomID != roomID.String() {
		t.Fatalf("write room should be %q, got %q", roomID, writer.events[0].RoomID)
	}
	if writer.events[0].StateKey != "tkt-1" {
		t.Fatalf("write state key should be 'tkt-1', got %q", writer.events[0].StateKey)
	}
}

// --- End-to-end: evaluateGatesForEvents ---

func TestEvaluateGatesForEventsPipelineGate(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$pipeline-result-1"),
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("build-check"),
			Content: map[string]any{
				"pipeline_ref": "build-check",
				"conclusion":   "success",
			},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("pipeline gate should be satisfied, got %q", content.Gates[0].Status)
	}
	if content.Gates[0].SatisfiedBy != "$pipeline-result-1" {
		t.Fatalf("satisfied_by should be event ID, got %q", content.Gates[0].SatisfiedBy)
	}
}

func TestEvaluateGatesForEventsTicketGate(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	// Ticket tkt-2 has a gate waiting for tkt-1 to close.
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "blocker",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-2": {
			Version:   1,
			Title:     "waiting on blocker",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "blocker-done",
					Type:      "ticket",
					Status:    "pending",
					TicketID:  "tkt-1",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Simulate tkt-1 being updated to closed (the event that arrived via /sync).
	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$tkt1-closed"),
			Type:     schema.EventTypeTicket,
			StateKey: stringPtr("tkt-1"),
			Content: map[string]any{
				"status": "closed",
			},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	content, _ := ts.rooms[roomID].index.Get("tkt-2")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("ticket gate should be satisfied, got %q", content.Gates[0].Status)
	}
}

func TestEvaluateGatesForEventsMultipleGatesOnOneTicket(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "multi-gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
				{
					ID:          "lint",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "lint-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Only the build-check event arrives. The lint-check gate should remain pending.
	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$build-result"),
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("build-check"),
			Content: map[string]any{
				"pipeline_ref": "build-check",
				"conclusion":   "success",
			},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("ci gate should be satisfied, got %q", content.Gates[0].Status)
	}
	if content.Gates[1].Status != "pending" {
		t.Fatalf("lint gate should remain pending, got %q", content.Gates[1].Status)
	}
}

func TestEvaluateGatesForEventsBothGatesSatisfiedByBatchEvents(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "multi-gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
				{
					ID:          "lint",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "lint-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Both results arrive in the same sync batch.
	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$build-result"),
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("build-check"),
			Content: map[string]any{
				"pipeline_ref": "build-check",
				"conclusion":   "success",
			},
		},
		{
			EventID:  ref.MustParseEventID("$lint-result"),
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("lint-check"),
			Content: map[string]any{
				"pipeline_ref": "lint-check",
				"conclusion":   "success",
			},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("ci gate should be satisfied, got %q", content.Gates[0].Status)
	}
	if content.Gates[1].Status != "satisfied" {
		t.Fatalf("lint gate should be satisfied, got %q", content.Gates[1].Status)
	}

	// Should have written two state events (one per gate satisfaction).
	if len(writer.events) != 2 {
		t.Fatalf("expected 2 Matrix writes, got %d", len(writer.events))
	}
}

func TestEvaluateGatesSkipsAlreadySatisfiedGates(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "already satisfied",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "satisfied",
					PipelineRef: "build-check",
					SatisfiedAt: "2026-01-01T01:00:00Z",
					SatisfiedBy: "$earlier",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$new-result"),
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("build-check"),
			Content: map[string]any{
				"pipeline_ref": "build-check",
				"conclusion":   "success",
			},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	// No writes should happen — gate is already satisfied.
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for already-satisfied gate, got %d", len(writer.events))
	}
}

func TestEvaluateGatesNoMatchDoesNotWrite(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Unrelated event.
	events := []messaging.Event{
		{
			EventID:  ref.MustParseEventID("$unrelated"),
			Type:     "m.bureau.workspace",
			StateKey: stringPtr(""),
			Content:  map[string]any{"status": "active"},
		},
	}

	ts.evaluateGatesForEvents(context.Background(), roomID, ts.rooms[roomID], events)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("gate should remain pending, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for non-matching event, got %d", len(writer.events))
	}
}

// --- processRoomSync integration test ---

func TestProcessRoomSyncTriggersGateEvaluation(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build-check",
					Conclusion:  "success",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// Pipeline result arrives in the timeline (as state events do
	// during incremental /sync).
	room := messaging.JoinedRoom{
		Timeline: messaging.TimelineSection{
			Events: []messaging.Event{
				{
					EventID:  ref.MustParseEventID("$pipeline-done"),
					Type:     schema.EventTypePipelineResult,
					StateKey: stringPtr("build-check"),
					Content: map[string]any{
						"pipeline_ref": "build-check",
						"conclusion":   "success",
					},
				},
			},
		},
	}

	ts.processRoomSync(context.Background(), roomID, room)

	content, _ := ts.rooms[roomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("gate should be satisfied after processRoomSync, got %q", content.Gates[0].Status)
	}
}

func TestProcessRoomSyncTicketCloseTriggersGate(t *testing.T) {
	writer := &fakeWriterForGates{}
	ts := newGateTestServiceWithWriter(writer)
	roomID := testRoomID("!room:local")

	// tkt-2 has a ticket gate waiting for tkt-1 to close.
	// tkt-1 starts as "open" in the index.
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "blocker",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-2": {
			Version:   1,
			Title:     "dependent",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "wait-for-blocker",
					Type:      "ticket",
					Status:    "pending",
					TicketID:  "tkt-1",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	// tkt-1 transitions to closed via /sync.
	closedContent := toContentMap(t, ticket.TicketContent{
		Version:   1,
		Title:     "blocker",
		Status:    "closed",
		Type:      "task",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		ClosedAt:  "2026-01-15T12:00:00Z",
	})

	room := messaging.JoinedRoom{
		Timeline: messaging.TimelineSection{
			Events: []messaging.Event{
				{
					EventID:  ref.MustParseEventID("$tkt1-close"),
					Type:     schema.EventTypeTicket,
					StateKey: stringPtr("tkt-1"),
					Content:  closedContent,
				},
			},
		},
	}

	ts.processRoomSync(context.Background(), roomID, room)

	// tkt-1 should now be closed in the index (from indexing).
	tkt1, _ := ts.rooms[roomID].index.Get("tkt-1")
	if tkt1.Status != "closed" {
		t.Fatalf("tkt-1 should be closed, got %q", tkt1.Status)
	}

	// tkt-2's gate should be satisfied (from gate evaluation).
	tkt2, _ := ts.rooms[roomID].index.Get("tkt-2")
	if tkt2.Gates[0].Status != "satisfied" {
		t.Fatalf("ticket gate on tkt-2 should be satisfied, got %q", tkt2.Gates[0].Status)
	}
}

// --- matchStateEventCondition unit tests ---
//
// These verify the extracted condition logic works independently of
// room routing. Same-room tests are covered by the existing
// matchStateEventGate tests above; these ensure the extracted
// function matches identically.

func TestMatchStateEventConditionBasic(t *testing.T) {
	gate := &ticket.TicketGate{
		Type:      "state_event",
		EventType: "m.bureau.workspace",
	}
	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("ws-1"),
		Content:  map[string]any{"status": "active"},
	}
	if !matchStateEventCondition(gate, event) {
		t.Fatal("condition should match on event type alone")
	}
}

func TestMatchStateEventConditionWrongType(t *testing.T) {
	gate := &ticket.TicketGate{
		Type:      "state_event",
		EventType: "m.bureau.workspace",
	}
	event := messaging.Event{
		Type:     "m.bureau.pipeline_result",
		StateKey: stringPtr(""),
		Content:  map[string]any{},
	}
	if matchStateEventCondition(gate, event) {
		t.Fatal("condition should not match on different event type")
	}
}

func TestMatchStateEventConditionStateKey(t *testing.T) {
	gate := &ticket.TicketGate{
		Type:      "state_event",
		EventType: "m.bureau.workspace",
		StateKey:  "ws-1",
	}
	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("ws-1"),
		Content:  map[string]any{},
	}
	if !matchStateEventCondition(gate, event) {
		t.Fatal("condition should match on event type + state key")
	}
}

func TestMatchStateEventConditionWrongStateKey(t *testing.T) {
	gate := &ticket.TicketGate{
		Type:      "state_event",
		EventType: "m.bureau.workspace",
		StateKey:  "ws-1",
	}
	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr("ws-2"),
		Content:  map[string]any{},
	}
	if matchStateEventCondition(gate, event) {
		t.Fatal("condition should not match on different state key")
	}
}

// --- Cross-room gate evaluation tests ---

func TestCrossRoomGateSatisfiedByWatchedRoomEvent(t *testing.T) {
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci/results:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Ticket room has a cross-room gate watching CI results.
	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI from other room",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "build-check",
					RoomAlias: ref.MustParseRoomAlias("#ci/results:bureau.local"),
				},
			},
		},
	})

	// Sync batch includes events from the CI room (not a ticket room).
	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!ci-room:local"): {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$ci-result-1"),
						Type:     schema.EventTypePipelineResult,
						StateKey: stringPtr("build-check"),
						Content: map[string]any{
							"pipeline_ref": "build-check",
							"conclusion":   "success",
						},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("cross-room gate should be satisfied, got %q", content.Gates[0].Status)
	}
	if content.Gates[0].SatisfiedBy != "$ci-result-1" {
		t.Fatalf("satisfied_by should be event ID, got %q", content.Gates[0].SatisfiedBy)
	}
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write, got %d", len(writer.events))
	}
	// The write should target the ticket room, not the watched room.
	if writer.events[0].RoomID != ticketRoomID.String() {
		t.Fatalf("write should target ticket room %q, got %q", ticketRoomID, writer.events[0].RoomID)
	}
}

func TestCrossRoomGateNoEventsFromWatchedRoom(t *testing.T) {
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci/results:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: ref.MustParseRoomAlias("#ci/results:bureau.local"),
				},
			},
		},
	})

	// Sync batch has no events from the watched room.
	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!other-room:local"): {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$ev1"),
						Type:     "m.room.topic",
						StateKey: stringPtr(""),
						Content:  map[string]any{"topic": "hello"},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("gate should remain pending when watched room has no events, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes, got %d", len(writer.events))
	}
}

func TestCrossRoomGateUnresolvableAlias(t *testing.T) {
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{}, // empty — alias not found
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "bad alias gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: ref.MustParseRoomAlias("#nonexistent:bureau.local"),
				},
			},
		},
	})

	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("gate should remain pending when alias is unresolvable, got %q", content.Gates[0].Status)
	}
}

func TestCrossRoomGateAliasCaching(t *testing.T) {
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci/results:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// First resolution should call the resolver.
	roomID, err := ts.resolveAliasWithCache(context.Background(), ref.MustParseRoomAlias("#ci/results:bureau.local"))
	if err != nil {
		t.Fatalf("first resolve failed: %v", err)
	}
	if roomID != testRoomID("!ci-room:local") {
		t.Fatalf("expected !ci-room:local, got %q", roomID)
	}
	if resolver.callCount != 1 {
		t.Fatalf("expected 1 resolver call, got %d", resolver.callCount)
	}

	// Second resolution should use the cache.
	roomID, err = ts.resolveAliasWithCache(context.Background(), ref.MustParseRoomAlias("#ci/results:bureau.local"))
	if err != nil {
		t.Fatalf("cached resolve failed: %v", err)
	}
	if roomID != testRoomID("!ci-room:local") {
		t.Fatalf("expected !ci-room:local from cache, got %q", roomID)
	}
	if resolver.callCount != 1 {
		t.Fatalf("expected still 1 resolver call (cached), got %d", resolver.callCount)
	}
}

func TestCrossRoomGateSkipsSameRoomGates(t *testing.T) {
	// Same-room state_event gates (no RoomAlias) should be handled by
	// the regular evaluateGatesForEvents path, not cross-room evaluation.
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{aliases: map[string]string{}}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "same-room gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "local",
					Type:      "state_event",
					Status:    "pending",
					EventType: "m.bureau.workspace",
					// No RoomAlias — same-room gate.
				},
			},
		},
	})

	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		ticketRoomID: {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$ev1"),
						Type:     "m.bureau.workspace",
						StateKey: stringPtr("ws-1"),
						Content:  map[string]any{"status": "active"},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	// Same-room gate should NOT be touched by cross-room evaluation.
	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("same-room gate should remain pending in cross-room evaluation, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for same-room gate in cross-room path, got %d", len(writer.events))
	}
}

func TestCrossRoomGateSkipsNonStateEventTypes(t *testing.T) {
	// Only state_event gates use RoomAlias. Pipeline and ticket gates
	// should not be processed by cross-room evaluation even if they
	// somehow had RoomAlias set (which they shouldn't, but defensive).
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "pipeline gate with alias",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build",
				},
			},
		},
	})

	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!ci-room:local"): {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$ev1"),
						Type:     schema.EventTypePipelineResult,
						StateKey: stringPtr("build"),
						Content: map[string]any{
							"pipeline_ref": "build",
							"conclusion":   "success",
						},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("pipeline gate should not be touched by cross-room evaluation, got %q", content.Gates[0].Status)
	}
}

func TestCrossRoomGateContentMatch(t *testing.T) {
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#deploy/staging:bureau.local": "!deploy-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "wait for staging deploy",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "staging-deploy",
					Type:      "state_event",
					Status:    "pending",
					EventType: "m.bureau.deploy",
					RoomAlias: ref.MustParseRoomAlias("#deploy/staging:bureau.local"),
					ContentMatch: schema.ContentMatch{
						"status": schema.Eq("live"),
					},
				},
			},
		},
	})

	// Event matches type but not content.
	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!deploy-room:local"): {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$deploy-1"),
						Type:     "m.bureau.deploy",
						StateKey: stringPtr("staging"),
						Content:  map[string]any{"status": "deploying"},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("gate should remain pending when content doesn't match, got %q", content.Gates[0].Status)
	}

	// Now send an event that matches content.
	joinedRooms[testRoomID("!deploy-room:local")] = messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{
					EventID:  ref.MustParseEventID("$deploy-2"),
					Type:     "m.bureau.deploy",
					StateKey: stringPtr("staging"),
					Content:  map[string]any{"status": "live"},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ = ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("gate should be satisfied when content matches, got %q", content.Gates[0].Status)
	}
	if content.Gates[0].SatisfiedBy != "$deploy-2" {
		t.Fatalf("satisfied_by should be $deploy-2, got %q", content.Gates[0].SatisfiedBy)
	}
}

func TestCrossRoomGateTimelineEvents(t *testing.T) {
	// State events can arrive in the timeline section during incremental
	// sync. Cross-room evaluation should pick them up.
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci/results:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "timeline event gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "lint",
					RoomAlias: ref.MustParseRoomAlias("#ci/results:bureau.local"),
				},
			},
		},
	})

	// Event arrives in the timeline section (not state section).
	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!ci-room:local"): {
			Timeline: messaging.TimelineSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$lint-result"),
						Type:     schema.EventTypePipelineResult,
						StateKey: stringPtr("lint"),
						Content: map[string]any{
							"pipeline_ref": "lint",
							"conclusion":   "success",
						},
					},
				},
			},
		},
	}

	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("gate should be satisfied by timeline state event, got %q", content.Gates[0].Status)
	}
}

func TestCrossRoomGateNilResolverIsNoOp(t *testing.T) {
	// When no resolver is configured (tests, or session not available),
	// cross-room evaluation should be a no-op.
	writer := &fakeWriterForGates{}
	ts := &TicketService{
		writer:     writer,
		resolver:   nil, // no resolver
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "cross-room gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: ref.MustParseRoomAlias("#ci/results:bureau.local"),
				},
			},
		},
	})

	joinedRooms := map[ref.RoomID]messaging.JoinedRoom{
		testRoomID("!ci-room:local"): {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  ref.MustParseEventID("$ev1"),
						Type:     schema.EventTypePipelineResult,
						StateKey: stringPtr("build"),
						Content:  map[string]any{"pipeline_ref": "build"},
					},
				},
			},
		},
	}

	// Should not panic or error — just no-op.
	ts.evaluateCrossRoomGates(context.Background(), joinedRooms)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("gate should remain pending with nil resolver, got %q", content.Gates[0].Status)
	}
}

func TestCollectStateEvents(t *testing.T) {
	room := messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{EventID: ref.MustParseEventID("$s1"), Type: "m.room.name", StateKey: stringPtr("")},
				{EventID: ref.MustParseEventID("$s2"), Type: "m.bureau.ticket", StateKey: stringPtr("tkt-1")},
			},
		},
		Timeline: messaging.TimelineSection{
			Events: []messaging.Event{
				{EventID: ref.MustParseEventID("$t1"), Type: "m.room.message"},                                // no state key — timeline-only
				{EventID: ref.MustParseEventID("$t2"), Type: "m.bureau.ticket", StateKey: stringPtr("tkt-2")}, // state event in timeline
			},
		},
	}

	events := collectStateEvents(room)
	if len(events) != 3 {
		t.Fatalf("expected 3 state events (2 from state + 1 from timeline), got %d", len(events))
	}
	// Verify order: state events first, then timeline state events.
	if events[0].EventID != ref.MustParseEventID("$s1") {
		t.Fatalf("first event should be $s1, got %s", events[0].EventID)
	}
	if events[1].EventID != ref.MustParseEventID("$s2") {
		t.Fatalf("second event should be $s2, got %s", events[1].EventID)
	}
	if events[2].EventID != ref.MustParseEventID("$t2") {
		t.Fatalf("third event should be $t2, got %s", events[2].EventID)
	}
}

// --- handleSync cross-room integration test ---

func TestHandleSyncCrossRoomGateEvaluation(t *testing.T) {
	writer := &fakeWriterForGates{}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{
			"#ci/results:bureau.local": "!ci-room:local",
		},
	}
	ts := &TicketService{
		writer:     writer,
		resolver:   resolver,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := testRoomID("!tickets:local")
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "cross-room via handleSync",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "build-check",
					RoomAlias: ref.MustParseRoomAlias("#ci/results:bureau.local"),
				},
			},
		},
	})

	// Full sync response with events from both the ticket room and
	// the watched CI room.
	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				ticketRoomID: {}, // ticket room, no new events
				testRoomID("!ci-room:local"): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								EventID:  ref.MustParseEventID("$ci-result"),
								Type:     schema.EventTypePipelineResult,
								StateKey: stringPtr("build-check"),
								Content: map[string]any{
									"pipeline_ref": "build-check",
									"conclusion":   "success",
								},
							},
						},
					},
				},
			},
		},
	}

	ts.handleSync(context.Background(), response)

	content, _ := ts.rooms[ticketRoomID].index.Get("tkt-1")
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("cross-room gate should be satisfied via handleSync, got %q", content.Gates[0].Status)
	}
}

// --- computeTimerTarget tests ---

func TestComputeTimerTargetCreatedBase(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:        "soak",
		Type:      "timer",
		Status:    "pending",
		Duration:  "1h",
		CreatedAt: "2026-01-01T00:00:00Z",
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := computeTimerTarget(gate, baseTime); err != nil {
		t.Fatalf("computeTimerTarget: %v", err)
	}

	if gate.Target != "2026-01-01T01:00:00Z" {
		t.Fatalf("Target = %q, want %q", gate.Target, "2026-01-01T01:00:00Z")
	}
}

func TestComputeTimerTargetSkipsExistingTarget(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "soak",
		Type:     "timer",
		Status:   "pending",
		Duration: "1h",
		Target:   "2026-06-01T00:00:00Z", // Already set by caller.
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := computeTimerTarget(gate, baseTime); err != nil {
		t.Fatalf("computeTimerTarget: %v", err)
	}

	// Target should be unchanged.
	if gate.Target != "2026-06-01T00:00:00Z" {
		t.Fatalf("Target should be unchanged, got %q", gate.Target)
	}
}

func TestComputeTimerTargetSkipsNonTimer(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "ci",
		Type:     "pipeline",
		Status:   "pending",
		Duration: "1h",
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := computeTimerTarget(gate, baseTime); err != nil {
		t.Fatalf("computeTimerTarget: %v", err)
	}

	if gate.Target != "" {
		t.Fatalf("non-timer gate should not get a Target, got %q", gate.Target)
	}
}

func TestComputeTimerTargetInvalidDuration(t *testing.T) {
	gate := &ticket.TicketGate{
		ID:       "soak",
		Type:     "timer",
		Status:   "pending",
		Duration: "bogus",
	}

	err := computeTimerTarget(gate, testClockEpoch)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// --- enrichTimerTargets tests ---

func TestEnrichTimerTargetsCreatedBase(t *testing.T) {
	index := newTestIndex(nil)
	content := &ticket.TicketContent{
		Version:   1,
		Title:     "timer test",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "soak",
				Type:      "timer",
				Status:    "pending",
				Duration:  "2h",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	if content.Gates[0].Target != "2026-01-01T12:00:00Z" {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, "2026-01-01T12:00:00Z")
	}
}

func TestEnrichTimerTargetsExplicitCreatedBase(t *testing.T) {
	index := newTestIndex(nil)
	content := &ticket.TicketContent{
		Version:   1,
		Title:     "timer test",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "soak",
				Type:      "timer",
				Status:    "pending",
				Duration:  "30m",
				Base:      "created",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	if content.Gates[0].Target != "2026-01-01T10:30:00Z" {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, "2026-01-01T10:30:00Z")
	}
}

func TestEnrichTimerTargetsUnblockedBaseNoBlockers(t *testing.T) {
	index := newTestIndex(nil)
	content := &ticket.TicketContent{
		Version:   1,
		Title:     "unblocked timer",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		// No BlockedBy — effectively unblocked at creation.
		Gates: []ticket.TicketGate{
			{
				ID:        "soak",
				Type:      "timer",
				Status:    "pending",
				Duration:  "1h",
				Base:      "unblocked",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	if content.Gates[0].Target != "2026-01-01T11:00:00Z" {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, "2026-01-01T11:00:00Z")
	}
}

func TestEnrichTimerTargetsUnblockedBaseBlockersClosed(t *testing.T) {
	index := newTestIndex(map[string]ticket.TicketContent{
		"tkt-blocker": {
			Version:   1,
			Title:     "blocker",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T09:00:00Z",
		},
	})

	content := &ticket.TicketContent{
		Version:   1,
		Title:     "unblocked timer",
		Status:    "open",
		Type:      "task",
		BlockedBy: []string{"tkt-blocker"},
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "soak",
				Type:      "timer",
				Status:    "pending",
				Duration:  "1h",
				Base:      "unblocked",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	// Blocker already closed → target computed at creation time.
	if content.Gates[0].Target != "2026-01-01T11:00:00Z" {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, "2026-01-01T11:00:00Z")
	}
}

func TestEnrichTimerTargetsUnblockedBaseBlockersOpen(t *testing.T) {
	index := newTestIndex(map[string]ticket.TicketContent{
		"tkt-blocker": {
			Version:   1,
			Title:     "blocker",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})

	content := &ticket.TicketContent{
		Version:   1,
		Title:     "unblocked timer",
		Status:    "open",
		Type:      "task",
		BlockedBy: []string{"tkt-blocker"},
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "soak",
				Type:      "timer",
				Status:    "pending",
				Duration:  "1h",
				Base:      "unblocked",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	// Blocker still open → target should remain empty.
	if content.Gates[0].Target != "" {
		t.Fatalf("Target should be empty when blockers are open, got %q", content.Gates[0].Target)
	}
}

func TestEnrichTimerTargetsPreservesCallerTarget(t *testing.T) {
	index := newTestIndex(nil)
	content := &ticket.TicketContent{
		Version:   1,
		Title:     "absolute timer",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T10:00:00Z",
		UpdatedAt: "2026-01-01T10:00:00Z",
		Gates: []ticket.TicketGate{
			{
				ID:        "deadline",
				Type:      "timer",
				Status:    "pending",
				Target:    "2026-06-01T00:00:00Z",
				CreatedAt: "2026-01-01T10:00:00Z",
			},
		},
	}

	enrichTimerTargets(content, index)

	if content.Gates[0].Target != "2026-06-01T00:00:00Z" {
		t.Fatalf("caller-set Target should be preserved, got %q", content.Gates[0].Target)
	}
}

// --- resolveUnblockedTimerTargets tests ---

func TestResolveUnblockedTimerTargetsComputesTarget(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock at the moment blockers clear.
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-blocker": {
			Version:   1,
			Title:     "blocker",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-02-01T12:00:00Z",
			ClosedAt:  "2026-02-01T12:00:00Z",
		},
		"tkt-dependent": {
			Version:   1,
			Title:     "depends on blocker",
			Status:    "open",
			Type:      "task",
			BlockedBy: []string{"tkt-blocker"},
			CreatedAt: "2026-01-15T00:00:00Z",
			UpdatedAt: "2026-01-15T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak-after-unblock",
					Type:      "timer",
					Status:    "pending",
					Duration:  "24h",
					Base:      "unblocked",
					CreatedAt: "2026-01-15T00:00:00Z",
					// Target empty — waiting for unblock.
				},
			},
		},
	})

	ts.resolveUnblockedTimerTargets(context.Background(), roomID, ts.rooms[roomID], []string{"tkt-blocker"})

	content, _ := ts.rooms[roomID].index.Get("tkt-dependent")
	// Target should be computed from clock time (2026-02-01T12:00:00Z) + 24h.
	wantTarget := "2026-02-02T12:00:00Z"
	if content.Gates[0].Target != wantTarget {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, wantTarget)
	}
	if content.UpdatedAt != "2026-02-01T12:00:00Z" {
		t.Fatalf("UpdatedAt = %q, want clock time", content.UpdatedAt)
	}
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write, got %d", len(writer.events))
	}
}

func TestResolveUnblockedTimerTargetsSkipsPartiallyBlocked(t *testing.T) {
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-blocker-a": {
			Version:   1,
			Title:     "blocker A",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-02-01T12:00:00Z",
		},
		"tkt-blocker-b": {
			Version:   1,
			Title:     "blocker B",
			Status:    "open", // Still open!
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-dependent": {
			Version:   1,
			Title:     "needs both blockers",
			Status:    "open",
			Type:      "task",
			BlockedBy: []string{"tkt-blocker-a", "tkt-blocker-b"},
			CreatedAt: "2026-01-15T00:00:00Z",
			UpdatedAt: "2026-01-15T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Base:      "unblocked",
					CreatedAt: "2026-01-15T00:00:00Z",
				},
			},
		},
	})

	// Only blocker-a closed. Dependent still has blocker-b open.
	ts.resolveUnblockedTimerTargets(context.Background(), roomID, ts.rooms[roomID], []string{"tkt-blocker-a"})

	content, _ := ts.rooms[roomID].index.Get("tkt-dependent")
	if content.Gates[0].Target != "" {
		t.Fatalf("Target should remain empty when not all blockers closed, got %q", content.Gates[0].Target)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes when partially blocked, got %d", len(writer.events))
	}
}

func TestResolveUnblockedTimerTargetsSkipsCreatedBase(t *testing.T) {
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-blocker": {
			Version:   1,
			Title:     "blocker",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-02-01T12:00:00Z",
		},
		"tkt-dependent": {
			Version:   1,
			Title:     "created base timer",
			Status:    "open",
			Type:      "task",
			BlockedBy: []string{"tkt-blocker"},
			CreatedAt: "2026-01-15T00:00:00Z",
			UpdatedAt: "2026-01-15T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Base:      "created",
					Target:    "2026-01-15T01:00:00Z", // Already computed at creation.
					CreatedAt: "2026-01-15T00:00:00Z",
				},
			},
		},
	})

	ts.resolveUnblockedTimerTargets(context.Background(), roomID, ts.rooms[roomID], []string{"tkt-blocker"})

	// Should not rewrite — gate has base="created" and Target already set.
	content, _ := ts.rooms[roomID].index.Get("tkt-dependent")
	if content.Gates[0].Target != "2026-01-15T01:00:00Z" {
		t.Fatalf("Target should be unchanged, got %q", content.Gates[0].Target)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for created-base gate, got %d", len(writer.events))
	}
}

func TestResolveUnblockedTimerTargetsMultipleBlockersSameBatch(t *testing.T) {
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[ref.RoomID]*roomState),
		aliasCache: make(map[ref.RoomAlias]ref.RoomID),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-blocker-a": {
			Version:   1,
			Title:     "blocker A",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-02-01T12:00:00Z",
		},
		"tkt-blocker-b": {
			Version:   1,
			Title:     "blocker B",
			Status:    "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-02-01T12:00:00Z",
		},
		"tkt-dependent": {
			Version:   1,
			Title:     "needs both blockers",
			Status:    "open",
			Type:      "task",
			BlockedBy: []string{"tkt-blocker-a", "tkt-blocker-b"},
			CreatedAt: "2026-01-15T00:00:00Z",
			UpdatedAt: "2026-01-15T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					Base:      "unblocked",
					CreatedAt: "2026-01-15T00:00:00Z",
				},
			},
		},
	})

	// Both blockers close in the same sync batch.
	ts.resolveUnblockedTimerTargets(context.Background(), roomID, ts.rooms[roomID], []string{"tkt-blocker-a", "tkt-blocker-b"})

	content, _ := ts.rooms[roomID].index.Get("tkt-dependent")
	wantTarget := "2026-02-01T13:00:00Z"
	if content.Gates[0].Target != wantTarget {
		t.Fatalf("Target = %q, want %q", content.Gates[0].Target, wantTarget)
	}
	// Should write exactly once (not twice for each blocker).
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 Matrix write (not per-blocker), got %d", len(writer.events))
	}
}

// --- Test helpers ---

// newTestIndex creates a ticket index pre-populated with the given
// tickets. Used by enrichTimerTargets tests.
func newTestIndex(tickets map[string]ticket.TicketContent) *ticketindex.Index {
	index := ticketindex.NewIndex()
	for id, content := range tickets {
		index.Put(id, content)
	}
	return index
}

// fakeAliasResolver implements aliasResolver for tests. Returns the
// room ID from the aliases map, or an error if not found.
type fakeAliasResolver struct {
	aliases   map[string]string
	callCount int
}

func (f *fakeAliasResolver) ResolveAlias(_ context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
	f.callCount++
	raw, exists := f.aliases[alias.String()]
	if !exists {
		return ref.RoomID{}, fmt.Errorf("alias not found: %s", alias)
	}
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("fakeAliasResolver: bad room ID %q: %w", raw, err)
	}
	return roomID, nil
}

// fakeWriterForGates records state events without actually writing to
// Matrix. Simpler than the socket_test.go fakeWriter since gate tests
// don't need thread safety (single-goroutine evaluation).
type fakeWriterForGates struct {
	events []writtenEventForGates
	// notify receives after each write. Tests block on this to
	// synchronize with the timer loop goroutine. Buffered.
	notify chan struct{}
}

type writtenEventForGates struct {
	RoomID    string
	EventType ref.EventType
	StateKey  string
	Content   any
}

func (f *fakeWriterForGates) SendStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error) {
	f.events = append(f.events, writtenEventForGates{
		RoomID:    roomID.String(),
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
	if f.notify != nil {
		select {
		case f.notify <- struct{}{}:
		default:
		}
	}
	return ref.MustParseEventID("$event-" + stateKey), nil
}

// newGateTestService creates a TicketService for gate evaluation tests
// with no writer (suitable for match-only tests that don't call satisfyGate).
func newGateTestService() *TicketService {
	return &TicketService{
		clock:       clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:       make(map[ref.RoomID]*roomState),
		subscribers: make(map[ref.RoomID][]*subscriber),
		aliasCache:  make(map[ref.RoomAlias]ref.RoomID),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newGateTestServiceWithWriter creates a TicketService for gate
// evaluation tests that also verify Matrix writes.
func newGateTestServiceWithWriter(writer matrixWriter) *TicketService {
	return &TicketService{
		writer:      writer,
		clock:       clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:       make(map[ref.RoomID]*roomState),
		subscribers: make(map[ref.RoomID][]*subscriber),
		aliasCache:  make(map[ref.RoomAlias]ref.RoomID),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// --- hasRecurringGates tests ---

func TestHasRecurringGatesWithSchedule(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "daily", Type: "timer", Status: "satisfied", Schedule: "0 7 * * *"},
		},
	}
	if !hasRecurringGates(content) {
		t.Fatal("should detect recurring gate with Schedule")
	}
}

func TestHasRecurringGatesWithInterval(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "poll", Type: "timer", Status: "satisfied", Interval: "4h"},
		},
	}
	if !hasRecurringGates(content) {
		t.Fatal("should detect recurring gate with Interval")
	}
}

func TestHasRecurringGatesNonRecurring(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "soak", Type: "timer", Status: "satisfied", Duration: "1h", Target: "2026-01-01T01:00:00Z"},
		},
	}
	if hasRecurringGates(content) {
		t.Fatal("one-shot timer should not be detected as recurring")
	}
}

func TestHasRecurringGatesNonTimer(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "ci", Type: "pipeline", Status: "pending", PipelineRef: "build"},
		},
	}
	if hasRecurringGates(content) {
		t.Fatal("pipeline gate should not be detected as recurring")
	}
}

func TestHasRecurringGatesEmpty(t *testing.T) {
	content := &ticket.TicketContent{}
	if hasRecurringGates(content) {
		t.Fatal("empty gates should not be detected as recurring")
	}
}

// --- rearmRecurringGates tests ---

func TestRearmRecurringGatesSchedule(t *testing.T) {
	// Gate with cron schedule "0 7 * * *" (daily at 7am UTC).
	// Close happens at 2026-01-15 12:00:00 UTC.
	// Next occurrence should be 2026-01-16 07:00:00 UTC.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:          "daily",
				Type:        "timer",
				Status:      "satisfied",
				Duration:    "1h",
				Target:      "2026-01-15T07:00:00Z",
				Schedule:    "0 7 * * *",
				SatisfiedAt: "2026-01-15T07:00:01Z",
				SatisfiedBy: "timer",
				FireCount:   2,
				CreatedAt:   "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should have re-armed")
	}

	gate := content.Gates[0]
	if gate.Status != "pending" {
		t.Fatalf("Status = %q, want pending", gate.Status)
	}
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Fatalf("Target = %q, want 2026-01-16T07:00:00Z", gate.Target)
	}
	if gate.SatisfiedAt != "" {
		t.Fatalf("SatisfiedAt should be cleared, got %q", gate.SatisfiedAt)
	}
	if gate.SatisfiedBy != "" {
		t.Fatalf("SatisfiedBy should be cleared, got %q", gate.SatisfiedBy)
	}
	if gate.FireCount != 3 {
		t.Fatalf("FireCount = %d, want 3", gate.FireCount)
	}
	if gate.LastFiredAt != "2026-01-15T12:00:00Z" {
		t.Fatalf("LastFiredAt = %q, want 2026-01-15T12:00:00Z", gate.LastFiredAt)
	}
}

func TestRearmRecurringGatesInterval(t *testing.T) {
	// Gate with interval "4h". Close happens at 2026-01-15 12:00:00 UTC.
	// Next occurrence: now + 4h = 2026-01-15 16:00:00 UTC.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:          "poll",
				Type:        "timer",
				Status:      "satisfied",
				Duration:    "4h",
				Target:      "2026-01-15T08:00:00Z",
				Interval:    "4h",
				SatisfiedAt: "2026-01-15T08:00:01Z",
				SatisfiedBy: "timer",
				FireCount:   0,
				CreatedAt:   "2026-01-15T04:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should have re-armed")
	}

	gate := content.Gates[0]
	if gate.Status != "pending" {
		t.Fatalf("Status = %q, want pending", gate.Status)
	}
	if gate.Target != "2026-01-15T16:00:00Z" {
		t.Fatalf("Target = %q, want 2026-01-15T16:00:00Z", gate.Target)
	}
	if gate.FireCount != 1 {
		t.Fatalf("FireCount = %d, want 1", gate.FireCount)
	}
}

func TestRearmRecurringGatesMaxOccurrencesExhausted(t *testing.T) {
	// Gate has fired 4 times, MaxOccurrences is 5. After this fire
	// (FireCount becomes 5), it reaches the limit and should NOT re-arm.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:             "daily",
				Type:           "timer",
				Status:         "satisfied",
				Duration:       "1h",
				Target:         "2026-01-15T07:00:00Z",
				Schedule:       "0 7 * * *",
				SatisfiedAt:    "2026-01-15T07:00:01Z",
				SatisfiedBy:    "timer",
				FireCount:      4,
				MaxOccurrences: 5,
				CreatedAt:      "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if rearmed {
		t.Fatal("should NOT re-arm when MaxOccurrences exhausted")
	}

	gate := content.Gates[0]
	// FireCount should still be incremented (records the final fire).
	if gate.FireCount != 5 {
		t.Fatalf("FireCount = %d, want 5", gate.FireCount)
	}
	if gate.LastFiredAt != "2026-01-15T12:00:00Z" {
		t.Fatalf("LastFiredAt = %q, want 2026-01-15T12:00:00Z", gate.LastFiredAt)
	}
	// Status should remain satisfied (not re-armed).
	if gate.Status != "satisfied" {
		t.Fatalf("Status = %q, want satisfied", gate.Status)
	}
}

func TestRearmRecurringGatesMaxOccurrencesNotYetExhausted(t *testing.T) {
	// FireCount is 3, MaxOccurrences is 5. After this fire
	// (FireCount becomes 4), limit not reached — should re-arm.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:             "daily",
				Type:           "timer",
				Status:         "satisfied",
				Duration:       "1h",
				Target:         "2026-01-15T07:00:00Z",
				Schedule:       "0 7 * * *",
				SatisfiedAt:    "2026-01-15T07:00:01Z",
				SatisfiedBy:    "timer",
				FireCount:      3,
				MaxOccurrences: 5,
				CreatedAt:      "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should re-arm when MaxOccurrences not yet exhausted")
	}

	gate := content.Gates[0]
	if gate.FireCount != 4 {
		t.Fatalf("FireCount = %d, want 4", gate.FireCount)
	}
	if gate.Status != "pending" {
		t.Fatalf("Status = %q, want pending", gate.Status)
	}
}

func TestRearmRecurringGatesUnlimitedOccurrences(t *testing.T) {
	// MaxOccurrences=0 means unlimited. Should always re-arm.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:             "daily",
				Type:           "timer",
				Status:         "satisfied",
				Duration:       "1h",
				Target:         "2026-01-15T07:00:00Z",
				Schedule:       "0 7 * * *",
				SatisfiedAt:    "2026-01-15T07:00:01Z",
				SatisfiedBy:    "timer",
				FireCount:      999,
				MaxOccurrences: 0,
				CreatedAt:      "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should re-arm when MaxOccurrences is 0 (unlimited)")
	}
	if content.Gates[0].FireCount != 1000 {
		t.Fatalf("FireCount = %d, want 1000", content.Gates[0].FireCount)
	}
}

func TestRearmRecurringGatesMixedGates(t *testing.T) {
	// Ticket has both a recurring timer gate and a non-recurring
	// pipeline gate. Only the timer gate should be re-armed.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:          "ci",
				Type:        "pipeline",
				Status:      "satisfied",
				PipelineRef: "build-check",
				SatisfiedAt: "2026-01-14T12:00:00Z",
				SatisfiedBy: "$event",
				CreatedAt:   "2026-01-10T00:00:00Z",
			},
			{
				ID:          "daily",
				Type:        "timer",
				Status:      "satisfied",
				Duration:    "1h",
				Target:      "2026-01-15T07:00:00Z",
				Schedule:    "0 7 * * *",
				SatisfiedAt: "2026-01-15T07:00:01Z",
				SatisfiedBy: "timer",
				CreatedAt:   "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should re-arm the recurring timer gate")
	}

	// Pipeline gate should be untouched.
	if content.Gates[0].Status != "satisfied" {
		t.Fatalf("pipeline gate Status = %q, want satisfied", content.Gates[0].Status)
	}
	if content.Gates[0].SatisfiedAt != "2026-01-14T12:00:00Z" {
		t.Fatalf("pipeline gate SatisfiedAt should be untouched")
	}

	// Timer gate should be re-armed.
	if content.Gates[1].Status != "pending" {
		t.Fatalf("timer gate Status = %q, want pending", content.Gates[1].Status)
	}
	if content.Gates[1].Target != "2026-01-16T07:00:00Z" {
		t.Fatalf("timer gate Target = %q, want 2026-01-16T07:00:00Z", content.Gates[1].Target)
	}
}

func TestRearmRecurringGatesInvalidSchedule(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:       "bad",
				Type:     "timer",
				Status:   "satisfied",
				Duration: "1h",
				Schedule: "bogus cron expression",
			},
		},
	}

	_, err := rearmRecurringGates(content, now)
	if err == nil {
		t.Fatal("should return error for invalid cron schedule")
	}
}

func TestRearmRecurringGatesInvalidInterval(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:       "bad",
				Type:     "timer",
				Status:   "satisfied",
				Duration: "1h",
				Interval: "not-a-duration",
			},
		},
	}

	_, err := rearmRecurringGates(content, now)
	if err == nil {
		t.Fatal("should return error for invalid interval")
	}
}

func TestRearmRecurringGatesMissedOccurrences(t *testing.T) {
	// Close happens 3 days after the last scheduled occurrence.
	// The next target should be the FIRST future occurrence, not
	// any of the missed ones. cron.Next(now) returns strictly
	// after now, so this works automatically.
	//
	// Schedule: daily at 7am. Last target: Jan 12. Close: Jan 15 12:00.
	// Missed: Jan 13 07:00, Jan 14 07:00, Jan 15 07:00.
	// Next target should be Jan 16 07:00.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{
				ID:          "daily",
				Type:        "timer",
				Status:      "satisfied",
				Duration:    "1h",
				Target:      "2026-01-12T07:00:00Z",
				Schedule:    "0 7 * * *",
				SatisfiedAt: "2026-01-12T07:00:01Z",
				SatisfiedBy: "timer",
				FireCount:   1,
				CreatedAt:   "2026-01-10T00:00:00Z",
			},
		},
	}

	rearmed, err := rearmRecurringGates(content, now)
	if err != nil {
		t.Fatalf("rearmRecurringGates: %v", err)
	}
	if !rearmed {
		t.Fatal("should have re-armed")
	}

	gate := content.Gates[0]
	// Should skip all missed occurrences and land on the next
	// future one: 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Fatalf("Target = %q, want 2026-01-16T07:00:00Z (should skip missed occurrences)", gate.Target)
	}
	if gate.FireCount != 2 {
		t.Fatalf("FireCount = %d, want 2", gate.FireCount)
	}
}

// --- stripRecurringGates tests ---

func TestStripRecurringGatesRemovesRecurring(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "ci", Type: "pipeline", Status: "satisfied", PipelineRef: "build"},
			{ID: "daily", Type: "timer", Status: "satisfied", Schedule: "0 7 * * *"},
			{ID: "soak", Type: "timer", Status: "pending", Duration: "1h"},
			{ID: "poll", Type: "timer", Status: "satisfied", Interval: "4h"},
		},
	}

	stripRecurringGates(content)

	if len(content.Gates) != 2 {
		t.Fatalf("expected 2 gates after strip, got %d", len(content.Gates))
	}
	if content.Gates[0].ID != "ci" {
		t.Fatalf("first gate should be ci, got %q", content.Gates[0].ID)
	}
	if content.Gates[1].ID != "soak" {
		t.Fatalf("second gate should be soak (non-recurring timer), got %q", content.Gates[1].ID)
	}
}

func TestStripRecurringGatesNoRecurring(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "ci", Type: "pipeline", Status: "satisfied", PipelineRef: "build"},
			{ID: "soak", Type: "timer", Status: "pending", Duration: "1h"},
		},
	}

	stripRecurringGates(content)

	if len(content.Gates) != 2 {
		t.Fatalf("expected 2 gates (none recurring), got %d", len(content.Gates))
	}
}

func TestStripRecurringGatesAllRecurring(t *testing.T) {
	content := &ticket.TicketContent{
		Gates: []ticket.TicketGate{
			{ID: "daily", Type: "timer", Status: "satisfied", Schedule: "0 7 * * *"},
			{ID: "poll", Type: "timer", Status: "satisfied", Interval: "4h"},
		},
	}

	stripRecurringGates(content)

	if len(content.Gates) != 0 {
		t.Fatalf("expected 0 gates after stripping all recurring, got %d", len(content.Gates))
	}
}

// --- synthesizeRecurringGate tests ---

func TestSynthesizeRecurringGateSchedule(t *testing.T) {
	testClock := clock.Fake(testClockEpoch) // 2026-01-15T12:00:00Z
	ts := &TicketService{clock: testClock}

	gate, err := ts.synthesizeRecurringGate("0 7 * * *", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gate.ID != "schedule" {
		t.Errorf("ID: got %q, want 'schedule'", gate.ID)
	}
	if gate.Type != "timer" {
		t.Errorf("Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Status != "pending" {
		t.Errorf("Status: got %q, want 'pending'", gate.Status)
	}
	if gate.Schedule != "0 7 * * *" {
		t.Errorf("Schedule: got %q, want '0 7 * * *'", gate.Schedule)
	}
	// Next 7am UTC after 2026-01-15T12:00:00Z is 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Errorf("Target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}
}

func TestSynthesizeRecurringGateInterval(t *testing.T) {
	testClock := clock.Fake(testClockEpoch) // 2026-01-15T12:00:00Z
	ts := &TicketService{clock: testClock}

	gate, err := ts.synthesizeRecurringGate("", "4h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gate.ID != "interval" {
		t.Errorf("ID: got %q, want 'interval'", gate.ID)
	}
	if gate.Interval != "4h" {
		t.Errorf("Interval: got %q, want '4h'", gate.Interval)
	}
	// 12:00 + 4h = 16:00.
	if gate.Target != "2026-01-15T16:00:00Z" {
		t.Errorf("Target: got %q, want '2026-01-15T16:00:00Z'", gate.Target)
	}
}

func TestSynthesizeRecurringGateInvalidSchedule(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeRecurringGate("not-a-cron", "")
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestSynthesizeRecurringGateInvalidInterval(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeRecurringGate("", "not-a-duration")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestSynthesizeRecurringGateIntervalTooShort(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeRecurringGate("", "5s")
	if err == nil {
		t.Fatal("expected error for interval below minimum recurrence")
	}
}

// --- synthesizeDeferGate tests ---

func TestSynthesizeDeferGateUntil(t *testing.T) {
	testClock := clock.Fake(testClockEpoch) // 2026-01-15T12:00:00Z
	ts := &TicketService{clock: testClock}

	gate, err := ts.synthesizeDeferGate("2026-02-01T09:00:00Z", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gate.ID != "defer" {
		t.Errorf("ID: got %q, want 'defer'", gate.ID)
	}
	if gate.Type != "timer" {
		t.Errorf("Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Status != "pending" {
		t.Errorf("Status: got %q, want 'pending'", gate.Status)
	}
	if gate.Target != "2026-02-01T09:00:00Z" {
		t.Errorf("Target: got %q, want '2026-02-01T09:00:00Z'", gate.Target)
	}
	// Defer gates are one-shot: no Schedule, no Interval.
	if gate.Schedule != "" {
		t.Errorf("Schedule: got %q, want empty", gate.Schedule)
	}
	if gate.Interval != "" {
		t.Errorf("Interval: got %q, want empty", gate.Interval)
	}
}

func TestSynthesizeDeferGateFor(t *testing.T) {
	testClock := clock.Fake(testClockEpoch)
	ts := &TicketService{clock: testClock}

	gate, err := ts.synthesizeDeferGate("", "48h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gate.ID != "defer" {
		t.Errorf("ID: got %q, want 'defer'", gate.ID)
	}
	// testClockEpoch + 48h = 2026-01-17T12:00:00Z.
	if gate.Target != "2026-01-17T12:00:00Z" {
		t.Errorf("Target: got %q, want '2026-01-17T12:00:00Z'", gate.Target)
	}
}

func TestSynthesizeDeferGateUntilInThePast(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeDeferGate("2025-01-01T00:00:00Z", "")
	if err == nil {
		t.Fatal("expected error for until time in the past")
	}
}

func TestSynthesizeDeferGateInvalidDuration(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeDeferGate("", "not-a-duration")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestSynthesizeDeferGateNegativeDuration(t *testing.T) {
	ts := &TicketService{clock: clock.Fake(testClockEpoch)}

	_, err := ts.synthesizeDeferGate("", "-1h")
	if err == nil {
		t.Fatal("expected error for negative duration")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{name: "zero", duration: 0, expected: "0s"},
		{name: "sub-second", duration: 500 * time.Millisecond, expected: "0s"},
		{name: "seconds only", duration: 45 * time.Second, expected: "45s"},
		{name: "one second", duration: time.Second, expected: "1s"},
		{name: "minutes and seconds", duration: 3*time.Minute + 15*time.Second, expected: "3m 15s"},
		{name: "minutes only", duration: 10 * time.Minute, expected: "10m"},
		{name: "hours and minutes", duration: 2*time.Hour + 30*time.Minute, expected: "2h 30m"},
		{name: "hours only", duration: 5 * time.Hour, expected: "5h"},
		{name: "days and hours", duration: 3*24*time.Hour + 4*time.Hour, expected: "3d 4h"},
		{name: "days only", duration: 7 * 24 * time.Hour, expected: "7d"},
		{name: "days truncates minutes", duration: 2*24*time.Hour + 3*time.Hour + 15*time.Minute, expected: "2d 3h"},
		{name: "hours truncates seconds", duration: 1*time.Hour + 30*time.Minute + 45*time.Second, expected: "1h 30m"},
		{name: "large duration", duration: 30*24*time.Hour + 12*time.Hour, expected: "30d 12h"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := formatDuration(test.duration)
			if result != test.expected {
				t.Errorf("formatDuration(%v) = %q, want %q", test.duration, result, test.expected)
			}
		})
	}
}

// --- matchReviewGate unit tests ---
//
// Review gates evaluate against index state (typed TicketContent) rather
// than raw event content. The event is just a trigger; the index
// provides the review field and review_finding children.

// reviewGateIndex creates an index with a ticket containing the given
// review field and gate, plus optional review_finding children.
func reviewGateIndex(ticketReview *ticket.TicketReview, children []ticket.TicketContent) (*ticketindex.Index, *ticket.TicketGate) {
	idx := ticketindex.NewIndex()
	gate := ticket.TicketGate{
		ID:     "review-approval",
		Type:   "review",
		Status: "pending",
	}
	parent := testTicket("Review parent")
	parent.Status = "review"
	parent.Review = ticketReview
	parent.Gates = []ticket.TicketGate{gate}
	idx.Put("tkt-review", parent)

	for i, child := range children {
		childID := fmt.Sprintf("tkt-finding-%d", i+1)
		child.Parent = "tkt-review"
		idx.Put(childID, child)
	}

	return idx, &gate
}

func TestMatchReviewGateAllApproved(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved"},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match when all reviewers approved")
	}
}

func TestMatchReviewGatePendingReviewer(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending"},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when a reviewer is still pending")
	}
}

func TestMatchReviewGateChangesRequested(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "changes_requested"},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when a reviewer requested changes")
	}
}

func TestMatchReviewGateNoReview(t *testing.T) {
	index, _ := reviewGateIndex(nil, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when review field is absent")
	}
}

func TestMatchReviewGateEmptyReviewers(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when reviewers list is empty")
	}
}

func TestMatchReviewGateTicketNotInIndex(t *testing.T) {
	index := ticketindex.NewIndex()

	if matchReviewGate(index, "tkt-nonexistent") {
		t.Fatal("review gate should not match when ticket is not in index")
	}
}

func TestMatchReviewGateSingleReviewerApproved(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match with single approved reviewer")
	}
}

func TestMatchReviewGateCommented(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "commented"},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when a reviewer only commented")
	}
}

// --- matchReviewGate with review_finding children ---

func TestMatchReviewGateAllApprovedWithClosedFindings(t *testing.T) {
	finding1 := testTicket("Fix typo")
	finding1.Type = "review_finding"
	finding1.Status = "closed"

	finding2 := testTicket("Handle error")
	finding2.Type = "review_finding"
	finding2.Status = "closed"

	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}, []ticket.TicketContent{finding1, finding2})

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match when all reviewers approved and all findings closed")
	}
}

func TestMatchReviewGateAllApprovedWithOpenFinding(t *testing.T) {
	finding1 := testTicket("Fix typo")
	finding1.Type = "review_finding"
	finding1.Status = "closed"

	finding2 := testTicket("Handle error")
	finding2.Type = "review_finding"
	finding2.Status = "open"

	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}, []ticket.TicketContent{finding1, finding2})

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when a review_finding child is still open")
	}
}

func TestMatchReviewGatePendingReviewerWithClosedFindings(t *testing.T) {
	finding := testTicket("Fix typo")
	finding.Type = "review_finding"
	finding.Status = "closed"

	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending"},
		},
	}, []ticket.TicketContent{finding})

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match when reviewer is pending even if all findings are closed")
	}
}

func TestMatchReviewGateNoFindingsNoChange(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match with approved reviewers and no findings (backwards compatible)")
	}
}

func TestMatchReviewGateNonFindingChildrenIgnored(t *testing.T) {
	// Non-review_finding children should not affect the review gate.
	subtask := testTicket("Subtask")
	subtask.Type = "task"
	subtask.Status = "open"

	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}, []ticket.TicketContent{subtask})

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should ignore non-review_finding children")
	}
}

// --- Tier-aware matchReviewGate tests ---

func TestMatchReviewGateThresholdTwoOfThreeApproved(t *testing.T) {
	threshold := 2
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@carol:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match: 2 of 3 approved meets threshold of 2")
	}
}

func TestMatchReviewGateThresholdTwoOfThreeOnlyOneApproved(t *testing.T) {
	threshold := 2
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@carol:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match: only 1 of 3 approved, threshold is 2")
	}
}

func TestMatchReviewGateMultiTierMixed(t *testing.T) {
	// Tier 0: threshold 1, two reviewers (one approved) — satisfied.
	// Tier 1: nil threshold (all must approve), one reviewer pending — not satisfied.
	threshold1 := 1
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@lead:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold1},
			{Tier: 1, Threshold: nil},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match: tier 1 (all-must-approve) has pending reviewer")
	}
}

func TestMatchReviewGateMultiTierAllSatisfied(t *testing.T) {
	threshold1 := 1
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@lead:bureau.local"), Disposition: "approved", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold1},
			{Tier: 1, Threshold: nil},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match: tier 0 (1-of-2 approved) and tier 1 (all approved)")
	}
}

func TestMatchReviewGateNilThresholdAllMustApprove(t *testing.T) {
	// Tier with nil threshold requires all reviewers to approve.
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "changes_requested", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: nil},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match: nil threshold means all must approve")
	}
}

func TestMatchReviewGateBackwardCompatNoThresholds(t *testing.T) {
	// No TierThresholds → legacy behavior: all must approve, ignoring Tier field.
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved", Tier: 1},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match in legacy mode: all approved")
	}
}

func TestMatchReviewGateBackwardCompatNoThresholdsOnePending(t *testing.T) {
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 1},
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match in legacy mode: one pending")
	}
}

func TestMatchReviewGateThresholdWithOpenFinding(t *testing.T) {
	// Tier threshold met but review_finding child is still open.
	threshold := 1
	finding := testTicket("Fix typo")
	finding.Type = "review_finding"
	finding.Status = "open"

	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
		},
	}, []ticket.TicketContent{finding})

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match: threshold met but open finding")
	}
}

func TestMatchReviewGateThresholdExactlyMet(t *testing.T) {
	// Exactly threshold number of approvals.
	threshold := 3
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@carol:bureau.local"), Disposition: "approved", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
		},
	}, nil)

	if !matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should match: exactly 3 approvals meeting threshold of 3")
	}
}

func TestMatchReviewGateReviewersInTierWithoutThreshold(t *testing.T) {
	// Reviewers exist in tier 2 but no TierThreshold entry for tier 2.
	// Should default to all-must-approve for that tier.
	threshold := 1
	index, _ := reviewGateIndex(&ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@lead:bureau.local"), Disposition: "pending", Tier: 2},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
			// No entry for tier 2.
		},
	}, nil)

	if matchReviewGate(index, "tkt-review") {
		t.Fatal("review gate should not match: tier 2 has no threshold entry, defaults to all-must-approve")
	}
}
