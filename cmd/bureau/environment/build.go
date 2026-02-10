// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// defaultOutDir is the standard location for built environment profiles.
// Each profile gets its own symlink: /var/bureau/environment/<profile>.
const defaultOutDir = "/var/bureau/environment"

func buildCommand() *cli.Command {
	var (
		flakeRef      string
		outLink       string
		overrideInput []string
	)

	return &cli.Command{
		Name:    "build",
		Summary: "Build an environment profile",
		Description: `Build a Nix environment profile and place the result symlink at a
standard location. The profile name corresponds to a package output
in the environment flake (use "bureau environment list" to see what's
available).

The default output location is /var/bureau/environment/<profile>. Use
--out-link to place the symlink elsewhere (e.g., for the Buildbarn
runner).

The build output is a directory containing bin/, lib/, share/, etc.
with symlinks into /nix/store for all packages in the profile.`,
		Usage: "bureau environment build <profile> [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("build", pflag.ContinueOnError)
			flagSet.StringVar(&flakeRef, "flake", defaultFlakeRef, "flake reference for the environment repo")
			flagSet.StringVar(&outLink, "out-link", "", "output symlink path (default: /var/bureau/environment/<profile>)")
			flagSet.StringArrayVar(&overrideInput, "override-input", nil, "override a flake input (format: name=flakeref)")
			return flagSet
		},
		Examples: []cli.Example{
			{
				Description: "Build the workstation profile",
				Command:     "bureau environment build workstation",
			},
			{
				Description: "Build for the Buildbarn runner",
				Command:     "bureau environment build workstation --out-link deploy/buildbarn/runner-env",
			},
			{
				Description: "Build from a local checkout",
				Command:     "bureau environment build workstation --flake path:./environment",
			},
		},
		Run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau environment build <profile>")
			}

			profile := args[0]

			if outLink == "" {
				outLink = filepath.Join(defaultOutDir, profile)
			}

			options, err := parseOverrideInputs(overrideInput)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Building profile %q from %s...\n", profile, flakeRef)

			storePath, err := buildProfile(flakeRef, profile, outLink, options)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "Built %s\n", storePath)
			fmt.Fprintf(os.Stdout, "  -> %s\n", outLink)
			return nil
		},
	}
}
