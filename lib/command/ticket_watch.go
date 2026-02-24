// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// WatchTicketParams configures a ticket watch.
type WatchTicketParams struct {
	// Session is the authenticated Matrix session used for
	// GetStateEvent and /sync polling.
	Session messaging.Session

	// RoomID is the room where the ticket state event lives.
	RoomID ref.RoomID

	// TicketID is the ticket to watch (e.g., "pip-a3f9"). This is the
	// state key of the m.bureau.ticket event.
	TicketID ref.TicketID

	// OnProgress is called on each ticket state change with the previous
	// and current content. The first call has a zero-value previous. Nil
	// skips progress callbacks.
	OnProgress func(previous, current ticket.TicketContent)
}

// WatchTicket watches a ticket via Matrix /sync until it closes. Returns
// immediately if the ticket is already closed when first checked.
//
// The function first reads the current ticket state via GetStateEvent. If
// the ticket is already closed, it returns without setting up a /sync
// watcher. This handles the race where the pipeline finished between
// the caller receiving the accepted ack and calling WatchTicket.
//
// If the ticket is still open, WatchTicket sets up a room watcher
// filtered to state events and blocks until the ticket's status becomes
// "closed". The OnProgress callback (if non-nil) is called on each
// ticket state update, enabling callers to print step progress.
//
// Returns the final TicketContent with status=="closed". The caller
// inspects content.Pipeline.Conclusion for the outcome.
func WatchTicket(ctx context.Context, params WatchTicketParams) (*ticket.TicketContent, error) {
	stateKey := params.TicketID.String()

	// Check if the ticket is already closed. This avoids a /sync watch
	// when the pipeline finished before we started watching.
	existing, err := params.Session.GetStateEvent(ctx, params.RoomID, schema.EventTypeTicket, stateKey)
	if err == nil {
		var content ticket.TicketContent
		if parseErr := json.Unmarshal(existing, &content); parseErr == nil && content.Status == ticket.StatusClosed {
			return &content, nil
		}
	}
	// If GetStateEvent fails (ticket doesn't exist yet) or the ticket
	// isn't closed, fall through to the /sync watch.

	// Watch for ticket state events via /sync. The filter restricts to
	// state events only — no timeline messages — to minimize noise.
	watcher, err := messaging.WatchRoom(ctx, params.Session, params.RoomID, &messaging.SyncFilter{
		TimelineTypes: []string{},
	})
	if err != nil {
		return nil, fmt.Errorf("setting up ticket watch: %w", err)
	}

	var previous ticket.TicketContent
	for {
		event, err := watcher.WaitForEvent(ctx, func(event messaging.Event) bool {
			if event.Type != schema.EventTypeTicket {
				return false
			}
			if event.StateKey == nil || *event.StateKey != stateKey {
				return false
			}
			return true
		})
		if err != nil {
			return nil, fmt.Errorf("watching ticket %s: %w", params.TicketID, err)
		}

		var content ticket.TicketContent
		if err := unmarshalEventContent(event, &content); err != nil {
			return nil, fmt.Errorf("parsing ticket update: %w", err)
		}

		if params.OnProgress != nil {
			params.OnProgress(previous, content)
		}
		previous = content

		if content.Status == ticket.StatusClosed {
			return &content, nil
		}
	}
}

// StepProgressWriter returns an OnProgress callback that prints pipeline
// step advancement to the given writer. Each step change prints a line:
//
//	[N/M] step-name
//
// Steps that don't advance the step counter (e.g., status changes without
// step progression) are silently skipped.
func StepProgressWriter(writer io.Writer) func(previous, current ticket.TicketContent) {
	return func(previous, current ticket.TicketContent) {
		if current.Pipeline == nil {
			return
		}
		previousStep := 0
		if previous.Pipeline != nil {
			previousStep = previous.Pipeline.CurrentStep
		}
		if current.Pipeline.CurrentStep > previousStep {
			fmt.Fprintf(writer, "  [%d/%d] %s\n",
				current.Pipeline.CurrentStep,
				current.Pipeline.TotalSteps,
				current.Pipeline.CurrentStepName)
		}
	}
}

// unmarshalEventContent unmarshals the Content field of a messaging.Event
// into a typed struct. Event.Content is map[string]any from the Matrix
// /sync response, so this does a JSON round-trip to get typed fields.
func unmarshalEventContent(event messaging.Event, target any) error {
	data, err := json.Marshal(event.Content)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
