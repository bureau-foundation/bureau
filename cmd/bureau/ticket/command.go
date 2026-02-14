// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "ticket" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "ticket",
		Summary: "Ticket management commands",
		Description: `View and manage Bureau tickets.

Tickets are work items tracked in Matrix rooms. The ticket viewer provides
an interactive terminal UI for browsing, filtering, and inspecting tickets
loaded from a beads JSONL file or the ticket service.`,
		Subcommands: []*cli.Command{
			ViewerCommand(),
		},
	}
}
