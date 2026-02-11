// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "machine" parent command with all subcommands.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "machine",
		Summary: "Manage fleet machines",
		Description: `Provision, list, and decommission machines in the Bureau fleet.

The "provision" subcommand creates a machine's Matrix account and writes
a bootstrap config file. Transfer this file to the new machine and start
the launcher with --bootstrap-file to complete registration.

The "list" subcommand shows all machines that have published keys to the
fleet's machines room.

The "decommission" subcommand removes a machine from the fleet: clears
its state events, kicks it from all rooms, and cleans up its config room.`,
		Subcommands: []*cli.Command{
			provisionCommand(),
			listCommand(),
			decommissionCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Provision a new worker machine",
				Command:     "bureau machine provision machine/worker-01 --credential-file ./bureau-creds --output bootstrap.json",
			},
			{
				Description: "List all fleet machines",
				Command:     "bureau machine list --credential-file ./bureau-creds",
			},
			{
				Description: "Remove a machine from the fleet",
				Command:     "bureau machine decommission machine/worker-01 --credential-file ./bureau-creds",
			},
		},
	}
}
