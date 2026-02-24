// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestWorkspaceRoomPowerLevels(t *testing.T) {
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	machineUserID := ref.MustParseUserID("@machine/workstation:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}

	// Machine should have power level 50 (can invite principals into
	// workspace rooms, but cannot modify power levels or project config).
	if users[machineUserID.String()] != 50 {
		t.Errorf("machine power level = %v, want 50", users[machineUserID.String()])
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Workspace rooms are collaboration spaces: events_default should be 0
	// (agents can send messages freely), unlike config rooms where
	// events_default is 100 (admin-only).
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Event-specific power levels.
	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Admin-only events (PL 100).
	for _, eventType := range []ref.EventType{EventTypeProject, EventTypeStewardship} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Admin-only power level management (PL 100). Continuwuity validates
	// all fields in m.room.power_levels against sender PL (not just changed
	// ones), so only admin can modify power levels.
	if events[ref.EventType("m.room.power_levels")] != 100 {
		t.Errorf("m.room.power_levels power level = %v, want 100", events[ref.EventType("m.room.power_levels")])
	}

	// Default-level events (PL 0): workspace state, worktree lifecycle, and
	// layout. Room membership is the authorization boundary — the room is
	// invite-only.
	for _, eventType := range []ref.EventType{EventTypeWorkspace, EventTypeWorktree, EventTypeLayout} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.encryption"), ref.EventType("m.room.server_acl"),
		ref.EventType("m.room.tombstone"), ref.EventType("m.space.child"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// state_default is 0: room membership is the authorization boundary.
	// New Bureau state event types work without updating power levels.
	if levels["state_default"] != 0 {
		t.Errorf("state_default = %v, want 0", levels["state_default"])
	}

	// Moderation actions require power level 100.
	for _, field := range []string{"ban", "kick", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite should require PL 50 (machine can invite principals).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestWorkspaceRoomPowerLevelsSameUser(t *testing.T) {
	// When admin and machine are the same user, the users map should
	// have exactly one entry (no duplicate).
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for same user, got %d", len(users))
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
}

func TestWorkspaceRoomPowerLevelsEmptyMachine(t *testing.T) {
	// When machine user ID is zero, the users map should have only the admin.
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, ref.UserID{})

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for empty machine, got %d", len(users))
	}
}
