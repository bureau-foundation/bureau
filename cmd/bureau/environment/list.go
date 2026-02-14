// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// environmentListParams holds the parameters for the environment list command.
type environmentListParams struct {
	FlakeRef      string   `json:"flake_ref"       flag:"flake"          desc:"flake reference for the environment repo" default:"github:bureau-foundation/environment"`
	OverrideInput []string `json:"override_input"  flag:"override-input" desc:"override a flake input (format: name=flakeref)"`
	OutputJSON    bool     `json:"-"               flag:"json"           desc:"output as JSON"`
}

func listCommand() *cli.Command {
	var params environmentListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List available environment profiles",
		Description: `Query the environment flake for available profiles on the current
system. Each profile is a Nix package output that can be built with
"bureau environment build <name>".

By default, queries the Bureau environment repo
(github:bureau-foundation/environment). Use --flake to query a
different source.`,
		Usage: "bureau environment list [flags]",
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("list", &params)
		},
		Params: func() any { return &params },
		Examples: []cli.Example{
			{
				Description: "List profiles from the default environment repo",
				Command:     "bureau environment list",
			},
			{
				Description: "List profiles from a local checkout",
				Command:     "bureau environment list --flake path:./environment",
			},
			{
				Description: "Override the bureau input with a local checkout",
				Command:     "bureau environment list --override-input bureau=path:../bureau",
			},
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			options, err := parseOverrideInputs(params.OverrideInput)
			if err != nil {
				return err
			}

			profiles, err := listProfiles(params.FlakeRef, options)
			if err != nil {
				return err
			}

			if len(profiles) == 0 {
				if params.OutputJSON {
					return cli.WriteJSON([]profileInfo{})
				}
				fmt.Fprintf(os.Stderr, "No profiles found for %s on %s.\n", params.FlakeRef, currentSystem())
				return nil
			}

			sort.Slice(profiles, func(i, j int) bool {
				return profiles[i].Name < profiles[j].Name
			})

			if params.OutputJSON {
				return cli.WriteJSON(profiles)
			}

			tw := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(tw, "PROFILE\tPACKAGE\n")
			for _, profile := range profiles {
				fmt.Fprintf(tw, "%s\t%s\n", profile.Name, profile.DerivationName)
			}
			return tw.Flush()
		},
	}
}
