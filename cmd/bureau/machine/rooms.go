// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// resolveFleetRooms resolves a fleet's three scoped rooms (machine, service,
// fleet config) from the fleet ref. Returns the resolved room IDs.
func resolveFleetRooms(ctx context.Context, session messaging.Session, fleet ref.Fleet) (machineRoomID, serviceRoomID, fleetRoomID string, err error) {
	aliases := []struct {
		alias  string
		target *string
		name   string
	}{
		{fleet.MachineRoomAlias(), &machineRoomID, "fleet machine room"},
		{fleet.ServiceRoomAlias(), &serviceRoomID, "fleet service room"},
		{fleet.RoomAlias(), &fleetRoomID, "fleet config room"},
	}

	for _, alias := range aliases {
		roomID, resolveErr := session.ResolveAlias(ctx, alias.alias)
		if resolveErr != nil {
			return "", "", "", fmt.Errorf("resolve %s (%s): %w", alias.name, alias.alias, resolveErr)
		}
		*alias.target = roomID
	}

	return machineRoomID, serviceRoomID, fleetRoomID, nil
}

// machineRoom describes a Bureau room that machines are invited to during
// provisioning and kicked from during decommissioning.
type machineRoom struct {
	displayName string // human-readable name for log messages
}

// resolvedRoom holds a resolved room (alias → room ID).
type resolvedRoom struct {
	machineRoom
	alias  string // full alias for log messages (e.g., "#bureau/template:bureau.local")
	roomID string
}

// resolveGlobalRooms resolves all namespace-scoped global rooms (template,
// pipeline, system) to room IDs. The namespace provides the room alias
// methods. Returns the resolved rooms and any that could not be resolved.
// Resolution failures for individual rooms are not fatal — the caller
// decides whether to proceed based on the specific context (provisioning
// vs decommissioning).
func resolveGlobalRooms(ctx context.Context, session messaging.Session, namespace ref.Namespace) (resolved []resolvedRoom, failed []resolvedRoom, errors []error) {
	rooms := []struct {
		alias       string
		displayName string
	}{
		{namespace.TemplateRoomAlias(), "template room"},
		{namespace.PipelineRoomAlias(), "pipeline room"},
		{namespace.SystemRoomAlias(), "system room"},
	}

	for _, room := range rooms {
		roomID, err := session.ResolveAlias(ctx, room.alias)
		if err != nil {
			entry := resolvedRoom{
				machineRoom: machineRoom{displayName: room.displayName},
				alias:       room.alias,
			}
			failed = append(failed, entry)
			errors = append(errors, fmt.Errorf("resolve %s (%s): %w", room.displayName, room.alias, err))
			continue
		}
		resolved = append(resolved, resolvedRoom{
			machineRoom: machineRoom{displayName: room.displayName},
			alias:       room.alias,
			roomID:      roomID,
		})
	}
	return resolved, failed, errors
}

// checkMachineMembership checks whether a machine user has any active
// memberships (join or invite) in the given rooms. Returns the list of
// rooms where the machine still has an active membership.
func checkMachineMembership(ctx context.Context, session messaging.Session, machineUserID string, rooms []resolvedRoom) []resolvedRoom {
	var activeRooms []resolvedRoom
	for _, room := range rooms {
		members, err := session.GetRoomMembers(ctx, room.roomID)
		if err != nil {
			// If we can't read membership, treat it conservatively as
			// potentially active. The caller should fail-safe.
			activeRooms = append(activeRooms, room)
			continue
		}
		for _, member := range members {
			if member.UserID == machineUserID && (member.Membership == "join" || member.Membership == "invite") {
				activeRooms = append(activeRooms, room)
				break
			}
		}
	}
	return activeRooms
}
