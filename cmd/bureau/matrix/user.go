// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
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

// userCreateParams holds the parameters for the matrix user create command.
// Username is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode. Credential-related flags are excluded from MCP schema via
// json:"-" since they involve reading secrets from files/stdin, which is
// not appropriate for MCP.
type userCreateParams struct {
	Username              string `json:"username"    desc:"Matrix username to register" required:"true"`
	CredentialFile        string `json:"-"           flag:"credential-file"         desc:"path to Bureau credential file from 'bureau matrix setup' (provides homeserver URL and registration token)"`
	HomeserverURL         string `json:"-"           flag:"homeserver"              desc:"Matrix homeserver URL (overrides credential file; default http://localhost:6167)"`
	RegistrationTokenFile string `json:"-"           flag:"registration-token-file" desc:"path to file containing registration token, or - for stdin (overrides credential file)"`
	PasswordFile          string `json:"-"           flag:"password-file"           desc:"path to file containing password, or - to prompt interactively (default: derive from registration token)"`
	ServerName            string `json:"server_name" flag:"server-name"             desc:"Matrix server name for constructing user IDs" default:"bureau.local"`
	Operator              bool   `json:"operator"    flag:"operator"                desc:"invite the user to all Bureau infrastructure rooms (requires --credential-file)"`
	cli.JSONOutput
}

// userCreateResult is the JSON output for matrix user create.
type userCreateResult struct {
	UserID        string `json:"user_id"                desc:"created user's Matrix ID"`
	AccessToken   string `json:"access_token,omitempty" desc:"session access token"`
	AlreadyExists bool   `json:"already_exists"         desc:"true if user already existed"`
}

// userCreateCommand returns the "user create" subcommand for registering a new
// Matrix account. This uses Client.Register directly (no existing session
// needed), similar to how setup bootstraps the admin account.
//
// With --operator, also invites the user to all Bureau infrastructure rooms
// (space, system, machines, services). This is the primary onboarding path
// for human operators after running "bureau matrix setup".
func userCreateCommand() *cli.Command {
	var params userCreateParams

	return &cli.Command{
		Name:    "create",
		Summary: "Register a new Matrix account",
		Description: `Register a new Matrix account on the homeserver.

The registration token can come from --credential-file (which contains the
token from "bureau matrix setup") or from --registration-token-file. The
credential file is the easiest path after initial setup.

By default, the password is derived deterministically from the registration
token. This is appropriate for agent accounts that never need interactive
login. For human accounts (e.g., to log in via Element), use --password-file
to set a chosen password. Use --password-file - to be prompted interactively.

With --operator, the user is also invited to all Bureau infrastructure rooms
(the space plus system, machines, and services). This is the recommended
way to onboard a human operator after initial server setup.
The command is idempotent: if the account already exists, it skips creation
and proceeds directly to ensuring room membership.`,
		Usage: "bureau matrix user create <username> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create an operator account (recommended after setup)",
				Command:     "bureau matrix user create ben --credential-file ./bureau-creds --password-file - --operator",
			},
			{
				Description: "Create an agent account (derived password, no room invites)",
				Command:     "bureau matrix user create iree-builder --credential-file ./bureau-creds",
			},
			{
				Description: "Create an account with explicit token file",
				Command:     "bureau matrix user create ben --registration-token-file .env-token --password-file -",
			},
		},
		Annotations:    cli.Create(),
		Output:         func() any { return &userCreateResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/user/create"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, username comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Username = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.Username == "" {
				return cli.Validation("username is required\n\nUsage: bureau matrix user create <username> [flags]")
			}
			username := params.Username
			if params.Operator && params.CredentialFile == "" {
				return cli.Validation("--operator requires --credential-file (needed for admin session and Bureau room discovery)")
			}

			// Parse the credential file once. Both the registration flow
			// and the operator invite flow read from it.
			var credentials map[string]string
			if params.CredentialFile != "" {
				var err error
				credentials, err = cli.ReadCredentialFile(params.CredentialFile)
				if err != nil {
					return cli.Internal("read credential file: %w", err)
				}
			}

			// Resolve registration token and homeserver URL. The credential
			// file from "bureau matrix setup" contains both, so after initial
			// bootstrap --credential-file is all you need. Explicit flags
			// override the credential file values.
			homeserverURL := params.HomeserverURL
			var registrationToken string
			if credentials != nil {
				registrationToken = credentials["MATRIX_REGISTRATION_TOKEN"]
				if homeserverURL == "" {
					homeserverURL = credentials["MATRIX_HOMESERVER_URL"]
				}
			}
			if params.RegistrationTokenFile != "" {
				if params.RegistrationTokenFile == "-" && params.PasswordFile == "-" {
					return cli.Validation("--registration-token-file and --password-file cannot both be - (stdin)")
				}
				tokenBuffer, err := secret.ReadFromPath(params.RegistrationTokenFile)
				if err != nil {
					return cli.Internal("read registration token: %w", err)
				}
				defer tokenBuffer.Close()
				registrationToken = tokenBuffer.String()
			}
			if registrationToken == "" {
				return cli.Validation("registration token is required (use --credential-file or --registration-token-file)")
			}
			if homeserverURL == "" {
				homeserverURL = "http://localhost:6167"
			}

			var (
				passwordBuffer *secret.Buffer
				err            error
			)
			if params.PasswordFile != "" {
				passwordBuffer, err = readPassword(params.PasswordFile)
				if err != nil {
					return cli.Internal("read password: %w", err)
				}
				defer passwordBuffer.Close()
			} else {
				// Agent accounts use a password derived from the registration
				// token. The derived value is deterministic, so re-running
				// with the same token produces the same account.
				tokenBuffer, tokenErr := secret.NewFromString(registrationToken)
				if tokenErr != nil {
					return cli.Internal("protecting registration token: %w", tokenErr)
				}
				defer tokenBuffer.Close()
				passwordBuffer, err = cli.DeriveAdminPassword(tokenBuffer)
				if err != nil {
					return cli.Internal("derive password: %w", err)
				}
				defer passwordBuffer.Close()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return cli.Internal("create matrix client: %w", err)
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			// Register the account. In operator mode, M_USER_IN_USE means
			// the account already exists — verify the password matches
			// (if one was explicitly provided) and proceed to room invites.
			userID := ref.MatrixUserID(username, serverName)
			registrationTokenBuffer, tokenErr := secret.NewFromString(registrationToken)
			if tokenErr != nil {
				return cli.Internal("protecting registration token: %w", tokenErr)
			}
			defer registrationTokenBuffer.Close()

			alreadyExists := false
			session, registerErr := client.Register(ctx, messaging.RegisterRequest{
				Username:          username,
				Password:          passwordBuffer,
				RegistrationToken: registrationTokenBuffer,
			})
			if registerErr != nil {
				if params.Operator && messaging.IsMatrixError(registerErr, messaging.ErrCodeUserInUse) {
					// Account exists. Log in to get a session for joining
					// rooms (invite alone leaves the user in limbo —
					// they must also join to become a full member).
					alreadyExists = true
					var loginErr error
					session, loginErr = client.Login(ctx, username, passwordBuffer)
					if loginErr != nil {
						if params.PasswordFile != "" {
							return cli.Conflict("account %s already exists but the provided password does not match (the existing password was not changed)", userID)
						}
						return cli.Conflict("account %s already exists and login with derived password failed: %w", userID, loginErr)
					}
					fmt.Fprintf(os.Stderr, "Account %s already exists, logged in.\n", userID)
					fmt.Fprintf(os.Stderr, "Ensuring room membership.\n")
				} else {
					return cli.Internal("register user %q: %w", username, registerErr)
				}
			} else if !params.OutputJSON {
				fmt.Fprintf(os.Stdout, "User ID:       %s\n", session.UserID())
				fmt.Fprintf(os.Stdout, "Access Token:  %s\n", session.AccessToken())
			}

			if params.Operator {
				// Operator mode: admin invites, then the user's own session
				// joins each room. Invite-only rooms require both steps.
				if err := onboardOperator(ctx, client, credentials, userID, session); err != nil {
					return err
				}
			}

			result := userCreateResult{
				UserID:        session.UserID().String(),
				AlreadyExists: alreadyExists,
			}
			if !alreadyExists {
				result.AccessToken = session.AccessToken()
			}
			if done, err := params.EmitJSON(result); done {
				return err
			}

			return nil
		},
	}
}

// onboardOperator invites a user to all Bureau infrastructure rooms and
// joins them, so they're a full member immediately. The admin session
// (from the credential file) handles invites; the user's own session
// handles joins. Each step is idempotent: already-invited users skip
// the invite, already-joined users skip the join.
func onboardOperator(ctx context.Context, client *messaging.Client, credentials map[string]string, userID ref.UserID, userSession *messaging.DirectSession) error {
	adminUserIDString := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminUserIDString == "" || adminToken == "" {
		return cli.Validation("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}

	adminUserID, err := ref.ParseUserID(adminUserIDString)
	if err != nil {
		return cli.Internal("parse admin user ID: %w", err)
	}

	adminSession, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return cli.Internal("creating admin session: %w", err)
	}
	defer adminSession.Close()

	// Bureau infrastructure rooms from the credential file. The space is
	// first (so the user can see the hierarchy), then all standard rooms
	// defined in standardRooms (same list doctor validates against).
	type bureauRoom struct {
		name          string
		credentialKey string
	}
	bureauRooms := []bureauRoom{
		{"bureau (space)", "MATRIX_SPACE_ROOM"},
	}
	for _, room := range standardRooms {
		bureauRooms = append(bureauRooms, bureauRoom{
			name:          room.alias,
			credentialKey: room.credentialKey,
		})
	}

	for _, room := range bureauRooms {
		roomIDString := credentials[room.credentialKey]
		if roomIDString == "" {
			return cli.Validation("credential file missing %s", room.credentialKey)
		}

		roomID, parseErr := ref.ParseRoomID(roomIDString)
		if parseErr != nil {
			return cli.Validation("invalid room ID for %s: %w", room.credentialKey, parseErr)
		}

		// Step 1: Admin invites the user. Idempotent — M_FORBIDDEN
		// means the user is already invited or already a member.
		alreadyMember := false
		err := adminSession.InviteUser(ctx, roomID, userID)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				alreadyMember = true
			} else {
				return cli.Internal("invite %s to %s (%s): %w", userID, room.name, roomID, err)
			}
		}

		// Step 2: User joins the room. Idempotent — joining a room
		// you're already in is a no-op.
		_, err = userSession.JoinRoom(ctx, roomID)
		if err != nil {
			return cli.Internal("join %s (%s): %w", room.name, roomID, err)
		}

		if alreadyMember {
			fmt.Fprintf(os.Stderr, "  %-20s already a member\n", room.name)
		} else {
			fmt.Fprintf(os.Stderr, "  %-20s joined\n", room.name)
		}
	}

	fmt.Fprintf(os.Stderr, "Operator %s is a member of all Bureau rooms.\n", userID)
	return nil
}

// readPassword reads a password from a file path into a secret.Buffer.
// If the path is "-", it prompts on the terminal with echo disabled and
// asks for confirmation. If stdin is not a terminal (piped input), it
// reads one line instead. The caller must Close the returned buffer.
func readPassword(path string) (*secret.Buffer, error) {
	if path != "-" {
		return secret.ReadFromPath(path)
	}

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		// Stdin is piped — read one line without prompting.
		return secret.ReadFromPath("-")
	}

	// Interactive terminal — prompt with echo disabled, confirm.
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(stdinFd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, cli.Internal("reading password: %w", err)
	}

	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(stdinFd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		secret.Zero(first)
		return nil, cli.Internal("reading password confirmation: %w", err)
	}

	match := len(first) == len(second)
	if match {
		for index := range first {
			if first[index] != second[index] {
				match = false
				break
			}
		}
	}
	secret.Zero(second)

	if !match {
		secret.Zero(first)
		return nil, cli.Validation("passwords do not match")
	}

	// Move into mmap-backed buffer; NewFromBytes zeros the source.
	buffer, err := secret.NewFromBytes(first)
	if err != nil {
		secret.Zero(first)
		return nil, err
	}
	return buffer, nil
}

// userListParams holds the parameters for the matrix user list command.
type userListParams struct {
	cli.SessionConfig
	Room string `json:"room" flag:"room" desc:"room alias or ID to list members of"`
	cli.JSONOutput
}

// userListEntry holds the JSON-serializable data for a single user listing.
type userListEntry struct {
	UserID      string `json:"user_id"                desc:"user's Matrix ID"`
	DisplayName string `json:"display_name,omitempty" desc:"user's display name"`
	Membership  string `json:"membership,omitempty"   desc:"membership state in room"`
}

// userListCommand returns the "user list" subcommand for listing Matrix users.
// With --room, lists members of a specific room. Without --room, aggregates
// unique members across all joined rooms.
func userListCommand() *cli.Command {
	var params userListParams

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
				Command:     "bureau matrix user list --room '#bureau/machine:bureau.local' --credential-file ./creds",
			},
			{
				Description: "List all known users across joined rooms",
				Command:     "bureau matrix user list --credential-file ./creds",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &[]userListEntry{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/user/list"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			if params.Room != "" {
				return listRoomMembers(ctx, sess, params.Room, &params.JSONOutput)
			}
			return listAllMembers(ctx, sess, &params.JSONOutput)
		},
	}
}

// listRoomMembers lists members of a single room, resolving aliases as needed.
func listRoomMembers(ctx context.Context, session messaging.Session, roomIDOrAlias string, jsonOutput *cli.JSONOutput) error {
	roomID, err := resolveRoom(ctx, session, roomIDOrAlias)
	if err != nil {
		return err
	}

	members, err := session.GetRoomMembers(ctx, roomID)
	if err != nil {
		return cli.Internal("get room members: %w", err)
	}

	var entries []userListEntry
	for _, member := range members {
		entries = append(entries, userListEntry{
			UserID:      member.UserID.String(),
			DisplayName: member.DisplayName,
			Membership:  member.Membership,
		})
	}
	if done, err := jsonOutput.EmitJSON(entries); done {
		return err
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
func listAllMembers(ctx context.Context, session messaging.Session, jsonOutput *cli.JSONOutput) error {
	rooms, err := session.JoinedRooms(ctx)
	if err != nil {
		return cli.Internal("get joined rooms: %w", err)
	}

	// Collect unique members across all rooms.
	uniqueMembers := make(map[ref.UserID]messaging.RoomMember)
	for _, roomID := range rooms {
		members, err := session.GetRoomMembers(ctx, roomID)
		if err != nil {
			return cli.Internal("get members for room %s: %w", roomID, err)
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

	var entries []userListEntry
	for _, member := range uniqueMembers {
		displayName := member.DisplayName
		if displayName == "" {
			fetched, err := session.GetDisplayName(ctx, member.UserID)
			if err == nil {
				displayName = fetched
			}
		}
		entries = append(entries, userListEntry{
			UserID:      member.UserID.String(),
			DisplayName: displayName,
		})
	}
	if done, err := jsonOutput.EmitJSON(entries); done {
		return err
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "USER ID\tDISPLAY NAME")
	for _, member := range uniqueMembers {
		displayName := member.DisplayName
		if displayName == "" {
			// The /members endpoint may not include display names. Fall
			// back to the profile endpoint for users without one.
			fetched, err := session.GetDisplayName(ctx, member.UserID)
			if err == nil {
				displayName = fetched
			}
		}
		fmt.Fprintf(writer, "%s\t%s\n", member.UserID, displayName)
	}
	return writer.Flush()
}

// userInviteParams holds the parameters for the matrix user invite command.
// UserID is positional in CLI mode (args[0]) and a named property in JSON/MCP mode.
type userInviteParams struct {
	cli.SessionConfig
	UserID string `json:"user_id" desc:"Matrix user ID to invite (e.g. @alice:bureau.local)" required:"true"`
	Room   string `json:"room"    flag:"room" desc:"room alias or ID to invite the user to (required)"`
	cli.JSONOutput
}

// userInviteResult is the JSON output for matrix user invite.
type userInviteResult struct {
	UserID string `json:"user_id" desc:"invited user's Matrix ID"`
	RoomID string `json:"room_id" desc:"room the user was invited to"`
}

// userInviteCommand returns the "user invite" subcommand.
func userInviteCommand() *cli.Command {
	var params userInviteParams

	return &cli.Command{
		Name:    "invite",
		Summary: "Invite a user to a room",
		Description: `Invite a Matrix user to a room. The room can be specified by alias
(e.g., "#bureau/machine:bureau.local") or by room ID.`,
		Usage: "bureau matrix user invite <user-id> --room <room> [flags]",
		Examples: []cli.Example{
			{
				Description: "Invite a user to a room by alias",
				Command:     "bureau matrix user invite @alice:bureau.local --room '#bureau/machine:bureau.local' --credential-file ./creds",
			},
		},
		Annotations:    cli.Idempotent(),
		Output:         func() any { return &userInviteResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/user/invite"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, user ID comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.UserID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.UserID == "" {
				return cli.Validation("user ID is required\n\nusage: bureau matrix user invite <user-id> --room <room>")
			}
			targetUserID := params.UserID

			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			roomID, err := resolveRoom(ctx, sess, params.Room)
			if err != nil {
				return err
			}

			parsedTargetUserID, err := ref.ParseUserID(targetUserID)
			if err != nil {
				return cli.Validation("invalid user ID %q: %w", targetUserID, err)
			}

			if err := sess.InviteUser(ctx, roomID, parsedTargetUserID); err != nil {
				return cli.Internal("invite user: %w", err)
			}

			if done, err := params.EmitJSON(userInviteResult{
				UserID: targetUserID,
				RoomID: roomID.String(),
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Invited %s to %s\n", targetUserID, params.Room)
			return nil
		},
	}
}

// userKickParams holds the parameters for the matrix user kick command.
// UserID is positional in CLI mode (args[0]) and a named property in JSON/MCP mode.
type userKickParams struct {
	cli.SessionConfig
	UserID string `json:"user_id" desc:"Matrix user ID to kick (e.g. @bob:bureau.local)" required:"true"`
	Room   string `json:"room"    flag:"room"   desc:"room alias or ID to kick the user from (required)"`
	Reason string `json:"reason"  flag:"reason" desc:"reason for the kick"`
	cli.JSONOutput
}

// userKickResult is the JSON output for matrix user kick.
type userKickResult struct {
	UserID string `json:"user_id" desc:"kicked user's Matrix ID"`
	RoomID string `json:"room_id" desc:"room the user was kicked from"`
}

// userKickCommand returns the "user kick" subcommand.
func userKickCommand() *cli.Command {
	var params userKickParams

	return &cli.Command{
		Name:    "kick",
		Summary: "Kick a user from a room",
		Description: `Kick (remove) a Matrix user from a room. The room can be specified by
alias or room ID. An optional --reason provides context for the kick.`,
		Usage: "bureau matrix user kick <user-id> --room <room> [flags]",
		Examples: []cli.Example{
			{
				Description: "Kick a user from a room",
				Command:     "bureau matrix user kick @bob:bureau.local --room '#bureau/machine:bureau.local' --credential-file ./creds",
			},
			{
				Description: "Kick with a reason",
				Command:     "bureau matrix user kick @bob:bureau.local --room '#bureau/machine:bureau.local' --reason 'decommissioned' --credential-file ./creds",
			},
		},
		Annotations:    cli.Destructive(),
		Output:         func() any { return &userKickResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/user/kick"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, user ID comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.UserID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.UserID == "" {
				return cli.Validation("user ID is required\n\nusage: bureau matrix user kick <user-id> --room <room>")
			}
			targetUserID := params.UserID

			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			roomID, err := resolveRoom(ctx, sess, params.Room)
			if err != nil {
				return err
			}

			directSession, ok := sess.(*messaging.DirectSession)
			if !ok {
				return cli.Validation("user kick requires operator credentials (not available inside sandboxes)")
			}
			parsedKickUserID, err := ref.ParseUserID(targetUserID)
			if err != nil {
				return cli.Validation("invalid user ID %q: %w", targetUserID, err)
			}
			if err := directSession.KickUser(ctx, roomID, parsedKickUserID, params.Reason); err != nil {
				return cli.Internal("kick user: %w", err)
			}

			if done, err := params.EmitJSON(userKickResult{
				UserID: targetUserID,
				RoomID: roomID.String(),
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "Kicked %s from %s\n", targetUserID, params.Room)
			return nil
		},
	}
}

// userWhoAmIParams holds the parameters for the matrix user whoami command.
type userWhoAmIParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

// userWhoAmIResult is the JSON output for matrix user whoami.
type userWhoAmIResult struct {
	UserID string `json:"user_id" desc:"authenticated user's Matrix ID"`
}

// userWhoAmICommand returns the "user whoami" subcommand.
func userWhoAmICommand() *cli.Command {
	var params userWhoAmIParams

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
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &userWhoAmIResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/user/whoami"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			userID, err := sess.WhoAmI(ctx)
			if err != nil {
				return cli.Internal("whoami: %w", err)
			}

			if done, err := params.EmitJSON(userWhoAmIResult{UserID: userID.String()}); done {
				return err
			}

			fmt.Fprintln(os.Stdout, userID)
			return nil
		},
	}
}
