// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "ticket" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "ticket",
		Summary: "Manage Bureau tickets",
		Description: `View and manage Bureau tickets via the ticket service.

Tickets are work items tracked as Matrix state events. The ticket
service maintains an in-memory index for fast queries and manages
lifecycle transitions, dependency graphs, and gate coordination.

Commands connect to the ticket service's Unix socket. Inside a
sandbox, the socket and token paths default to the standard
provisioned locations. Outside a sandbox, use --socket and
--token-file flags (or BUREAU_TICKET_SOCKET and BUREAU_TICKET_TOKEN
environment variables).

Room-scoped commands (list, ready, blocked, ranked, stats) require
a --room flag with the Matrix room ID. Ticket-scoped commands (show,
close, deps) take a ticket ID as a positional argument and look up
the ticket across all rooms.`,
		Subcommands: []*cli.Command{
			// Operator commands.
			enableCommand(),

			// Query commands.
			listCommand(),
			showCommand(),
			readyCommand(),
			blockedCommand(),
			rankedCommand(),
			grepCommand(),
			statsCommand(),
			infoCommand(),
			depsCommand(),
			childrenCommand(),
			epicHealthCommand(),
			upcomingCommand(),

			// Mutation commands.
			createCommand(),
			updateCommand(),
			closeCommand(),
			reopenCommand(),
			batchCommand(),
			deferCommand(),

			// Transfer commands.
			exportCommand(),
			importCommand(),

			// Subcommand groups.
			gateCommand(),
			depCommand(),

			// Interactive viewer (pre-existing).
			ViewerCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List open tickets in a room",
				Command:     "bureau ticket list --room '!abc:bureau.local' --status open",
			},
			{
				Description: "Show ticket details",
				Command:     "bureau ticket show tkt-a3f9",
			},
			{
				Description: "List ready tickets (unblocked, all gates satisfied)",
				Command:     "bureau ticket ready --room '!abc:bureau.local'",
			},
			{
				Description: "Create a new ticket",
				Command:     "bureau ticket create --room '!abc:bureau.local' --title 'Fix auth' --type bug --priority 1",
			},
			{
				Description: "Close a ticket",
				Command:     "bureau ticket close tkt-a3f9 --reason 'Fixed in commit abc'",
			},
			{
				Description: "Search tickets by regex",
				Command:     "bureau ticket grep 'memory.*leak'",
			},
			{
				Description: "Show ranked tickets for assignment",
				Command:     "bureau ticket ranked --room '!abc:bureau.local' --json",
			},
			{
				Description: "Export tickets to a file",
				Command:     "bureau ticket export --room '!abc:bureau.local' --file archive.jsonl",
			},
			{
				Description: "Import tickets into a room",
				Command:     "bureau ticket import --room '!new:bureau.local' --file archive.jsonl",
			},
		},
	}
}
