// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/messaging"
)

// ConfigureModelRoom sets up a Matrix room for model service
// configuration. This is the production path for enabling model
// alias management in a room:
//
//   - Publishes m.bureau.service_binding with state_key "model"
//     (binds the model service to this room for daemon socket discovery)
//   - Invites the model service principal
//   - Configures power levels so the service can publish alias events
//     (PL 50 for the service user and m.bureau.model_alias events;
//     PL 100 for provider, account, and service binding events)
//
// Idempotent: safe to call on a room that's already configured.
func ConfigureModelRoom(ctx context.Context, logger *slog.Logger, session messaging.Session, roomID ref.RoomID, serviceEntity ref.Entity) error {
	serviceUserID := serviceEntity.UserID()

	// Publish service binding (state_key="model"). The daemon's token
	// minting searches config rooms for m.bureau.service_binding with
	// the role as state key. Without this, operators running
	// "bureau model --service" cannot discover the model service socket.
	binding := schema.ServiceBindingContent{Principal: serviceEntity}
	if _, err := session.SendStateEvent(ctx, roomID, schema.EventTypeServiceBinding, "model", binding); err != nil {
		return fmt.Errorf("publishing service binding to room %s: %w", roomID, err)
	}

	// Invite the service to the room. Idempotent — M_FORBIDDEN when
	// the service is already a member is expected and silenced.
	if err := session.InviteUser(ctx, roomID, serviceUserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			return fmt.Errorf("inviting model service %s to room %s: %w", serviceUserID, roomID, err)
		}
	}

	// Configure power levels for the model service:
	//   - Service user PL 50: can publish m.bureau.model_alias events
	//     (needed for the model/sync action to create aliases from catalog discovery)
	//   - m.bureau.model_alias PL 50: allows service (and operators with PL >= 50)
	//     to create/update/delete aliases
	//   - m.bureau.model_provider PL 100: admin-only (prevents agents from
	//     reconfiguring provider endpoints)
	//   - m.bureau.model_account PL 100: admin-only (credential binding is
	//     security-critical)
	//   - m.bureau.service_binding PL 100: admin-only (prevents service
	//     reassignment)
	if err := schema.GrantPowerLevels(ctx, session, roomID, schema.PowerLevelGrants{
		Users: map[ref.UserID]int{serviceUserID: 50},
		Events: map[ref.EventType]int{
			modelschema.EventTypeModelAlias:    50,
			modelschema.EventTypeModelProvider: 100,
			modelschema.EventTypeModelAccount:  100,
			schema.EventTypeServiceBinding:     100,
		},
	}); err != nil {
		return fmt.Errorf("configuring power levels in room %s: %w", roomID, err)
	}

	logger.Info("model config room configured",
		"room_id", roomID,
		"service_user_id", serviceUserID,
	)

	return nil
}

// --- CLI enable command ---

type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Host       string `json:"host"        flag:"host"        desc:"fleet-scoped machine localpart (use 'local' to auto-detect)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
	Room       string `json:"room"        flag:"room"        desc:"existing room ID or alias for the model config room (created if not specified)"`
}

type enableResult struct {
	ServiceUserID string     `json:"service_user_id" desc:"model service Matrix user ID"`
	ConfigRoomID  ref.RoomID `json:"config_room_id"  desc:"room configured for model service"`
}

func enableCommand() *cli.Command {
	var params enableParams

	return &cli.Command{
		Name:    "enable",
		Summary: "Configure a room for model service use",
		Description: `Set up a Matrix room as the model service's configuration room.
This configures the service binding, invites the model service, and
grants appropriate power levels so the service can publish model alias
state events (required for catalog sync).

If --room is not specified, a new room named "model-config" is created.

The model service must already be deployed on the target machine
(present in the machine config). This command configures the room —
it does not deploy the service binary.

Idempotent: safe to re-run on an already-configured room.`,
		Usage: "bureau model enable --host <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable model service config on the local machine",
				Command:     "bureau model enable --host local --credential-file ./creds",
			},
			{
				Description: "Use an existing room for model config",
				Command:     "bureau model enable --host local --room '!abc:bureau.local' --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &enableResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/model/enable"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if params.Host == "" {
				return cli.Validation("--host is required")
			}
			return runModelEnable(ctx, logger, &params)
		},
	}
}

func runModelEnable(ctx context.Context, logger *slog.Logger, params *enableParams) error {
	host := params.Host
	if host == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return err
		}
		host = resolved
		logger.Info("resolved local machine", "host", host)
	}

	params.ServerName = cli.ResolveServerName(params.ServerName)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	machineRef, err := ref.ParseMachine(host, serverName)
	if err != nil {
		return cli.Validation("invalid host: %w", err)
	}
	fleet := machineRef.Fleet()

	// Derive the model service principal from the fleet.
	servicePrincipal := "service/model"
	serviceEntity, err := ref.NewEntityFromAccountLocalpart(fleet, servicePrincipal)
	if err != nil {
		return cli.Internal("invalid service principal %q: %w", servicePrincipal, err)
	}

	// Connect as admin for room creation and power level configuration.
	adminSession, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer adminSession.Close()

	// Resolve or create the config room.
	var configRoomID ref.RoomID
	if params.Room != "" {
		// Resolve the provided room ID or alias.
		configRoomID, err = cli.ResolveRoom(ctx, params.Room)
		if err != nil {
			return cli.NotFound("room %q not found: %w", params.Room, err)
		}
	} else {
		// Create a new config room.
		room, createErr := adminSession.CreateRoom(ctx, messaging.CreateRoomRequest{
			Name: "model-config",
		})
		if createErr != nil {
			return cli.Internal("creating model config room: %w", createErr)
		}
		configRoomID = room.RoomID
		logger.Info("created model config room", "room_id", configRoomID)
	}

	// Configure the room for model service use.
	if err := ConfigureModelRoom(ctx, logger, adminSession, configRoomID, serviceEntity); err != nil {
		return cli.Internal("configuring model room: %w", err)
	}

	result := enableResult{
		ServiceUserID: serviceEntity.UserID().String(),
		ConfigRoomID:  configRoomID,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("model service enabled",
		"service_user_id", serviceEntity.UserID(),
		"config_room_id", configRoomID,
	)

	return nil
}
