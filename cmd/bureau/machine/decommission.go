// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// decommissionParams holds the parameters for the machine decommission command.
// Credential file paths are excluded from MCP schema since they involve reading
// secrets from files.
type decommissionParams struct {
	cli.SessionConfig
}

func decommissionCommand() *cli.Command {
	var params decommissionParams

	return &cli.Command{
		Name:    "decommission",
		Summary: "Remove a machine from the fleet",
		Description: `Decommission a machine by cleaning up its state from the Bureau fleet.

The first argument is a fleet localpart (e.g., "bureau/fleet/prod").
The second argument is the bare machine name within the fleet (e.g.,
"worker-01"). The server name is derived from the connected session's
identity.

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
		Usage: "bureau machine decommission <fleet-localpart> <machine-name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Remove a worker machine",
				Command:     "bureau machine decommission bureau/fleet/prod worker-01 --credential-file ./bureau-creds",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/decommission"},
		Annotations:    cli.Destructive(),
		Run: func(args []string) error {
			if len(args) < 2 {
				return cli.Validation("fleet localpart and machine name are required\n\nUsage: bureau machine decommission <fleet-localpart> <machine-name> [flags]")
			}
			if len(args) > 2 {
				return cli.Validation("unexpected argument: %s", args[2])
			}
			if params.SessionConfig.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}

			return runDecommission(args[0], args[1], &params)
		},
	}
}

func runDecommission(fleetLocalpart, machineName string, params *decommissionParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("read credential file: %w", err)
	}

	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		return cli.Validation("credential file missing MATRIX_HOMESERVER_URL")
	}
	adminUserID := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminUserID == "" || adminToken == "" {
		return cli.Validation("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}

	// Derive server name from admin user ID.
	server, err := ref.ServerFromUserID(adminUserID)
	if err != nil {
		return cli.Internal("cannot determine server name from admin user ID: %w", err)
	}

	// Parse and validate the fleet localpart.
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// Construct the machine ref within the fleet.
	machine, err := ref.NewMachine(fleet, machineName)
	if err != nil {
		return cli.Validation("invalid machine name: %v", err)
	}

	machineUsername := machine.Localpart()
	machineUserID := machine.UserID()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("create matrix client: %w", err)
	}

	adminSession, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return cli.Internal("create admin session: %w", err)
	}
	defer adminSession.Close()

	fmt.Fprintf(os.Stderr, "Decommissioning %s (%s)...\n", machineUsername, machineUserID)

	// Resolve global Bureau rooms (template, pipeline, system) for kick.
	namespace := fleet.Namespace()
	globalRooms, failedRooms, resolveErrors := resolveGlobalRooms(ctx, adminSession, namespace)
	for index, room := range failedRooms {
		fmt.Fprintf(os.Stderr, "  Warning: could not resolve %s: %v\n", room.displayName, resolveErrors[index])
	}

	// Resolve the fleet-scoped rooms for state event cleanup and kicking.
	machineRoomID, serviceRoomID, fleetRoomID, err := resolveFleetRooms(ctx, adminSession, fleet)
	if err != nil {
		return cli.NotFound("fleet rooms could not be resolved — cannot clear machine state: %w", err)
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineUsername, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_key: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_key\n")
	}

	_, err = adminSession.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus, machineUsername, map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_status: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "  Cleared machine_status\n")
	}

	// Clean up the per-machine config room: clear state events and kick.
	configAlias := machine.RoomAlias()
	configRoomID, err := adminSession.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			fmt.Fprintf(os.Stderr, "  Config room %s does not exist (skipping)\n", configAlias)
		} else {
			return cli.NotFound("resolve config room %q: %w", configAlias, err)
		}
	} else {
		// Clear machine_config.
		_, err = adminSession.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUsername, map[string]any{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not clear machine_config: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Cleared machine_config from config room\n")
		}

		// Find and clear all credentials state events in the config room.
		if _, err := clearConfigRoomCredentials(ctx, adminSession, configRoomID); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}

		// Kick the machine from the config room.
		err = adminSession.KickUser(ctx, configRoomID, machineUserID, "machine decommissioned")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from config room: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from config room\n")
		}
	}

	// Kick from all global Bureau rooms.
	for _, room := range globalRooms {
		err = adminSession.KickUser(ctx, room.roomID, machineUserID, "machine decommissioned")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from %s: %v\n", room.alias, err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from %s\n", room.alias)
		}
	}

	// Kick from all fleet-scoped rooms.
	fleetRooms := []resolvedRoom{
		{machineRoom: machineRoom{displayName: "fleet machine room"}, roomID: machineRoomID},
		{machineRoom: machineRoom{displayName: "fleet service room"}, roomID: serviceRoomID},
		{machineRoom: machineRoom{displayName: "fleet config room"}, roomID: fleetRoomID},
	}
	for _, room := range fleetRooms {
		err = adminSession.KickUser(ctx, room.roomID, machineUserID, "machine decommissioned")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not kick from %s: %v\n", room.displayName, err)
		} else {
			fmt.Fprintf(os.Stderr, "  Kicked from %s\n", room.displayName)
		}
	}

	// Verify: check that the machine has zero active memberships in Bureau
	// rooms. This catches cases where a kick silently failed or a race
	// condition left stale membership.
	fmt.Fprintf(os.Stderr, "\nVerifying cleanup...\n")
	allRooms := make([]resolvedRoom, 0, len(globalRooms)+len(fleetRooms))
	allRooms = append(allRooms, globalRooms...)
	allRooms = append(allRooms, fleetRooms...)
	activeRooms := checkMachineMembership(ctx, adminSession, machineUserID, allRooms)

	// Also check the config room if it exists.
	if configRoomID != "" {
		configResolved := resolvedRoom{
			machineRoom: machineRoom{displayName: "config room"},
			alias:       configAlias,
			roomID:      configRoomID,
		}
		configActive := checkMachineMembership(ctx, adminSession, machineUserID, []resolvedRoom{configResolved})
		activeRooms = append(activeRooms, configActive...)
	}

	if len(activeRooms) > 0 {
		fmt.Fprintf(os.Stderr, "  FAILED: machine still has active membership in %d room(s):\n", len(activeRooms))
		for _, room := range activeRooms {
			fmt.Fprintf(os.Stderr, "    - %s (%s)\n", room.displayName, room.roomID)
		}
		return cli.Internal("decommission incomplete: machine still has %d active room membership(s) — re-provisioning will not be possible until these are cleared", len(activeRooms))
	}

	fmt.Fprintf(os.Stderr, "  All Bureau room memberships cleared\n")
	fmt.Fprintf(os.Stderr, "\nMachine %s decommissioned.\n", machineUsername)
	fmt.Fprintf(os.Stderr, "To re-provision, run: bureau machine provision %s %s --credential-file <creds>\n", fleet.Localpart(), machine.Name())

	return nil
}

// clearConfigRoomCredentials finds all m.bureau.credentials state events
// in the config room and clears them by sending empty content. Returns the
// state keys (principal localparts) of credentials that were successfully
// cleared, plus any errors encountered reading room state.
func clearConfigRoomCredentials(ctx context.Context, session messaging.Session, roomID string) ([]string, error) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, cli.Internal("read config room state: %w", err)
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
			fmt.Fprintf(os.Stderr, "  Warning: could not clear credentials for %s: %v\n", *event.StateKey, err)
		} else {
			cleared = append(cleared, *event.StateKey)
		}
	}

	if len(cleared) > 0 {
		fmt.Fprintf(os.Stderr, "  Cleared %d credential(s) from config room\n", len(cleared))
	}
	return cleared, nil
}
