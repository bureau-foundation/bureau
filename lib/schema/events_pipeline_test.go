// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestPipelineRoomPowerLevels(t *testing.T) {
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	levels := PipelineRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if len(users) != 1 {
		t.Errorf("users map should have exactly 1 entry, got %d", len(users))
	}

	// Default user power level should be 0 (other members can read).
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Pipeline events require power level 100 (admin-only writes).
	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypePipeline] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypePipeline, events[EventTypePipeline])
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

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Administrative actions all require power level 100.
	for _, field := range []string{"state_default", "ban", "kick", "invite", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite is 100 (unlike config/workspace rooms where machine can
	// invite at PL 50 — pipeline rooms have no machine tier).
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}
