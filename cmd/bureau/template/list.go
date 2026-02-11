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

// listCommand returns the "list" subcommand for listing templates in a room.
func listCommand() *cli.Command {
	var (
		serverName string
		outputJSON bool
	)

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
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for resolving room aliases")
			flagSet.BoolVar(&outputJSON, "json", false, "output as JSON instead of a table")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau template list [flags] <room-alias-localpart>")
			}

			roomLocalpart := args[0]
			roomAlias := principal.RoomAlias(roomLocalpart, serverName)

			ctx, cancel, session, err := connectOperator()
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

			if outputJSON {
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
