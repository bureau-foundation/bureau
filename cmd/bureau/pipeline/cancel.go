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

// pipelineCancelParams holds the parameters for the pipeline cancel command.
type pipelineCancelParams struct {
	cli.JSONOutput
	ticketcli.TicketConnection
	TicketID string `json:"ticket_id" desc:"pipeline ticket ID (e.g. pip-a3f9)" required:"true"`
	Reason   string `json:"reason"    flag:"reason"   desc:"cancellation reason"`
}

// cancelResult is the JSON output for pipeline cancel.
type cancelResult struct {
	TicketID ref.TicketID `json:"ticket_id" desc:"cancelled ticket ID"`
	Status   string       `json:"status"    desc:"new ticket status"`
}

// cancelCommand returns the "cancel" subcommand that closes a pipeline
// ticket, signaling the executor to abort.
func cancelCommand() *cli.Command {
	var params pipelineCancelParams

	return &cli.Command{
		Name:    "cancel",
		Summary: "Cancel a running pipeline",
		Description: `Close a pipeline ticket via the ticket service, signaling the executor
to abort. The executor detects the ticket closure on its next status
check and terminates.`,
		Usage: "bureau pipeline cancel [flags] <ticket-id>",
		Examples: []cli.Example{
			{
				Description: "Cancel a pipeline",
				Command:     "bureau pipeline cancel pip-a3f9 --reason 'wrong parameters'",
			},
		},
		Params:      func() any { return &params },
		Output:      func() any { return &cancelResult{} },
		Annotations: cli.Create(),
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.TicketID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline cancel [flags] <ticket-id>")
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

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			closeArgs := map[string]any{
				"ticket": ticketID.String(),
			}
			if params.Reason != "" {
				closeArgs["reason"] = params.Reason
			}

			var result struct {
				Status string `json:"status"`
			}
			if err := client.Call(ctx, "close", closeArgs, &result); err != nil {
				return fmt.Errorf("closing ticket %s: %w", ticketID, err)
			}

			if done, err := params.EmitJSON(cancelResult{
				TicketID: ticketID,
				Status:   result.Status,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Cancelled %s\n", ticketID)
			return nil
		},
	}
}
