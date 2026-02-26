// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// DecommissionParams holds the parameters for machine decommission.
type DecommissionParams struct {
	// Machine is the typed machine reference within the fleet.
	Machine ref.Machine
}

// decommissionParams holds the CLI-specific parameters for the machine decommission command.
type decommissionParams struct {
	cli.SessionConfig
}

func decommissionCommand() *cli.Command {
	var params decommissionParams

	return &cli.Command{
		Name:    "decommission",
		Summary: "Remove a machine from the fleet",
		Description: `Decommission a machine by cleaning up its state from the Bureau fleet.

The argument is a machine reference — either a bare localpart (e.g.,
"bureau/fleet/prod/machine/worker-01") or a full Matrix user ID (e.g.,
"@bureau/fleet/prod/machine/worker-01:remote.server"). The @ sigil
distinguishes the two forms. When a bare localpart is given, the server
name is derived from the connected admin session.

This removes the machine's key and status from the machine room, clears
its config room state events (machine_config, credentials), and kicks
the machine account from all Bureau rooms (system, machine, service,
template, pipeline, fleet, and the per-machine config room).

After decommission, the machine name can be re-provisioned with
"bureau machine provision". The machine's Matrix account remains on the
homeserver but has zero Bureau room memberships and no active state.

The command verifies cleanup at the end. If any Bureau room membership
remains (due to a homeserver issue or race condition), it reports the
failure explicitly.`,
		Usage: "bureau machine decommission <machine-ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Remove a worker machine",
				Command:     "bureau machine decommission bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
			{
				Description: "Decommission on a remote homeserver",
				Command:     "bureau machine decommission @bureau/fleet/prod/machine/worker-01:remote.server --credential-file ./bureau-creds",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/decommission"},
		Annotations:    cli.Destructive(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine reference is required").
					WithHint("Usage: bureau machine decommission <machine-ref> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (machine reference), got %d", len(args))
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required").
					WithHint("Pass --credential-file with the file from 'bureau matrix setup'.")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			genericSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer genericSession.Close()

			session, ok := genericSession.(*messaging.DirectSession)
			if !ok {
				return cli.Validation("decommission requires a direct session (credential file), not a proxy session").
					WithHint("This command must be run by an operator with --credential-file, not from inside a sandbox.")
			}

			defaultServer, err := ref.ServerFromUserID(session.UserID().String())
			if err != nil {
				return cli.Internal("cannot determine server name from session: %w", err)
			}
			machine, err := resolveMachineArg(args[0], defaultServer)
			if err != nil {
				return cli.Validation("invalid machine reference: %v", err)
			}

			logger = logger.With(
				"machine", machine.Localpart(),
			)

			if err := Decommission(ctx, session, DecommissionParams{
				Machine: machine,
			}, logger); err != nil {
				return err
			}

			logger.Info("to re-provision, run: bureau machine provision <machine-ref> --credential-file <creds>",
				"machine", machine.Localpart(),
			)

			return nil
		},
	}
}

// Decommission removes a machine from the Bureau fleet by clearing its
// state events, kicking it from all Bureau rooms, and verifying zero
// remaining memberships. The caller provides a DirectSession (required
// for KickUser) and a context with an appropriate deadline.
//
// After decommission, the machine name can be re-provisioned with Provision.
func Decommission(ctx context.Context, session *messaging.DirectSession, params DecommissionParams, logger *slog.Logger) error {
	machine := params.Machine
	fleet := machine.Fleet()
	machineUsername := machine.Localpart()
	machineUserID := machine.UserID()

	logger.Info("decommissioning machine",
		"machine_user_id", machineUserID.String(),
	)

	// Resolve global Bureau rooms (template, pipeline, system) for kick.
	namespace := fleet.Namespace()
	globalRooms, failedRooms, resolveErrors := resolveGlobalRooms(ctx, session, namespace)
	for index, room := range failedRooms {
		logger.Warn("could not resolve global room",
			"room", room.displayName,
			"error", resolveErrors[index],
		)
	}

	// Resolve the fleet-scoped rooms for state event cleanup and kicking.
	machineRoomID, serviceRoomID, fleetRoomID, err := resolveFleetRooms(ctx, session, fleet)
	if err != nil {
		return cli.NotFound("fleet rooms could not be resolved — cannot clear machine state: %w", err).
			WithHint("Has 'bureau matrix setup' been run for this fleet? Check with 'bureau matrix doctor'.")
	}

	_, err = session.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineUsername, map[string]any{})
	if err != nil {
		logger.Warn("could not clear machine_key", "error", err)
	} else {
		logger.Info("cleared machine_key")
	}

	_, err = session.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus, machineUsername, map[string]any{})
	if err != nil {
		logger.Warn("could not clear machine_status", "error", err)
	} else {
		logger.Info("cleared machine_status")
	}

	// Clean up the per-machine config room: clear state events and kick.
	configAlias := machine.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			logger.Info("config room does not exist, skipping", "alias", configAlias)
		} else {
			return cli.Transient("resolve config room %q: %w", configAlias, err).
				WithHint("Check that the homeserver is running. Run 'bureau matrix doctor' to diagnose.")
		}
	} else {
		// Clear machine_config.
		_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUsername, map[string]any{})
		if err != nil {
			logger.Warn("could not clear machine_config", "error", err)
		} else {
			logger.Info("cleared machine_config from config room")
		}

		// Find and clear all credentials state events in the config room.
		if _, err := clearConfigRoomCredentials(ctx, session, configRoomID, logger); err != nil {
			logger.Warn("could not clear config room credentials", "error", err)
		}

		err = session.KickUser(ctx, configRoomID, machineUserID, "machine decommissioned")
		if err != nil {
			logger.Warn("could not kick from config room", "error", err)
		} else {
			logger.Info("kicked from config room")
		}
	}

	for _, room := range globalRooms {
		err = session.KickUser(ctx, room.roomID, machineUserID, "machine decommissioned")
		if err != nil {
			logger.Warn("could not kick from room", "room", room.alias, "error", err)
		} else {
			logger.Info("kicked from room", "room", room.alias)
		}
	}

	// Kick from all fleet-scoped rooms.
	fleetRooms := []resolvedRoom{
		{machineRoom: machineRoom{displayName: "fleet machine room"}, roomID: machineRoomID},
		{machineRoom: machineRoom{displayName: "fleet service room"}, roomID: serviceRoomID},
		{machineRoom: machineRoom{displayName: "fleet config room"}, roomID: fleetRoomID},
	}
	for _, room := range fleetRooms {
		err = session.KickUser(ctx, room.roomID, machineUserID, "machine decommissioned")
		if err != nil {
			logger.Warn("could not kick from room", "room", room.displayName, "error", err)
		} else {
			logger.Info("kicked from room", "room", room.displayName)
		}
	}

	// Verify: check that the machine has zero active memberships in Bureau
	// rooms. This catches cases where a kick silently failed or a race
	// condition left stale membership.
	logger.Info("verifying cleanup")
	allRooms := make([]resolvedRoom, 0, len(globalRooms)+len(fleetRooms))
	allRooms = append(allRooms, globalRooms...)
	allRooms = append(allRooms, fleetRooms...)
	activeRooms := checkMachineMembership(ctx, session, machineUserID, allRooms)

	// Also check the config room if it exists.
	if !configRoomID.IsZero() {
		configResolved := resolvedRoom{
			machineRoom: machineRoom{displayName: "config room"},
			alias:       configAlias,
			roomID:      configRoomID,
		}
		configActive := checkMachineMembership(ctx, session, machineUserID, []resolvedRoom{configResolved})
		activeRooms = append(activeRooms, configActive...)
	}

	if len(activeRooms) > 0 {
		roomDescriptions := make([]string, len(activeRooms))
		for index, room := range activeRooms {
			roomDescriptions[index] = fmt.Sprintf("%s (%s)", room.displayName, room.roomID)
		}
		logger.Error("machine still has active room memberships after decommission",
			"count", len(activeRooms),
			"rooms", roomDescriptions,
		)
		return cli.Conflict("decommission incomplete: machine still has %d active room membership(s)", len(activeRooms)).
			WithHint("The homeserver may not have processed all kick operations yet. Retry the decommission,\n" +
				"or manually remove the machine from the listed rooms via 'bureau matrix room kick'.")
	}

	logger.Info("machine decommissioned",
		"machine_name", machine.Name(),
	)

	return nil
}

// clearConfigRoomCredentials finds all m.bureau.credentials state events
// in the config room and clears them by sending empty content. Returns the
// state keys (principal localparts) of credentials that were successfully
// cleared, plus any errors encountered reading room state.
func clearConfigRoomCredentials(ctx context.Context, session messaging.Session, roomID ref.RoomID, logger *slog.Logger) ([]string, error) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, cli.Transient("read config room state: %w", err)
	}

	var cleared []string
	for _, event := range events {
		if event.Type != schema.EventTypeCredentials {
			continue
		}
		if event.StateKey == nil {
			continue
		}

		// Check if the credentials have already been cleared (empty content).
		contentBytes, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}
		if string(contentBytes) == "{}" || string(contentBytes) == "null" {
			continue
		}

		_, err = session.SendStateEvent(ctx, roomID, schema.EventTypeCredentials, *event.StateKey, map[string]any{})
		if err != nil {
			logger.Warn("could not clear credentials", "principal", *event.StateKey, "error", err)
		} else {
			cleared = append(cleared, *event.StateKey)
		}
	}

	if len(cleared) > 0 {
		logger.Info("cleared credentials from config room", "count", len(cleared))
	}
	return cleared, nil
}
