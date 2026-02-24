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
		Description: `Manage fleet machines: provisioning, listing, upgrades, decommission, and revocation.

The "provision" subcommand creates a machine's Matrix account and writes
a bootstrap config file. Transfer this file to the new machine and start
the launcher with --bootstrap-file to complete registration.

The "list" subcommand shows all machines that have published keys to the
fleet's machine room.

The "upgrade" subcommand publishes a BureauVersion state event to trigger
the daemon's binary self-update mechanism. Point it at a bureau-host-env
Nix derivation and the daemon handles the rest: prefetching, hash
comparison, and atomic exec() transitions.

The "decommission" subcommand removes a machine from the fleet: clears
its state events, kicks it from all rooms, and cleans up its config room.

The "revoke" subcommand is for emergency credential revocation of a
compromised machine. It deactivates the machine's Matrix account (forcing
the daemon to self-destruct), clears all state, and publishes a
revocation event for fleet-wide notification.`,
		Subcommands: []*cli.Command{
			provisionCommand(),
			listCommand(),
			upgradeCommand(),
			decommissionCommand(),
			revokeCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Provision a new worker machine",
				Command:     "bureau machine provision bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds --output bootstrap.json",
			},
			{
				Description: "List all fleet machines",
				Command:     "bureau machine list bureau/fleet/prod --credential-file ./bureau-creds",
			},
			{
				Description: "Upgrade the local machine's Bureau binaries",
				Command:     "bureau machine upgrade --local --host-env /nix/store/...-bureau-host-env --credential-file ./bureau-creds",
			},
			{
				Description: "Remove a machine from the fleet",
				Command:     "bureau machine decommission bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
			{
				Description: "Emergency revoke a compromised machine",
				Command:     "bureau machine revoke bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds --reason 'suspected compromise'",
			},
		},
	}
}
