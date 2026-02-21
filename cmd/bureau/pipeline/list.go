// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// listParams holds the parameters for the pipeline list command. The
// Room field is positional in CLI mode (args[0]) and a named property
// in JSON/MCP mode.
type listParams struct {
	cli.JSONOutput
	Room       string `json:"room"         desc:"room alias localpart (e.g. bureau/pipeline)" required:"true"`
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
}

// pipelineEntry is a single pipeline in the list output. Declared at
// package level so the MCP server can reflect its type for outputSchema.
type pipelineEntry struct {
	Name        string `json:"name"        desc:"pipeline name (state key)"`
	Description string `json:"description" desc:"human-readable pipeline description"`
	Steps       int    `json:"steps"       desc:"number of steps in the pipeline"`
}

// listCommand returns the "list" subcommand for listing pipelines in a room.
func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List pipelines in a room",
		Description: `List all automation pipelines in a Matrix room. Shows each pipeline's
name, description, and step count.

The room argument is a room alias localpart (e.g., "bureau/pipeline").
It is resolved to a full Matrix alias using the --server-name flag.`,
		Usage: "bureau pipeline list [flags] <room-alias-localpart>",
		Examples: []cli.Example{
			{
				Description: "List built-in pipelines",
				Command:     "bureau pipeline list bureau/pipeline",
			},
			{
				Description: "List project pipelines as JSON",
				Command:     "bureau pipeline list --json iree/pipeline",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]pipelineEntry{} },
		RequiredGrants: []string{"command/pipeline/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			// In CLI mode, the room comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau pipeline list [flags] <room-alias-localpart>")
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			roomAlias := ref.MustParseRoomAlias(schema.FullRoomAlias(params.Room, serverName))

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return cli.NotFound("resolving room alias %q: %w", roomAlias, err)
			}

			// Fetch all state events in the room, then filter for pipelines.
			events, err := session.GetRoomState(ctx, roomID)
			if err != nil {
				return cli.Internal("getting room state: %w", err)
			}

			var pipelines []pipelineEntry
			for _, event := range events {
				if event.Type != schema.EventTypePipeline {
					continue
				}
				if event.StateKey == nil {
					continue
				}

				description, _ := event.Content["description"].(string)

				// Extract step count from the untyped content map.
				stepCount := 0
				if steps, ok := event.Content["steps"].([]any); ok {
					stepCount = len(steps)
				}

				pipelines = append(pipelines, pipelineEntry{
					Name:        *event.StateKey,
					Description: description,
					Steps:       stepCount,
				})
			}

			if done, err := params.EmitJSON(pipelines); done {
				return err
			}

			if len(pipelines) == 0 {
				fmt.Fprintf(os.Stderr, "no pipelines found in %s\n", roomAlias)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "NAME\tDESCRIPTION\tSTEPS\n")
			for _, entry := range pipelines {
				fmt.Fprintf(writer, "%s\t%s\t%d\n", entry.Name, entry.Description, entry.Steps)
			}
			return writer.Flush()
		},
	}
}
