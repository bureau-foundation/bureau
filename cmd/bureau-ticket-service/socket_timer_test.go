// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func TestHandleCreateWithSchedule(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "daily health check",
		"type":     "chore",
		"priority": 3,
		"schedule": "0 7 * * *",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(content.Gates))
	}
	gate := content.Gates[0]
	if gate.ID != "schedule" {
		t.Errorf("gate ID: got %q, want 'schedule'", gate.ID)
	}
	if gate.Type != "timer" {
		t.Errorf("gate Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Schedule != "0 7 * * *" {
		t.Errorf("gate Schedule: got %q, want '0 7 * * *'", gate.Schedule)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z. Next 7am is 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithInterval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "rotate API keys",
		"type":     "chore",
		"priority": 3,
		"interval": "720h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(content.Gates))
	}
	gate := content.Gates[0]
	if gate.ID != "interval" {
		t.Errorf("gate ID: got %q, want 'interval'", gate.ID)
	}
	if gate.Interval != "720h" {
		t.Errorf("gate Interval: got %q, want '720h'", gate.Interval)
	}
	// testClockEpoch + 720h = 2026-01-15T12:00:00Z + 30 days = 2026-02-14T12:00:00Z.
	if gate.Target != "2026-02-14T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-02-14T12:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithScheduleAndExistingGates(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Create with both an explicit gate and a convenience schedule.
	// The schedule gate should be appended alongside the explicit gate.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "reviewed health check",
		"type":     "chore",
		"priority": 2,
		"schedule": "0 7 * * *",
		"gates": []map[string]any{
			{
				"id":     "approval",
				"type":   "human",
				"status": "pending",
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 2 {
		t.Fatalf("expected 2 gates (explicit + schedule), got %d", len(content.Gates))
	}
	if content.Gates[0].ID != "approval" {
		t.Errorf("first gate ID: got %q, want 'approval'", content.Gates[0].ID)
	}
	if content.Gates[1].ID != "schedule" {
		t.Errorf("second gate ID: got %q, want 'schedule'", content.Gates[1].ID)
	}
}

func TestHandleCreateScheduleAndIntervalMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad request",
		"type":     "task",
		"priority": 2,
		"schedule": "0 7 * * *",
		"interval": "4h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive schedule and interval")
	}
}

func TestHandleCreateInvalidSchedule(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad schedule",
		"type":     "task",
		"priority": 2,
		"schedule": "not-a-cron",
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestHandleCreateInvalidInterval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad interval",
		"type":     "task",
		"priority": 2,
		"interval": "not-a-duration",
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// --- handleDefer tests ---

func TestHandleDeferWithFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "24h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-open" {
		t.Errorf("ID: got %q, want 'tkt-open'", result.ID)
	}

	// Verify the defer gate was created.
	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := result.Content.Gates[gateIndex]
	if gate.Type != "timer" {
		t.Errorf("gate Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch + 24h = 2026-01-16T12:00:00Z.
	if gate.Target != "2026-01-16T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T12:00:00Z'", gate.Target)
	}
}

func TestHandleDeferWithUntil(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2026-02-01T09:00:00Z",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := result.Content.Gates[gateIndex]
	if gate.Target != "2026-02-01T09:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-02-01T09:00:00Z'", gate.Target)
	}
}

func TestHandleDeferUpdatesExistingGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()

	// First defer.
	err := env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "1h",
	}, nil)
	if err != nil {
		t.Fatalf("first defer: %v", err)
	}

	// Second defer should update the same gate, not create a second one.
	var result mutationResponse
	err = env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "48h",
	}, &result)
	if err != nil {
		t.Fatalf("second defer: %v", err)
	}

	// Should still be exactly one defer gate.
	deferCount := 0
	for _, gate := range result.Content.Gates {
		if gate.ID == "defer" {
			deferCount++
		}
	}
	if deferCount != 1 {
		t.Fatalf("expected exactly 1 defer gate, got %d", deferCount)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	gate := result.Content.Gates[gateIndex]
	// testClockEpoch + 48h = 2026-01-17T12:00:00Z.
	if gate.Target != "2026-01-17T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-17T12:00:00Z'", gate.Target)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
}

func TestHandleDeferRequiresUntilOrFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
	}, nil)
	if err == nil {
		t.Fatal("expected error when neither until nor for is provided")
	}
}

func TestHandleDeferUntilAndForMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2026-02-01T09:00:00Z",
		"for":    "24h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive until and for")
	}
}

func TestHandleDeferUntilInThePast(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2025-01-01T00:00:00Z",
	}, nil)
	if err == nil {
		t.Fatal("expected error for until time in the past")
	}
}

func TestHandleDeferClosedTicketSucceeds(t *testing.T) {
	// Deferring a closed ticket is allowed — the defer gate is added
	// and becomes relevant when the ticket is reopened. This avoids
	// forcing callers to reopen before deferring.
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"for":    "24h",
	}, &result)
	if err != nil {
		t.Fatalf("defer on closed ticket should succeed: %v", err)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on closed ticket")
	}
}

func TestHandleCreateWithDeferFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":      "!room:bureau.local",
		"title":     "deferred task",
		"type":      "task",
		"priority":  3,
		"defer_for": "24h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	gateIndex := findGate(content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := content.Gates[gateIndex]
	// testClockEpoch + 24h = 2026-01-16T12:00:00Z.
	if gate.Target != "2026-01-16T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T12:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithDeferUntilAndDeferForMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":        "!room:bureau.local",
		"title":       "bad request",
		"type":        "task",
		"priority":    2,
		"defer_until": "2026-02-01T00:00:00Z",
		"defer_for":   "24h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive defer_until and defer_for")
	}
}

// --- upcoming-gates tests ---

func TestHandleUpcomingGates(t *testing.T) {
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room1:bureau.local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-timer1": {
				Version:   1,
				Title:     "scheduled check",
				Status:    "open",
				Priority:  2,
				Type:      "chore",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{
						ID:       "schedule",
						Type:     "timer",
						Status:   "pending",
						Schedule: "0 7 * * *",
						Target:   "2026-01-16T07:00:00Z",
					},
				},
			},
			"tkt-timer2": {
				Version:   1,
				Title:     "interval poll",
				Status:    "open",
				Priority:  3,
				Type:      "chore",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{
						ID:       "interval",
						Type:     "timer",
						Status:   "pending",
						Interval: "4h",
						Target:   "2026-01-15T16:00:00Z",
					},
				},
			},
			"tkt-satisfied": {
				Version:   1,
				Title:     "already fired",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{
						ID:          "done",
						Type:        "timer",
						Status:      "satisfied",
						Target:      "2026-01-14T00:00:00Z",
						SatisfiedAt: "2026-01-14T00:00:00Z",
					},
				},
			},
		}),
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Should return 2 entries (timer1 and timer2). The satisfied gate
	// on tkt-satisfied should be excluded.
	if len(results) != 2 {
		t.Fatalf("expected 2 upcoming gates, got %d", len(results))
	}

	// Results should be sorted by target time: timer2 (16:00) before timer1 (07:00 next day).
	if results[0].TicketID != "tkt-timer2" {
		t.Errorf("first entry: got %q, want 'tkt-timer2'", results[0].TicketID)
	}
	if results[1].TicketID != "tkt-timer1" {
		t.Errorf("second entry: got %q, want 'tkt-timer1'", results[1].TicketID)
	}

	// Check metadata.
	if results[0].GateID != "interval" {
		t.Errorf("first gate ID: got %q, want 'interval'", results[0].GateID)
	}
	if results[0].Interval != "4h" {
		t.Errorf("first interval: got %q, want '4h'", results[0].Interval)
	}
	if results[0].Title != "interval poll" {
		t.Errorf("first title: got %q, want 'interval poll'", results[0].Title)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z, target is 2026-01-15T16:00:00Z = 4h ahead.
	if results[0].UntilFire != "4h" {
		t.Errorf("first until_fire: got %q, want '4h'", results[0].UntilFire)
	}

	if results[1].Schedule != "0 7 * * *" {
		t.Errorf("second schedule: got %q, want '0 7 * * *'", results[1].Schedule)
	}
	// 2026-01-16T07:00:00Z - 2026-01-15T12:00:00Z = 19h.
	if results[1].UntilFire != "19h" {
		t.Errorf("second until_fire: got %q, want '19h'", results[1].UntilFire)
	}
}

func TestHandleUpcomingGatesRoomFilter(t *testing.T) {
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room1:bureau.local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-a": {
				Version: 1, Title: "room1 task", Status: "open",
				Priority: 2, Type: "chore",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{ID: "t", Type: "timer", Status: "pending", Target: "2026-01-16T00:00:00Z"},
				},
			},
		}),
		testRoomID("!room2:bureau.local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-b": {
				Version: 1, Title: "room2 task", Status: "open",
				Priority: 2, Type: "chore",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{ID: "t", Type: "timer", Status: "pending", Target: "2026-01-17T00:00:00Z"},
				},
			},
		}),
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{
		"room": "!room1:bureau.local",
	}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 gate from room1, got %d", len(results))
	}
	if results[0].TicketID != "tkt-a" {
		t.Errorf("ticket ID: got %q, want 'tkt-a'", results[0].TicketID)
	}
}

func TestHandleUpcomingGatesEmpty(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// mutationRooms has a tkt-gated ticket but its gates are human/pipeline type, not timer.
	if len(results) != 0 {
		t.Errorf("expected 0 upcoming gates from mutationRooms, got %d", len(results))
	}
}

// --- Timer lifecycle tests ---

// waitForGateStatus blocks until the named gate on the ticket reaches
// the expected status. It synchronizes with the timer loop goroutine
// by waiting for fakeWriter write notifications — each notification
// means a state event was written, so the test checks whether the
// write produced the expected gate status. If the gate never reaches
// the expected status, Go's test timeout catches the hang.
func waitForGateStatus(t *testing.T, env *testEnv, room, ticketID, gateID, expectedStatus string) ticket.TicketContent {
	t.Helper()

	for {
		<-env.writer.notify

		var result mutationResponse
		err := env.client.Call(context.Background(), "show", map[string]any{
			"ticket": ticketID,
			"room":   room,
		}, &result)
		if err != nil {
			t.Fatalf("show %s: %v", ticketID, err)
		}

		for _, gate := range result.Content.Gates {
			if gate.ID == gateID && gate.Status == expectedStatus {
				return result.Content
			}
		}
	}
}

func TestTimerLifecycleScheduleFireAndRearm(t *testing.T) {
	// Full lifecycle: create ticket with schedule → gate fires →
	// close ticket → auto-rearm → gate fires again → end-recurrence.
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Step 1: Create a ticket with a cron schedule (daily at 7am UTC).
	// testClockEpoch is 2026-01-15T12:00:00Z, so first target is
	// 2026-01-16T07:00:00Z (19 hours from now).
	var createResult createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "daily check",
		"type":     "chore",
		"priority": 3,
		"schedule": "0 7 * * *",
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ticketID := createResult.ID

	// Verify the initial gate state.
	var showResult mutationResponse
	err = env.client.Call(ctx, "show", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &showResult)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if len(showResult.Content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(showResult.Content.Gates))
	}
	gate := showResult.Content.Gates[0]
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Fatalf("initial target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}

	// Step 2: Advance clock past the first target. The timer loop
	// should fire the gate.
	env.clock.Advance(20 * time.Hour) // Now: 2026-01-16T08:00:00Z

	content := waitForGateStatus(t, env, room, ticketID, "schedule", "satisfied")
	if content.Gates[0].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[0].SatisfiedBy)
	}

	// Step 3: Close the ticket. Since it has a recurring schedule gate,
	// it should auto-rearm: reopen as "open" with a new target.
	var closeResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// After rearm, status should be "open" (not "closed").
	if closeResult.Content.Status != "open" {
		t.Fatalf("status after rearm: got %q, want 'open'", closeResult.Content.Status)
	}
	rearmedGate := closeResult.Content.Gates[0]
	if rearmedGate.Status != "pending" {
		t.Errorf("gate status after rearm: got %q, want 'pending'", rearmedGate.Status)
	}
	if rearmedGate.FireCount != 1 {
		t.Errorf("fire_count after rearm: got %d, want 1", rearmedGate.FireCount)
	}
	// Next 7am after 2026-01-16T08:00:00Z is 2026-01-17T07:00:00Z.
	if rearmedGate.Target != "2026-01-17T07:00:00Z" {
		t.Errorf("rearmed target: got %q, want '2026-01-17T07:00:00Z'", rearmedGate.Target)
	}

	// Step 4: Advance to the second occurrence and verify it fires again.
	env.clock.Advance(24 * time.Hour) // Now: 2026-01-17T08:00:00Z

	waitForGateStatus(t, env, room, ticketID, "schedule", "satisfied")

	// Step 5: Close with --end-recurrence to permanently close.
	// Use a fresh result variable to avoid stale slice values from
	// the previous close response (CBOR omitempty skips empty slices,
	// so decoding into a pre-used struct leaves old values).
	var endResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket":         ticketID,
		"room":           room,
		"end_recurrence": true,
		"reason":         "no longer needed",
	}, &endResult)
	if err != nil {
		t.Fatalf("close with end_recurrence: %v", err)
	}
	if endResult.Content.Status != "closed" {
		t.Errorf("status after end-recurrence: got %q, want 'closed'", endResult.Content.Status)
	}
	// Recurring gates should be stripped.
	for _, gate := range endResult.Content.Gates {
		if gate.Schedule != "" || gate.Interval != "" {
			t.Errorf("recurring gate %q was not stripped", gate.ID)
		}
	}
}

func TestTimerLifecycleIntervalFireAndRearm(t *testing.T) {
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Create a ticket with a 4-hour interval.
	// testClockEpoch is 2026-01-15T12:00:00Z, target: 16:00:00Z.
	var createResult createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "periodic poll",
		"type":     "chore",
		"priority": 3,
		"interval": "4h",
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ticketID := createResult.ID

	// Advance past the first target.
	env.clock.Advance(5 * time.Hour) // Now: 2026-01-15T17:00:00Z

	content := waitForGateStatus(t, env, room, ticketID, "interval", "satisfied")
	if content.Gates[0].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[0].SatisfiedBy)
	}

	// Close the ticket. Interval rearm: next target is now + 4h.
	var closeResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	if closeResult.Content.Status != "open" {
		t.Fatalf("status after rearm: got %q, want 'open'", closeResult.Content.Status)
	}
	rearmedGate := closeResult.Content.Gates[0]
	// Rearm target: 2026-01-15T17:00:00Z + 4h = 2026-01-15T21:00:00Z.
	if rearmedGate.Target != "2026-01-15T21:00:00Z" {
		t.Errorf("rearmed target: got %q, want '2026-01-15T21:00:00Z'", rearmedGate.Target)
	}
}

func TestTimerLifecycleMaxOccurrences(t *testing.T) {
	// A recurring gate with max_occurrences=2 should stop rearming
	// after 2 fires.
	rooms := map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-max": {
				Version:   1,
				Title:     "limited recurring",
				Status:    "open",
				Priority:  2,
				Type:      "chore",
				CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []ticket.TicketGate{
					{
						ID:             "interval",
						Type:           "timer",
						Status:         "pending",
						Interval:       "1h",
						Target:         "2026-01-15T13:00:00Z",
						MaxOccurrences: 2,
						CreatedAt:      "2026-01-01T00:00:00Z",
					},
				},
			},
		}),
	}

	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Fire the first occurrence.
	env.clock.Advance(2 * time.Hour) // Now: 2026-01-15T14:00:00Z
	waitForGateStatus(t, env, room, "tkt-max", "interval", "satisfied")

	// Close — should rearm (fire_count becomes 1, max is 2).
	var closeResult mutationResponse
	err := env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-max",
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	if closeResult.Content.Status != "open" {
		t.Fatalf("expected rearm after first fire, got status %q", closeResult.Content.Status)
	}

	// Fire the second occurrence.
	env.clock.Advance(2 * time.Hour) // Now: 2026-01-15T16:00:00Z
	waitForGateStatus(t, env, room, "tkt-max", "interval", "satisfied")

	// Close — should NOT rearm (fire_count becomes 2 == max_occurrences).
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-max",
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if closeResult.Content.Status != "closed" {
		t.Fatalf("expected closed after max occurrences, got status %q", closeResult.Content.Status)
	}
}

func TestTimerLifecycleDeferCreateAndFire(t *testing.T) {
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Defer tkt-open for 2 hours.
	var deferResult mutationResponse
	err := env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   room,
		"for":    "2h",
	}, &deferResult)
	if err != nil {
		t.Fatalf("defer: %v", err)
	}

	// Verify the gate is pending.
	gateIndex := findGate(deferResult.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate")
	}
	if deferResult.Content.Gates[gateIndex].Status != "pending" {
		t.Fatalf("gate status: got %q, want 'pending'", deferResult.Content.Gates[gateIndex].Status)
	}

	// Advance past the defer target.
	env.clock.Advance(3 * time.Hour) // Now: 2026-01-15T15:00:00Z

	content := waitForGateStatus(t, env, room, "tkt-open", "defer", "satisfied")
	if content.Gates[gateIndex].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[gateIndex].SatisfiedBy)
	}
}

// --- Deadline tests ---

func TestDeadlineCRUD(t *testing.T) {
	room := "!deadline-room:bureau.local"

	t.Run("CreateWithDeadline", func(t *testing.T) {
		rooms := map[ref.RoomID]*roomState{testRoomID(room): newTrackedRoom(nil)}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result createResponse
		err := env.client.Call(context.Background(), "create", map[string]any{
			"room": room, "title": "Ship v2 API", "type": "task", "priority": 2,
			"deadline": "2026-03-01T00:00:00Z",
		}, &result)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		content, exists := rooms[testRoomID(room)].index.Get(result.ID)
		if !exists {
			t.Fatalf("ticket %s not in index", result.ID)
		}
		if content.Deadline != "2026-03-01T00:00:00Z" {
			t.Errorf("deadline: got %q, want %q", content.Deadline, "2026-03-01T00:00:00Z")
		}
	})

	t.Run("CreateWithInvalidDeadline", func(t *testing.T) {
		rooms := map[ref.RoomID]*roomState{testRoomID(room): newTrackedRoom(nil)}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result createResponse
		err := env.client.Call(context.Background(), "create", map[string]any{
			"room": room, "title": "Bad deadline", "type": "task", "priority": 2,
			"deadline": "not-a-date",
		}, &result)
		if err == nil {
			t.Fatal("expected error for invalid deadline")
		}
	})

	t.Run("UpdateSetDeadline", func(t *testing.T) {
		rooms := map[ref.RoomID]*roomState{testRoomID(room): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-1": ticketFixture("No deadline yet", "open"),
		})}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result mutationResponse
		err := env.client.Call(context.Background(), "update", map[string]any{
			"ticket": "tkt-1", "room": room, "deadline": "2026-04-01T00:00:00Z",
		}, &result)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if result.Content.Deadline != "2026-04-01T00:00:00Z" {
			t.Errorf("deadline: got %q, want %q", result.Content.Deadline, "2026-04-01T00:00:00Z")
		}
	})

	t.Run("UpdateClearDeadline", func(t *testing.T) {
		fixture := ticketFixture("Has deadline", "open")
		fixture.Deadline = "2026-04-01T00:00:00Z"
		rooms := map[ref.RoomID]*roomState{testRoomID(room): newTrackedRoom(map[string]ticket.TicketContent{
			"tkt-1": fixture,
		})}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result mutationResponse
		err := env.client.Call(context.Background(), "update", map[string]any{
			"ticket": "tkt-1", "room": room, "deadline": "",
		}, &result)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if result.Content.Deadline != "" {
			t.Errorf("deadline should be cleared, got %q", result.Content.Deadline)
		}
	})
}

// --- Deadline timer lifecycle tests ---

func TestDeadlineFireAddsNote(t *testing.T) {
	room := "!deadline-room:bureau.local"
	// Deadline 2 hours from test epoch.
	deadline := testClockEpoch.Add(2 * time.Hour).UTC().Format(time.RFC3339)

	rooms := map[ref.RoomID]*roomState{
		testRoomID(room): newTrackedRoom(nil),
	}
	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	var result createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "Ticket with deadline",
		"type":     "task",
		"priority": 2,
		"deadline": deadline,
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Advance past the deadline. The AfterFunc fires synchronously,
	// the timer loop goroutine processes it and writes.
	env.clock.Advance(3 * time.Hour)

	// Wait for the deadline note write. The create RPC also wrote,
	// so we loop until we see a note (skipping the create write).
	for {
		<-env.writer.notify
		var showResult showResponse
		if err := env.client.Call(ctx, "show", map[string]any{
			"ticket": result.ID,
			"room":   room,
		}, &showResult); err != nil {
			t.Fatalf("show: %v", err)
		}
		if len(showResult.Content.Notes) > 0 {
			if len(showResult.Content.Notes) != 1 {
				t.Fatalf("expected 1 note, got %d", len(showResult.Content.Notes))
			}
			expectedBody := "Deadline passed: " + deadline
			if showResult.Content.Notes[0].Body != expectedBody {
				t.Errorf("note body: got %q, want %q", showResult.Content.Notes[0].Body, expectedBody)
			}
			return
		}
	}
}

func TestDeadlineStaleEntryIgnored(t *testing.T) {
	// Update a deadline from 2h to 10h. Advance past both at once.
	// The stale (2h) heap entry should be skipped by lazy deletion
	// in fireDeadlineLocked (target mismatch). Only the new (10h)
	// deadline should produce a note. Asserting exactly 1 note
	// proves the stale entry didn't fire.
	room := "!deadline-room:bureau.local"
	originalDeadline := testClockEpoch.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	newDeadline := testClockEpoch.Add(10 * time.Hour).UTC().Format(time.RFC3339)

	rooms := map[ref.RoomID]*roomState{
		testRoomID(room): newTrackedRoom(nil),
	}
	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	var result createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "Deadline will change",
		"type":     "task",
		"priority": 2,
		"deadline": originalDeadline,
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update the deadline to later.
	var updateResult mutationResponse
	err = env.client.Call(ctx, "update", map[string]any{
		"ticket":   result.ID,
		"room":     room,
		"deadline": newDeadline,
	}, &updateResult)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Advance past both deadlines. The stale 2h entry fires first
	// but is skipped (target != content.Deadline). The 10h entry
	// fires and writes a note.
	env.clock.Advance(11 * time.Hour)

	// Wait for the deadline note write.
	for {
		<-env.writer.notify
		var showResult showResponse
		if err := env.client.Call(ctx, "show", map[string]any{
			"ticket": result.ID,
			"room":   room,
		}, &showResult); err != nil {
			t.Fatalf("show: %v", err)
		}
		if len(showResult.Content.Notes) > 0 {
			if len(showResult.Content.Notes) != 1 {
				t.Fatalf("expected exactly 1 note (stale entry should not fire), got %d", len(showResult.Content.Notes))
			}
			expectedBody := "Deadline passed: " + newDeadline
			if showResult.Content.Notes[0].Body != expectedBody {
				t.Errorf("note body: got %q, want %q", showResult.Content.Notes[0].Body, expectedBody)
			}
			return
		}
	}
}
