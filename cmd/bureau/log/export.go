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

type exportParams struct {
	LogArtifactConnection
	Session    string `json:"session"    flag:"session"    desc:"session ID (defaults to latest)"`
	OutputPath string `json:"output"     flag:"output,o"   desc:"output file path (required)"`
}

func exportCommand() *cli.Command {
	var params exportParams

	return &cli.Command{
		Name:    "export",
		Summary: "Export captured terminal output to a file",
		Description: `Export the raw terminal output from a captured session to a file.

The source argument identifies the principal whose output to export
(e.g., "my_bureau/fleet/prod/agent/reviewer"). If multiple sessions
exist for the source, the latest is exported unless --session is
specified.

Output is written as raw bytes, preserving the exact byte stream
captured from the process's terminal.`,
		Usage: "bureau log export <source> -o <file> [flags]",
		Examples: []cli.Example{
			{
				Description: "Export latest output from an agent",
				Command:     "bureau log export my_bureau/fleet/prod/agent/reviewer -o output.bin --service",
			},
			{
				Description: "Export a specific session",
				Command:     "bureau log export my_bureau/fleet/prod/agent/reviewer -o output.bin --session abc123 --service",
			},
		},
		Params:         func() any { return &params },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/log/export"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("source argument required\n\nUsage: bureau log export <source> -o <file> [flags]")
			}
			source := args[0]

			if params.OutputPath == "" {
				return cli.Validation("output path required (-o <file>)\n\nUsage: bureau log export <source> -o <file> [flags]")
			}

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

			if len(content.Chunks) == 0 {
				return fmt.Errorf("session %s has no stored output", tagName)
			}

			file, err := os.Create(params.OutputPath)
			if err != nil {
				return cli.Internal("creating output file: %w", err)
			}
			defer file.Close()

			if err := writeChunks(ctx, client, content.Chunks, file); err != nil {
				// Clean up partial file on error.
				file.Close()
				os.Remove(params.OutputPath)
				return err
			}

			logger.Info("exported output",
				"tag", tagName,
				"status", string(content.Status),
				"size", formatBytes(content.TotalBytes),
				"chunks", len(content.Chunks),
				"output", params.OutputPath,
			)
			return nil
		},
	}
}
