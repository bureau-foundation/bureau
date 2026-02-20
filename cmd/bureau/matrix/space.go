// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
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

// spaceCreateParams holds the parameters for the matrix space create command.
type spaceCreateParams struct {
	cli.SessionConfig
	Name  string `json:"name"  flag:"name"  desc:"space display name (defaults to alias)"`
	Topic string `json:"topic" flag:"topic" desc:"space topic"`
	cli.JSONOutput
}

// spaceCreateResult is the JSON output for matrix space create.
type spaceCreateResult struct {
	RoomID string `json:"room_id" desc:"created space's Matrix room ID"`
	Alias  string `json:"alias"   desc:"space alias"`
}

func spaceCreateCommand() *cli.Command {
	var params spaceCreateParams

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new space",
		Description: `Create a new Matrix space. Spaces are rooms with type "m.space" that
act as containers for organizing related rooms.

The alias is the local part of the space's Matrix alias (e.g., "iree"
becomes "#iree:bureau.local"). The display name defaults to the alias
if not specified.`,
		Usage: "bureau matrix space create <alias> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a space with a display name",
				Command:     "bureau matrix space create iree --name IREE --credential-file ./creds",
			},
			{
				Description: "Create a space with a topic",
				Command:     "bureau matrix space create research --topic 'Research coordination' --credential-file ./creds",
			},
		},
		Annotations:    cli.Create(),
		Output:         func() any { return &spaceCreateResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/space/create"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("space alias is required\n\nUsage: bureau matrix space create <alias> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			alias := args[0]

			name := params.Name
			if name == "" {
				name = alias
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			response, err := sess.CreateRoom(ctx, messaging.CreateRoomRequest{
				Name:       name,
				Alias:      alias,
				Topic:      params.Topic,
				Preset:     "private_chat",
				Visibility: "private",
				CreationContent: map[string]any{
					"type": "m.space",
				},
			})
			if err != nil {
				return cli.Internal("create space: %w", err)
			}

			if done, err := params.EmitJSON(spaceCreateResult{
				RoomID: response.RoomID.String(),
				Alias:  alias,
			}); done {
				return err
			}

			fmt.Fprintln(os.Stdout, response.RoomID)
			return nil
		},
	}
}

// spaceListParams holds the parameters for the matrix space list command.
type spaceListParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

// spaceEntry holds the JSON-serializable data for a single space.
type spaceEntry struct {
	RoomID string `json:"room_id"         desc:"space's Matrix room ID"`
	Alias  string `json:"alias,omitempty" desc:"space alias"`
	Name   string `json:"name,omitempty"  desc:"space display name"`
}

func spaceListCommand() *cli.Command {
	var params spaceListParams

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
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &[]spaceEntry{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/space/list"},
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomIDs, err := sess.JoinedRooms(ctx)
			if err != nil {
				return cli.Internal("list joined rooms: %w", err)
			}

			var spaces []spaceEntry
			for _, roomID := range roomIDs {
				isSpace, name, alias := inspectSpaceState(ctx, sess, roomID)
				if !isSpace {
					continue
				}
				spaces = append(spaces, spaceEntry{
					RoomID: roomID.String(),
					Alias:  alias,
					Name:   name,
				})
			}

			if done, err := params.EmitJSON(spaces); done {
				return err
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(writer, "ROOM ID\tALIAS\tNAME")
			for _, space := range spaces {
				fmt.Fprintf(writer, "%s\t%s\t%s\n", space.RoomID, space.Alias, space.Name)
			}
			return writer.Flush()
		},
	}
}

// spaceDeleteParams holds the parameters for the matrix space delete command.
type spaceDeleteParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

// spaceDeleteResult is the JSON output for matrix space delete.
type spaceDeleteResult struct {
	RoomID string `json:"room_id" desc:"left space's Matrix room ID"`
}

func spaceDeleteCommand() *cli.Command {
	var params spaceDeleteParams

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
		Annotations:    cli.Destructive(),
		Output:         func() any { return &spaceDeleteResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/space/delete"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("space alias or room ID is required\n\nUsage: bureau matrix space delete <alias-or-id> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			target := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, sess, target)
			if err != nil {
				return err
			}

			directSession, ok := sess.(*messaging.DirectSession)
			if !ok {
				return cli.Validation("space leave requires operator credentials (not available inside sandboxes)")
			}
			if err := directSession.LeaveRoom(ctx, roomID); err != nil {
				return cli.Internal("leave space: %w", err)
			}

			if done, err := params.EmitJSON(spaceDeleteResult{RoomID: roomID.String()}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Left space %s\n", roomID)
			return nil
		},
	}
}

// spaceMembersParams holds the parameters for the matrix space members command.
type spaceMembersParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

func spaceMembersCommand() *cli.Command {
	var params spaceMembersParams

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
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &[]messaging.RoomMember{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/space/members"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("space alias or room ID is required\n\nUsage: bureau matrix space members <alias-or-id> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			target := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, sess, target)
			if err != nil {
				return err
			}

			members, err := sess.GetRoomMembers(ctx, roomID)
			if err != nil {
				return cli.Internal("get space members: %w", err)
			}

			if done, err := params.EmitJSON(members); done {
				return err
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
func inspectSpaceState(ctx context.Context, session messaging.Session, roomID ref.RoomID) (bool, string, string) {
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
