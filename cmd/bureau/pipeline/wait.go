// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// pipelineWaitParams holds the parameters for the pipeline wait command.
type pipelineWaitParams struct {
	cli.JSONOutput
	TicketID   string        `json:"ticket_id"    desc:"pipeline ticket ID (e.g. pip-a3f9)" required:"true"`
	Room       string        `json:"room"         flag:"room"        desc:"room ID where the ticket lives (required)" required:"true"`
	Timeout    time.Duration `json:"timeout"      flag:"timeout"     desc:"maximum time to wait (0 means no limit, Ctrl-C to cancel)" default:"0"`
	ServerName string        `json:"server_name"  flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`

	// clock is injected by tests for deterministic timeout control.
	// Nil in production; defaults to clock.Real().
	clock clock.Clock
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

By default, waits indefinitely (Ctrl-C to cancel). Use --timeout to
set a ceiling for automation (e.g., --timeout 8h for training jobs).

Exit code 0 for conclusion "success", 1 otherwise.`,
		Usage: "bureau pipeline wait [flags] <ticket-id> --room <room>",
		Examples: []cli.Example{
			{
				Description: "Wait for a pipeline to finish",
				Command:     "bureau pipeline wait pip-a3f9 --room '!project:bureau.local'",
			},
			{
				Description: "Wait up to 8 hours for a training pipeline",
				Command:     "bureau pipeline wait pip-a3f9 --room '!project:bureau.local' --timeout 8h",
			},
		},
		Params: func() any { return &params },
		Output: func() any { return &waitResult{} },
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
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
			session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer session.Close()

			watchCtx := ctx
			if params.Timeout > 0 {
				clk := params.clock
				if clk == nil {
					clk = clock.Real()
				}
				var watchCancel context.CancelFunc
				watchCtx, watchCancel = context.WithCancel(ctx)
				defer watchCancel()
				timer := clk.AfterFunc(params.Timeout, watchCancel)
				defer timer.Stop()
			}

			final, err := command.WatchTicket(watchCtx, command.WatchTicketParams{
				Session:    session,
				RoomID:     roomID,
				TicketID:   ticketID,
				OnProgress: command.StepProgressWriter(os.Stderr),
			})
			if err != nil {
				return err
			}

			return emitWaitResult(logger, params, ticketID, *final)
		},
	}
}

// emitWaitResult formats and outputs the final ticket state.
func emitWaitResult(logger *slog.Logger, params pipelineWaitParams, ticketID ref.TicketID, content ticket.TicketContent) error {
	pipelineRef := ""
	conclusion := ""
	stepCount := 0
	if content.Pipeline != nil {
		pipelineRef = content.Pipeline.PipelineRef
		conclusion = string(content.Pipeline.Conclusion)
		stepCount = content.Pipeline.TotalSteps
	}

	result := waitResult{
		TicketID:    ticketID,
		Status:      string(content.Status),
		Conclusion:  conclusion,
		PipelineRef: pipelineRef,
		StepCount:   stepCount,
		NoteCount:   len(content.Notes),
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("pipeline completed",
		"ticket_id", ticketID,
		"conclusion", conclusion,
		"pipeline_ref", pipelineRef,
		"steps", stepCount,
		"notes", len(content.Notes),
	)

	if conclusion != "success" {
		return &cli.ExitError{Code: 1}
	}
	return nil
}
