// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// environmentStatusParams holds the parameters for the environment status command.
type environmentStatusParams struct {
	cli.JSONOutput
}

func statusCommand() *cli.Command {
	var params environmentStatusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show deployed environment profiles",
		Description: `Show environment profiles that are currently built and available on
this machine. Checks the standard output directory
(/var/bureau/environment/) and the Buildbarn runner-env symlink
for active profile deployments.

Each entry shows the profile name and the Nix store path it resolves
to. If two machines show the same store path, they have byte-identical
environments.`,
		Usage:          "bureau environment status [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &[]statusEntry{} },
		RequiredGrants: []string{"command/environment/status"},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			var entries []statusEntry

			// Check the standard environment directory.
			entries = append(entries, scanDirectory(defaultOutDir)...)

			// Check the Buildbarn runner-env symlink (may be elsewhere
			// in the source tree or an absolute path).
			buildbarnPaths := []string{
				"deploy/buildbarn/runner-env",
				"/var/bureau/buildbarn/runner-env",
			}
			for _, path := range buildbarnPaths {
				if entry, ok := resolveSymlink(path, "buildbarn"); ok {
					entries = append(entries, entry)
				}
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				fmt.Fprintln(os.Stderr, "No environment profiles deployed.")
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "Build one with: bureau environment build <profile>")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(tw, "LOCATION\tSTORE PATH\n")
			for _, entry := range entries {
				fmt.Fprintf(tw, "%s\t%s\n", entry.Location, entry.StorePath)
			}
			return tw.Flush()
		},
	}
}

type statusEntry struct {
	Location  string `json:"location"   desc:"profile symlink location"`
	StorePath string `json:"store_path" desc:"resolved Nix store path"`
}

// scanDirectory reads symlinks in a directory, resolving each to its
// Nix store path.
func scanDirectory(directory string) []statusEntry {
	dirEntries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}

	var entries []statusEntry
	for _, dirEntry := range dirEntries {
		fullPath := filepath.Join(directory, dirEntry.Name())
		target, err := os.Readlink(fullPath)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(target, "/nix/store/") {
			continue
		}
		entries = append(entries, statusEntry{
			Location:  fullPath,
			StorePath: target,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Location < entries[j].Location
	})

	return entries
}

// resolveSymlink checks if a path is a symlink into /nix/store and
// returns a status entry if so.
func resolveSymlink(path, label string) (statusEntry, bool) {
	target, err := os.Readlink(path)
	if err != nil {
		return statusEntry{}, false
	}
	if !strings.HasPrefix(target, "/nix/store/") {
		return statusEntry{}, false
	}
	return statusEntry{
		Location:  fmt.Sprintf("%s (%s)", path, label),
		StorePath: target,
	}, true
}
