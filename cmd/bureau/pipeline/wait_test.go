// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// TestWaitAlreadyClosed verifies that "pipeline wait" returns immediately
// when the ticket is already closed at the time of the initial GetStateEvent
// check — no /sync watch is set up.
func TestWaitAlreadyClosed(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	// Register a closed ticket that GetStateEvent will find.
	closedTicket := ticket.TicketContent{
		Version: ticket.TicketContentVersion,
		Title:   "test pipeline",
		Status:  ticket.StatusClosed,
		Type:    ticket.TypePipeline,
		Pipeline: &ticket.PipelineExecutionContent{
			PipelineRef: "bureau/pipeline:test",
			TotalSteps:  3,
			Conclusion:  pipeline.ConclusionSuccess,
		},
	}
	ticketJSON, err := json.Marshal(closedTicket)
	if err != nil {
		t.Fatalf("marshal ticket: %v", err)
	}
	state.mu.Lock()
	stateKey := ticketRoomID + "\x00" + string(schema.EventTypeTicket) + "\x00" + "pip-done"
	state.stateEvents[stateKey] = ticketJSON
	state.mu.Unlock()

	startTestServer(t, state)

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	err = cmd.Run(context.Background(), []string{"pip-done"}, testLogger())
	if err != nil {
		t.Fatalf("wait should succeed for already-closed ticket: %v", err)
	}
}

// TestWaitAlreadyClosedFailure verifies that an already-closed ticket with
// a non-success conclusion causes exit code 1.
func TestWaitAlreadyClosedFailure(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	failedTicket := ticket.TicketContent{
		Version: ticket.TicketContentVersion,
		Title:   "test pipeline",
		Status:  ticket.StatusClosed,
		Type:    ticket.TypePipeline,
		Pipeline: &ticket.PipelineExecutionContent{
			PipelineRef: "bureau/pipeline:test",
			TotalSteps:  3,
			Conclusion:  pipeline.ConclusionFailure,
		},
	}
	ticketJSON, err := json.Marshal(failedTicket)
	if err != nil {
		t.Fatalf("marshal ticket: %v", err)
	}
	state.mu.Lock()
	stateKey := ticketRoomID + "\x00" + string(schema.EventTypeTicket) + "\x00" + "pip-fail"
	state.stateEvents[stateKey] = ticketJSON
	state.mu.Unlock()

	startTestServer(t, state)

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	err = cmd.Run(context.Background(), []string{"pip-fail"}, testLogger())
	if err == nil {
		t.Fatal("wait should fail for non-success conclusion")
	}
	// The command returns ExitError{Code: 1} for non-success conclusions.
	if !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("expected exit code 1 error, got: %v", err)
	}
}

// TestWaitTimeoutFires verifies that --timeout terminates the wait when
// the ticket never closes. Uses a fake clock: the test advances past the
// timeout duration, which fires the AfterFunc that cancels the watch context.
// No real wall-clock delay.
func TestWaitTimeoutFires(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	// No ticket registered for GetStateEvent — returns 404, which
	// WatchTicket treats as "not closed yet" and enters the /sync loop.
	startTestServer(t, state)

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	cmd := waitCommand()
	params := cmd.Params().(*pipelineWaitParams)
	params.clock = fakeClock

	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
		"--timeout", "30m",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	// Run the wait in a goroutine — it blocks until the ticket closes
	// or the timeout fires.
	errorChannel := make(chan error, 1)
	go func() {
		errorChannel <- cmd.Run(context.Background(), []string{"pip-never"}, testLogger())
	}()

	// Wait for the AfterFunc timer to be registered, then advance
	// past the timeout. The AfterFunc cancels the watch context.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(30 * time.Minute)

	err := <-errorChannel
	if err == nil {
		t.Fatal("wait should fail when timeout fires")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context-related error, got: %v", err)
	}
}

// TestWaitTicketCloses verifies that "pipeline wait" returns successfully
// when the ticket closes via a /sync state event delivery before the
// timeout fires. The fake clock never advances, proving the ticket close
// (not the timeout) terminated the wait.
func TestWaitTicketCloses(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	// Queue a closed ticket state event for delivery via /sync. The
	// initial GetStateEvent will return 404 (ticket not registered in
	// stateEvents), so WatchTicket enters the /sync loop. The first
	// incremental /sync delivers the closed ticket.
	closedContent := map[string]any{
		"version": ticket.TicketContentVersion,
		"title":   "test pipeline",
		"status":  string(ticket.StatusClosed),
		"type":    string(ticket.TypePipeline),
		"pipeline": map[string]any{
			"pipeline_ref": "bureau/pipeline:test",
			"total_steps":  3,
			"conclusion":   string(pipeline.ConclusionSuccess),
		},
	}
	state.queueSyncStateEvent(ticketRoomID, string(schema.EventTypeTicket), "pip-closing", closedContent)

	startTestServer(t, state)

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	cmd := waitCommand()
	params := cmd.Params().(*pipelineWaitParams)
	params.clock = fakeClock

	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
		"--timeout", "1h",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	// The mock delivers the closed ticket on the first incremental
	// /sync. No clock advance needed — the ticket closes before the
	// timeout fires.
	err := cmd.Run(context.Background(), []string{"pip-closing"}, testLogger())
	if err != nil {
		t.Fatalf("wait should succeed when ticket closes: %v", err)
	}
}

// TestWaitTicketClosesWithFailure verifies that a ticket closing with a
// non-success conclusion via /sync delivery causes exit code 1.
func TestWaitTicketClosesWithFailure(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	failedContent := map[string]any{
		"version": ticket.TicketContentVersion,
		"title":   "test pipeline",
		"status":  string(ticket.StatusClosed),
		"type":    string(ticket.TypePipeline),
		"pipeline": map[string]any{
			"pipeline_ref": "bureau/pipeline:test",
			"total_steps":  3,
			"conclusion":   string(pipeline.ConclusionFailure),
		},
	}
	state.queueSyncStateEvent(ticketRoomID, string(schema.EventTypeTicket), "pip-fails", failedContent)

	startTestServer(t, state)

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	err := cmd.Run(context.Background(), []string{"pip-fails"}, testLogger())
	if err == nil {
		t.Fatal("wait should fail for non-success conclusion")
	}
	if !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("expected exit code 1 error, got: %v", err)
	}
}

// TestWaitZeroTimeoutNoTimer verifies that --timeout 0 (the default) does
// not register any timer with the clock. The wait completes when the
// ticket closes, with no timeout involved.
func TestWaitZeroTimeoutNoTimer(t *testing.T) {
	state := newPipelineTestState()
	ticketRoomID := "!ticket-room:test.local"

	closedContent := map[string]any{
		"version": ticket.TicketContentVersion,
		"title":   "test pipeline",
		"status":  string(ticket.StatusClosed),
		"type":    string(ticket.TypePipeline),
		"pipeline": map[string]any{
			"pipeline_ref": "bureau/pipeline:test",
			"total_steps":  2,
			"conclusion":   string(pipeline.ConclusionSuccess),
		},
	}
	state.queueSyncStateEvent(ticketRoomID, string(schema.EventTypeTicket), "pip-zero", closedContent)

	startTestServer(t, state)

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	cmd := waitCommand()
	params := cmd.Params().(*pipelineWaitParams)
	params.clock = fakeClock

	if err := cmd.FlagSet().Parse([]string{
		"--room", ticketRoomID,
		"--timeout", "0",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	err := cmd.Run(context.Background(), []string{"pip-zero"}, testLogger())
	if err != nil {
		t.Fatalf("wait should succeed with --timeout=0: %v", err)
	}

	// Verify no timer was registered — zero timeout skips the clock entirely.
	if fakeClock.PendingCount() != 0 {
		t.Errorf("expected no pending timers with --timeout=0, got %d", fakeClock.PendingCount())
	}
}

// TestWaitMissingTicketID verifies the validation error for missing ticket ID.
func TestWaitMissingTicketID(t *testing.T) {
	t.Parallel()

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", "!room:test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{}, testLogger())
	if err == nil {
		t.Fatal("expected error for missing ticket ID")
	}
	if !strings.Contains(err.Error(), "ticket ID is required") {
		t.Errorf("error %q should mention ticket ID", err.Error())
	}
}

// TestWaitMissingRoom verifies the validation error for missing --room.
func TestWaitMissingRoom(t *testing.T) {
	t.Parallel()

	cmd := waitCommand()
	err := cmd.Run(context.Background(), []string{"pip-test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for missing --room")
	}
	if !strings.Contains(err.Error(), "--room is required") {
		t.Errorf("error %q should mention --room", err.Error())
	}
}

// TestWaitInvalidTicketID verifies the validation error for a malformed ticket ID.
func TestWaitInvalidTicketID(t *testing.T) {
	t.Parallel()

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", "!room:test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	// Ticket ID must have a dash separator (e.g., "pip-a3f9"). A string
	// without a dash fails ParseTicketID before reaching ConnectOperator.
	err := cmd.Run(context.Background(), []string{"nodash"}, testLogger())
	if err == nil {
		t.Fatal("expected error for invalid ticket ID")
	}
	if !strings.Contains(err.Error(), "invalid ticket ID") {
		t.Errorf("error %q should mention invalid ticket ID", err.Error())
	}
}

// TestWaitInvalidRoom verifies the validation error for a malformed room ID.
func TestWaitInvalidRoom(t *testing.T) {
	t.Parallel()

	cmd := waitCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", "not-a-room-id",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"pip-test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for invalid room ID")
	}
	if !strings.Contains(err.Error(), "invalid --room") {
		t.Errorf("error %q should mention invalid --room", err.Error())
	}
}
