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
	"golang.org/x/term"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
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
// Credential-related flags are excluded from MCP schema via json:"-" since they
// involve reading secrets from files/stdin, which is not appropriate for MCP.
type userCreateParams struct {
	CredentialFile        string `json:"-"           flag:"credential-file"         desc:"path to Bureau credential file from 'bureau matrix setup' (provides homeserver URL and registration token)"`
	HomeserverURL         string `json:"-"           flag:"homeserver"              desc:"Matrix homeserver URL (overrides credential file; default http://localhost:6167)"`
	RegistrationTokenFile string `json:"-"           flag:"registration-token-file" desc:"path to file containing registration token, or - for stdin (overrides credential file)"`
	PasswordFile          string `json:"-"           flag:"password-file"           desc:"path to file containing password, or - to prompt interactively (default: derive from registration token)"`
	ServerName            string `json:"server_name" flag:"server-name"             desc:"Matrix server name for constructing user IDs" default:"bureau.local"`
	Operator              bool   `json:"operator"    flag:"operator"                desc:"invite the user to all Bureau infrastructure rooms (requires --credential-file)"`
	OutputJSON            bool   `json:"-"           flag:"json"                    desc:"output as JSON"`
}

// userCreateResult is the JSON output for matrix user create.
type userCreateResult struct {
	UserID        string `json:"user_id"`
	AccessToken   string `json:"access_token,omitempty"`
	AlreadyExists bool   `json:"already_exists"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("user create", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("username is required\n\nUsage: bureau matrix user create <username> [flags]")
			}
			username := args[0]
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if params.Operator && params.CredentialFile == "" {
				return fmt.Errorf("--operator requires --credential-file (needed for admin session and Bureau room discovery)")
			}

			// Parse the credential file once. Both the registration flow
			// and the operator invite flow read from it.
			var credentials map[string]string
			if params.CredentialFile != "" {
				var err error
				credentials, err = cli.ReadCredentialFile(params.CredentialFile)
				if err != nil {
					return fmt.Errorf("read credential file: %w", err)
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
					return fmt.Errorf("--registration-token-file and --password-file cannot both be - (stdin)")
				}
				tokenBuffer, err := secret.ReadFromPath(params.RegistrationTokenFile)
				if err != nil {
					return fmt.Errorf("read registration token: %w", err)
				}
				defer tokenBuffer.Close()
				registrationToken = tokenBuffer.String()
			}
			if registrationToken == "" {
				return fmt.Errorf("registration token is required (use --credential-file or --registration-token-file)")
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
					return fmt.Errorf("read password: %w", err)
				}
				defer passwordBuffer.Close()
			} else {
				// Agent accounts use a password derived from the registration
				// token. The derived value is deterministic, so re-running
				// with the same token produces the same account.
				tokenBuffer, tokenErr := secret.NewFromString(registrationToken)
				if tokenErr != nil {
					return fmt.Errorf("protecting registration token: %w", tokenErr)
				}
				defer tokenBuffer.Close()
				passwordBuffer, err = deriveAdminPassword(tokenBuffer)
				if err != nil {
					return fmt.Errorf("derive password: %w", err)
				}
				defer passwordBuffer.Close()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return fmt.Errorf("create matrix client: %w", err)
			}

			// Register the account. In operator mode, M_USER_IN_USE means
			// the account already exists — verify the password matches
			// (if one was explicitly provided) and proceed to room invites.
			userID := fmt.Sprintf("@%s:%s", username, params.ServerName)
			registrationTokenBuffer, tokenErr := secret.NewFromString(registrationToken)
			if tokenErr != nil {
				return fmt.Errorf("protecting registration token: %w", tokenErr)
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
							return fmt.Errorf("account %s already exists but the provided password does not match (the existing password was not changed)", userID)
						}
						return fmt.Errorf("account %s already exists and login with derived password failed: %w", userID, loginErr)
					}
					fmt.Fprintf(os.Stderr, "Account %s already exists, logged in.\n", userID)
					fmt.Fprintf(os.Stderr, "Ensuring room membership.\n")
				} else {
					return fmt.Errorf("register user %q: %w", username, registerErr)
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

			if params.OutputJSON {
				result := userCreateResult{
					UserID:        session.UserID(),
					AlreadyExists: alreadyExists,
				}
				if !alreadyExists {
					result.AccessToken = session.AccessToken()
				}
				return cli.WriteJSON(result)
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
func onboardOperator(ctx context.Context, client *messaging.Client, credentials map[string]string, userID string, userSession *messaging.Session) error {
	adminUserID := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminUserID == "" || adminToken == "" {
		return fmt.Errorf("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}

	adminSession, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return fmt.Errorf("creating admin session: %w", err)
	}
	defer adminSession.Close()

	// Bureau infrastructure rooms from the credential file, in the order
	// they appear in setup. Each entry maps a human-readable name to the
	// credential file key holding the room ID.
	bureauRooms := []struct {
		name          string
		credentialKey string
	}{
		{"bureau (space)", "MATRIX_SPACE_ROOM"},
		{"bureau/system", "MATRIX_SYSTEM_ROOM"},
		{"bureau/machine", "MATRIX_MACHINE_ROOM"},
		{"bureau/service", "MATRIX_SERVICE_ROOM"},
	}

	for _, room := range bureauRooms {
		roomID := credentials[room.credentialKey]
		if roomID == "" {
			return fmt.Errorf("credential file missing %s", room.credentialKey)
		}

		// Step 1: Admin invites the user. Idempotent — M_FORBIDDEN
		// means the user is already invited or already a member.
		alreadyMember := false
		err := adminSession.InviteUser(ctx, roomID, userID)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				alreadyMember = true
			} else {
				return fmt.Errorf("invite %s to %s (%s): %w", userID, room.name, roomID, err)
			}
		}

		// Step 2: User joins the room. Idempotent — joining a room
		// you're already in is a no-op.
		_, err = userSession.JoinRoom(ctx, roomID)
		if err != nil {
			return fmt.Errorf("join %s (%s): %w", room.name, roomID, err)
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
		return nil, fmt.Errorf("reading password: %w", err)
	}

	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(stdinFd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		secret.Zero(first)
		return nil, fmt.Errorf("reading password confirmation: %w", err)
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
		return nil, fmt.Errorf("passwords do not match")
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
	Room       string `json:"room" flag:"room" desc:"room alias or ID to list members of"`
	OutputJSON bool   `json:"-"    flag:"json" desc:"output as JSON"`
}

// userListEntry holds the JSON-serializable data for a single user listing.
type userListEntry struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	Membership  string `json:"membership,omitempty"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("user list", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			if params.Room != "" {
				return listRoomMembers(ctx, sess, params.Room, params.OutputJSON)
			}
			return listAllMembers(ctx, sess, params.OutputJSON)
		},
	}
}

// listRoomMembers lists members of a single room, resolving aliases as needed.
func listRoomMembers(ctx context.Context, session *messaging.Session, roomIDOrAlias string, outputJSON bool) error {
	roomID, err := resolveRoom(ctx, session, roomIDOrAlias)
	if err != nil {
		return err
	}

	members, err := session.GetRoomMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("get room members: %w", err)
	}

	if outputJSON {
		var entries []userListEntry
		for _, member := range members {
			entries = append(entries, userListEntry{
				UserID:      member.UserID,
				DisplayName: member.DisplayName,
				Membership:  member.Membership,
			})
		}
		return cli.WriteJSON(entries)
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
func listAllMembers(ctx context.Context, session *messaging.Session, outputJSON bool) error {
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

	if outputJSON {
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
				UserID:      member.UserID,
				DisplayName: displayName,
			})
		}
		return cli.WriteJSON(entries)
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

// userInviteParams holds the parameters for the matrix user invite command.
type userInviteParams struct {
	cli.SessionConfig
	Room       string `json:"room" flag:"room" desc:"room alias or ID to invite the user to (required)"`
	OutputJSON bool   `json:"-"    flag:"json" desc:"output as JSON"`
}

// userInviteResult is the JSON output for matrix user invite.
type userInviteResult struct {
	UserID string `json:"user_id"`
	RoomID string `json:"room_id"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("invite", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("usage: bureau matrix user invite <user-id> --room <room>")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			targetUserID := args[0]

			if params.Room == "" {
				return fmt.Errorf("--room is required")
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

			if err := sess.InviteUser(ctx, roomID, targetUserID); err != nil {
				return fmt.Errorf("invite user: %w", err)
			}

			if params.OutputJSON {
				return cli.WriteJSON(userInviteResult{
					UserID: targetUserID,
					RoomID: roomID,
				})
			}

			fmt.Fprintf(os.Stdout, "Invited %s to %s\n", targetUserID, params.Room)
			return nil
		},
	}
}

// userKickParams holds the parameters for the matrix user kick command.
type userKickParams struct {
	cli.SessionConfig
	Room       string `json:"room"   flag:"room"   desc:"room alias or ID to kick the user from (required)"`
	Reason     string `json:"reason" flag:"reason" desc:"reason for the kick"`
	OutputJSON bool   `json:"-"      flag:"json"   desc:"output as JSON"`
}

// userKickResult is the JSON output for matrix user kick.
type userKickResult struct {
	UserID string `json:"user_id"`
	RoomID string `json:"room_id"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("kick", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("usage: bureau matrix user kick <user-id> --room <room>")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			targetUserID := args[0]

			if params.Room == "" {
				return fmt.Errorf("--room is required")
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

			if err := sess.KickUser(ctx, roomID, targetUserID, params.Reason); err != nil {
				return fmt.Errorf("kick user: %w", err)
			}

			if params.OutputJSON {
				return cli.WriteJSON(userKickResult{
					UserID: targetUserID,
					RoomID: roomID,
				})
			}

			fmt.Fprintf(os.Stdout, "Kicked %s from %s\n", targetUserID, params.Room)
			return nil
		},
	}
}

// userWhoAmIParams holds the parameters for the matrix user whoami command.
type userWhoAmIParams struct {
	cli.SessionConfig
	OutputJSON bool `json:"-" flag:"json" desc:"output as JSON"`
}

// userWhoAmIResult is the JSON output for matrix user whoami.
type userWhoAmIResult struct {
	UserID string `json:"user_id"`
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
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("whoami", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			userID, err := sess.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("whoami: %w", err)
			}

			if params.OutputJSON {
				return cli.WriteJSON(userWhoAmIResult{UserID: userID})
			}

			fmt.Fprintln(os.Stdout, userID)
			return nil
		},
	}
}
