// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// enableParams holds the parameters for the ticket enable command.
type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Space      string `json:"space"       flag:"space"       desc:"project space alias (e.g., iree) — scopes the ticket service to rooms in this space"`
	Host       string `json:"host"        flag:"host"        desc:"machine localpart to run the ticket service on (e.g., machine/workstation; use 'local' to auto-detect)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	Prefix     string `json:"prefix"      flag:"prefix"      desc:"ticket ID prefix for rooms in this space" default:"tkt"`
}

func enableCommand() *cli.Command {
	var params enableParams

	return &cli.Command{
		Name:    "enable",
		Summary: "Enable ticket management for a space",
		Description: `Bootstrap the ticket service for a project space.

This operator command configures a ticket service scoped to a Matrix space.
One ticket service instance manages tickets across all rooms within a space,
isolated from ticket services in other spaces.

The command:
  - Registers a Matrix account for the ticket service
  - Publishes a PrincipalAssignment to the machine's config room
  - For each existing room in the space: publishes m.bureau.ticket_config,
    sets the m.bureau.room_service binding (role=ticket), invites the
    service, and configures power levels
  - The daemon handles inviting the service to rooms created after enable

Example:

  bureau ticket enable --space iree --host machine/workstation --credential-file ./creds

This creates service principal "service/ticket/iree", adds it to the
workstation's MachineConfig, and enables tickets in all rooms under
#iree:bureau.local.`,
		Usage: "bureau ticket enable --space <space> --host <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable tickets for the iree space on a workstation",
				Command:     "bureau ticket enable --space iree --host machine/workstation --credential-file ./creds",
			},
			{
				Description: "Enable tickets on the local machine (auto-detect)",
				Command:     "bureau ticket enable --space iree --host local --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &enableResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/ticket/enable"},
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.Space == "" {
				return cli.Validation("--space is required")
			}
			if params.Host == "" {
				return cli.Validation("--host is required")
			}
			return runEnable(&params)
		},
	}
}

// enableResult is the JSON output of the enable command.
type enableResult struct {
	ServicePrincipal string   `json:"service_principal" desc:"service principal localpart"`
	ServiceUserID    string   `json:"service_user_id"   desc:"service Matrix user ID"`
	Machine          string   `json:"machine"           desc:"machine hosting the service"`
	SpaceAlias       string   `json:"space_alias"       desc:"Matrix space alias"`
	SpaceRoomID      string   `json:"space_room_id"     desc:"Matrix space room ID"`
	RoomsConfigured  []string `json:"rooms_configured"  desc:"room IDs configured for tickets"`
}

func runEnable(params *enableParams) error {
	// Resolve "local" to the actual machine localpart.
	host := params.Host
	if host == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return cli.Internal("resolving local machine identity: %w", err)
		}
		host = resolved
		fmt.Fprintf(os.Stderr, "Resolved --host=local to %s\n", host)
	}

	if err := principal.ValidateLocalpart(host); err != nil {
		return cli.Internal("invalid host: %w", err)
	}

	// Derive the service principal localpart from the space name.
	servicePrincipal := "service/ticket/" + params.Space
	if err := principal.ValidateLocalpart(servicePrincipal); err != nil {
		return cli.Internal("invalid service principal %q: %w", servicePrincipal, err)
	}

	serviceUserID := principal.MatrixUserID(servicePrincipal, params.ServerName)
	spaceAlias := schema.FullRoomAlias(params.Space, params.ServerName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	if params.SessionConfig.CredentialFile == "" {
		return cli.Validation("--credential-file is required for service account registration")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("reading credentials: %w", err)
	}

	// Connect as admin for state event publishing and room management.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting admin session: %w", err)
	}
	defer adminSession.Close()

	// Step 1: Register the ticket service Matrix account (idempotent).
	if err := registerServiceAccount(ctx, credentials, servicePrincipal, params.ServerName); err != nil {
		return cli.Internal("registering service account: %w", err)
	}

	// Step 2: Resolve the space and discover child rooms.
	spaceRoomID, err := adminSession.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		return cli.NotFound("resolving space %s: %w (has 'bureau matrix space create %s' been run?)", spaceAlias, err, params.Space)
	}

	childRoomIDs, err := getSpaceChildren(ctx, adminSession, spaceRoomID)
	if err != nil {
		return cli.Internal("listing space children: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Space %s (%s): %d rooms\n", spaceAlias, spaceRoomID, len(childRoomIDs))

	// Step 3: Publish PrincipalAssignment to the machine config room.
	if err := publishPrincipalAssignment(ctx, adminSession, host, servicePrincipal, params.Space, params.ServerName); err != nil {
		return cli.Internal("publishing principal assignment: %w", err)
	}

	// Step 4: Configure each room for ticket management.
	configuredRooms := make([]string, 0, len(childRoomIDs))
	for _, roomID := range childRoomIDs {
		if err := configureRoom(ctx, adminSession, roomID, serviceUserID, params.Prefix); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: failed to configure room %s: %v\n", roomID, err)
			continue
		}
		configuredRooms = append(configuredRooms, roomID)
		fmt.Fprintf(os.Stderr, "  Configured room %s\n", roomID)
	}

	// Step 5: Invite the service to the space itself so the daemon's
	// /sync delivers new room events to the service.
	if err := adminSession.InviteUser(ctx, spaceRoomID, serviceUserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			fmt.Fprintf(os.Stderr, "  WARNING: failed to invite service to space: %v\n", err)
		}
	}

	if done, err := params.EmitJSON(enableResult{
		ServicePrincipal: servicePrincipal,
		ServiceUserID:    serviceUserID,
		Machine:          host,
		SpaceAlias:       spaceAlias,
		SpaceRoomID:      spaceRoomID,
		RoomsConfigured:  configuredRooms,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nTicket service enabled:\n")
	fmt.Fprintf(os.Stderr, "  Service:  %s (%s)\n", servicePrincipal, serviceUserID)
	fmt.Fprintf(os.Stderr, "  Machine:  %s\n", host)
	fmt.Fprintf(os.Stderr, "  Space:    %s (%s)\n", spaceAlias, spaceRoomID)
	fmt.Fprintf(os.Stderr, "  Rooms:    %d configured\n", len(configuredRooms))
	fmt.Fprintf(os.Stderr, "\nNext steps:\n")
	fmt.Fprintf(os.Stderr, "  1. Deploy the ticket service binary to %s\n", host)
	fmt.Fprintf(os.Stderr, "  2. Configure it with Matrix session credentials (session.json)\n")
	fmt.Fprintf(os.Stderr, "  3. Start the ticket service — it will self-register via #bureau/service\n")

	return nil
}

// registerServiceAccount registers a Matrix account for the ticket service.
// Idempotent: M_USER_IN_USE is silently ignored.
func registerServiceAccount(ctx context.Context, credentials map[string]string, servicePrincipal, serverName string) error {
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		homeserverURL = "http://localhost:6167"
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("creating matrix client: %w", err)
	}

	// Derive password from registration token (same as bureau matrix user create
	// for agent accounts — deterministic, no interactive login needed).
	tokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer tokenBuffer.Close()

	passwordBuffer, err := cli.DeriveAdminPassword(tokenBuffer)
	if err != nil {
		return cli.Internal("deriving password: %w", err)
	}
	defer passwordBuffer.Close()

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	// The Matrix username is the full localpart with slashes, which becomes
	// the user ID @service/ticket/<space>:<server>.
	session, registerErr := client.Register(ctx, messaging.RegisterRequest{
		Username:          servicePrincipal,
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	if registerErr != nil {
		if messaging.IsMatrixError(registerErr, messaging.ErrCodeUserInUse) {
			fmt.Fprintf(os.Stderr, "Service account %s already exists.\n", principal.MatrixUserID(servicePrincipal, serverName))
			return nil
		}
		return cli.Internal("registering %s: %w", servicePrincipal, registerErr)
	}
	defer session.Close()

	fmt.Fprintf(os.Stderr, "Registered service account %s\n", session.UserID())
	return nil
}

// getSpaceChildren returns the room IDs of all child rooms in a space.
func getSpaceChildren(ctx context.Context, session messaging.Session, spaceRoomID string) ([]string, error) {
	events, err := session.GetRoomState(ctx, spaceRoomID)
	if err != nil {
		return nil, cli.Internal("fetching space state: %w", err)
	}

	var children []string
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			children = append(children, *event.StateKey)
		}
	}
	return children, nil
}

// publishPrincipalAssignment adds the ticket service to the machine's
// MachineConfig via read-modify-write. AutoStart is false because the
// ticket service binary is externally managed — it runs its own Matrix
// session and binds its CBOR socket directly to RunDirSocketPath. If
// AutoStart were true, the daemon would create a proxy at the same
// socket path, conflicting with the service binary.
//
// The PrincipalAssignment still serves a purpose with AutoStart=false:
// rebuildAuthorizationIndex processes ALL principals regardless of
// AutoStart, so the service appears in the authorization index for
// grant resolution and service token minting.
func publishPrincipalAssignment(ctx context.Context, session messaging.Session, host, servicePrincipal, space, serverName string) error {
	configRoomAlias := schema.FullRoomAlias(schema.EntityConfigRoomAlias(host), serverName)
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return cli.NotFound("resolving config room %s: %w (has the machine been registered?)", configRoomAlias, err)
	}

	// Read existing MachineConfig.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, host)
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingContent, &config); unmarshalErr != nil {
			return cli.Internal("parsing existing machine config: %w", unmarshalErr)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Internal("reading machine config: %w", err)
	}

	// Check if the principal already exists.
	for _, existing := range config.Principals {
		if existing.Localpart == servicePrincipal {
			fmt.Fprintf(os.Stderr, "Principal %s already in MachineConfig, skipping.\n", servicePrincipal)
			return nil
		}
	}

	config.Principals = append(config.Principals, schema.PrincipalAssignment{
		Localpart: servicePrincipal,
		AutoStart: false,
		Labels: map[string]string{
			"role":    "service",
			"service": "ticket",
			"space":   space,
		},
	})

	_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, host, config)
	if err != nil {
		return cli.Internal("publishing machine config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Published PrincipalAssignment for %s to %s\n", servicePrincipal, host)
	return nil
}

// configureRoom sets up ticket management in a single room:
//   - Publishes m.bureau.ticket_config (enables ticket management)
//   - Publishes m.bureau.room_service with state_key "ticket" (binds the service)
//   - Invites the service principal
//   - Configures power levels (service at PL 10, m.bureau.ticket at PL 10,
//     m.bureau.ticket_config and m.bureau.room_service at PL 100)
func configureRoom(ctx context.Context, session messaging.Session, roomID, serviceUserID, prefix string) error {
	// Publish ticket config (singleton, state_key="").
	ticketConfig := schema.TicketConfigContent{
		Version: schema.TicketConfigVersion,
		Prefix:  prefix,
	}
	_, err := session.SendStateEvent(ctx, roomID, schema.EventTypeTicketConfig, "", ticketConfig)
	if err != nil {
		return cli.Internal("publishing ticket config: %w", err)
	}

	// Publish room service binding (state_key="ticket").
	roomService := schema.RoomServiceContent{
		Principal: serviceUserID,
	}
	_, err = session.SendStateEvent(ctx, roomID, schema.EventTypeRoomService, "ticket", roomService)
	if err != nil {
		return cli.Internal("publishing room service binding: %w", err)
	}

	// Invite the service to the room (idempotent — M_FORBIDDEN means already a member).
	if err := session.InviteUser(ctx, roomID, serviceUserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			return cli.Internal("inviting service: %w", err)
		}
	}

	// Configure power levels. Read-modify-write on the existing power levels
	// to add the ticket service user and ticket event type requirements.
	if err := configureTicketPowerLevels(ctx, session, roomID, serviceUserID); err != nil {
		return cli.Internal("configuring power levels: %w", err)
	}

	return nil
}

// configureTicketPowerLevels performs a read-modify-write on a room's
// m.room.power_levels to add ticket-related entries:
//   - Service principal user gets PL 10
//   - m.bureau.ticket event type requires PL 10
//   - m.bureau.ticket_config requires PL 100 (admin-only)
//   - m.bureau.room_service requires PL 100 (admin-only)
func configureTicketPowerLevels(ctx context.Context, session messaging.Session, roomID, serviceUserID string) error {
	// Read current power levels.
	content, err := session.GetStateEvent(ctx, roomID, schema.MatrixEventTypePowerLevels, "")
	if err != nil {
		return cli.Internal("reading power levels: %w", err)
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(content, &powerLevels); err != nil {
		return cli.Internal("parsing power levels: %w", err)
	}

	// Ensure the users map exists and set the service principal to PL 10.
	users, _ := powerLevels["users"].(map[string]any)
	if users == nil {
		users = make(map[string]any)
		powerLevels["users"] = users
	}
	users[serviceUserID] = 10

	// Ensure the events map exists and set ticket event type PLs.
	events, _ := powerLevels["events"].(map[string]any)
	if events == nil {
		events = make(map[string]any)
		powerLevels["events"] = events
	}
	events[schema.EventTypeTicket] = 10
	events[schema.EventTypeTicketConfig] = 100
	events[schema.EventTypeRoomService] = 100

	// Write back the updated power levels.
	_, err = session.SendStateEvent(ctx, roomID, schema.MatrixEventTypePowerLevels, "", powerLevels)
	if err != nil {
		return cli.Internal("updating power levels: %w", err)
	}

	return nil
}
