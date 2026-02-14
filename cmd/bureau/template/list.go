// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// listParams holds the parameters for the template list command. The
// Room field is positional in CLI mode (args[0]) and a named property
// in JSON/MCP mode.
type listParams struct {
	Room       string `json:"room"         desc:"room alias localpart (e.g. bureau/template)" required:"true"`
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	OutputJSON bool   `json:"-"            flag:"json"         desc:"output as JSON instead of a table"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("list", &params)
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/template/list"},
		Run: func(args []string) error {
			// In CLI mode, the room comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return fmt.Errorf("expected 1 positional argument, got %d", len(args))
			}
			if params.Room == "" {
				return fmt.Errorf("room is required\n\nusage: bureau template list [flags] <room-alias-localpart>")
			}

			roomAlias := principal.RoomAlias(params.Room, params.ServerName)

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return fmt.Errorf("resolving room alias %q: %w", roomAlias, err)
			}

			// Fetch all state events in the room, then filter for templates.
			events, err := session.GetRoomState(ctx, roomID)
			if err != nil {
				return fmt.Errorf("getting room state: %w", err)
			}

			type templateEntry struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Inherits    string `json:"inherits,omitempty"`
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
				inherits, _ := event.Content["inherits"].(string)

				templates = append(templates, templateEntry{
					Name:        *event.StateKey,
					Description: description,
					Inherits:    inherits,
				})
			}

			if params.OutputJSON {
				// Ensure empty array in JSON output, not null.
				if templates == nil {
					templates = []templateEntry{}
				}
				data, err := json.MarshalIndent(templates, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal JSON: %w", err)
				}
				fmt.Fprintln(os.Stdout, string(data))
				return nil
			}

			if len(templates) == 0 {
				fmt.Fprintf(os.Stderr, "no templates found in %s\n", roomAlias)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "NAME\tDESCRIPTION\tINHERITS\n")
			for _, entry := range templates {
				fmt.Fprintf(writer, "%s\t%s\t%s\n", entry.Name, entry.Description, entry.Inherits)
			}
			return writer.Flush()
		},
	}
}
