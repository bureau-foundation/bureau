// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package matrix implements the bureau matrix subcommands for interacting
// with the Bureau Matrix homeserver. Most subcommands route through the
// agent's proxy socket (which holds Matrix credentials and enforces
// MatrixPolicy). The "setup" subcommand is an exception — it talks
// directly to the homeserver since the proxy doesn't exist yet during
// bootstrap.
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
			SpaceCommand(),
			RoomCommand(),
			UserCommand(),
			SendCommand(),
			StateCommand(),
		},
	}
}
