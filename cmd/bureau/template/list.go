// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// listParams holds the parameters for the template list command. The
// Room field is positional in CLI mode (args[0]) and a named property
// in JSON/MCP mode.
type listParams struct {
	cli.JSONOutput
	Room       string `json:"room"         desc:"room alias localpart (e.g. bureau/template)" required:"true"`
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
}

// templateEntry is a single template in the list output. Declared at
// package level so the MCP server can reflect its type for outputSchema.
type templateEntry struct {
	Name        string   `json:"name"                desc:"template name (state key)"`
	Description string   `json:"description"         desc:"human-readable template description"`
	Inherits    []string `json:"inherits,omitempty"   desc:"parent template references (e.g. bureau/template:base)"`
}

// listCommand returns the "list" subcommand for listing templates in a room.
func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List templates in a room",
		Description: `List all sandbox templates in a Matrix room. Shows each template's name,
description, and inheritance reference (if any).

The room argument is a room alias localpart (e.g., "bureau/template").
It is resolved to a full Matrix alias using the --server-name flag.`,
		Usage: "bureau template list [flags] <room-alias-localpart>",
		Examples: []cli.Example{
			{
				Description: "List built-in templates",
				Command:     "bureau template list bureau/template",
			},
			{
				Description: "List project templates as JSON",
				Command:     "bureau template list --json iree/template",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]templateEntry{} },
		RequiredGrants: []string{"command/template/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// In CLI mode, the room comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau template list [flags] <room-alias-localpart>")
			}

			params.ServerName = cli.ResolveServerName(params.ServerName)

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			roomAlias := ref.MustParseRoomAlias(schema.FullRoomAlias(params.Room, serverName))

			ctx, cancel, session, err := cli.ConnectOperator(ctx)
			if err != nil {
				return err
			}
			defer cancel()

			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return cli.NotFound("resolving room alias %q: %w", roomAlias, err)
			}

			// Fetch all state events in the room, then filter for templates.
			events, err := session.GetRoomState(ctx, roomID)
			if err != nil {
				return cli.Internal("getting room state: %w", err)
			}

			var templates []templateEntry
			for _, event := range events {
				if event.Type != schema.EventTypeTemplate {
					continue
				}
				if event.StateKey == nil {
					continue
				}

				// Extract description and inherits from the Content map.
				description, _ := event.Content["description"].(string)
				var inherits []string
				if inheritsRaw, ok := event.Content["inherits"].([]any); ok {
					for _, item := range inheritsRaw {
						if s, ok := item.(string); ok {
							inherits = append(inherits, s)
						}
					}
				}

				templates = append(templates, templateEntry{
					Name:        *event.StateKey,
					Description: description,
					Inherits:    inherits,
				})
			}

			if done, err := params.EmitJSON(templates); done {
				return err
			}

			if len(templates) == 0 {
				logger.Info("no templates found", "room", roomAlias)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "NAME\tDESCRIPTION\tINHERITS\n")
			for _, entry := range templates {
				fmt.Fprintf(writer, "%s\t%s\t%s\n", entry.Name, entry.Description, strings.Join(entry.Inherits, ", "))
			}
			return writer.Flush()
		},
	}
}
