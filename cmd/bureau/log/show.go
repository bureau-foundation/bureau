// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

type showParams struct {
	LogArtifactConnection
	Session string `json:"session" flag:"session" desc:"session ID (defaults to latest)"`
}

func showCommand() *cli.Command {
	var params showParams

	return &cli.Command{
		Name:    "show",
		Summary: "Display captured terminal output",
		Description: `Display the raw terminal output from a captured session.

The source argument identifies the principal whose output to display
(e.g., "my_bureau/fleet/prod/agent/reviewer"). If multiple sessions
exist for the source, the latest is shown unless --session is specified.

Output is written to stdout as raw bytes, preserving ANSI escape
sequences and terminal formatting. Pipe through 'less -R' for paged
viewing with color support.`,
		Usage: "bureau log show <source> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show latest output from an agent",
				Command:     "bureau log show my_bureau/fleet/prod/agent/reviewer --service",
			},
			{
				Description: "Show a specific session",
				Command:     "bureau log show my_bureau/fleet/prod/agent/reviewer --session abc123 --service",
			},
			{
				Description: "Page output with color support",
				Command:     "bureau log show my_bureau/fleet/prod/agent/reviewer --service | less -R",
			},
		},
		Params:         func() any { return &params },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/log/show"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("source argument required\n\nUsage: bureau log show <source> [flags]")
			}
			source := args[0]

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			content, tagName, err := resolveLogContent(ctx, client, source, params.Session)
			if err != nil {
				return err
			}

			logger.Info("showing output",
				"tag", tagName,
				"status", content.Status,
				"size", formatBytes(content.TotalBytes),
				"chunks", len(content.Chunks),
			)

			if len(content.Chunks) == 0 {
				fmt.Fprintln(os.Stderr, "session has no stored output")
				return nil
			}

			return writeChunks(ctx, client, content.Chunks, os.Stdout)
		},
	}
}
