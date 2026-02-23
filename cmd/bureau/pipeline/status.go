// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	ticketcli "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/service"
)

// pipelineStatusParams holds the parameters for the pipeline status command.
type pipelineStatusParams struct {
	cli.JSONOutput
	ticketcli.TicketConnection
	TicketID string `json:"ticket_id" desc:"pipeline ticket ID (e.g. pip-a3f9)" required:"true"`
}

// statusResult is the JSON output for pipeline status.
type statusResult struct {
	TicketID        ref.TicketID `json:"ticket_id"         desc:"pipeline ticket ID"`
	Status          string       `json:"status"            desc:"ticket status"`
	PipelineRef     string       `json:"pipeline_ref"      desc:"pipeline reference"`
	CurrentStep     int          `json:"current_step"      desc:"current step number (1-based)"`
	TotalSteps      int          `json:"total_steps"       desc:"total pipeline steps"`
	CurrentStepName string       `json:"current_step_name" desc:"name of the current step"`
	Conclusion      string       `json:"conclusion"        desc:"pipeline conclusion (empty while running)"`
	NoteCount       int          `json:"note_count"        desc:"number of ticket notes"`
}

// statusCommand returns the "status" subcommand that shows the current
// state of a pipeline ticket via the ticket service socket.
func statusCommand() *cli.Command {
	var params pipelineStatusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show pipeline ticket status",
		Description: `Query the ticket service for the current state of a pipeline ticket.
This uses the ticket service unix socket, not Matrix — it works inside
sandboxes and returns immediately without /sync overhead.`,
		Usage: "bureau pipeline status [flags] <ticket-id>",
		Examples: []cli.Example{
			{
				Description: "Check pipeline status",
				Command:     "bureau pipeline status pip-a3f9",
			},
		},
		Params: func() any { return &params },
		Output: func() any { return &statusResult{} },
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.TicketID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline status [flags] <ticket-id>")
			}
			if params.TicketID == "" {
				return cli.Validation("ticket ID is required")
			}

			ticketID, err := ref.ParseTicketID(params.TicketID)
			if err != nil {
				return cli.Validation("invalid ticket ID: %w", err)
			}

			client, err := service.NewServiceClient(params.SocketPath, params.TokenPath)
			if err != nil {
				return fmt.Errorf("connecting to ticket service: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			var result struct {
				Status          string            `json:"status"`
				Title           string            `json:"title"`
				Type            string            `json:"type"`
				Priority        int               `json:"priority"`
				PipelineRef     string            `json:"pipeline_ref"`
				CurrentStep     int               `json:"current_step"`
				TotalSteps      int               `json:"total_steps"`
				CurrentStepName string            `json:"current_step_name"`
				Conclusion      string            `json:"conclusion"`
				Variables       map[string]string `json:"variables"`
				NoteCount       int               `json:"note_count"`
			}
			if err := client.Call(ctx, "show", map[string]any{"ticket": ticketID.String()}, &result); err != nil {
				return fmt.Errorf("querying ticket %s: %w", ticketID, err)
			}

			if done, err := params.EmitJSON(statusResult{
				TicketID:        ticketID,
				Status:          result.Status,
				PipelineRef:     result.PipelineRef,
				CurrentStep:     result.CurrentStep,
				TotalSteps:      result.TotalSteps,
				CurrentStepName: result.CurrentStepName,
				Conclusion:      result.Conclusion,
				NoteCount:       result.NoteCount,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Ticket %s (%s)\n", ticketID, result.Status)
			if result.PipelineRef != "" {
				fmt.Fprintf(os.Stdout, "  pipeline:  %s\n", result.PipelineRef)
			}
			if result.CurrentStepName != "" {
				fmt.Fprintf(os.Stdout, "  step:      [%d/%d] %s\n",
					result.CurrentStep, result.TotalSteps, result.CurrentStepName)
			} else if result.TotalSteps > 0 {
				fmt.Fprintf(os.Stdout, "  steps:     %d\n", result.TotalSteps)
			}
			if result.Conclusion != "" {
				fmt.Fprintf(os.Stdout, "  conclusion: %s\n", result.Conclusion)
			}
			return nil
		},
	}
}
