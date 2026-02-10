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

// UserCommand returns the "user" subcommand group for managing Matrix users.
func UserCommand() *cli.Command {
	return &cli.Command{
		Name:    "user",
		Summary: "Manage Matrix users",
		Description: `Create, list, invite, kick, and identify Matrix users.

The "create" subcommand registers a new account on the homeserver (requires
a registration token). All other subcommands operate on an existing session
via --credential-file or --homeserver/--token/--user-id.`,
		Subcommands: []*cli.Command{
			userCreateCommand(),
			userListCommand(),
			userInviteCommand(),
			userKickCommand(),
			userWhoAmICommand(),
		},
	}
}

// userCreateCommand returns the "user create" subcommand for registering a new
// Matrix account. This uses Client.Register directly (no existing session
// needed), similar to how setup bootstraps the admin account.
func userCreateCommand() *cli.Command {
	var (
		homeserverURL         string
		registrationTokenFile string
		serverName            string
	)

	return &cli.Command{
		Name:    "create",
		Summary: "Register a new Matrix account",
		Description: `Register a new Matrix account on the homeserver. Requires a registration
token (read from file or stdin). The account is created with the specified
username and a password derived from the registration token.

Does not require an existing session — this creates a fresh account.`,
		Usage: "bureau matrix user create <username> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a user with token from file",
				Command:     "bureau matrix user create alice --registration-token-file /run/secrets/token",
			},
			{
				Description: "Create a user with token from stdin",
				Command:     "echo $TOKEN | bureau matrix user create bob --registration-token-file -",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("create", pflag.ContinueOnError)
			flagSet.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
			flagSet.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (required)")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for constructing user IDs")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("username is required\n\nUsage: bureau matrix user create <username> [flags]")
			}
			username := args[0]
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}

			if registrationTokenFile == "" {
				return fmt.Errorf("--registration-token-file is required (use - for stdin)")
			}

			registrationToken, err := readSecret(registrationTokenFile)
			if err != nil {
				return fmt.Errorf("read registration token: %w", err)
			}
			if registrationToken == "" {
				return fmt.Errorf("registration token is empty")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return fmt.Errorf("create matrix client: %w", err)
			}

			password := deriveAdminPassword(registrationToken)
			session, err := client.Register(ctx, messaging.RegisterRequest{
				Username:          username,
				Password:          password,
				RegistrationToken: registrationToken,
			})
			if err != nil {
				return fmt.Errorf("register user %q: %w", username, err)
			}

			fmt.Fprintf(os.Stdout, "User ID:       %s\n", session.UserID())
			fmt.Fprintf(os.Stdout, "Access Token:  %s\n", session.AccessToken())
			return nil
		},
	}
}

// userListCommand returns the "user list" subcommand for listing Matrix users.
// With --room, lists members of a specific room. Without --room, aggregates
// unique members across all joined rooms.
func userListCommand() *cli.Command {
	var (
		session  SessionConfig
		roomFlag string
	)

	return &cli.Command{
		Name:    "list",
		Summary: "List Matrix users",
		Description: `List Matrix users visible to the authenticated session.

With --room, lists members of a specific room (by alias or ID).
Without --room, aggregates unique members across all rooms the
authenticated user has joined.`,
		Usage: "bureau matrix user list [flags]",
		Examples: []cli.Example{
			{
				Description: "List members of a specific room",
				Command:     "bureau matrix user list --room '#bureau/agents:bureau.local' --credential-file ./creds",
			},
			{
				Description: "List all known users across joined rooms",
				Command:     "bureau matrix user list --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&roomFlag, "room", "", "room alias or ID to list members of (optional)")
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
				return err
			}

			if roomFlag != "" {
				return listRoomMembers(ctx, sess, roomFlag)
			}
			return listAllMembers(ctx, sess)
		},
	}
}

// listRoomMembers lists members of a single room, resolving aliases as needed.
func listRoomMembers(ctx context.Context, session *messaging.Session, roomIDOrAlias string) error {
	roomID, err := resolveRoom(ctx, session, roomIDOrAlias)
	if err != nil {
		return err
	}

	members, err := session.GetRoomMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("get room members: %w", err)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "USER ID\tDISPLAY NAME\tMEMBERSHIP")
	for _, member := range members {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", member.UserID, member.DisplayName, member.Membership)
	}
	return writer.Flush()
}

// listAllMembers aggregates unique members across all joined rooms, fetching
// display names for each unique user.
func listAllMembers(ctx context.Context, session *messaging.Session) error {
	rooms, err := session.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("get joined rooms: %w", err)
	}

	// Collect unique members across all rooms.
	uniqueMembers := make(map[string]messaging.RoomMember)
	for _, roomID := range rooms {
		members, err := session.GetRoomMembers(ctx, roomID)
		if err != nil {
			return fmt.Errorf("get members for room %s: %w", roomID, err)
		}
		for _, member := range members {
			if member.Membership != "join" {
				continue
			}
			if _, exists := uniqueMembers[member.UserID]; !exists {
				uniqueMembers[member.UserID] = member
			}
		}
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "USER ID\tDISPLAY NAME")
	for _, member := range uniqueMembers {
		displayName := member.DisplayName
		if displayName == "" {
			// The /members endpoint may not include display names. Fall
			// back to the profile endpoint for users without one.
			fetched, err := session.GetDisplayName(ctx, member.UserID)
			if err != nil {
				// Profile lookup failures are non-fatal for listing; the
				// user might have left the server or restricted their profile.
				displayName = ""
			} else {
				displayName = fetched
			}
		}
		fmt.Fprintf(writer, "%s\t%s\n", member.UserID, displayName)
	}
	return writer.Flush()
}

// userInviteCommand returns the "user invite" subcommand.
func userInviteCommand() *cli.Command {
	var session SessionConfig

	return &cli.Command{
		Name:    "invite",
		Summary: "Invite a user to a room",
		Description: `Invite a Matrix user to a room. The room can be specified by alias
(e.g., "#bureau/agents:bureau.local") or by room ID.`,
		Usage: "bureau matrix user invite <user-id> <room-alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Invite a user to a room by alias",
				Command:     "bureau matrix user invite @alice:bureau.local '#bureau/agents:bureau.local' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("invite", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: bureau matrix user invite <user-id> <room-alias-or-id>")
			}
			targetUserID := args[0]
			roomIDOrAlias := args[1]
			if len(args) > 2 {
				return fmt.Errorf("unexpected argument: %s", args[2])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return err
			}

			roomID, err := resolveRoom(ctx, sess, roomIDOrAlias)
			if err != nil {
				return err
			}

			if err := sess.InviteUser(ctx, roomID, targetUserID); err != nil {
				return fmt.Errorf("invite user: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Invited %s to %s\n", targetUserID, roomIDOrAlias)
			return nil
		},
	}
}

// userKickCommand returns the "user kick" subcommand.
func userKickCommand() *cli.Command {
	var (
		session SessionConfig
		reason  string
	)

	return &cli.Command{
		Name:    "kick",
		Summary: "Kick a user from a room",
		Description: `Kick (remove) a Matrix user from a room. The room can be specified by
alias or room ID. An optional --reason provides context for the kick.`,
		Usage: "bureau matrix user kick <user-id> <room-alias-or-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Kick a user from a room",
				Command:     "bureau matrix user kick @bob:bureau.local '#bureau/agents:bureau.local' --credential-file ./creds",
			},
			{
				Description: "Kick with a reason",
				Command:     "bureau matrix user kick @bob:bureau.local '#bureau/agents:bureau.local' --reason 'misbehaving agent' --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("kick", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&reason, "reason", "", "reason for the kick (optional)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: bureau matrix user kick <user-id> <room-alias-or-id>")
			}
			targetUserID := args[0]
			roomIDOrAlias := args[1]
			if len(args) > 2 {
				return fmt.Errorf("unexpected argument: %s", args[2])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := session.Connect(ctx)
			if err != nil {
				return err
			}

			roomID, err := resolveRoom(ctx, sess, roomIDOrAlias)
			if err != nil {
				return err
			}

			if err := sess.KickUser(ctx, roomID, targetUserID, reason); err != nil {
				return fmt.Errorf("kick user: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Kicked %s from %s\n", targetUserID, roomIDOrAlias)
			return nil
		},
	}
}

// userWhoAmICommand returns the "user whoami" subcommand.
func userWhoAmICommand() *cli.Command {
	var session SessionConfig

	return &cli.Command{
		Name:    "whoami",
		Summary: "Show the authenticated user's ID",
		Description: `Display the Matrix user ID of the currently authenticated session.
Useful for verifying that credentials are valid and identifying which
account is in use.`,
		Usage: "bureau matrix user whoami [flags]",
		Examples: []cli.Example{
			{
				Description: "Show current user ID",
				Command:     "bureau matrix user whoami --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("whoami", pflag.ContinueOnError)
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
				return err
			}

			userID, err := sess.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("whoami: %w", err)
			}

			fmt.Fprintln(os.Stdout, userID)
			return nil
		},
	}
}
