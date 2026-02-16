// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// environmentListParams holds the parameters for the environment list command.
type environmentListParams struct {
	cli.JSONOutput
	FlakeRef      string   `json:"flake_ref"       flag:"flake"          desc:"flake reference for the environment repo" default:"github:bureau-foundation/environment"`
	OverrideInput []string `json:"override_input"  flag:"override-input" desc:"override a flake input (format: name=flakeref)"`
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
		Usage:          "bureau environment list [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &[]profileInfo{} },
		RequiredGrants: []string{"command/environment/list"},
		Annotations:    cli.ReadOnly(),
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

			sort.Slice(profiles, func(i, j int) bool {
				return profiles[i].Name < profiles[j].Name
			})

			if done, err := params.EmitJSON(profiles); done {
				return err
			}

			if len(profiles) == 0 {
				fmt.Fprintf(os.Stderr, "No profiles found for %s on %s.\n", params.FlakeRef, currentSystem())
				return nil
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
