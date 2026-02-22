// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// Result is a parsed command result received from the daemon. It wraps
// the fields from [schema.CommandResultMessage] with typed accessors
// and preserves the raw [messaging.Event] for callers that need event
// metadata (event ID, sender, timestamp).
type Result struct {
	// Status is the command outcome: "success" or "error". The daemon
	// always posts "success" for commands that completed without error,
	// even for async commands that return "accepted" in their result
	// payload.
	Status string

	// ResultData is the command-specific result payload as raw JSON.
	// For synchronous commands, this contains the full result. For
	// accepted acks, it contains ticket_id and room. Callers unmarshal
	// into their own types via [UnmarshalResult].
	ResultData json.RawMessage

	// Error is the error message when Status is "error".
	Error string

	// DurationMS is the server-side execution time in milliseconds.
	DurationMS int64

	// RequestID is the correlation identifier matching the original
	// command's request_id field.
	RequestID string

	// TicketID is the ticket ID from accepted results (e.g., "pip-a3f9").
	// Present when the daemon creates a ticket for an async operation
	// (pipeline execution, worktree add/remove). Extracted from the
	// "ticket_id" field in ResultData.
	TicketID ref.TicketID

	// TicketRoom is the room ID where the ticket was created. Present
	// alongside TicketID. Extracted from the "room" field in ResultData.
	TicketRoom ref.RoomID

	// Principal is the executor principal name for async commands.
	// Present in older-style "accepted" results that include a
	// principal field. Newer ticket-driven results use TicketID instead.
	Principal string

	// ExitCode is the pipeline exit code. Non-nil only for pipeline
	// results (the second result from async commands).
	ExitCode *int

	// Steps contains per-step outcomes for pipeline results.
	Steps []pipeline.PipelineStepResult

	// Outputs contains pipeline output variables.
	Outputs map[string]string

	// LogEventID is the thread root for pipeline log events.
	LogEventID ref.EventID

	// Event is the raw Matrix event for metadata access (event ID,
	// sender, timestamp).
	Event messaging.Event
}

// IsAccepted returns true when the daemon acknowledged an async command
// by creating a ticket. The command result status is "success" (the
// command handler completed), but the result payload contains
// status:"accepted" and a ticket_id. The pipeline executor drives all
// subsequent progress through the ticket.
func (r *Result) IsAccepted() bool {
	return !r.TicketID.IsZero()
}

// IsSuccess returns true when the command completed successfully.
func (r *Result) IsSuccess() bool {
	return r.Status == "success"
}

// IsError returns true when the command failed.
func (r *Result) IsError() bool {
	return r.Status == "error"
}

// IsPipelineResult returns true when this result contains pipeline
// execution details (exit code, step results). This distinguishes
// the pipeline completion result from the initial "accepted" ack.
func (r *Result) IsPipelineResult() bool {
	return r.ExitCode != nil
}

// Err returns an error if the command result indicates failure.
// Returns nil for "success" and "accepted" statuses. Use this to
// collapse the common error-check pattern:
//
//	if err := result.Err(); err != nil { return err }
func (r *Result) Err() error {
	if r.Status == "error" {
		return fmt.Errorf("daemon error: %s", r.Error)
	}
	return nil
}

// WriteAcceptedHint writes the standard acceptance display for async
// commands. For ticket-driven commands, prints the ticket ID, room,
// and a "bureau pipeline wait" hint. Falls back to the principal-based
// "bureau observe" hint for older-style accepted results.
// Writes nothing for synchronous commands (no ticket, no principal).
func (r *Result) WriteAcceptedHint(writer io.Writer) {
	if !r.TicketID.IsZero() {
		fmt.Fprintf(writer, "  ticket:  %s\n", r.TicketID)
		if !r.TicketRoom.IsZero() {
			fmt.Fprintf(writer, "  room:    %s\n", r.TicketRoom)
		}
		fmt.Fprintf(writer, "\nWatch progress: bureau pipeline wait %s --room %s\n",
			r.TicketID, r.TicketRoom)
		return
	}
	if r.Principal != "" {
		fmt.Fprintf(writer, "  executor:  %s\n", r.Principal)
		fmt.Fprintf(writer, "\nObserve progress: bureau observe %s\n", r.Principal)
	}
}

// UnmarshalResult unmarshals ResultData into the provided target.
// Use this to extract typed result payloads from successful commands.
func (r *Result) UnmarshalResult(target any) error {
	if len(r.ResultData) == 0 {
		return fmt.Errorf("no result data")
	}
	return json.Unmarshal(r.ResultData, target)
}

// parseResult extracts a Result from a raw messaging.Event whose
// content is a command_result message. For accepted results (status
// "success" with a ticket_id in the result payload), extracts the
// TicketID and TicketRoom fields from the result data.
func parseResult(event messaging.Event) (*Result, error) {
	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		return nil, fmt.Errorf("marshaling result content: %w", err)
	}

	var raw schema.CommandResultMessage
	if err := json.Unmarshal(contentJSON, &raw); err != nil {
		return nil, fmt.Errorf("parsing command result: %w", err)
	}

	result := &Result{
		Status:     raw.Status,
		ResultData: raw.Result,
		Error:      raw.Error,
		DurationMS: raw.DurationMS,
		RequestID:  raw.RequestID,
		Principal:  raw.Principal,
		ExitCode:   raw.ExitCode,
		Steps:      raw.Steps,
		Outputs:    raw.Outputs,
		LogEventID: raw.LogEventID,
		Event:      event,
	}

	// Extract ticket fields from the result payload. Async command
	// handlers (pipeline.execute, worktree.add, worktree.remove) return
	// {status:"accepted", ticket_id:"pip-xxx", room:"!room:server"} as
	// the result data. Parse these into typed fields so callers don't
	// need to unmarshal ResultData themselves for the common case.
	if raw.Status == "success" && len(raw.Result) > 0 {
		var accepted struct {
			TicketID string `json:"ticket_id"`
			Room     string `json:"room"`
		}
		if json.Unmarshal(raw.Result, &accepted) == nil && accepted.TicketID != "" {
			if ticketID, err := ref.ParseTicketID(accepted.TicketID); err == nil {
				result.TicketID = ticketID
			}
			if accepted.Room != "" {
				if roomID, err := ref.ParseRoomID(accepted.Room); err == nil {
					result.TicketRoom = roomID
				}
			}
		}
	}

	return result, nil
}
