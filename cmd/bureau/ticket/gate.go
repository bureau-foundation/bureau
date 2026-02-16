// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// gateCommand returns the "gate" subcommand group for managing ticket
// gates (async coordination conditions).
func gateCommand() *cli.Command {
	return &cli.Command{
		Name:    "gate",
		Summary: "Manage ticket gates",
		Description: `Manage async coordination conditions (gates) on tickets.

Gates are conditions that must be satisfied before a ticket is
considered ready. Types include: human (manual approval), pipeline
(CI completion), state_event (Matrix event match), ticket (other
ticket closure), and timer (time-based).`,
		Subcommands: []*cli.Command{
			gateResolveCommand(),
			gateUpdateCommand(),
		},
	}
}

// --- gate resolve ---

type gateResolveParams struct {
	TicketConnection
	cli.JSONOutput
	Ticket string `json:"ticket" desc:"ticket ID" required:"true"`
	Gate   string `json:"gate"   desc:"gate ID"   required:"true"`
}

func gateResolveCommand() *cli.Command {
	var params gateResolveParams

	return &cli.Command{
		Name:    "resolve",
		Summary: "Manually resolve a human gate",
		Description: `Satisfy a human-type gate on a ticket. Only gates of type "human"
can be resolved manually — programmatic gates (pipeline, state_event,
ticket, timer) are satisfied automatically by the service's sync loop
or via "gate update".

The gate is identified by its ID within the ticket.`,
		Usage: "bureau ticket gate resolve <ticket-id> <gate-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Approve a human review gate",
				Command:     "bureau ticket gate resolve tkt-a3f9 review-gate",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		RequiredGrants: []string{"command/ticket/gate/resolve"},
		Run: func(args []string) error {
			if len(args) >= 1 {
				params.Ticket = args[0]
			}
			if len(args) >= 2 {
				params.Gate = args[1]
			}
			if len(args) > 2 {
				return fmt.Errorf("expected 2 positional arguments, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket gate resolve <ticket-id> <gate-id>")
			}
			if params.Gate == "" {
				return fmt.Errorf("gate ID is required\n\nUsage: bureau ticket gate resolve <ticket-id> <gate-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var result mutationResult
			if err := client.Call(ctx, "resolve-gate", map[string]any{
				"ticket": params.Ticket,
				"gate":   params.Gate,
			}, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "gate %q on %s resolved\n", params.Gate, result.ID)
			return nil
		},
	}
}

// --- gate update ---

type gateUpdateParams struct {
	TicketConnection
	cli.JSONOutput
	Ticket      string `json:"ticket"       desc:"ticket ID" required:"true"`
	Gate        string `json:"gate"         desc:"gate ID"   required:"true"`
	Status      string `json:"status"       flag:"status,s"     desc:"new gate status (pending or satisfied)" required:"true"`
	SatisfiedBy string `json:"satisfied_by" flag:"satisfied-by"  desc:"what satisfied the gate (event ID, user ID, etc.)"`
}

func gateUpdateCommand() *cli.Command {
	var params gateUpdateParams

	return &cli.Command{
		Name:    "update",
		Summary: "Update a gate's status programmatically",
		Description: `Update a gate's status on a ticket. Unlike "gate resolve" (which is
restricted to human gates), this command works on any gate type and
is the entry point for external systems (CI, pipelines) to report
gate satisfaction.`,
		Usage: "bureau ticket gate update <ticket-id> <gate-id> --status STATUS [flags]",
		Examples: []cli.Example{
			{
				Description: "Mark a CI gate as satisfied",
				Command:     "bureau ticket gate update tkt-a3f9 ci-gate --status satisfied --satisfied-by 'pipeline/build:123'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		RequiredGrants: []string{"command/ticket/gate/update"},
		Run: func(args []string) error {
			if len(args) >= 1 {
				params.Ticket = args[0]
			}
			if len(args) >= 2 {
				params.Gate = args[1]
			}
			if len(args) > 2 {
				return fmt.Errorf("expected 2 positional arguments, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket gate update <ticket-id> <gate-id> --status STATUS")
			}
			if params.Gate == "" {
				return fmt.Errorf("gate ID is required\n\nUsage: bureau ticket gate update <ticket-id> <gate-id> --status STATUS")
			}
			if params.Status == "" {
				return fmt.Errorf("--status is required (pending or satisfied)")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{
				"ticket": params.Ticket,
				"gate":   params.Gate,
				"status": params.Status,
			}
			if params.SatisfiedBy != "" {
				fields["satisfied_by"] = params.SatisfiedBy
			}

			var result mutationResult
			if err := client.Call(ctx, "update-gate", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "gate %q on %s updated to %s\n", params.Gate, result.ID, params.Status)
			return nil
		},
	}
}
