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
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- matchGateEvent unit tests ---

func TestMatchPipelineGateMatches(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("pipeline gate should match on matching pipeline_ref and conclusion")
	}
}

func TestMatchPipelineGateWrongRef(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("pipeline gate should not match on different pipeline_ref")
	}
}

func TestMatchPipelineGateWrongConclusion(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("pipeline gate should not match on wrong conclusion")
	}
}

func TestMatchPipelineGateAnyConclusionMatches(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("pipeline gate with empty conclusion should match any completed result")
	}
}

func TestMatchPipelineGateWrongEventType(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("pipeline gate should not match on wrong event type")
	}
}

// --- Ticket gate tests ---

func TestMatchTicketGateClosedMatches(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("ticket gate should match when watched ticket reaches closed")
	}
}

func TestMatchTicketGateNotClosedDoesNotMatch(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("ticket gate should not match when ticket is not closed")
	}
}

func TestMatchTicketGateWrongTicketID(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("ticket gate should not match for a different ticket ID")
	}
}

// --- State event gate tests ---

func TestMatchStateEventGateBasicMatch(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("state_event gate should match on event_type + state_key")
	}
}

func TestMatchStateEventGateWrongEventType(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("state_event gate should not match on wrong event type")
	}
}

func TestMatchStateEventGateWrongStateKey(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("state_event gate should not match on wrong state key")
	}
}

func TestMatchStateEventGateNoStateKeyMatchesAny(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("state_event gate with no state_key should match any state key")
	}
}

func TestMatchStateEventGateWithContentMatch(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("state_event gate should match when content_match criteria are satisfied")
	}
}

func TestMatchStateEventGateContentMatchFails(t *testing.T) {
	gate := &schema.TicketGate{
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

	if matchGateEvent(gate, event) {
		t.Fatal("state_event gate should not match when content_match criteria fail")
	}
}

func TestMatchStateEventGateWithNumericContentMatch(t *testing.T) {
	gate := &schema.TicketGate{
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

	if !matchGateEvent(gate, event) {
		t.Fatal("state_event gate should match on numeric content_match")
	}
}

func TestMatchStateEventGateSkipsCrossRoom(t *testing.T) {
	gate := &schema.TicketGate{
		ID:        "cross-room",
		Type:      "state_event",
		Status:    "pending",
		EventType: "m.bureau.workspace",
		RoomAlias: "#other/room:bureau.local",
	}

	event := messaging.Event{
		Type:     "m.bureau.workspace",
		StateKey: stringPtr(""),
		Content:  map[string]any{},
	}

	if matchGateEvent(gate, event) {
		t.Fatal("state_event gate with RoomAlias should be skipped (cross-room not yet supported)")
	}
}

// --- Human gate is never auto-matched ---

func TestHumanGateNotAutoMatched(t *testing.T) {
	ts := newGateTestService()
	roomID := "!room:local"
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version: 1,
			Title:   "gated ticket",
			Status:  "open",
			Type:    "task",
			Gates: []schema.TicketGate{
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
		EventID:  "$ev1",
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
	gate := &schema.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "1h",
	}
	now := time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC) // 1h + 1s after creation
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !expired {
		t.Fatal("timer should be expired")
	}
}

func TestTimerExpiredExactDeadline(t *testing.T) {
	gate := &schema.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "1h",
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) // Exactly at deadline.
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !expired {
		t.Fatal("timer should be expired at exact deadline")
	}
}

func TestTimerNotExpired(t *testing.T) {
	gate := &schema.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "24h",
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // 12h — only half the duration.
	expired, err := timerExpired(gate, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expired {
		t.Fatal("timer should not be expired before deadline")
	}
}

func TestTimerInvalidDuration(t *testing.T) {
	gate := &schema.TicketGate{
		CreatedAt: "2026-01-01T00:00:00Z",
		Duration:  "bogus",
	}
	// The now value is irrelevant — parsing fails before comparison.
	_, err := timerExpired(gate, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestTimerInvalidCreatedAt(t *testing.T) {
	gate := &schema.TicketGate{
		CreatedAt: "not-a-timestamp",
		Duration:  "1h",
	}
	// The now value is irrelevant — parsing fails before comparison.
	_, err := timerExpired(gate, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error for invalid created_at")
	}
}

// --- evaluateTimerGates integration test ---

func TestEvaluateTimerGatesSatisfiesExpired(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock set to 2h after gate creation — timer has expired.
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := "!room:local"
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "timer gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.evaluateTimerGates(context.Background())

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
}

func TestEvaluateTimerGatesSkipsUnexpired(t *testing.T) {
	writer := &fakeWriterForGates{}
	// Clock set to 30m after gate creation — timer has NOT expired (1h duration).
	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := "!room:local"
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-timer": {
			Version:   1,
			Title:     "timer gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "soak",
					Type:      "timer",
					Status:    "pending",
					Duration:  "1h",
					CreatedAt: "2026-01-01T00:00:00Z",
				},
			},
		},
	})

	ts.evaluateTimerGates(context.Background())

	content, _ := ts.rooms[roomID].index.Get("tkt-timer")
	if content.Gates[0].Status != "pending" {
		t.Fatalf("unexpired timer gate should remain pending, got %q", content.Gates[0].Status)
	}
	if len(writer.events) != 0 {
		t.Fatalf("expected no Matrix writes, got %d", len(writer.events))
	}
}

// --- satisfyGate integration test ---

func TestSatisfyGateWritesToMatrixAndUpdatesIndex(t *testing.T) {
	writer := &fakeWriterForGates{}
	fakeClock := clock.Fake(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	ts := &TicketService{
		writer:     writer,
		clock:      fakeClock,
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	roomID := "!room:local"
	content := schema.TicketContent{
		Version:   1,
		Title:     "gated",
		Status:    "open",
		Type:      "task",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
		Gates: []schema.TicketGate{
			{
				ID:        "ci-pass",
				Type:      "pipeline",
				Status:    "pending",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
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
	if writer.events[0].RoomID != roomID {
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
			EventID:  "$pipeline-result-1",
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
	roomID := "!room:local"

	// Ticket tkt-2 has a gate waiting for tkt-1 to close.
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
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
			Gates: []schema.TicketGate{
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
			EventID:  "$tkt1-closed",
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "multi-gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
			EventID:  "$build-result",
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "multi-gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
			EventID:  "$build-result",
			Type:     schema.EventTypePipelineResult,
			StateKey: stringPtr("build-check"),
			Content: map[string]any{
				"pipeline_ref": "build-check",
				"conclusion":   "success",
			},
		},
		{
			EventID:  "$lint-result",
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "already satisfied",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
			EventID:  "$new-result",
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "gated",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
			EventID:  "$unrelated",
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
	roomID := "!room:local"

	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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
					EventID:  "$pipeline-done",
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
	roomID := "!room:local"

	// tkt-2 has a ticket gate waiting for tkt-1 to close.
	// tkt-1 starts as "open" in the index.
	ts.rooms[roomID] = newTrackedRoom(map[string]schema.TicketContent{
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
			Gates: []schema.TicketGate{
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
	closedContent := toContentMap(t, schema.TicketContent{
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
					EventID:  "$tkt1-close",
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
	gate := &schema.TicketGate{
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
	gate := &schema.TicketGate{
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
	gate := &schema.TicketGate{
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
	gate := &schema.TicketGate{
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Ticket room has a cross-room gate watching CI results.
	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI from other room",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "build-check",
					RoomAlias: "#ci/results:bureau.local",
				},
			},
		},
	})

	// Sync batch includes events from the CI room (not a ticket room).
	joinedRooms := map[string]messaging.JoinedRoom{
		"!ci-room:local": {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$ci-result-1",
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
	if writer.events[0].RoomID != ticketRoomID {
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "needs CI",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: "#ci/results:bureau.local",
				},
			},
		},
	})

	// Sync batch has no events from the watched room.
	joinedRooms := map[string]messaging.JoinedRoom{
		"!other-room:local": {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$ev1",
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "bad alias gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: "#nonexistent:bureau.local",
				},
			},
		},
	})

	joinedRooms := map[string]messaging.JoinedRoom{}

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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// First resolution should call the resolver.
	roomID, err := ts.resolveAliasWithCache(context.Background(), "#ci/results:bureau.local")
	if err != nil {
		t.Fatalf("first resolve failed: %v", err)
	}
	if roomID != "!ci-room:local" {
		t.Fatalf("expected !ci-room:local, got %q", roomID)
	}
	if resolver.callCount != 1 {
		t.Fatalf("expected 1 resolver call, got %d", resolver.callCount)
	}

	// Second resolution should use the cache.
	roomID, err = ts.resolveAliasWithCache(context.Background(), "#ci/results:bureau.local")
	if err != nil {
		t.Fatalf("cached resolve failed: %v", err)
	}
	if roomID != "!ci-room:local" {
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "same-room gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
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

	joinedRooms := map[string]messaging.JoinedRoom{
		ticketRoomID: {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$ev1",
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "pipeline gate with alias",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:          "ci",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build",
				},
			},
		},
	})

	joinedRooms := map[string]messaging.JoinedRoom{
		"!ci-room:local": {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$ev1",
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "wait for staging deploy",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "staging-deploy",
					Type:      "state_event",
					Status:    "pending",
					EventType: "m.bureau.deploy",
					RoomAlias: "#deploy/staging:bureau.local",
					ContentMatch: schema.ContentMatch{
						"status": schema.Eq("live"),
					},
				},
			},
		},
	})

	// Event matches type but not content.
	joinedRooms := map[string]messaging.JoinedRoom{
		"!deploy-room:local": {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$deploy-1",
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
	joinedRooms["!deploy-room:local"] = messaging.JoinedRoom{
		State: messaging.StateSection{
			Events: []messaging.Event{
				{
					EventID:  "$deploy-2",
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "timeline event gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "lint",
					RoomAlias: "#ci/results:bureau.local",
				},
			},
		},
	})

	// Event arrives in the timeline section (not state section).
	joinedRooms := map[string]messaging.JoinedRoom{
		"!ci-room:local": {
			Timeline: messaging.TimelineSection{
				Events: []messaging.Event{
					{
						EventID:  "$lint-result",
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "cross-room gate",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					RoomAlias: "#ci/results:bureau.local",
				},
			},
		},
	})

	joinedRooms := map[string]messaging.JoinedRoom{
		"!ci-room:local": {
			State: messaging.StateSection{
				Events: []messaging.Event{
					{
						EventID:  "$ev1",
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
				{EventID: "$s1", Type: "m.room.name", StateKey: stringPtr("")},
				{EventID: "$s2", Type: "m.bureau.ticket", StateKey: stringPtr("tkt-1")},
			},
		},
		Timeline: messaging.TimelineSection{
			Events: []messaging.Event{
				{EventID: "$t1", Type: "m.room.message"},                                // no state key — timeline-only
				{EventID: "$t2", Type: "m.bureau.ticket", StateKey: stringPtr("tkt-2")}, // state event in timeline
			},
		},
	}

	events := collectStateEvents(room)
	if len(events) != 3 {
		t.Fatalf("expected 3 state events (2 from state + 1 from timeline), got %d", len(events))
	}
	// Verify order: state events first, then timeline state events.
	if events[0].EventID != "$s1" {
		t.Fatalf("first event should be $s1, got %s", events[0].EventID)
	}
	if events[1].EventID != "$s2" {
		t.Fatalf("second event should be $s2, got %s", events[1].EventID)
	}
	if events[2].EventID != "$t2" {
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
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ticketRoomID := "!tickets:local"
	ts.rooms[ticketRoomID] = newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "cross-room via handleSync",
			Status:    "open",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "cross-ci",
					Type:      "state_event",
					Status:    "pending",
					EventType: schema.EventTypePipelineResult,
					StateKey:  "build-check",
					RoomAlias: "#ci/results:bureau.local",
				},
			},
		},
	})

	// Full sync response with events from both the ticket room and
	// the watched CI room.
	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				ticketRoomID: {}, // ticket room, no new events
				"!ci-room:local": {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								EventID:  "$ci-result",
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

// --- Test helpers ---

// fakeAliasResolver implements aliasResolver for tests. Returns the
// room ID from the aliases map, or an error if not found.
type fakeAliasResolver struct {
	aliases   map[string]string
	callCount int
}

func (f *fakeAliasResolver) ResolveAlias(_ context.Context, alias string) (string, error) {
	f.callCount++
	roomID, exists := f.aliases[alias]
	if !exists {
		return "", fmt.Errorf("alias not found: %s", alias)
	}
	return roomID, nil
}

// fakeWriterForGates records state events without actually writing to
// Matrix. Simpler than the socket_test.go fakeWriter since gate tests
// don't need thread safety (single-goroutine evaluation).
type fakeWriterForGates struct {
	events []writtenEventForGates
}

type writtenEventForGates struct {
	RoomID    string
	EventType string
	StateKey  string
	Content   any
}

func (f *fakeWriterForGates) SendStateEvent(_ context.Context, roomID, eventType, stateKey string, content any) (string, error) {
	f.events = append(f.events, writtenEventForGates{
		RoomID:    roomID,
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
	return "$event-" + stateKey, nil
}

// newGateTestService creates a TicketService for gate evaluation tests
// with no writer (suitable for match-only tests that don't call satisfyGate).
func newGateTestService() *TicketService {
	return &TicketService{
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newGateTestServiceWithWriter creates a TicketService for gate
// evaluation tests that also verify Matrix writes.
func newGateTestServiceWithWriter(writer matrixWriter) *TicketService {
	return &TicketService{
		writer:     writer,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:      make(map[string]*roomState),
		aliasCache: make(map[string]string),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
