// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package suggest implements the "bureau suggest" command, which
// searches the entire CLI command tree by natural language description
// using BM25 relevance ranking.
package suggest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "suggest" command. The root parameter is the
// top-level CLI command tree, used to build the BM25 search index
// over all leaf commands.
func Command(root *cli.Command) *cli.Command {
	return &cli.Command{
		Name:    "suggest",
		Summary: "Search for commands by natural language description",
		Description: `Search all Bureau commands using natural language queries.

Uses BM25 relevance ranking to match your query against command names,
descriptions, and flag metadata across the entire command tree.

Unlike tab completion or typo correction, this finds commands by what
they do, not just by name similarity.`,
		Usage: "bureau suggest <query...>",
		Examples: []cli.Example{
			{
				Description: "Find commands related to user creation",
				Command:     `bureau suggest "how do I create a user"`,
			},
			{
				Description: "Find commands for credential management",
				Command:     `bureau suggest "provision credentials"`,
			},
			{
				Description: "Find pipeline-related commands",
				Command:     "bureau suggest pipeline",
			},
		},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("query required\n\nUsage: bureau suggest <query...>")
			}

			query := strings.Join(args, " ")
			suggestions := cli.SuggestSemantic(query, root, 10)

			if len(suggestions) == 0 {
				fmt.Printf("No commands match %q.\n", query)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
			for _, suggestion := range suggestions {
				fmt.Fprintf(writer, "  %s\t%s\n", suggestion.Path, suggestion.Summary)
			}
			writer.Flush()
			return nil
		},
	}
}
