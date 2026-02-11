// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "environment" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "environment",
		Summary: "Manage fleet environment profiles",
		Description: `Manage Nix-based environment profiles for the Bureau fleet.

Each profile defines a complete execution environment for a class of
machine: the set of packages available to Buildbarn runners, sandbox
agents, and tests. Profiles are defined as Nix flake outputs in the
environment repo (bureau-foundation/environment by default).

The nixpkgs pin (flake.lock) in the environment repo determines exact
package versions. All machines evaluating the same lock get byte-
identical binaries, eliminating version drift across the fleet.

Requires nix to be installed and on PATH.`,
		Subcommands: []*cli.Command{
			listCommand(),
			buildCommand(),
			statusCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List available profiles",
				Command:     "bureau environment list",
			},
			{
				Description: "Build the workstation profile for the Buildbarn runner",
				Command:     "bureau environment build workstation",
			},
			{
				Description: "Build from a custom flake",
				Command:     "bureau environment build workstation --flake github:myorg/environment",
			},
			{
				Description: "Show what's currently deployed",
				Command:     "bureau environment status",
			},
		},
	}
}
