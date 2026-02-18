// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "matrix" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "matrix",
		Summary: "Matrix homeserver operations",
		Description: `Manage the Bureau Matrix homeserver: bootstrap, spaces, rooms, users,
and messaging.

The "setup" subcommand bootstraps a fresh homeserver (creates admin
account, spaces, and rooms). All other subcommands operate on an
existing deployment, routing through the agent's proxy socket.`,
		Subcommands: []*cli.Command{
			SetupCommand(),
			DoctorCommand(),
			SpaceCommand(),
			RoomCommand(),
			UserCommand(),
			SendCommand(),
			StateCommand(),
			InspectCommand(),
			WatchCommand(),
			MessagesCommand(),
		},
	}
}
