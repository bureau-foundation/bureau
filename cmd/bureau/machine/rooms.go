// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// machineRoom describes a Bureau room that machines are invited to during
// provisioning and kicked from during decommissioning.
type machineRoom struct {
	alias       string // localpart, e.g. "bureau/machine"
	displayName string // human-readable name for log messages
}

// machineGlobalRooms lists every global Bureau room a machine should be
// a member of. This is the single source of truth for the provision and
// decommission commands. The per-machine config room is handled separately
// since its alias depends on the machine name.
//
// Adding a new global room here automatically includes it in provisioning
// (invite), decommission (kick), and re-provision verification (membership
// check).
var machineGlobalRooms = []machineRoom{
	{schema.RoomAliasMachine, "machine room"},
	{schema.RoomAliasService, "service room"},
	{schema.RoomAliasTemplate, "template room"},
	{schema.RoomAliasPipeline, "pipeline room"},
	{schema.RoomAliasSystem, "system room"},
	{schema.RoomAliasFleet, "fleet room"},
}

// resolvedRoom holds a resolved global room (alias → room ID).
type resolvedRoom struct {
	machineRoom
	roomID string
}

// resolveGlobalRooms resolves all machineGlobalRooms aliases to room IDs.
// Returns the resolved rooms and any that could not be resolved. Resolution
// failures for individual rooms are not fatal — the caller decides whether
// to proceed based on the specific context (provisioning vs decommissioning).
func resolveGlobalRooms(ctx context.Context, session messaging.Session, serverName string) (resolved []resolvedRoom, failed []machineRoom, errors []error) {
	for _, room := range machineGlobalRooms {
		fullAlias := principal.RoomAlias(room.alias, serverName)
		roomID, err := session.ResolveAlias(ctx, fullAlias)
		if err != nil {
			failed = append(failed, room)
			errors = append(errors, fmt.Errorf("resolve %s (%s): %w", room.displayName, fullAlias, err))
			continue
		}
		resolved = append(resolved, resolvedRoom{
			machineRoom: room,
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
