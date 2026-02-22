// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
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

			final, err := command.WatchTicket(ctx, command.WatchTicketParams{
				Session:    session,
				RoomID:     roomID,
				TicketID:   ticketID,
				OnProgress: command.StepProgressWriter(os.Stderr),
			})
			if err != nil {
				return err
			}

			return emitWaitResult(params, ticketID, *final)
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
