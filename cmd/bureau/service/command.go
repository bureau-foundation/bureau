// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "service" parent command with all subcommands.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "service",
		Summary: "Create, inspect, and manage service principals",
		Description: `Manage service principals across the Bureau fleet.

Services are principals running inside Bureau sandboxes, typically long-running
server processes (ticket service, artifact cache, STT engine, etc.) that provide
capabilities to agents and other services via direct sockets or Matrix.

The "create" command performs the full deployment sequence: register a Matrix
account, provision encrypted credentials, and assign the service to a machine.
The daemon detects the assignment and creates the sandbox.

Inspection commands (list, show) combine Matrix state events with fleet
controller data: replica counts, placement scores, failover policies, and
instance details. Both Matrix and the fleet controller are required.

Placement commands (plan, place, unplace) manage fleet-managed service
placement across machines.`,
		Subcommands: []*cli.Command{
			createCommand(),
			listCommand(),
			showCommand(),
			destroyCommand(),

			// Fleet controller commands.
			planCommand(),
			placeCommand(),
			unplaceCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Create a service on a specific machine",
				Command:     "bureau service create bureau/template:ticket-service --machine machine/workstation --name service/ticket --credential-file ./creds",
			},
			{
				Description: "List all services across all machines",
				Command:     "bureau service list --credential-file ./creds",
			},
			{
				Description: "Show service details (auto-discovers machine)",
				Command:     "bureau service show service/ticket --credential-file ./creds",
			},
			{
				Description: "Preview placement scoring for a service",
				Command:     "bureau service plan service/stt/whisper",
			},
			{
				Description: "Place a service on the best candidate machine",
				Command:     "bureau service place service/stt/whisper",
			},
			{
				Description: "Remove a service assignment",
				Command:     "bureau service destroy service/ticket --credential-file ./creds",
			},
		},
	}
}
