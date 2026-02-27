// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// ArtifactRoomPowerLevels returns the power level structure for the
// artifact coordination room (#bureau/artifact). Per-artifact metadata
// and tag mappings live in the artifact service, not as Matrix state
// events. The room carries only admin-level configuration
// (artifact.EventTypeArtifactScope) and coordination messages.
//
// Room membership is invite-only — the admin invites artifact service
// principals and machine daemons during setup.
func ArtifactRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	return roomPowerLevels(powerLevelSpec{
		users: map[string]any{
			adminUserID.String(): 100,
		},
		events:        AdminProtectedEvents(),
		eventsDefault: 100,
		stateDefault:  100,
		invite:        100,
	})
}
