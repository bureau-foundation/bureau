// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/messaging"
)

// SpaceCommand returns the "space" subcommand group for managing Matrix spaces.
func SpaceCommand() *cli.Command {
	return &cli.Command{
		Name:    "space",
		Summary: "Manage Matrix spaces",
		Description: `Create, list, delete, and inspect Matrix spaces.

Spaces are Matrix rooms with type "m.space". They act as containers for
organizing related rooms (channels). Bureau uses spaces to group agent
workspaces, project channels, and service directories.`,
		Subcommands: []*cli.Command{
			spaceCreateCommand(),
			spaceListCommand(),
			spaceDeleteCommand(),
			spaceMembersCommand(),
		},
	}
}

func spaceCreateCommand() *cli.Command {
	var (
		session SessionConfig
		alias   string
		topic   string
	)

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new space",
		Description: `Create a new Matrix space. Spaces are rooms with type "m.space" that
act as containers for organizing related rooms.

The name is required. An alias is optional and defaults to the name
(lowercased, spaces replaced with hyphens). The alias is the local
part only — the server name is appended automatically.`,
		Usage: "bureau matrix space create <name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a space with a custom alias",
				Command:     "bureau matrix space create 'My Project' --alias my-project --credential-file ./creds",
			},
			{
				Description: "Create a space with a topic",
				Command:     "bureau matrix space create 'Research' --topic 'Research coordination' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("space create", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&alias, "alias", "", "local alias for the space (defaults to lowercased name with hyphens)")
			flagSet.StringVar(&topic, "topic", "", "space topic")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("space name is required\n\nUsage: bureau matrix space create <name> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			name := args[0]

			if alias == "" {
				alias = defaultAlias(name)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			response, err := sess.CreateRoom(ctx, messaging.CreateRoomRequest{
				Name:       name,
				Alias:      alias,
				Topic:      topic,
				Preset:     "private_chat",
				Visibility: "private",
				CreationContent: map[string]any{
					"type": "m.space",
				},
			})
			if err != nil {
				return fmt.Errorf("create space: %w", err)
			}

			fmt.Fprintln(os.Stdout, response.RoomID)
			return nil
		},
	}
}

func spaceListCommand() *cli.Command {
	var session SessionConfig

	return &cli.Command{
		Name:    "list",
		Summary: "List spaces you have joined",
		Description: `List all Matrix spaces the authenticated user has joined.

Fetches the joined room list, then inspects each room's state to
identify spaces (rooms with creation type "m.space"). Displays a
table of room ID, alias, and name.`,
		Usage: "bureau matrix space list [flags]",
		Examples: []cli.Example{
			{
				Description: "List spaces using a credential file",
				Command:     "bureau matrix space list --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("space list", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			roomIDs, err := sess.JoinedRooms(ctx)
			if err != nil {
				return fmt.Errorf("list joined rooms: %w", err)
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "ROOM ID\tALIAS\tNAME")

			for _, roomID := range roomIDs {
				isSpace, name, alias := inspectSpaceState(ctx, sess, roomID)
				if !isSpace {
					continue
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\n", roomID, alias, name)
			}

			return writer.Flush()
		},
	}
}

func spaceDeleteCommand() *cli.Command {
	var session SessionConfig

	return &cli.Command{
		Name:    "delete",
		Summary: "Leave a space",
		Description: `Leave a Matrix space by alias or room ID.

Matrix does not support room deletion — leaving is the closest
equivalent. If all members leave, the homeserver may eventually
reclaim the room.`,
		Usage: "bureau matrix space delete <alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Leave a space by alias",
				Command:     "bureau matrix space delete '#my-project:bureau.local' --credential-file ./creds",
			},
			{
				Description: "Leave a space by room ID",
				Command:     "bureau matrix space delete '!abc123:bureau.local' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("space delete", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("space alias or room ID is required\n\nUsage: bureau matrix space delete <alias-or-id> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			target := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, sess, target)
			if err != nil {
				return err
			}

			if err := sess.LeaveRoom(ctx, roomID); err != nil {
				return fmt.Errorf("leave space: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Left space %s\n", roomID)
			return nil
		},
	}
}

func spaceMembersCommand() *cli.Command {
	var session SessionConfig

	return &cli.Command{
		Name:    "members",
		Summary: "List members of a space",
		Description: `List all members of a Matrix space by alias or room ID.

Displays a table of user ID, display name, and membership state
(join, invite, leave, ban).`,
		Usage: "bureau matrix space members <alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "List members by alias",
				Command:     "bureau matrix space members '#my-project:bureau.local' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("space members", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("space alias or room ID is required\n\nUsage: bureau matrix space members <alias-or-id> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			target := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, sess, target)
			if err != nil {
				return err
			}

			members, err := sess.GetRoomMembers(ctx, roomID)
			if err != nil {
				return fmt.Errorf("get space members: %w", err)
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "USER ID\tDISPLAY NAME\tMEMBERSHIP")
			for _, member := range members {
				fmt.Fprintf(writer, "%s\t%s\t%s\n", member.UserID, member.DisplayName, member.Membership)
			}
			return writer.Flush()
		},
	}
}

// inspectSpaceState fetches room state and determines whether the room is a
// space. Returns (isSpace, name, canonicalAlias). If the room state cannot be
// fetched, returns (false, "", "").
func inspectSpaceState(ctx context.Context, session *messaging.Session, roomID string) (bool, string, string) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return false, "", ""
	}

	var isSpace bool
	var name string
	var canonicalAlias string

	for _, event := range events {
		switch event.Type {
		case "m.room.create":
			if roomType, ok := event.Content["type"].(string); ok && roomType == "m.space" {
				isSpace = true
			}
		case "m.room.name":
			if roomName, ok := event.Content["name"].(string); ok {
				name = roomName
			}
		case "m.room.canonical_alias":
			if roomAlias, ok := event.Content["alias"].(string); ok {
				canonicalAlias = roomAlias
			}
		}
	}

	return isSpace, name, canonicalAlias
}

// defaultAlias derives a room alias from a human-readable name by lowercasing,
// replacing whitespace runs with hyphens, and stripping non-alphanumeric
// characters except hyphens.
func defaultAlias(name string) string {
	lower := strings.ToLower(name)
	var builder strings.Builder
	previousHyphen := false
	for _, character := range lower {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
			previousHyphen = false
		case character == ' ' || character == '_' || character == '-':
			if !previousHyphen && builder.Len() > 0 {
				builder.WriteRune('-')
				previousHyphen = true
			}
		}
	}
	result := builder.String()
	return strings.TrimRight(result, "-")
}
