// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "fleet" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "fleet",
		Summary: "Fleet controller setup and machine diagnostics",
		Description: `Commands for fleet controller setup and configuration.

A fleet is an infrastructure isolation boundary within a namespace.
Each fleet has its own machines, services, and fleet controller.
Use "bureau fleet setup" to create the fleet rooms and deploy the
fleet controller in one step, or use "bureau fleet create" and
"bureau fleet enable" separately for staged provisioning.

Service-related operations (list, show, plan, place, unplace) live
under "bureau service". Machine-related operations (list, show) live
under "bureau machine". Both command groups require a fleet controller
connection for enrichment data.

The "status" command connects to the fleet controller's Unix socket
for aggregate health. Operator commands (setup, create, enable, config)
use direct Matrix access via --credential-file.`,
		Subcommands: []*cli.Command{
			// Operator commands.
			setupCommand(),
			createCommand(),
			enableCommand(),
			configCommand(),

			// Query commands.
			statusCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Set up a fleet (create rooms + deploy controller)",
				Command:     "bureau fleet setup bureau/fleet/prod --host workstation --credential-file ./creds",
			},
			{
				Description: "Check fleet controller status",
				Command:     "bureau fleet status",
			},
			{
				Description: "Create fleet rooms without deploying a controller",
				Command:     "bureau fleet create bureau/fleet/prod --credential-file ./creds",
			},
		},
	}
}
