// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// defaultOutDir is the standard location for built environment profiles.
// Each profile gets its own symlink: /var/bureau/environment/<profile>.
const defaultOutDir = "/var/bureau/environment"

// buildParams holds the parameters for the environment build command.
// Profile is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type buildParams struct {
	cli.JSONOutput
	Profile       string   `json:"profile"         desc:"Nix profile name to build" required:"true"`
	FlakeRef      string   `json:"flake_ref"       flag:"flake"          desc:"flake reference for the environment repo" default:"github:bureau-foundation/environment"`
	OutLink       string   `json:"-"               flag:"out-link"       desc:"output symlink path (default: /var/bureau/environment/<profile>)"`
	OverrideInput []string `json:"override_input"  flag:"override-input" desc:"override a flake input (format: name=flakeref)"`
}

// buildResult is the JSON output for environment build.
type buildResult struct {
	Profile   string `json:"profile"    desc:"Nix profile name"`
	StorePath string `json:"store_path" desc:"Nix store path"`
	OutLink   string `json:"out_link"   desc:"output symlink path"`
}

func buildCommand() *cli.Command {
	var params buildParams

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
		Usage:          "bureau environment build <profile> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &buildResult{} },
		RequiredGrants: []string{"command/environment/build"},
		Annotations:    cli.Create(),
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
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, profile comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Profile = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau environment build <profile>")
			}
			if params.Profile == "" {
				return cli.Validation("profile is required\n\nusage: bureau environment build <profile>")
			}

			profile := params.Profile

			outLink := params.OutLink
			if outLink == "" {
				outLink = filepath.Join(defaultOutDir, profile)
			}

			options, err := parseOverrideInputs(params.OverrideInput)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Building profile %q from %s...\n", profile, params.FlakeRef)

			storePath, err := buildProfile(params.FlakeRef, profile, outLink, options)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(buildResult{
				Profile:   profile,
				StorePath: storePath,
				OutLink:   outLink,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Built %s\n", storePath)
			fmt.Fprintf(os.Stdout, "  -> %s\n", outLink)
			return nil
		},
	}
}
