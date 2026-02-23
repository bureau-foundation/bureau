// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// createParams holds the parameters for the fleet create command.
type createParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Invite []string `json:"invite" flag:"invite" desc:"Matrix user ID to invite to fleet rooms (repeatable)"`
}

// createResult is the JSON output of the fleet create command.
type createResult struct {
	Fleet         string     `json:"fleet"           desc:"fleet localpart"`
	FleetRoomID   ref.RoomID `json:"fleet_room_id"   desc:"fleet config room ID"`
	MachineRoomID ref.RoomID `json:"machine_room_id" desc:"machine presence room ID"`
	ServiceRoomID ref.RoomID `json:"service_room_id" desc:"service directory room ID"`
}

func createCommand() *cli.Command {
	var params createParams

	return &cli.Command{
		Name:    "create",
		Summary: "Create fleet rooms in a namespace",
		Description: `Create the three Matrix rooms that define a fleet: the fleet config
room, the machine presence room, and the service directory room.

The argument is a fleet localpart in the form "namespace/fleet/name"
(e.g., "bureau/fleet/prod"). The server name is derived from the
connected session's identity.

A fleet is an infrastructure isolation boundary within a namespace.
Each fleet has its own set of machines, services, and (optionally) a
fleet controller. Multiple fleets in the same namespace share
templates, pipelines, and artifacts but have independent machine pools
and service directories.

This command:
  - Validates the fleet localpart
  - Resolves the namespace space to verify it exists
  - Creates three rooms with proper power levels:
      #<ns>/fleet/<name>           fleet config, HA leases, service defs
      #<ns>/fleet/<name>/machine   machine keys, heartbeats, WebRTC
      #<ns>/fleet/<name>/service   service registrations
  - Adds all three rooms as children of the namespace space
  - Optionally invites users to all fleet rooms

Safe to re-run: all operations are idempotent (existing rooms are left
unchanged).`,
		Usage: "bureau fleet create <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a production fleet",
				Command:     "bureau fleet create bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Create a fleet and invite a user",
				Command:     "bureau fleet create bureau/fleet/staging --credential-file ./creds --invite @alice:bureau.local",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &createResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/create"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runCreate(args[0], &params)
		},
	}
}

func runCreate(fleetLocalpart string, params *createParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting: %w", err)
	}
	defer session.Close()

	// Derive server name from the connected session's identity.
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	// Parse and validate the fleet localpart. This validates the namespace
	// name, fleet name, and all derived identities before touching any rooms.
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	logger := cli.NewCommandLogger().With("command", "fleet/create", "fleet", fleet.Localpart())

	rooms, err := EnsureFleetRooms(ctx, session, fleet, logger)
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

	result := createResult{
		Fleet:         fleet.Localpart(),
		FleetRoomID:   rooms.ConfigRoomID,
		MachineRoomID: rooms.MachineRoomID,
		ServiceRoomID: rooms.ServiceRoomID,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nFleet created: %s\n", fleet.Localpart())
	fmt.Fprintf(os.Stderr, "  Config:   %s (%s)\n", fleet.RoomAlias(), rooms.ConfigRoomID)
	fmt.Fprintf(os.Stderr, "  Machine:  %s (%s)\n", fleet.MachineRoomAlias(), rooms.MachineRoomID)
	fmt.Fprintf(os.Stderr, "  Service:  %s (%s)\n", fleet.ServiceRoomAlias(), rooms.ServiceRoomID)
	if len(params.Invite) > 0 {
		fmt.Fprintf(os.Stderr, "  Invited:  %v\n", params.Invite)
	}

	return nil
}

// FleetRooms holds the Matrix room IDs for the three rooms that define a fleet.
type FleetRooms struct {
	ConfigRoomID  ref.RoomID
	MachineRoomID ref.RoomID
	ServiceRoomID ref.RoomID
}

// EnsureFleetRooms creates the three fleet rooms (config, machine, service)
// if they don't exist and adds them as children of the namespace space. All
// identity information comes from the fleet ref — no string decomposition
// at the call site.
func EnsureFleetRooms(ctx context.Context, session messaging.Session, fleet ref.Fleet, logger *slog.Logger) (FleetRooms, error) {
	// Resolve the namespace space that will parent these rooms.
	spaceAlias := fleet.Namespace().SpaceAlias()
	spaceRoomID, err := session.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return FleetRooms{}, cli.NotFound("namespace space %s not found (run 'bureau matrix setup' first)", spaceAlias)
		}
		return FleetRooms{}, cli.Internal("resolving namespace space %s: %w", spaceAlias, err)
	}

	adminUserID := session.UserID()
	server := fleet.Server()
	namespace := fleet.Namespace().Name()
	fleetName := fleet.FleetName()

	// Fleet config room: HA leases, fleet service definitions, fleet config.
	configRoomID, err := idempotentCreateRoom(ctx, session, fleet.RoomAlias(), spaceRoomID, server,
		messaging.CreateRoomRequest{
			Name:                      fmt.Sprintf("Fleet: %s/%s", namespace, fleetName),
			Alias:                     fleet.Localpart(),
			Topic:                     "Fleet configuration, HA leases, and service definitions",
			Preset:                    "private_chat",
			Visibility:                "private",
			PowerLevelContentOverride: schema.FleetRoomPowerLevels(adminUserID),
		}, logger)
	if err != nil {
		return FleetRooms{}, err
	}

	// Machine presence room: keys, hardware info, heartbeats, WebRTC signaling.
	machineRoomID, err := idempotentCreateRoom(ctx, session, fleet.MachineRoomAlias(), spaceRoomID, server,
		messaging.CreateRoomRequest{
			Name:                      fmt.Sprintf("Machines: %s/%s", namespace, fleetName),
			Alias:                     fleet.MachineRoomAliasLocalpart(),
			Topic:                     "Machine keys, hardware info, status heartbeats, and WebRTC signaling",
			Preset:                    "private_chat",
			Visibility:                "private",
			PowerLevelContentOverride: schema.MachineRoomPowerLevels(adminUserID),
		}, logger)
	if err != nil {
		return FleetRooms{}, err
	}

	// Service directory room: service registrations and discovery.
	serviceRoomID, err := idempotentCreateRoom(ctx, session, fleet.ServiceRoomAlias(), spaceRoomID, server,
		messaging.CreateRoomRequest{
			Name:                      fmt.Sprintf("Services: %s/%s", namespace, fleetName),
			Alias:                     fleet.ServiceRoomAliasLocalpart(),
			Topic:                     "Service registrations and discovery",
			Preset:                    "private_chat",
			Visibility:                "private",
			PowerLevelContentOverride: schema.ServiceRoomPowerLevels(adminUserID),
		}, logger)
	if err != nil {
		return FleetRooms{}, err
	}

	return FleetRooms{
		ConfigRoomID:  configRoomID,
		MachineRoomID: machineRoomID,
		ServiceRoomID: serviceRoomID,
	}, nil
}

// InviteToFleetRooms invites a user to all three fleet rooms. Silently
// skips rooms the user has already joined.
func InviteToFleetRooms(ctx context.Context, session messaging.Session, rooms FleetRooms, userID ref.UserID, logger *slog.Logger) error {
	for _, roomID := range []ref.RoomID{rooms.ConfigRoomID, rooms.MachineRoomID, rooms.ServiceRoomID} {
		if err := session.InviteUser(ctx, roomID, userID); err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				continue
			}
			return cli.Internal("inviting %s to room %s: %w", userID, roomID, err)
		}
	}
	logger.Info("invited user to fleet rooms", "user_id", userID.String())
	return nil
}

// idempotentCreateRoom resolves a room alias; if the room exists, returns
// its ID. Otherwise creates the room and adds it as a child of the given
// space. The alias parameter is the full "#localpart:server" form used for
// the resolve check; the CreateRoomRequest.Alias field is the localpart
// used by the Matrix create-room API.
func idempotentCreateRoom(ctx context.Context, session messaging.Session, alias ref.RoomAlias, spaceRoomID ref.RoomID, server ref.ServerName, request messaging.CreateRoomRequest, logger *slog.Logger) (ref.RoomID, error) {
	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("room already exists", "alias", alias.String(), "room_id", roomID.String())
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return ref.RoomID{}, cli.Internal("resolving %s: %w", alias, err)
	}

	response, err := session.CreateRoom(ctx, request)
	if err != nil {
		return ref.RoomID{}, cli.Internal("creating room %s: %w", alias, err)
	}

	// State key for m.space.child is the room ID string per Matrix spec.
	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID.String(),
		map[string]any{"via": []string{server.String()}})
	if err != nil {
		return ref.RoomID{}, cli.Internal("adding %s as child of namespace space: %w", alias, err)
	}

	logger.Info("created room", "alias", alias.String(), "room_id", response.RoomID.String())
	return response.RoomID, nil
}
