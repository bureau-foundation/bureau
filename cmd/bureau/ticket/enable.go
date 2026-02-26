// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	ticketschema "github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// enableParams holds the parameters for the ticket enable command.
type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Space      string `json:"space"       flag:"space"       desc:"project space alias (e.g., iree) — scopes the ticket service to rooms in this space"`
	Host       string `json:"host"        flag:"host"        desc:"fleet-scoped machine localpart (e.g., bureau/fleet/prod/machine/workstation; use 'local' to auto-detect)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
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
  - Validates the ticket-service template exists in Matrix
  - Registers a Matrix account for the ticket service
  - Provisions encrypted credentials to the target machine
  - Publishes a PrincipalAssignment to the machine's config room
  - For each existing room in the space: publishes m.bureau.ticket_config,
    sets the m.bureau.service_binding (role=ticket), invites the
    service, and configures power levels

The ticket service uses the "ticket-service" template (published by
"bureau matrix setup"). The daemon manages the service's sandbox
lifecycle — starting it automatically when the assignment is published.

Re-running ticket enable on a space is safe: if the service is already
deployed, it skips deployment and re-configures rooms (useful when new
rooms have been added to the space).

Example:

  bureau ticket enable --space iree --host bureau/fleet/prod/machine/workstation --credential-file ./creds

This creates service principal "service/ticket/iree", adds it to the
workstation's MachineConfig, and enables tickets in all rooms under
#iree:bureau.local.`,
		Usage: "bureau ticket enable --space <space> --host <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable tickets for the iree space on a workstation",
				Command:     "bureau ticket enable --space iree --host bureau/fleet/prod/machine/workstation --credential-file ./creds",
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
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.Space == "" {
				return cli.Validation("--space is required")
			}
			if params.Host == "" {
				return cli.Validation("--host is required")
			}
			return runEnable(ctx, logger, &params)
		},
	}
}

// enableResult is the JSON output of the enable command.
type enableResult struct {
	ServicePrincipal string      `json:"service_principal" desc:"service principal localpart"`
	ServiceUserID    string      `json:"service_user_id"   desc:"service Matrix user ID"`
	Machine          string      `json:"machine"           desc:"machine hosting the service"`
	ConfigRoomID     ref.RoomID  `json:"config_room_id"    desc:"config room where the assignment was published"`
	ConfigEventID    ref.EventID `json:"config_event_id"   desc:"event ID of the MachineConfig state event"`
	SpaceAlias       string      `json:"space_alias"       desc:"Matrix space alias"`
	SpaceRoomID      string      `json:"space_room_id"     desc:"Matrix space room ID"`
	RoomsConfigured  []string    `json:"rooms_configured"  desc:"room IDs configured for tickets"`
}

func runEnable(ctx context.Context, logger *slog.Logger, params *enableParams) error {
	// Resolve "local" to the actual machine localpart.
	host := params.Host
	if host == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return cli.Internal("resolving local machine identity: %w", err)
		}
		host = resolved
		logger.Info("resolved local machine", "host", host)
	}

	params.ServerName = cli.ResolveServerName(params.ServerName)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}

	// Parse and validate the machine localpart as a typed ref. The
	// fleet is derived from the machine ref since every machine belongs
	// to exactly one fleet.
	machineRef, err := ref.ParseMachine(host, serverName)
	if err != nil {
		return cli.Validation("invalid host: %w", err)
	}
	fleet := machineRef.Fleet()

	// Derive the service principal from the space name, scoped to the
	// machine's fleet.
	servicePrincipal := "service/ticket/" + params.Space
	serviceEntity, err := ref.NewEntityFromAccountLocalpart(fleet, servicePrincipal)
	if err != nil {
		return cli.Internal("invalid service principal %q: %w", servicePrincipal, err)
	}

	serviceUserID := serviceEntity.UserID()

	namespace, err := ref.NewNamespace(serverName, params.Space)
	if err != nil {
		return cli.Validation("invalid space name: %w", err)
	}
	spaceAlias := namespace.SpaceAlias()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	if params.SessionConfig.CredentialFile == "" {
		return cli.Validation("--credential-file is required for service account registration")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("reading credentials: %w", err)
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN")
	}

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	// Connect as admin for state event publishing, credential
	// provisioning, and room management.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting admin session: %w", err)
	}
	defer adminSession.Close()

	// Account registration (client.Register) is unauthenticated and
	// needs a Client, not a Session.
	homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
	if err != nil {
		return err
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("creating matrix client: %w", err)
	}

	// Resolve the fleet machine room for credential provisioning.
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := adminSession.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return cli.Internal("resolve fleet machine room %q: %w", machineRoomAlias, err)
	}

	// The ticket-service template is published by "bureau matrix
	// setup". It inherits from service-base which provides the proxy
	// bootstrap environment variables.
	templateRef := schema.TemplateRef{
		Room:     "bureau/template",
		Template: "ticket-service",
	}

	// Deploy the ticket service through the standard principal
	// deployment path: register account, provision encrypted
	// credentials, invite to config room, publish MachineConfig
	// assignment. The daemon detects the assignment via /sync and
	// creates the ticket service's sandbox with AutoStart:true.
	createParams := principal.CreateParams{
		Machine:     machineRef,
		Principal:   serviceEntity,
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, templateReference schema.TemplateRef, sn ref.ServerName) error {
			_, err := templatedef.Fetch(ctx, adminSession, templateReference, sn)
			return err
		},
		HomeserverURL: homeserverURL,
		AutoStart:     true,
		MachineRoomID: machineRoomID,
		Labels: map[string]string{
			"role":    "service",
			"service": "ticket",
			"space":   params.Space,
		},
	}

	var createResult *principal.CreateResult
	createResult, err = principal.Create(ctx, client, adminSession, registrationTokenBuffer, credential.AsProvisionFunc(), createParams)
	if err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
			return cli.Internal("deploying ticket service: %w", err)
		}

		// The ticket service account already exists. This is
		// expected when re-running ticket enable to configure new
		// rooms added to the space. Ensure the MachineConfig
		// assignment is published — principal.Create skips the
		// entire flow (including config assignment) when the account
		// exists.
		logger.Info("ticket service account exists, ensuring config assignment",
			"user_id", serviceUserID.String())

		configRoomID, resolveErr := adminSession.ResolveAlias(ctx, machineRef.RoomAlias())
		if resolveErr != nil {
			return cli.Internal("resolve config room %s: %w", machineRef.RoomAlias(), resolveErr)
		}

		configEventID, assignErr := principal.AssignPrincipals(ctx, adminSession, configRoomID, []principal.CreateParams{createParams})
		if assignErr != nil {
			return cli.Internal("ensuring ticket service assignment: %w", assignErr)
		}

		createResult = &principal.CreateResult{
			PrincipalUserID: serviceUserID,
			Machine:         machineRef,
			TemplateRef:     templateRef,
			ConfigRoomID:    configRoomID,
			ConfigEventID:   configEventID,
		}
	} else {
		logger.Info("ticket service deployed",
			"user_id", createResult.PrincipalUserID,
			"config_room", createResult.ConfigRoomID,
			"config_event", createResult.ConfigEventID,
		)
	}

	// Resolve the space and discover child rooms.
	spaceRoomID, err := adminSession.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		return cli.NotFound("resolving space %s: %w (has 'bureau matrix space create %s' been run?)", spaceAlias, err, params.Space)
	}

	childRoomIDs, err := cli.GetSpaceChildren(ctx, adminSession, spaceRoomID)
	if err != nil {
		return cli.Internal("listing space children: %w", err)
	}

	logger.Info("discovered space", "alias", spaceAlias, "room_id", spaceRoomID, "child_rooms", len(childRoomIDs))

	// Configure each room for ticket management.
	configuredRooms := make([]string, 0, len(childRoomIDs))
	for _, roomID := range childRoomIDs {
		if err := ConfigureRoom(ctx, logger, adminSession, roomID, serviceEntity, ConfigureRoomParams{Prefix: params.Prefix}); err != nil {
			logger.Warn("failed to configure room", "room_id", roomID, "error", err)
			continue
		}
		configuredRooms = append(configuredRooms, roomID.String())
		logger.Info("configured room", "room_id", roomID)
	}

	// Invite the service to the space itself so the daemon's /sync
	// delivers new room events to the service.
	if err := adminSession.InviteUser(ctx, spaceRoomID, serviceUserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			logger.Warn("failed to invite service to space", "error", err)
		}
	}

	result := enableResult{
		ServicePrincipal: serviceEntity.Localpart(),
		ServiceUserID:    serviceUserID.String(),
		Machine:          host,
		ConfigRoomID:     createResult.ConfigRoomID,
		ConfigEventID:    createResult.ConfigEventID,
		SpaceAlias:       spaceAlias.String(),
		SpaceRoomID:      spaceRoomID.String(),
		RoomsConfigured:  configuredRooms,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("ticket service enabled",
		"service_principal", serviceEntity.Localpart(),
		"service_user_id", serviceUserID,
		"machine", host,
		"template", templateRef,
		"space_alias", spaceAlias,
		"space_room_id", spaceRoomID,
		"rooms_configured", len(configuredRooms),
	)

	return nil
}

// ConfigureRoomParams holds parameters for ConfigureRoom.
type ConfigureRoomParams struct {
	// Prefix is the ticket ID prefix for this room (e.g., "tkt", "pip").
	Prefix string

	// AllowedTypes restricts which ticket types can be created in this room.
	// An empty slice allows all types.
	AllowedTypes []ticketschema.TicketType
}

// ConfigureRoom enables ticket management in an existing room:
//   - Publishes m.bureau.ticket_config (enables ticket management)
//   - Publishes m.bureau.service_binding with state_key "ticket" (binds the service)
//   - Invites the service principal
//   - Configures power levels (service at PL 10, m.bureau.ticket at PL 10,
//     m.bureau.ticket_config and m.bureau.service_binding at PL 100)
func ConfigureRoom(ctx context.Context, logger *slog.Logger, session messaging.Session, roomID ref.RoomID, serviceEntity ref.Entity, params ConfigureRoomParams) error {
	// Publish ticket config (singleton, state_key="").
	ticketConfig := ticketschema.TicketConfigContent{
		Version:      ticketschema.TicketConfigVersion,
		Prefix:       params.Prefix,
		AllowedTypes: params.AllowedTypes,
	}
	_, err := session.SendStateEvent(ctx, roomID, schema.EventTypeTicketConfig, "", ticketConfig)
	if err != nil {
		return cli.Internal("publishing ticket config: %w", err)
	}

	// Publish service binding (state_key="ticket").
	binding := schema.ServiceBindingContent{
		Principal: serviceEntity,
	}
	_, err = session.SendStateEvent(ctx, roomID, schema.EventTypeServiceBinding, "ticket", binding)
	if err != nil {
		return cli.Internal("publishing service binding: %w", err)
	}

	// Invite the service to the room (idempotent — M_FORBIDDEN means already a member).
	serviceUserID := serviceEntity.UserID()
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

// configureTicketPowerLevels performs a batched read-modify-write on a room's
// m.room.power_levels to add ticket-related entries:
//   - Service principal user gets PL 10
//   - m.bureau.ticket event type requires PL 10
//   - m.bureau.ticket_config requires PL 100 (admin-only)
//   - m.bureau.service_binding requires PL 100 (admin-only)
func configureTicketPowerLevels(ctx context.Context, session messaging.Session, roomID ref.RoomID, serviceUserID ref.UserID) error {
	return schema.GrantPowerLevels(ctx, session, roomID, schema.PowerLevelGrants{
		Users: map[ref.UserID]int{serviceUserID: 10},
		Events: map[ref.EventType]int{
			schema.EventTypeTicket:         10,
			schema.EventTypeTicketConfig:   100,
			schema.EventTypeServiceBinding: 100,
		},
	})
}
