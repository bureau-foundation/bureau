// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/messaging"
)

// RoomCommand returns the "room" subcommand group for managing Matrix rooms.
func RoomCommand() *cli.Command {
	return &cli.Command{
		Name:    "room",
		Summary: "Manage Matrix rooms",
		Description: `Create, list, delete, and inspect Matrix rooms.

Rooms are the primary communication channels in Bureau. Each room belongs
to a space (via m.space.child state events). Bureau uses hierarchical room
aliases that mirror the principal naming convention:

  #bureau/machines:bureau.local       Machine keys and status
  #iree/amdgpu/general:bureau.local  IREE project discussion`,
		Subcommands: []*cli.Command{
			roomCreateCommand(),
			roomListCommand(),
			roomDeleteCommand(),
			roomMembersCommand(),
		},
	}
}

func roomCreateCommand() *cli.Command {
	var (
		session           cli.SessionConfig
		space             string
		name              string
		topic             string
		serverName        string
		memberStateEvents []string
	)

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new room in a space",
		Description: `Create a new Matrix room and add it as a child of a space.

The alias is required and follows Bureau's naming convention (e.g.,
"bureau/machines", "iree/amdgpu/general"). The --space flag specifies
the parent space by alias or room ID.

By default, only the admin can set state events. Use --member-state-event
(repeatable) to allow room members to set specific Bureau event types,
such as m.bureau.machine_key or m.bureau.service.`,
		Usage: "bureau matrix room create <alias> --space <space> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a project room",
				Command:     "bureau matrix room create iree/amdgpu/general --space '#iree:bureau.local' --name 'IREE AMDGPU General' --credential-file ./creds",
			},
			{
				Description: "Create a room where members can publish machine keys",
				Command:     "bureau matrix room create bureau/machines --space '#bureau:bureau.local' --name 'Bureau Machines' --member-state-event m.bureau.machine_key --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("room create", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&space, "space", "", "parent space alias or room ID (required)")
			flagSet.StringVar(&name, "name", "", "room display name (defaults to alias)")
			flagSet.StringVar(&topic, "topic", "", "room topic")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for m.space.child via field")
			flagSet.StringArrayVar(&memberStateEvents, "member-state-event", nil, "state event type that members can set (repeatable)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("room alias is required\n\nUsage: bureau matrix room create <alias> --space <space> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			alias := args[0]

			if space == "" {
				return fmt.Errorf("--space is required (rooms must belong to a space)")
			}

			if name == "" {
				name = alias
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			// Resolve the parent space.
			spaceRoomID, err := resolveRoom(ctx, sess, space)
			if err != nil {
				return fmt.Errorf("resolve space: %w", err)
			}

			// Build power levels. Admin-only by default, with optional
			// member-settable Bureau event types.
			powerLevels := adminOnlyPowerLevels(sess.UserID(), memberStateEvents)

			// Create the room.
			response, err := sess.CreateRoom(ctx, messaging.CreateRoomRequest{
				Name:                      name,
				Alias:                     alias,
				Topic:                     topic,
				Preset:                    "private_chat",
				Visibility:                "private",
				PowerLevelContentOverride: powerLevels,
			})
			if err != nil {
				return fmt.Errorf("create room: %w", err)
			}

			// Add as child of the parent space.
			_, err = sess.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID,
				map[string]any{
					"via": []string{serverName},
				})
			if err != nil {
				return fmt.Errorf("add room as child of space %s: %w", spaceRoomID, err)
			}

			fmt.Fprintln(os.Stdout, response.RoomID)
			return nil
		},
	}
}

func roomListCommand() *cli.Command {
	var (
		session cli.SessionConfig
		space   string
	)

	return &cli.Command{
		Name:    "list",
		Summary: "List rooms",
		Description: `List Matrix rooms. With --space, lists rooms that are children of the
specified space (by reading m.space.child state events). Without --space,
lists all joined rooms that are NOT spaces.`,
		Usage: "bureau matrix room list [flags]",
		Examples: []cli.Example{
			{
				Description: "List rooms in a space",
				Command:     "bureau matrix room list --space '#bureau:bureau.local' --credential-file ./creds",
			},
			{
				Description: "List all joined rooms",
				Command:     "bureau matrix room list --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("room list", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&space, "space", "", "list only rooms that are children of this space (alias or room ID)")
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

			if space != "" {
				return listSpaceChildren(ctx, sess, space)
			}
			return listAllRooms(ctx, sess)
		},
	}
}

// listSpaceChildren lists rooms that are children of a space by reading
// m.space.child state events from the space, then inspecting each child.
func listSpaceChildren(ctx context.Context, session *messaging.Session, spaceTarget string) error {
	spaceRoomID, err := resolveRoom(ctx, session, spaceTarget)
	if err != nil {
		return fmt.Errorf("resolve space: %w", err)
	}

	// Get all state events from the space.
	events, err := session.GetRoomState(ctx, spaceRoomID)
	if err != nil {
		return fmt.Errorf("get space state: %w", err)
	}

	// Extract child room IDs from m.space.child state events.
	// The state key of each m.space.child event is the child room ID.
	var childRoomIDs []string
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			childRoomIDs = append(childRoomIDs, *event.StateKey)
		}
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ROOM ID\tALIAS\tNAME\tTOPIC")

	for _, childRoomID := range childRoomIDs {
		roomName, roomAlias, roomTopic := inspectRoomState(ctx, session, childRoomID)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", childRoomID, roomAlias, roomName, roomTopic)
	}

	return writer.Flush()
}

// listAllRooms lists all joined rooms that are not spaces.
func listAllRooms(ctx context.Context, session *messaging.Session) error {
	roomIDs, err := session.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("list joined rooms: %w", err)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ROOM ID\tALIAS\tNAME\tTOPIC")

	for _, roomID := range roomIDs {
		isSpace, _, _ := inspectSpaceState(ctx, session, roomID)
		if isSpace {
			continue
		}
		roomName, roomAlias, roomTopic := inspectRoomState(ctx, session, roomID)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", roomID, roomAlias, roomName, roomTopic)
	}

	return writer.Flush()
}

// inspectRoomState fetches room state and extracts name, canonical alias,
// and topic. Returns empty strings for fields that aren't set or if the
// room state can't be fetched.
func inspectRoomState(ctx context.Context, session *messaging.Session, roomID string) (name, alias, topic string) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return "", "", ""
	}

	for _, event := range events {
		switch event.Type {
		case "m.room.name":
			if value, ok := event.Content["name"].(string); ok {
				name = value
			}
		case "m.room.canonical_alias":
			if value, ok := event.Content["alias"].(string); ok {
				alias = value
			}
		case "m.room.topic":
			if value, ok := event.Content["topic"].(string); ok {
				topic = value
			}
		}
	}

	return name, alias, topic
}

func roomDeleteCommand() *cli.Command {
	var session cli.SessionConfig

	return &cli.Command{
		Name:    "delete",
		Summary: "Leave a room",
		Description: `Leave a Matrix room by alias or room ID.

Matrix does not support room deletion — leaving is the closest
equivalent. If all members leave, the homeserver may eventually
reclaim the room.

To also remove the room from its parent space, use "bureau matrix state set"
to clear the m.space.child event in the space.`,
		Usage: "bureau matrix room delete <alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Leave a room by alias",
				Command:     "bureau matrix room delete '#iree/amdgpu/general:bureau.local' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("room delete", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("room alias or room ID is required\n\nUsage: bureau matrix room delete <alias-or-id> [flags]")
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
				return fmt.Errorf("leave room: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Left room %s\n", roomID)
			return nil
		},
	}
}

func roomMembersCommand() *cli.Command {
	var session cli.SessionConfig

	return &cli.Command{
		Name:    "members",
		Summary: "List members of a room",
		Description: `List all members of a Matrix room by alias or room ID.

Displays a table of user ID, display name, and membership state
(join, invite, leave, ban).`,
		Usage: "bureau matrix room members <alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "List room members",
				Command:     "bureau matrix room members '#bureau/machines:bureau.local' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("room members", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("room alias or room ID is required\n\nUsage: bureau matrix room members <alias-or-id> [flags]")
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
				return fmt.Errorf("get room members: %w", err)
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
