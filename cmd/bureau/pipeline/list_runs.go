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

// pipelineListRunsParams holds the parameters for the pipeline list-runs command.
type pipelineListRunsParams struct {
	cli.JSONOutput
	ticketcli.TicketConnection
	Room   string `json:"room"   flag:"room"   desc:"room ID to list pipeline tickets from" required:"true"`
	Status string `json:"status" flag:"status" desc:"filter by status (open, closed, in_progress)"`
}

// listRunEntry is one row in the list-runs output.
type listRunEntry struct {
	TicketID    ref.TicketID `json:"ticket_id"    desc:"pipeline ticket ID"`
	Status      string       `json:"status"       desc:"ticket status"`
	PipelineRef string       `json:"pipeline_ref" desc:"pipeline reference"`
	StepInfo    string       `json:"step_info"    desc:"current step or conclusion"`
}

// listRunsCommand returns the "list-runs" subcommand that lists pipeline
// tickets in a room via the ticket service socket.
func listRunsCommand() *cli.Command {
	var params pipelineListRunsParams

	return &cli.Command{
		Name:    "list-runs",
		Summary: "List pipeline runs in a room",
		Description: `Query the ticket service for pipeline tickets in a room. Lists
pipeline execution tickets with their status and current progress.`,
		Usage: "bureau pipeline list-runs [flags] --room <room>",
		Examples: []cli.Example{
			{
				Description: "List all pipeline runs",
				Command:     "bureau pipeline list-runs --room '!project:bureau.local'",
			},
			{
				Description: "List only running pipelines",
				Command:     "bureau pipeline list-runs --room '!project:bureau.local' --status open",
			},
		},
		Params: func() any { return &params },
		Output: func() any { return &[]listRunEntry{} },
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			roomID, err := ref.ParseRoomID(params.Room)
			if err != nil {
				return cli.Validation("invalid --room: %w", err)
			}

			client, err := service.NewServiceClient(params.SocketPath, params.TokenPath)
			if err != nil {
				return fmt.Errorf("connecting to ticket service: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			listArgs := map[string]any{
				"room": roomID.String(),
				"type": "pipeline",
			}
			if params.Status != "" {
				listArgs["status"] = params.Status
			}

			var entries []struct {
				TicketID        string `json:"ticket_id"`
				Status          string `json:"status"`
				PipelineRef     string `json:"pipeline_ref"`
				CurrentStep     int    `json:"current_step"`
				TotalSteps      int    `json:"total_steps"`
				CurrentStepName string `json:"current_step_name"`
				Conclusion      string `json:"conclusion"`
			}
			if err := client.Call(ctx, "list", listArgs, &entries); err != nil {
				return fmt.Errorf("listing pipeline tickets: %w", err)
			}

			results := make([]listRunEntry, len(entries))
			for i, entry := range entries {
				ticketID, _ := ref.ParseTicketID(entry.TicketID)
				stepInfo := entry.Conclusion
				if stepInfo == "" && entry.CurrentStepName != "" {
					stepInfo = fmt.Sprintf("[%d/%d] %s",
						entry.CurrentStep, entry.TotalSteps, entry.CurrentStepName)
				}
				results[i] = listRunEntry{
					TicketID:    ticketID,
					Status:      entry.Status,
					PipelineRef: entry.PipelineRef,
					StepInfo:    stepInfo,
				}
			}

			if done, err := params.EmitJSON(results); done {
				return err
			}

			if len(results) == 0 {
				fmt.Fprintln(os.Stderr, "No pipeline runs found")
				return nil
			}

			fmt.Fprintf(os.Stdout, "%-12s  %-12s  %-30s  %s\n",
				"TICKET", "STATUS", "PIPELINE", "STEP/CONCLUSION")
			for _, entry := range results {
				fmt.Fprintf(os.Stdout, "%-12s  %-12s  %-30s  %s\n",
					entry.TicketID, entry.Status, entry.PipelineRef, entry.StepInfo)
			}
			return nil
		},
	}
}
