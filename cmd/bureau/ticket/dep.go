// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"os"
	"slices"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// depCommand returns the "dep" subcommand group for managing ticket
// dependencies.
func depCommand() *cli.Command {
	return &cli.Command{
		Name:    "dep",
		Summary: "Manage ticket dependencies",
		Description: `Add or remove blocked_by dependencies between tickets.

These are convenience wrappers around "ticket update" that perform
a read-modify-write cycle: fetch the current ticket, modify its
blocked_by list, and send the update. The service validates that no
dependency cycles are created.`,
		Subcommands: []*cli.Command{
			depAddCommand(),
			depRemoveCommand(),
		},
	}
}

// --- dep add ---

type depAddParams struct {
	TicketConnection
	Ticket     string `json:"ticket"     desc:"ticket ID to modify"          required:"true"`
	DependsOn  string `json:"depends_on" desc:"ticket ID that blocks this one" required:"true"`
	OutputJSON bool   `json:"-"          flag:"json" desc:"output as JSON"`
}

func depAddCommand() *cli.Command {
	var params depAddParams

	return &cli.Command{
		Name:    "add",
		Summary: "Add a dependency (blocked_by)",
		Description: `Add a ticket to this ticket's blocked_by list. The target ticket
must be closed before this ticket becomes ready.

The service rejects the addition if it would create a dependency
cycle.`,
		Usage: "bureau ticket dep add <ticket-id> <depends-on-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Make tkt-a3f9 depend on tkt-b2c1",
				Command:     "bureau ticket dep add tkt-a3f9 tkt-b2c1",
			},
		},
		Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("dep-add", &params) },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/ticket/dep/add"},
		Run: func(args []string) error {
			if len(args) >= 1 {
				params.Ticket = args[0]
			}
			if len(args) >= 2 {
				params.DependsOn = args[1]
			}
			if len(args) > 2 {
				return fmt.Errorf("expected 2 positional arguments, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket dep add <ticket-id> <depends-on-id>")
			}
			if params.DependsOn == "" {
				return fmt.Errorf("depends-on ticket ID is required\n\nUsage: bureau ticket dep add <ticket-id> <depends-on-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			// Read-modify-write: fetch current ticket, add dependency,
			// send update with the full blocked_by list.
			var current showResult
			if err := client.Call(ctx, "show", map[string]any{"ticket": params.Ticket}, &current); err != nil {
				return fmt.Errorf("fetching ticket: %w", err)
			}

			blockedBy := current.Content.BlockedBy
			if slices.Contains(blockedBy, params.DependsOn) {
				fmt.Fprintf(os.Stderr, "%s already depends on %s\n", params.Ticket, params.DependsOn)
				return nil
			}

			blockedBy = append(blockedBy, params.DependsOn)

			var result mutationResult
			if err := client.Call(ctx, "update", map[string]any{
				"ticket":     params.Ticket,
				"blocked_by": blockedBy,
			}, &result); err != nil {
				return err
			}

			if params.OutputJSON {
				return cli.WriteJSON(result)
			}

			fmt.Fprintf(os.Stderr, "%s now depends on %s\n", params.Ticket, params.DependsOn)
			return nil
		},
	}
}

// --- dep remove ---

type depRemoveParams struct {
	TicketConnection
	Ticket     string `json:"ticket"     desc:"ticket ID to modify"                  required:"true"`
	DependsOn  string `json:"depends_on" desc:"ticket ID to remove from blocked_by"  required:"true"`
	OutputJSON bool   `json:"-"          flag:"json" desc:"output as JSON"`
}

func depRemoveCommand() *cli.Command {
	var params depRemoveParams

	return &cli.Command{
		Name:        "remove",
		Summary:     "Remove a dependency (blocked_by)",
		Description: `Remove a ticket from this ticket's blocked_by list.`,
		Usage:       "bureau ticket dep remove <ticket-id> <depends-on-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Remove tkt-b2c1 from tkt-a3f9's dependencies",
				Command:     "bureau ticket dep remove tkt-a3f9 tkt-b2c1",
			},
		},
		Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("dep-remove", &params) },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/ticket/dep/remove"},
		Run: func(args []string) error {
			if len(args) >= 1 {
				params.Ticket = args[0]
			}
			if len(args) >= 2 {
				params.DependsOn = args[1]
			}
			if len(args) > 2 {
				return fmt.Errorf("expected 2 positional arguments, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket dep remove <ticket-id> <depends-on-id>")
			}
			if params.DependsOn == "" {
				return fmt.Errorf("depends-on ticket ID is required\n\nUsage: bureau ticket dep remove <ticket-id> <depends-on-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			// Read-modify-write: fetch current ticket, remove dependency,
			// send update with the modified blocked_by list.
			var current showResult
			if err := client.Call(ctx, "show", map[string]any{"ticket": params.Ticket}, &current); err != nil {
				return fmt.Errorf("fetching ticket: %w", err)
			}

			blockedBy := current.Content.BlockedBy
			index := slices.Index(blockedBy, params.DependsOn)
			if index < 0 {
				return fmt.Errorf("%s does not depend on %s", params.Ticket, params.DependsOn)
			}

			blockedBy = slices.Delete(blockedBy, index, index+1)

			var result mutationResult
			if err := client.Call(ctx, "update", map[string]any{
				"ticket":     params.Ticket,
				"blocked_by": blockedBy,
			}, &result); err != nil {
				return err
			}

			if params.OutputJSON {
				return cli.WriteJSON(result)
			}

			fmt.Fprintf(os.Stderr, "%s no longer depends on %s\n", params.Ticket, params.DependsOn)
			return nil
		},
	}
}
