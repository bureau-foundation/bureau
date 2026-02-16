// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "fleet" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "fleet",
		Summary: "Fleet management operations",
		Description: `Commands for managing fleet controller placement, health monitoring,
and configuration.

The fleet controller manages service placement across machines. It
watches for FleetServiceContent events in #bureau/fleet, scores
candidate machines based on resource availability and placement
constraints, and writes PrincipalAssignment events to machine config
rooms.

Socket-based commands (status, list-machines, list-services, show,
place, unplace, plan) connect to the fleet controller's Unix socket.
Inside a sandbox, the socket and token paths default to the standard
provisioned locations. Outside a sandbox, use --socket and
--token-file flags (or BUREAU_FLEET_SOCKET and BUREAU_FLEET_TOKEN
environment variables).

Operator commands (enable, config) use direct Matrix access via
--credential-file.`,
		Subcommands: []*cli.Command{
			// Operator commands.
			enableCommand(),
			configCommand(),

			// Query commands.
			statusCommand(),
			listMachinesCommand(),
			listServicesCommand(),
			showMachineCommand(),
			showServiceCommand(),
			planCommand(),

			// Mutation commands.
			placeCommand(),
			unplaceCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Check fleet controller status",
				Command:     "bureau fleet status",
			},
			{
				Description: "List all tracked machines with health info",
				Command:     "bureau fleet list-machines",
			},
			{
				Description: "List all fleet-managed services",
				Command:     "bureau fleet list-services",
			},
			{
				Description: "Show detailed info for a machine",
				Command:     "bureau fleet show-machine machine/workstation",
			},
			{
				Description: "Show a service's placement and definition",
				Command:     "bureau fleet show-service service/stt/whisper",
			},
			{
				Description: "Dry-run placement scoring for a service",
				Command:     "bureau fleet plan service/stt/whisper",
			},
			{
				Description: "Place a service on the best candidate machine",
				Command:     "bureau fleet place service/stt/whisper",
			},
			{
				Description: "Bootstrap the fleet controller on a machine",
				Command:     "bureau fleet enable --name prod --host machine/workstation --credential-file ./creds",
			},
		},
	}
}
