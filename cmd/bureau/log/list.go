// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
	logschema "github.com/bureau-foundation/bureau/lib/schema/log"
)

// listEntry is the output type for the list command. Flattens
// LogContent metadata for tabular display with desc tags for MCP
// schema generation.
type listEntry struct {
	Source     string `json:"source"      desc:"source entity localpart"`
	SessionID  string `json:"session_id"  desc:"session identifier"`
	Status     string `json:"status"      desc:"lifecycle status: active, complete, or rotating"`
	TotalBytes int64  `json:"total_bytes" desc:"total output size in bytes"`
	Chunks     int    `json:"chunks"      desc:"number of stored output chunks"`
	Tag        string `json:"tag"         desc:"artifact tag name for this session"`
}

type listParams struct {
	LogArtifactConnection
	cli.JSONOutput
	Source string `json:"source" flag:"source,s" desc:"filter by source localpart prefix"`
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List captured output sessions",
		Description: `List output capture sessions stored in the artifact service.

Each session corresponds to one sandbox invocation's terminal output.
The session metadata includes the source (producing principal), status
(active, complete, or rotating), total output size, and chunk count.

Filter by source with --source to narrow results to a specific
principal or source prefix.`,
		Usage: "bureau log list [flags]",
		Examples: []cli.Example{
			{
				Description: "List all output sessions",
				Command:     "bureau log list --service",
			},
			{
				Description: "List sessions for a specific agent",
				Command:     "bureau log list --source my_bureau/fleet/prod/agent/reviewer --service",
			},
			{
				Description: "List as JSON",
				Command:     "bureau log list --service --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]listEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/log/list"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			// Build tag prefix for the search. All log metadata tags
			// use the naming convention "log/<source-localpart>/<session-id>".
			prefix := "log/"
			if params.Source != "" {
				prefix = "log/" + params.Source + "/"
			}

			tagsResponse, err := client.Tags(ctx, prefix)
			if err != nil {
				return fmt.Errorf("listing log tags: %w", err)
			}

			if len(tagsResponse.Tags) == 0 {
				if done, err := params.EmitJSON([]listEntry{}); done {
					return err
				}
				logger.Info("no output sessions found")
				return nil
			}

			// Fetch metadata for each tag to get full session details.
			// Log metadata artifacts are small (~500 bytes each), so
			// sequential fetches are acceptable for CLI latency.
			var entries []listEntry
			for _, tag := range tagsResponse.Tags {
				result, err := client.Fetch(ctx, tag.Name)
				if err != nil {
					logger.Warn("failed to fetch log metadata",
						"tag", tag.Name,
						"error", err,
					)
					continue
				}

				data, err := io.ReadAll(result.Content)
				result.Content.Close()
				if err != nil {
					logger.Warn("failed to read log metadata",
						"tag", tag.Name,
						"error", err,
					)
					continue
				}

				var content logschema.LogContent
				if err := codec.Unmarshal(data, &content); err != nil {
					logger.Warn("failed to decode log metadata",
						"tag", tag.Name,
						"error", err,
					)
					continue
				}

				entry := listEntry{
					SessionID:  content.SessionID,
					Status:     string(content.Status),
					TotalBytes: content.TotalBytes,
					Chunks:     len(content.Chunks),
					Tag:        tag.Name,
				}
				if !content.Source.IsZero() {
					entry.Source = content.Source.Localpart()
				}
				entries = append(entries, entry)
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no output sessions found")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "SOURCE\tSESSION\tSTATUS\tSIZE\tCHUNKS\n")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\n",
					entry.Source,
					truncateSession(entry.SessionID),
					entry.Status,
					formatBytes(entry.TotalBytes),
					entry.Chunks,
				)
			}
			return writer.Flush()
		},
	}
}

// truncateSession shortens a session ID for display. Session IDs are
// typically UUIDs; showing the first 12 characters is enough to
// distinguish sessions while keeping the table compact.
func truncateSession(sessionID string) string {
	if len(sessionID) <= 12 {
		return sessionID
	}
	return sessionID[:12]
}
