// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "fleet" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "fleet",
		Summary: "Fleet controller setup and machine diagnostics",
		Description: `Commands for fleet controller setup, configuration, and machine
diagnostics.

A fleet is an infrastructure isolation boundary within a namespace.
Each fleet has its own machines, services, and fleet controller.
Use "bureau fleet create" to create the fleet rooms, then
"bureau fleet enable" to bootstrap a fleet controller on a machine.

Service-related operations (list, show, plan, place, unplace) live
under "bureau service". The service commands automatically enrich
output with fleet controller data when a fleet controller is
reachable.

Socket-based commands (status, list-machines, show-machine) connect
to the fleet controller's Unix socket. Inside a sandbox, the socket
and token paths default to the standard provisioned locations. Outside
a sandbox, use --socket and --token-file flags (or BUREAU_FLEET_SOCKET
and BUREAU_FLEET_TOKEN environment variables).

Operator commands (create, enable, config) use direct Matrix access
via --credential-file.`,
		Subcommands: []*cli.Command{
			// Operator commands.
			createCommand(),
			enableCommand(),
			configCommand(),

			// Query commands.
			statusCommand(),
			listMachinesCommand(),
			showMachineCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Create a production fleet",
				Command:     "bureau fleet create bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Check fleet controller status",
				Command:     "bureau fleet status",
			},
			{
				Description: "List all tracked machines with health info",
				Command:     "bureau fleet list-machines",
			},
			{
				Description: "Show detailed info for a machine",
				Command:     "bureau fleet show-machine machine/workstation",
			},
			{
				Description: "Bootstrap the fleet controller on a machine",
				Command:     "bureau fleet enable bureau/fleet/prod --host workstation --credential-file ./creds",
			},
		},
	}
}
