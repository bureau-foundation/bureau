// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// pipelineWaitParams holds the parameters for the pipeline wait command.
type pipelineWaitParams struct {
	cli.JSONOutput
	TicketID   string `json:"ticket_id"    desc:"pipeline ticket ID (e.g. pip-a3f9)" required:"true"`
	Room       string `json:"room"         flag:"room"        desc:"room ID where the ticket lives (required)" required:"true"`
	ServerName string `json:"server_name"  flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// waitResult is the JSON output for pipeline wait.
type waitResult struct {
	TicketID    ref.TicketID `json:"ticket_id"    desc:"pipeline ticket ID"`
	Status      string       `json:"status"       desc:"ticket status"`
	Conclusion  string       `json:"conclusion"   desc:"pipeline conclusion (success, failure, aborted, cancelled)"`
	PipelineRef string       `json:"pipeline_ref" desc:"pipeline reference"`
	StepCount   int          `json:"step_count"   desc:"total pipeline steps"`
	NoteCount   int          `json:"note_count"   desc:"number of ticket notes"`
}

// waitCommand returns the "wait" subcommand that watches a pipeline
// ticket until it closes. Returns immediately if the ticket is already
// closed. Prints step progress as the executor updates the ticket.
func waitCommand() *cli.Command {
	var params pipelineWaitParams

	return &cli.Command{
		Name:    "wait",
		Summary: "Wait for a pipeline to complete",
		Description: `Watch a pipeline ticket until it closes, printing progress as the
executor runs each step. Returns immediately if the ticket is already
closed.

The --room flag specifies the room where the ticket lives. This is
printed by "bureau pipeline run" when the pipeline is accepted.

Exit code 0 for conclusion "success", 1 otherwise.`,
		Usage: "bureau pipeline wait [flags] <ticket-id> --room <room>",
		Examples: []cli.Example{
			{
				Description: "Wait for a pipeline to finish",
				Command:     "bureau pipeline wait pip-a3f9 --room '!project:bureau.local'",
			},
		},
		Params: func() any { return &params },
		Output: func() any { return &waitResult{} },
		Run: func(args []string) error {
			if len(args) == 1 {
				params.TicketID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline wait [flags] <ticket-id> --room <room>")
			}
			if params.TicketID == "" {
				return cli.Validation("ticket ID is required\n\nusage: bureau pipeline wait [flags] <ticket-id> --room <room>")
			}
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			ticketID, err := ref.ParseTicketID(params.TicketID)
			if err != nil {
				return cli.Validation("invalid ticket ID: %w", err)
			}

			roomID, err := ref.ParseRoomID(params.Room)
			if err != nil {
				return cli.Validation("invalid --room: %w", err)
			}

			// Connect to Matrix.
			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			// Check if the ticket is already closed by reading its
			// current state. This avoids a /sync watch when the
			// pipeline finished before we started watching.
			existing, err := session.GetStateEvent(ctx, roomID, schema.EventTypeTicket, ticketID.String())
			if err == nil {
				var content ticket.TicketContent
				if parseErr := json.Unmarshal(existing, &content); parseErr == nil && content.Status == "closed" {
					return emitWaitResult(params, ticketID, content)
				}
			}
			// If GetStateEvent fails (ticket doesn't exist yet) or the
			// ticket isn't closed, fall through to the /sync watch.

			// Watch for ticket state events via /sync. The filter
			// includes only m.bureau.ticket state events to minimize
			// noise.
			watcher, err := messaging.WatchRoom(ctx, session, roomID, &messaging.SyncFilter{
				TimelineTypes: []string{},
			})
			if err != nil {
				return fmt.Errorf("setting up ticket watch: %w", err)
			}

			var previousStep int
			for {
				event, err := watcher.WaitForEvent(ctx, func(event messaging.Event) bool {
					if event.Type != schema.EventTypeTicket {
						return false
					}
					if event.StateKey == nil || *event.StateKey != ticketID.String() {
						return false
					}
					return true
				})
				if err != nil {
					return fmt.Errorf("watching ticket %s: %w", ticketID, err)
				}

				var content ticket.TicketContent
				if err := unmarshalEventContent(event, &content); err != nil {
					return fmt.Errorf("parsing ticket update: %w", err)
				}

				// Print step progress when the executor advances.
				if content.Pipeline != nil && content.Pipeline.CurrentStep > previousStep {
					fmt.Fprintf(os.Stderr, "  [%d/%d] %s\n",
						content.Pipeline.CurrentStep,
						content.Pipeline.TotalSteps,
						content.Pipeline.CurrentStepName)
					previousStep = content.Pipeline.CurrentStep
				}

				if content.Status == "closed" {
					return emitWaitResult(params, ticketID, content)
				}
			}
		},
	}
}

// emitWaitResult formats and outputs the final ticket state.
func emitWaitResult(params pipelineWaitParams, ticketID ref.TicketID, content ticket.TicketContent) error {
	pipelineRef := ""
	conclusion := ""
	stepCount := 0
	if content.Pipeline != nil {
		pipelineRef = content.Pipeline.PipelineRef
		conclusion = content.Pipeline.Conclusion
		stepCount = content.Pipeline.TotalSteps
	}

	result := waitResult{
		TicketID:    ticketID,
		Status:      content.Status,
		Conclusion:  conclusion,
		PipelineRef: pipelineRef,
		StepCount:   stepCount,
		NoteCount:   len(content.Notes),
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "Pipeline %s %s\n", ticketID, conclusion)
	if pipelineRef != "" {
		fmt.Fprintf(os.Stderr, "  pipeline:   %s\n", pipelineRef)
	}
	fmt.Fprintf(os.Stderr, "  conclusion: %s\n", conclusion)
	fmt.Fprintf(os.Stderr, "  steps:      %d\n", stepCount)
	if len(content.Notes) > 0 {
		fmt.Fprintf(os.Stderr, "  notes:      %d\n", len(content.Notes))
	}

	if conclusion != "success" {
		return &cli.ExitError{Code: 1}
	}
	return nil
}

// unmarshalEventContent unmarshals the Content field of a messaging.Event
// into a typed struct via the JSON round-trip (Event.Content is
// map[string]any from the Matrix /sync response).
func unmarshalEventContent(event messaging.Event, target any) error {
	data, err := json.Marshal(event.Content)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
