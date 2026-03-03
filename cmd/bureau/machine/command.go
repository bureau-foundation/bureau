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
		Description: `Manage fleet machines: provisioning, deployment, health checks, upgrades,
decommission, uninstall, and emergency revocation.

The "list" subcommand shows all machines in the fleet with live
operational data from the fleet controller: CPU and memory utilization,
assignment counts, and labels.

The "show" subcommand displays detailed info for a single machine:
hardware inventory, current resource usage, and all assigned principals.

The "provision" subcommand creates a machine's Matrix account and writes
a bootstrap config file. Transfer this file to the new machine and run
"deploy local" to complete setup.

The "deploy" subcommand sets up Bureau on a target machine from the
bootstrap config: runs "doctor --fix" for infrastructure, executes
launcher first boot for homeserver registration, and starts services.

The "doctor" subcommand checks and optionally repairs the local machine's
Bureau infrastructure (system user, directories, binaries, systemd units,
sockets, Matrix connectivity).

The "upgrade" subcommand publishes a BureauVersion state event to trigger
the daemon's binary self-update mechanism. Point it at a bureau-host-env
Nix derivation and the daemon handles the rest: prefetching, hash
comparison, and atomic exec() transitions.

The "decommission" subcommand removes a machine from the fleet: clears
its state events, kicks it from all rooms, and cleans up its config room.

The "revoke" subcommand is for emergency credential revocation of a
compromised machine. It deactivates the machine's Matrix account (forcing
the daemon to self-destruct), clears all state, and publishes a
revocation event for fleet-wide notification.

The "cordon" subcommand adds the "cordoned" label to a machine, making
it ineligible for new fleet placements. Existing workloads continue.

The "drain" subcommand evacuates all fleet-managed services from a machine,
redistributing them across the fleet via the placement scoring engine. The
machine is automatically cordoned first to prevent new placements during
and after the drain. Use "uncordon" to re-enable the machine after
maintenance.

The "uncordon" subcommand removes the "cordoned" label, re-enabling
the machine for new placements.

The "label" subcommand adds, updates, or removes arbitrary labels on a
machine. Labels drive fleet placement constraints.

The "uninstall" subcommand removes Bureau from the local machine: stops
services, removes unit files, binaries, directories, and configuration.`,
		Subcommands: []*cli.Command{
			doctorCommand(),
			deployCommand(),
			provisionCommand(),
			listCommand(),
			showCommand(),
			upgradeCommand(),
			cordonCommand(),
			drainCommand(),
			uncordonCommand(),
			labelCommand(),
			decommissionCommand(),
			revokeCommand(),
			uninstallCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List all fleet machines",
				Command:     "bureau machine list bureau/fleet/prod --credential-file ./bureau-creds",
			},
			{
				Description: "Show detailed info for a machine",
				Command:     "bureau machine show machine/workstation",
			},
			{
				Description: "Provision a new worker machine",
				Command:     "bureau machine provision bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds --output bootstrap.json",
			},
			{
				Description: "Deploy Bureau locally from a bootstrap config",
				Command:     "sudo bureau machine deploy local --bootstrap-file bootstrap.json",
			},
			{
				Description: "Upgrade the local machine's Bureau binaries",
				Command:     "bureau machine upgrade --local --host-env /nix/store/...-bureau-host-env --credential-file ./bureau-creds",
			},
			{
				Description: "Cordon a machine for maintenance",
				Command:     "bureau machine cordon bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
			{
				Description: "Drain all services before maintenance",
				Command:     "bureau machine drain bureau/fleet/prod/machine/worker-01 --service",
			},
			{
				Description: "Set labels for placement constraints",
				Command:     "bureau machine label bureau/fleet/prod/machine/worker-01 gpu=h100 tier=production --credential-file ./bureau-creds",
			},
			{
				Description: "Remove a machine from the fleet",
				Command:     "bureau machine decommission bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
			{
				Description: "Emergency revoke a compromised machine",
				Command:     "bureau machine revoke bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds --reason 'suspected compromise'",
			},
			{
				Description: "Remove Bureau from this machine",
				Command:     "sudo bureau machine uninstall --dry-run",
			},
		},
	}
}
