// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Create tests ---

func TestHandleCreate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "new feature",
		"type":     "feature",
		"priority": 1,
		"labels":   []string{"backend"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!room:bureau.local" {
		t.Errorf("room: got %q, want !room:bureau.local", result.Room)
	}
	if result.ID == "" {
		t.Fatal("expected non-empty ticket ID")
	}
	if len(result.ID) < 8 { // "tkt-" + at least 4 hex chars
		t.Errorf("ticket ID too short: %q", result.ID)
	}

	// Verify ticket exists in the index.
	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}
	if content.Title != "new feature" {
		t.Errorf("title: got %q, want 'new feature'", content.Title)
	}
	if content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", content.Status, ticket.StatusOpen)
	}
	if content.CreatedBy != ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local") {
		t.Errorf("created_by: got %q, want '@bureau/fleet/prod/agent/tester:bureau.local'", content.CreatedBy)
	}

	// Verify state event was written to Matrix.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 1 {
		t.Fatalf("expected 1 written event, got %d", len(env.writer.events))
	}
	event := env.writer.events[0]
	if event.RoomID != "!room:bureau.local" {
		t.Errorf("event room: got %q", event.RoomID)
	}
	if event.StateKey != result.ID {
		t.Errorf("event state_key: got %q, want %q", event.StateKey, result.ID)
	}
}

func TestHandleCreateWithBlockedBy(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":       "!room:bureau.local",
		"title":      "blocked task",
		"type":       "task",
		"priority":   2,
		"blocked_by": []string{"tkt-open"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, _ := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if len(content.BlockedBy) != 1 || content.BlockedBy[0] != "tkt-open" {
		t.Errorf("blocked_by: got %v, want [tkt-open]", content.BlockedBy)
	}
}

func TestHandleCreateBlockedByNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":       "!room:bureau.local",
		"title":      "bad deps",
		"type":       "task",
		"priority":   2,
		"blocked_by": []string{"nonexistent"},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateParentNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "orphan subtask",
		"type":     "task",
		"priority": 2,
		"parent":   "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateMissingTitle(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"type":     "task",
		"priority": 2,
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateUnknownRoom(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!nonexistent:bureau.local",
		"title":    "test",
		"type":     "task",
		"priority": 2,
	}, nil)
	requireServiceError(t, err)
}

// --- Update tests ---

func TestHandleUpdateTitle(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"title":  "updated title",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Title != "updated title" {
		t.Errorf("title: got %q, want 'updated title'", result.Content.Title)
	}
	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusOpen)
	}
	if result.Content.UpdatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("updated_at: got %q, want '2026-01-15T12:00:00Z'", result.Content.UpdatedAt)
	}
}

func TestHandleUpdateClaimTicket(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-open",
		"room":     "!room:bureau.local",
		"status":   "in_progress",
		"assignee": "@bureau/fleet/prod/agent/tester:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusInProgress {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusInProgress)
	}
	if result.Content.Assignee != ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local") {
		t.Errorf("assignee: got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateContentionRejected(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// tkt-inprog is already in_progress. Trying to claim it should fail.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-inprog",
		"status":   "in_progress",
		"assignee": "@bureau/fleet/prod/agent/tester:bureau.local",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestHandleUpdateUnclaim(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"status": "open",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusOpen)
	}
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be auto-cleared, got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateInProgressRequiresAssignee(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Trying to claim without providing assignee.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"status": "in_progress",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateAssigneeRequiresActiveStatus(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Trying to set assignee on an open ticket without changing status
	// to in_progress or review.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-open",
		"assignee": "@bureau/fleet/prod/agent/tester:bureau.local",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateCycleDetection(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// tkt-dep already blocks on tkt-open. Making tkt-open block on
	// tkt-dep would create a cycle.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":     "tkt-open",
		"blocked_by": []string{"tkt-dep"},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateCloseViaUpdate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"status": "closed",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be auto-set")
	}
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be empty, got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "nonexistent",
		"title":  "test",
	}, nil)
	requireServiceError(t, err)
}

// --- Close tests ---

func TestHandleClose(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
	if result.Content.CloseReason != "done" {
		t.Errorf("close_reason: got %q, want 'done'", result.Content.CloseReason)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be set")
	}
}

func TestHandleCloseInProgress(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Closing an in_progress ticket should work and clear the assignee.
	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"reason": "agent finished",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be cleared, got %q", result.Content.Assignee)
	}
}

func TestHandleCloseAlreadyClosed(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"reason": "duplicate",
	}, nil)
	// closed → closed is a no-op status transition, not an error.
	// But we check via the validateStatusTransition behavior: same
	// status is allowed for all except in_progress.
	if err != nil {
		t.Fatalf("expected no error for closing already-closed ticket, got: %v", err)
	}
}

// --- Close with recurring gate re-arm tests ---

// recurringRooms returns rooms with tickets that have recurring timer
// gates, suitable for testing the auto-rearm behavior of handleClose.
func recurringRooms() map[ref.RoomID]*roomState {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-recurring-schedule": {
			Version:   1,
			Title:     "daily standup",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "daily",
					Type:      ticket.GateTimer,
					Status:    ticket.GateSatisfied,
					Duration:  "1h",
					Target:    "2026-01-15T07:00:00Z",
					Schedule:  "0 7 * * *",
					CreatedAt: "2026-01-10T00:00:00Z",
					FireCount: 4,
				},
			},
		},
		"tkt-recurring-interval": {
			Version:   1,
			Title:     "polling task",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:        "poll",
					Type:      ticket.GateTimer,
					Status:    ticket.GateSatisfied,
					Duration:  "4h",
					Target:    "2026-01-15T08:00:00Z",
					Interval:  "4h",
					CreatedAt: "2026-01-10T00:00:00Z",
					FireCount: 1,
				},
			},
		},
		"tkt-recurring-exhausted": {
			Version:   1,
			Title:     "limited recurring",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []ticket.TicketGate{
				{
					ID:             "limited",
					Type:           ticket.GateTimer,
					Status:         ticket.GateSatisfied,
					Duration:       "1h",
					Target:         "2026-01-15T07:00:00Z",
					Schedule:       "0 7 * * *",
					CreatedAt:      "2026-01-10T00:00:00Z",
					FireCount:      4,
					MaxOccurrences: 5,
				},
			},
		},
		"tkt-non-recurring": {
			Version:   1,
			Title:     "normal ticket",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
		},
	})

	return map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}
}

func TestHandleCloseRecurringScheduleRearms(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-schedule",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Ticket should reopen instead of closing.
	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q (re-armed)", result.Content.Status, ticket.StatusOpen)
	}
	if result.Content.ClosedAt != "" {
		t.Errorf("closed_at should be empty (re-armed), got %q", result.Content.ClosedAt)
	}
	if result.Content.CloseReason != "" {
		t.Errorf("close_reason should be empty (re-armed), got %q", result.Content.CloseReason)
	}
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be cleared, got %q", result.Content.Assignee)
	}

	// Gate should be re-armed with a new target.
	gate := result.Content.Gates[0]
	if gate.Status != ticket.GatePending {
		t.Errorf("gate status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z. Next cron occurrence
	// for "0 7 * * *" after that is 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Errorf("gate target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}
	if gate.FireCount != 5 {
		t.Errorf("gate fire_count: got %d, want 5", gate.FireCount)
	}
	if gate.LastFiredAt != "2026-01-15T12:00:00Z" {
		t.Errorf("gate last_fired_at: got %q, want '2026-01-15T12:00:00Z'", gate.LastFiredAt)
	}
	if gate.SatisfiedAt != "" {
		t.Errorf("gate satisfied_at should be cleared, got %q", gate.SatisfiedAt)
	}
	if gate.SatisfiedBy != "" {
		t.Errorf("gate satisfied_by should be cleared, got %q", gate.SatisfiedBy)
	}
}

func TestHandleCloseRecurringIntervalRearms(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-interval",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q (re-armed)", result.Content.Status, ticket.StatusOpen)
	}

	gate := result.Content.Gates[0]
	if gate.Status != ticket.GatePending {
		t.Errorf("gate status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch + 4h = 2026-01-15T16:00:00Z
	if gate.Target != "2026-01-15T16:00:00Z" {
		t.Errorf("gate target: got %q, want '2026-01-15T16:00:00Z'", gate.Target)
	}
	if gate.FireCount != 2 {
		t.Errorf("gate fire_count: got %d, want 2", gate.FireCount)
	}
}

func TestHandleCloseRecurringExhaustedClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-exhausted",
		"room":   "!room:bureau.local",
		"reason": "final",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// MaxOccurrences reached — ticket should close normally.
	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q (exhausted)", result.Content.Status, ticket.StatusClosed)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be set for normal close")
	}
	if result.Content.CloseReason != "final" {
		t.Errorf("close_reason: got %q, want 'final'", result.Content.CloseReason)
	}

	// Gate metadata should reflect the final fire.
	gate := result.Content.Gates[0]
	if gate.FireCount != 5 {
		t.Errorf("gate fire_count: got %d, want 5", gate.FireCount)
	}
}

func TestHandleCloseEndRecurrenceClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket":         "tkt-recurring-schedule",
		"room":           "!room:bureau.local",
		"reason":         "manual stop",
		"end_recurrence": true,
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// EndRecurrence strips recurring gates and closes normally.
	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
	if result.Content.CloseReason != "manual stop" {
		t.Errorf("close_reason: got %q, want 'manual stop'", result.Content.CloseReason)
	}
	if len(result.Content.Gates) != 0 {
		t.Errorf("gates should be empty after end_recurrence, got %d", len(result.Content.Gates))
	}
}

func TestHandleCloseNonRecurringClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-non-recurring",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
}

// --- Reopen tests ---

func TestHandleReopen(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusOpen)
	}
	if result.Content.ClosedAt != "" {
		t.Errorf("closed_at should be cleared, got %q", result.Content.ClosedAt)
	}
	if result.Content.CloseReason != "" {
		t.Errorf("close_reason should be cleared, got %q", result.Content.CloseReason)
	}
}

func TestHandleReopenNotClosed(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-open",
	}, nil)
	requireServiceError(t, err)
}

// --- Batch create tests ---

func TestHandleBatchCreate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result batchCreateResponse
	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "first task", "type": "task", "priority": 2},
			{"ref": "b", "title": "second task", "type": "task", "priority": 2, "blocked_by": []string{"a"}},
			{"ref": "c", "title": "third task", "type": "task", "priority": 2, "blocked_by": []string{"b"}},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!room:bureau.local" {
		t.Errorf("room: got %q", result.Room)
	}
	if len(result.Refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(result.Refs))
	}

	// Verify symbolic refs were resolved correctly.
	idA := result.Refs["a"]
	idB := result.Refs["b"]
	idC := result.Refs["c"]

	index := env.service.rooms[testRoomID("!room:bureau.local")].index
	contentB, exists := index.Get(idB)
	if !exists {
		t.Fatalf("ticket B (%s) not in index", idB)
	}
	if len(contentB.BlockedBy) != 1 || contentB.BlockedBy[0] != idA {
		t.Errorf("B.blocked_by: got %v, want [%s]", contentB.BlockedBy, idA)
	}

	contentC, exists := index.Get(idC)
	if !exists {
		t.Fatalf("ticket C (%s) not in index", idC)
	}
	if len(contentC.BlockedBy) != 1 || contentC.BlockedBy[0] != idB {
		t.Errorf("C.blocked_by: got %v, want [%s]", contentC.BlockedBy, idB)
	}

	// Verify all three state events were written.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 3 {
		t.Errorf("expected 3 written events, got %d", len(env.writer.events))
	}
}

func TestHandleBatchCreateDuplicateRef(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "first", "type": "task", "priority": 2},
			{"ref": "a", "title": "duplicate", "type": "task", "priority": 2},
		},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleBatchCreateInvalidRef(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "task", "type": "task", "priority": 2, "blocked_by": []string{"nonexistent"}},
		},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleBatchCreateWithExistingDep(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// blocked_by references an existing ticket (not a symbolic ref).
	var result batchCreateResponse
	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "depends on existing", "type": "task", "priority": 2, "blocked_by": []string{"tkt-open"}},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	idA := result.Refs["a"]
	content, _ := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(idA)
	if len(content.BlockedBy) != 1 || content.BlockedBy[0] != "tkt-open" {
		t.Errorf("blocked_by: got %v, want [tkt-open]", content.BlockedBy)
	}
}

// --- Resolve gate tests ---

func TestHandleResolveGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Find the human-review gate in the response.
	var gate *ticket.TicketGate
	for i := range result.Content.Gates {
		if result.Content.Gates[i].ID == "human-review" {
			gate = &result.Content.Gates[i]
			break
		}
	}
	if gate == nil {
		t.Fatal("human-review gate not found in response")
	}
	if gate.Status != ticket.GateSatisfied {
		t.Errorf("gate status: got %q, want 'satisfied'", gate.Status)
	}
	if gate.SatisfiedBy != "@bureau/fleet/prod/agent/tester:bureau.local" {
		t.Errorf("satisfied_by: got %q", gate.SatisfiedBy)
	}
	if gate.SatisfiedAt == "" {
		t.Error("satisfied_at should be set")
	}
}

func TestHandleResolveGateNotHuman(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// ci-pass is a pipeline gate, not human.
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "ci-pass",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleResolveGateAlreadySatisfied(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// First resolve succeeds.
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, nil)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second resolve should fail.
	err = env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleResolveGateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

// --- Update gate tests ---

func TestHandleUpdateGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket":       "tkt-gated",
		"room":         "!room:bureau.local",
		"gate":         "ci-pass",
		"status":       "satisfied",
		"satisfied_by": "$pipeline-event-123",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var gate *ticket.TicketGate
	for i := range result.Content.Gates {
		if result.Content.Gates[i].ID == "ci-pass" {
			gate = &result.Content.Gates[i]
			break
		}
	}
	if gate == nil {
		t.Fatal("ci-pass gate not found in response")
	}
	if gate.Status != ticket.GateSatisfied {
		t.Errorf("gate status: got %q, want 'satisfied'", gate.Status)
	}
	if gate.SatisfiedBy != "$pipeline-event-123" {
		t.Errorf("satisfied_by: got %q, want '$pipeline-event-123'", gate.SatisfiedBy)
	}
}

func TestHandleUpdateGateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "nonexistent",
		"status": "satisfied",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateGateMissingStatus(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "ci-pass",
	}, nil)
	requireServiceError(t, err)
}

// --- Fine-grained grant enforcement tests ---
//
// These verify that close and reopen operations require dedicated
// grants (ticket/close, ticket/reopen) separate from ticket/update.
// A principal with ticket/update but not ticket/close cannot close
// tickets — this enables the asymmetric permission model where PMs
// close tickets and workers only update them.

func TestCloseRequiresCloseGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/close.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "close" {
		t.Errorf("action: got %q, want 'close'", serviceErr.Action)
	}
}

func TestReopenRequiresReopenGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/reopen.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "reopen" {
		t.Errorf("action: got %q, want 'reopen'", serviceErr.Action)
	}
}

func TestUpdateToClosedRequiresCloseGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/close. Closing via the
	// update action (status: "closed") should still be denied.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"status": "closed",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestUpdateFromClosedRequiresReopenGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/reopen. Reopening via
	// the update action (status: "open" on a closed ticket) should
	// still be denied.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"status": "open",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestCloseAllowedWithCloseGrant(t *testing.T) {
	// Token has both ticket/update and ticket/close. Close should work.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate, ticket.ActionClose}},
	})
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("close with ticket/close grant should succeed: %v", err)
	}
	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
}

func TestReopenAllowedWithReopenGrant(t *testing.T) {
	// Token has both ticket/update and ticket/reopen. Reopen should work.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate, ticket.ActionReopen}},
	})
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("reopen with ticket/reopen grant should succeed: %v", err)
	}
	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusOpen)
	}
}

func TestWildcardGrantCoversCloseAndReopen(t *testing.T) {
	// Token has ticket/* which should match ticket/close and
	// ticket/reopen. This verifies that the existing tests using
	// ticket/* continue to work with the new grant checks.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/*"}},
	})
	defer env.cleanup()

	ctx := context.Background()

	// Close should succeed.
	err := env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, nil)
	if err != nil {
		t.Fatalf("close with ticket/* grant should succeed: %v", err)
	}

	// Reopen should succeed (tkt-closed is already closed).
	err = env.client.Call(ctx, "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, nil)
	if err != nil {
		t.Fatalf("reopen with ticket/* grant should succeed: %v", err)
	}
}

// --- Pipeline ticket tests ---

// TestHandleCreatePipelineTicket verifies that creating a ticket with
// type "pipeline" generates a pip- prefixed ID and stores the
// PipelineExecutionContent.
func TestHandleCreatePipelineTicket(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "deploy staging",
		"type":     "pipeline",
		"priority": 1,
		"pipeline": map[string]any{
			"pipeline_ref": "pipelines/deploy",
			"total_steps":  3,
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Pipeline tickets must use the pip- prefix.
	if !strings.HasPrefix(result.ID, "pip-") {
		t.Errorf("expected pip- prefix, got ID %q", result.ID)
	}

	// Verify pipeline content is stored.
	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index", result.ID)
	}
	if content.Type != ticket.TypePipeline {
		t.Errorf("type: got %q, want %q", content.Type, ticket.TypePipeline)
	}
	if content.Pipeline == nil {
		t.Fatal("pipeline content is nil")
	}
	if content.Pipeline.PipelineRef != "pipelines/deploy" {
		t.Errorf("pipeline_ref: got %q, want 'pipelines/deploy'", content.Pipeline.PipelineRef)
	}
	if content.Pipeline.TotalSteps != 3 {
		t.Errorf("total_steps: got %d, want 3", content.Pipeline.TotalSteps)
	}
}

// TestHandleCreatePipelineMissingContent verifies that creating a
// pipeline-type ticket without PipelineExecutionContent is rejected.
func TestHandleCreatePipelineMissingContent(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "deploy staging",
		"type":     "pipeline",
		"priority": 1,
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "pipeline content is required") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleCreateNonPipelineWithPipelineContent verifies that creating
// a non-pipeline ticket with PipelineExecutionContent is rejected.
func TestHandleCreateNonPipelineWithPipelineContent(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "regular task",
		"type":     "task",
		"priority": 2,
		"pipeline": map[string]any{
			"pipeline_ref": "pipelines/deploy",
			"total_steps":  3,
		},
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "pipeline content must be nil") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleUpdatePipelineProgress verifies that pipeline-specific
// fields (current_step, current_step_name, conclusion) can be updated
// via the Pipeline field on the update request.
func TestHandleUpdatePipelineProgress(t *testing.T) {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"pip-deploy": {
			Version:  1,
			Title:    "deploy staging",
			Status:   ticket.StatusInProgress,
			Priority: 1,
			Type:     ticket.TypePipeline,
			Pipeline: &ticket.PipelineExecutionContent{
				PipelineRef: "pipelines/deploy",
				TotalSteps:  3,
			},
			Assignee:  ref.MustParseUserID("@agent/executor:bureau.local"),
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "pip-deploy",
		"room":   "!room:bureau.local",
		"pipeline": map[string]any{
			"pipeline_ref":      "pipelines/deploy",
			"total_steps":       3,
			"current_step":      2,
			"current_step_name": "run tests",
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Pipeline == nil {
		t.Fatal("pipeline content is nil in response")
	}
	if result.Content.Pipeline.CurrentStep != 2 {
		t.Errorf("current_step: got %d, want 2", result.Content.Pipeline.CurrentStep)
	}
	if result.Content.Pipeline.CurrentStepName != "run tests" {
		t.Errorf("current_step_name: got %q, want 'run tests'", result.Content.Pipeline.CurrentStepName)
	}
}

// TestHandleUpdateTypeFromPipelineAutoClearsPipelineContent verifies
// that changing a ticket's type away from "pipeline" auto-clears the
// Pipeline content, since the CBOR request format cannot express
// "set to nil".
func TestHandleUpdateTypeFromPipelineAutoClearsPipelineContent(t *testing.T) {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"pip-deploy": {
			Version:  1,
			Title:    "deploy staging",
			Status:   ticket.StatusOpen,
			Priority: 1,
			Type:     ticket.TypePipeline,
			Pipeline: &ticket.PipelineExecutionContent{
				PipelineRef: "pipelines/deploy",
				TotalSteps:  3,
				Conclusion:  pipeline.ConclusionFailure,
			},
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "pip-deploy",
		"room":   "!room:bureau.local",
		"type":   "task",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Type != ticket.TypeTask {
		t.Errorf("type: got %q, want %q", result.Content.Type, ticket.TypeTask)
	}
	if result.Content.Pipeline != nil {
		t.Error("pipeline content should be nil after changing type away from pipeline")
	}
}

// TestHandleUpdateTypeToPipelineRequiresContent verifies that changing
// a ticket's type to "pipeline" without providing Pipeline content in
// the same request is rejected by validation.
func TestHandleUpdateTypeToPipelineRequiresContent(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"type":   "pipeline",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "pipeline content is required") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleCreateAllowedTypesEnforced verifies that the room's
// AllowedTypes configuration restricts which ticket types can be
// created.
func TestHandleCreateAllowedTypesEnforced(t *testing.T) {
	room := newTrackedRoom(nil)
	room.config.AllowedTypes = []ticket.TicketType{ticket.TypeTask, ticket.TypeBug}
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	// Creating a "task" should succeed.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "allowed task",
		"type":     "task",
		"priority": 2,
	}, &result)
	if err != nil {
		t.Fatalf("creating allowed type: %v", err)
	}

	// Creating a "pipeline" should be rejected.
	err = env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "deploy staging",
		"type":     "pipeline",
		"priority": 1,
		"pipeline": map[string]any{
			"pipeline_ref": "pipelines/deploy",
			"total_steps":  3,
		},
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "not allowed") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleUpdateAllowedTypesEnforced verifies that AllowedTypes is
// checked when a ticket's type is changed via update.
func TestHandleUpdateAllowedTypesEnforced(t *testing.T) {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-task": {
			Version:   1,
			Title:     "some task",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})
	room.config.AllowedTypes = []ticket.TicketType{ticket.TypeTask, ticket.TypeBug}
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	// Changing type to "bug" (allowed) should succeed.
	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-task",
		"room":   "!room:bureau.local",
		"type":   "bug",
	}, &result)
	if err != nil {
		t.Fatalf("changing to allowed type: %v", err)
	}

	// Changing type to "feature" (not allowed) should be rejected.
	err = env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-task",
		"room":   "!room:bureau.local",
		"type":   "feature",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "not allowed") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleBatchCreatePipelineTickets verifies that batch-create
// generates pip- prefixed IDs for pipeline-type tickets and tkt-
// prefixed IDs for non-pipeline tickets in the same batch.
func TestHandleBatchCreatePipelineTickets(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result batchCreateResponse
	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{
				"ref":      "deploy",
				"title":    "deploy staging",
				"type":     "pipeline",
				"priority": 1,
				"pipeline": map[string]any{
					"pipeline_ref": "pipelines/deploy",
					"total_steps":  5,
				},
			},
			{
				"ref":      "task",
				"title":    "review deploy",
				"type":     "task",
				"priority": 2,
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	deployID := result.Refs["deploy"]
	taskID := result.Refs["task"]

	if !strings.HasPrefix(deployID, "pip-") {
		t.Errorf("pipeline ticket should have pip- prefix, got %q", deployID)
	}
	if !strings.HasPrefix(taskID, "tkt-") {
		t.Errorf("task ticket should have tkt- prefix, got %q", taskID)
	}

	// Verify pipeline content was stored.
	index := env.service.rooms[testRoomID("!room:bureau.local")].index
	content, exists := index.Get(deployID)
	if !exists {
		t.Fatalf("pipeline ticket %s not in index", deployID)
	}
	if content.Pipeline == nil {
		t.Fatal("pipeline content is nil")
	}
	if content.Pipeline.PipelineRef != "pipelines/deploy" {
		t.Errorf("pipeline_ref: got %q", content.Pipeline.PipelineRef)
	}
	if content.Pipeline.TotalSteps != 5 {
		t.Errorf("total_steps: got %d, want 5", content.Pipeline.TotalSteps)
	}
}

// --- Add-note tests ---

// TestHandleAddNote verifies that adding a note produces the correct
// note ID, author, body, and timestamps.
func TestHandleAddNote(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"body":   "step 1/3: clone-repository... ok (2.1s)",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-open" {
		t.Errorf("id: got %q, want 'tkt-open'", result.ID)
	}
	if len(result.Content.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(result.Content.Notes))
	}

	note := result.Content.Notes[0]
	if note.ID != "n-1" {
		t.Errorf("note id: got %q, want 'n-1'", note.ID)
	}
	if note.Author != ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local") {
		t.Errorf("note author: got %q", note.Author)
	}
	if note.Body != "step 1/3: clone-repository... ok (2.1s)" {
		t.Errorf("note body: got %q", note.Body)
	}
	if note.CreatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("note created_at: got %q, want '2026-01-15T12:00:00Z'", note.CreatedAt)
	}
	if result.Content.UpdatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("updated_at: got %q, want '2026-01-15T12:00:00Z'", result.Content.UpdatedAt)
	}

	// Verify state event was written to Matrix.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 1 {
		t.Fatalf("expected 1 written event, got %d", len(env.writer.events))
	}
}

// TestHandleAddNoteSequentialIDs verifies that successive notes on the
// same ticket get sequential IDs (n-1, n-2).
func TestHandleAddNoteSequentialIDs(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()

	var firstResult mutationResponse
	err := env.client.Call(ctx, "add-note", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"body":   "first note",
	}, &firstResult)
	if err != nil {
		t.Fatalf("first add-note: %v", err)
	}

	var secondResult mutationResponse
	err = env.client.Call(ctx, "add-note", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"body":   "second note",
	}, &secondResult)
	if err != nil {
		t.Fatalf("second add-note: %v", err)
	}

	if len(secondResult.Content.Notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(secondResult.Content.Notes))
	}
	if secondResult.Content.Notes[0].ID != "n-1" {
		t.Errorf("first note id: got %q, want 'n-1'", secondResult.Content.Notes[0].ID)
	}
	if secondResult.Content.Notes[1].ID != "n-2" {
		t.Errorf("second note id: got %q, want 'n-2'", secondResult.Content.Notes[1].ID)
	}
	if secondResult.Content.Notes[0].Body != "first note" {
		t.Errorf("first note body: got %q", secondResult.Content.Notes[0].Body)
	}
	if secondResult.Content.Notes[1].Body != "second note" {
		t.Errorf("second note body: got %q", secondResult.Content.Notes[1].Body)
	}
}

// TestHandleAddNoteWithExistingNotes verifies that notes are appended
// correctly when the ticket already has notes, with IDs continuing
// from the existing count.
func TestHandleAddNoteWithExistingNotes(t *testing.T) {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-noted": {
			Version:  1,
			Title:    "ticket with notes",
			Status:   ticket.StatusOpen,
			Priority: 2,
			Type:     ticket.TypeTask,
			Notes: []ticket.TicketNote{
				{
					ID:        "n-1",
					Author:    ref.MustParseUserID("@agent/prior:bureau.local"),
					Body:      "existing note",
					CreatedAt: "2026-01-10T00:00:00Z",
				},
				{
					ID:        "n-2",
					Author:    ref.MustParseUserID("@agent/prior:bureau.local"),
					Body:      "another existing note",
					CreatedAt: "2026-01-11T00:00:00Z",
				},
			},
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-11T00:00:00Z",
		},
	})
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"ticket": "tkt-noted",
		"room":   "!room:bureau.local",
		"body":   "new note after existing ones",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Content.Notes) != 3 {
		t.Fatalf("expected 3 notes, got %d", len(result.Content.Notes))
	}

	newNote := result.Content.Notes[2]
	if newNote.ID != "n-3" {
		t.Errorf("new note id: got %q, want 'n-3'", newNote.ID)
	}
	if newNote.Body != "new note after existing ones" {
		t.Errorf("new note body: got %q", newNote.Body)
	}
}

// TestHandleAddNoteMissingBody verifies that an empty body is rejected.
func TestHandleAddNoteMissingBody(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"body":   "",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "body") {
		t.Errorf("expected error about body, got: %s", serviceErr.Message)
	}
}

// TestHandleAddNoteNotFound verifies that adding a note to a
// nonexistent ticket returns an error.
func TestHandleAddNoteNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"ticket": "nonexistent",
		"room":   "!room:bureau.local",
		"body":   "orphan note",
	}, nil)
	requireServiceError(t, err)
}

// TestHandleAddNoteMissingTicket verifies that a missing ticket field
// is rejected.
func TestHandleAddNoteMissingTicket(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"room": "!room:bureau.local",
		"body": "no ticket specified",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "ticket") {
		t.Errorf("expected error about ticket, got: %s", serviceErr.Message)
	}
}

// TestHandleBatchCreateAllowedTypesEnforced verifies that AllowedTypes
// is checked per-entry in a batch create. If any entry has a
// disallowed type, the entire batch is rejected.
func TestHandleBatchCreateAllowedTypesEnforced(t *testing.T) {
	room := newTrackedRoom(nil)
	room.config.AllowedTypes = []ticket.TicketType{ticket.TypeTask}
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "ok task", "type": "task", "priority": 2},
			{"ref": "b", "title": "bad pipeline", "type": "pipeline", "priority": 1, "pipeline": map[string]any{
				"pipeline_ref": "pipelines/test",
				"total_steps":  1,
			}},
		},
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "not allowed") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// --- Review status transition tests ---

// TestHandleUpdateInProgressToReview verifies that an in_progress ticket
// can transition to review when reviewers are provided, and that the
// assignee (author) is preserved.
func TestHandleUpdateInProgressToReview(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"status": "review",
		"review": map[string]any{
			"reviewers": []map[string]any{
				{"user_id": "@agent/reviewer:bureau.local", "disposition": "pending"},
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusReview {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusReview)
	}
	// Assignee persists from in_progress to review.
	if result.Content.Assignee != ref.MustParseUserID("@agent/worker:bureau.local") {
		t.Errorf("assignee should be preserved, got %q", result.Content.Assignee)
	}
	if result.Content.Review == nil || len(result.Content.Review.Reviewers) != 1 {
		t.Fatal("review should have 1 reviewer")
	}
}

// TestHandleUpdateReviewToInProgress verifies that a ticket in review
// can go back to in_progress (author iterating), with assignee preserved.
func TestHandleUpdateReviewToInProgress(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-review",
		"room":   "!room:bureau.local",
		"status": "in_progress",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusInProgress {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusInProgress)
	}
	// Assignee persists from review to in_progress.
	if result.Content.Assignee != ref.MustParseUserID("@agent/worker:bureau.local") {
		t.Errorf("assignee should be preserved, got %q", result.Content.Assignee)
	}
}

// TestHandleUpdateReviewToOpen verifies that a ticket in review can be
// released back to the pool, clearing the assignee.
func TestHandleUpdateReviewToOpen(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-review",
		"room":   "!room:bureau.local",
		"status": "open",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusOpen {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusOpen)
	}
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be cleared on review → open, got %q", result.Content.Assignee)
	}
}

// TestHandleUpdateReviewToClosed verifies that a reviewed ticket can
// be closed directly (approved and done).
func TestHandleUpdateReviewToClosed(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-review",
		"room":   "!room:bureau.local",
		"status": "closed",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be set")
	}
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be cleared on review → closed, got %q", result.Content.Assignee)
	}
}

// TestHandleUpdateReviewToBlocked verifies that a ticket in review can
// be blocked by an external dependency.
func TestHandleUpdateReviewToBlocked(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-review",
		"room":   "!room:bureau.local",
		"status": "blocked",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusBlocked {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusBlocked)
	}
}

// TestHandleUpdateReviewContention verifies that review → review is
// rejected as contention (same as in_progress → in_progress).
func TestHandleUpdateReviewContention(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-review",
		"room":   "!room:bureau.local",
		"status": "review",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "already in review") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleUpdateOpenToReview verifies the direct open → review
// transition (PM use case: no prior assignee).
func TestHandleUpdateOpenToReview(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"status": "review",
		"review": map[string]any{
			"reviewers": []map[string]any{
				{"user_id": "@agent/reviewer:bureau.local", "disposition": "pending"},
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusReview {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusReview)
	}
	// Open → review: no assignee required or preserved.
	if !result.Content.Assignee.IsZero() {
		t.Errorf("assignee should be empty for open → review, got %q", result.Content.Assignee)
	}
}

// TestHandleUpdateBlockedToReview verifies that a blocked ticket can
// resume directly into review.
func TestHandleUpdateBlockedToReview(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-blocked",
		"room":   "!room:bureau.local",
		"status": "review",
		"review": map[string]any{
			"reviewers": []map[string]any{
				{"user_id": "@agent/reviewer:bureau.local", "disposition": "pending"},
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusReview {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusReview)
	}
}

// TestHandleUpdateClosedToReviewRejected verifies that closed → review
// is not allowed (must reopen first via closed → open).
func TestHandleUpdateClosedToReviewRejected(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"status": "review",
		"review": map[string]any{
			"reviewers": []map[string]any{
				{"user_id": "@agent/reviewer:bureau.local", "disposition": "pending"},
			},
		},
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "invalid status transition") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleUpdateReviewRequiresReviewers verifies that entering review
// status without reviewers is rejected.
func TestHandleUpdateReviewRequiresReviewers(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// No review field at all.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"status": "review",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "reviewers are required") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleUpdateReviewAssigneeOnReview verifies that an assignee can
// be set on a review ticket (author set explicitly).
func TestHandleUpdateReviewAssigneeOnReview(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// First transition open → review with an assignee.
	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-open",
		"room":     "!room:bureau.local",
		"status":   "review",
		"assignee": "@agent/author:bureau.local",
		"review": map[string]any{
			"reviewers": []map[string]any{
				{"user_id": "@agent/reviewer:bureau.local", "disposition": "pending"},
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != ticket.StatusReview {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusReview)
	}
	if result.Content.Assignee != ref.MustParseUserID("@agent/author:bureau.local") {
		t.Errorf("assignee: got %q, want @agent/author:bureau.local", result.Content.Assignee)
	}
}

// --- Set-disposition tests ---

// TestHandleSetDisposition verifies the basic set-disposition flow:
// caller is in the reviewer list, ticket is in review, disposition is
// updated.
func TestHandleSetDisposition(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// The test token's subject is @bureau/fleet/prod/agent/tester:bureau.local,
	// which is in tkt-review's reviewer list.
	var result mutationResponse
	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"ticket":      "tkt-review",
		"room":        "!room:bureau.local",
		"disposition": "approved",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Review == nil {
		t.Fatal("review should not be nil")
	}
	// Find the tester's entry.
	testerID := ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local")
	found := false
	for _, reviewer := range result.Content.Review.Reviewers {
		if reviewer.UserID == testerID {
			found = true
			if reviewer.Disposition != ticket.DispositionApproved {
				t.Errorf("disposition: got %q, want approved", reviewer.Disposition)
			}
			if reviewer.UpdatedAt == "" {
				t.Error("updated_at should be set after set-disposition")
			}
		}
	}
	if !found {
		t.Error("tester reviewer entry not found in result")
	}
}

// TestHandleSetDispositionNotInReview verifies that set-disposition
// is rejected when the ticket is not in review status.
func TestHandleSetDispositionNotInReview(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"ticket":      "tkt-inprog",
		"room":        "!room:bureau.local",
		"disposition": "approved",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "not in review status") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleSetDispositionNotReviewer verifies that set-disposition is
// rejected when the caller is not in the reviewer list.
func TestHandleSetDispositionNotReviewer(t *testing.T) {
	// The test token's subject is @bureau/fleet/prod/agent/tester:bureau.local.
	// Create a review ticket where the tester is NOT a reviewer.
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-review-other": {
			Version:  ticket.TicketContentVersion,
			Title:    "review without tester",
			Status:   ticket.StatusReview,
			Priority: 2,
			Type:     ticket.TypeTask,
			Review: &ticket.TicketReview{
				Reviewers: []ticket.ReviewerEntry{
					{
						UserID:      ref.MustParseUserID("@agent/other-reviewer:bureau.local"),
						Disposition: ticket.DispositionPending,
					},
				},
			},
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"ticket":      "tkt-review-other",
		"room":        "!room:bureau.local",
		"disposition": "approved",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "not in the reviewer list") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleSetDispositionPendingRejected verifies that setting
// disposition to "pending" is rejected (it's the initial state).
func TestHandleSetDispositionPendingRejected(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"ticket":      "tkt-review",
		"room":        "!room:bureau.local",
		"disposition": "pending",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "cannot set disposition to pending") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// TestHandleSetDispositionInvalidDisposition verifies that an invalid
// disposition value is rejected.
func TestHandleSetDispositionInvalidDisposition(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"ticket":      "tkt-review",
		"room":        "!room:bureau.local",
		"disposition": "lgtm",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "invalid disposition") {
		t.Errorf("unexpected error: %s", serviceErr.Message)
	}
}

// --- Attachment tests ---

func TestHandleAddAttachment(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "add-attachment", map[string]any{
		"ticket":       "tkt-open",
		"room":         "!room:bureau.local",
		"ref":          "art-abc123def456",
		"label":        "build output",
		"content_type": "application/octet-stream",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-open" {
		t.Errorf("id: got %q, want 'tkt-open'", result.ID)
	}
	if len(result.Content.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(result.Content.Attachments))
	}

	attachment := result.Content.Attachments[0]
	if attachment.Ref != "art-abc123def456" {
		t.Errorf("ref: got %q, want 'art-abc123def456'", attachment.Ref)
	}
	if attachment.Label != "build output" {
		t.Errorf("label: got %q, want 'build output'", attachment.Label)
	}
	if attachment.ContentType != "application/octet-stream" {
		t.Errorf("content_type: got %q, want 'application/octet-stream'", attachment.ContentType)
	}
	if result.Content.UpdatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("updated_at: got %q, want '2026-01-15T12:00:00Z'", result.Content.UpdatedAt)
	}

	// Verify state event was written to Matrix.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 1 {
		t.Fatalf("expected 1 written event, got %d", len(env.writer.events))
	}
}

func TestHandleAddAttachmentUpsert(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()

	// First attach.
	err := env.client.Call(ctx, "add-attachment", map[string]any{
		"ticket":       "tkt-open",
		"room":         "!room:bureau.local",
		"ref":          "art-abc123def456",
		"label":        "original label",
		"content_type": "text/plain",
	}, nil)
	if err != nil {
		t.Fatalf("first add-attachment: %v", err)
	}

	// Upsert same ref with different label and content type.
	var result mutationResponse
	err = env.client.Call(ctx, "add-attachment", map[string]any{
		"ticket":       "tkt-open",
		"room":         "!room:bureau.local",
		"ref":          "art-abc123def456",
		"label":        "updated label",
		"content_type": "application/json",
	}, &result)
	if err != nil {
		t.Fatalf("upsert add-attachment: %v", err)
	}

	// Should still be exactly 1 attachment, not 2.
	if len(result.Content.Attachments) != 1 {
		t.Fatalf("expected 1 attachment after upsert, got %d", len(result.Content.Attachments))
	}
	if result.Content.Attachments[0].Label != "updated label" {
		t.Errorf("label: got %q, want 'updated label'", result.Content.Attachments[0].Label)
	}
	if result.Content.Attachments[0].ContentType != "application/json" {
		t.Errorf("content_type: got %q, want 'application/json'", result.Content.Attachments[0].ContentType)
	}
}

func TestHandleAddAttachmentMXCRejected(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "add-attachment", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"ref":    "mxc://bureau.local/abc123",
		"label":  "screenshot",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "mxc://") {
		t.Errorf("expected MXC rejection error, got: %s", serviceErr.Message)
	}
}

func TestHandleRemoveAttachment(t *testing.T) {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-attached": {
			Version:  1,
			Title:    "ticket with attachment",
			Status:   ticket.StatusOpen,
			Priority: 2,
			Type:     ticket.TypeTask,
			Attachments: []ticket.TicketAttachment{
				{
					Ref:         "art-aaa111",
					Label:       "first artifact",
					ContentType: "text/plain",
				},
				{
					Ref:         "art-bbb222",
					Label:       "second artifact",
					ContentType: "application/json",
				},
			},
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})
	rooms := map[ref.RoomID]*roomState{
		ref.MustParseRoomID("!room:bureau.local"): room,
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "remove-attachment", map[string]any{
		"ticket": "tkt-attached",
		"room":   "!room:bureau.local",
		"ref":    "art-aaa111",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Content.Attachments) != 1 {
		t.Fatalf("expected 1 remaining attachment, got %d", len(result.Content.Attachments))
	}
	if result.Content.Attachments[0].Ref != "art-bbb222" {
		t.Errorf("remaining attachment ref: got %q, want 'art-bbb222'", result.Content.Attachments[0].Ref)
	}
}

func TestHandleRemoveAttachmentNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "remove-attachment", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"ref":    "art-nonexistent",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "no attachment with ref") {
		t.Errorf("expected not-found error, got: %s", serviceErr.Message)
	}
}

func TestHandleAddAttachmentMissingFields(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()

	// Missing ticket.
	err := env.client.Call(ctx, "add-attachment", map[string]any{
		"room": "!room:bureau.local",
		"ref":  "art-abc123",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "ticket") {
		t.Errorf("expected missing ticket error, got: %s", serviceErr.Message)
	}

	// Missing ref.
	err = env.client.Call(ctx, "add-attachment", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
	}, nil)
	serviceErr = requireServiceError(t, err)
	if !strings.Contains(serviceErr.Message, "ref") {
		t.Errorf("expected missing ref error, got: %s", serviceErr.Message)
	}
}

func TestHandleAddAttachmentDenied(t *testing.T) {
	// Token has ticket/update but NOT ticket/attach.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{ticket.ActionUpdate}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "add-attachment", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"ref":    "art-abc123",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "add-attachment" {
		t.Errorf("action: got %q, want 'add-attachment'", serviceErr.Action)
	}
}

// --- Context ID on mutations ---

func TestHandleCreateWithContextID(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":       "!room:bureau.local",
		"title":      "task with context",
		"type":       "task",
		"priority":   2,
		"context_id": "ctx-abc12345",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}
	if content.ContextID != "ctx-abc12345" {
		t.Errorf("context_id: got %q, want %q", content.ContextID, "ctx-abc12345")
	}
}

func TestHandleUpdateWithContextID(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":     "tkt-open",
		"room":       "!room:bureau.local",
		"context_id": "ctx-update-001",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.ContextID != "ctx-update-001" {
		t.Errorf("context_id: got %q, want %q", result.Content.ContextID, "ctx-update-001")
	}

	// Verify a second update replaces the context_id.
	err = env.client.Call(context.Background(), "update", map[string]any{
		"ticket":     "tkt-open",
		"room":       "!room:bureau.local",
		"context_id": "ctx-update-002",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Content.ContextID != "ctx-update-002" {
		t.Errorf("context_id after second update: got %q, want %q", result.Content.ContextID, "ctx-update-002")
	}
}

func TestHandleCloseWithContextID(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket":     "tkt-open",
		"room":       "!room:bureau.local",
		"reason":     "done",
		"context_id": "ctx-close-001",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.ContextID != "ctx-close-001" {
		t.Errorf("context_id: got %q, want %q", result.Content.ContextID, "ctx-close-001")
	}
	if result.Content.Status != ticket.StatusClosed {
		t.Errorf("status: got %q, want %q", result.Content.Status, ticket.StatusClosed)
	}
}

func TestHandleAddNoteWithContextID(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "add-note", map[string]any{
		"ticket":     "tkt-open",
		"room":       "!room:bureau.local",
		"body":       "note with context",
		"context_id": "ctx-note-001",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Content.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(result.Content.Notes))
	}
	note := result.Content.Notes[0]
	if note.ContextID != "ctx-note-001" {
		t.Errorf("note context_id: got %q, want %q", note.ContextID, "ctx-note-001")
	}
}
