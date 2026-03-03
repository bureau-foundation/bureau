// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// setupParams holds the parameters for the fleet setup command. This is
// the union of createParams and enableParams: fleet rooms are created
// and the fleet controller is deployed in a single operation.
type setupParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Host    string   `json:"host"     flag:"host"     desc:"machine name within the fleet (e.g., workstation); use 'local' to auto-detect from the launcher session"`
	Invite  []string `json:"invite"   flag:"invite"   desc:"Matrix user ID to invite to fleet rooms (repeatable)"`
	DevTeam string   `json:"dev_team" flag:"dev-team"  desc:"dev team room alias (e.g., #bureau/dev:bureau.local); defaults to #<namespace>/dev:<server>"`
}

// setupResult is the JSON output of the fleet setup command. It combines
// the fleet room creation results with the fleet controller deployment
// results.
type setupResult struct {
	Fleet         string      `json:"fleet"            desc:"fleet localpart"`
	FleetRoomID   ref.RoomID  `json:"fleet_room_id"    desc:"fleet config room ID"`
	MachineRoomID ref.RoomID  `json:"machine_room_id"  desc:"machine presence room ID"`
	ServiceRoomID ref.RoomID  `json:"service_room_id"  desc:"service directory room ID"`
	Service       ref.Service `json:"service"          desc:"fleet controller service reference"`
	Machine       ref.Machine `json:"machine"          desc:"machine hosting the fleet controller"`
	ConfigRoomID  ref.RoomID  `json:"config_room_id"   desc:"config room where the fleet controller assignment was published"`
	ConfigEventID ref.EventID `json:"config_event_id"  desc:"event ID of the MachineConfig state event"`
	BindingCount  int         `json:"binding_count"    desc:"number of machine config rooms updated with fleet bindings"`
}

func setupCommand() *cli.Command {
	var params setupParams

	return &cli.Command{
		Name:    "setup",
		Summary: "Create fleet rooms and deploy the fleet controller",
		Description: `Set up a fleet in a single operation: create the fleet Matrix rooms
and deploy the fleet controller on a machine.

This combines "bureau fleet create" and "bureau fleet enable" into one
command. Both remain available as separate commands for cases where you
need them independently (e.g., creating fleet rooms before any machine
is provisioned).

The argument is a fleet localpart in the form "namespace/fleet/name"
(e.g., "bureau/fleet/prod"). The server name is derived from the
connected session's identity.

This command:
  - Creates the three fleet rooms (config, machine, service)
  - Adds all rooms as children of the namespace space
  - Publishes dev team metadata on all fleet rooms
  - Optionally invites users to all fleet rooms
  - Validates the fleet-controller template exists
  - Registers a Matrix account for the fleet controller
  - Provisions encrypted credentials to the target machine
  - Publishes a PrincipalAssignment to the target machine's config room
  - Publishes fleet service bindings to all machine config rooms

All operations are idempotent. Safe to re-run.`,
		Usage: "bureau fleet setup <namespace/fleet/name> --host <machine-name> --credential-file <path>",
		Examples: []cli.Example{
			{
				Description: "Set up a production fleet",
				Command:     "bureau fleet setup bureau/fleet/prod --host workstation --credential-file ./creds",
			},
			{
				Description: "Set up and invite a user",
				Command:     "bureau fleet setup bureau/fleet/staging --host workstation --credential-file ./creds --invite @alice:bureau.local",
			},
			{
				Description: "Set up on the local machine (auto-detect)",
				Command:     "bureau fleet setup bureau/fleet/prod --host local --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &setupResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/fleet/setup"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			if params.Host == "" {
				return cli.Validation("--host is required (machine name within the fleet, e.g., workstation)")
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required").
					WithHint("Pass --credential-file with the file from 'bureau matrix setup'.")
			}
			return runSetup(ctx, logger, args[0], &params)
		},
	}
}

func runSetup(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *setupParams) error {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Read credentials for registration token and admin session.
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return err
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file %s is missing MATRIX_REGISTRATION_TOKEN", params.SessionConfig.CredentialFile).
			WithHint("The credential file may be incomplete. Re-run 'bureau matrix setup' to regenerate it.")
	}

	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	// Single session for both phases.
	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

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

	// Derive server name from the connected session's identity.
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// Phase 1: Create fleet rooms.
	var devTeamOverride ref.RoomAlias
	if params.DevTeam != "" {
		devTeamOverride, err = ref.ParseRoomAlias(params.DevTeam)
		if err != nil {
			return cli.Validation("invalid --dev-team alias %q: %v", params.DevTeam, err)
		}
	}

	rooms, err := EnsureFleetRooms(ctx, session, fleet, devTeamOverride, logger)
	if err != nil {
		return err
	}

	for _, userIDString := range params.Invite {
		parsedUserID, err := ref.ParseUserID(userIDString)
		if err != nil {
			return cli.Validation("invalid invite user ID %q: %w", userIDString, err)
		}
		if err := InviteToFleetRooms(ctx, session, rooms, parsedUserID, logger); err != nil {
			return err
		}
	}

	logger.Info("fleet rooms created",
		"fleet", fleet.Localpart(),
		"config_room", rooms.ConfigRoomID,
		"machine_room", rooms.MachineRoomID,
		"service_room", rooms.ServiceRoomID,
	)

	// Phase 2: Deploy fleet controller.
	enableResult, err := EnableFleetController(ctx, logger, fleet, params.Host, session, homeserverURL, client, registrationTokenBuffer)
	if err != nil {
		return err
	}

	result := setupResult{
		Fleet:         fleet.Localpart(),
		FleetRoomID:   rooms.ConfigRoomID,
		MachineRoomID: rooms.MachineRoomID,
		ServiceRoomID: rooms.ServiceRoomID,
		Service:       enableResult.Service,
		Machine:       enableResult.Machine,
		ConfigRoomID:  enableResult.ConfigRoomID,
		ConfigEventID: enableResult.ConfigEventID,
		BindingCount:  enableResult.BindingCount,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("fleet setup complete",
		"fleet", fleet.Localpart(),
		"service", enableResult.Service.Localpart(),
		"machine", enableResult.Machine.Localpart(),
		"bindings", enableResult.BindingCount,
	)

	return nil
}
