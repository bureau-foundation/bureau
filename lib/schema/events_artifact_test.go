// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestArtifactRoomPowerLevels(t *testing.T) {
	adminUserID := ref.MustParseUserID("@bureau/admin:bureau.local")
	powerLevels := ArtifactRoomPowerLevels(adminUserID)

	// Admin has PL 100.
	users := powerLevels["users"].(map[string]any)
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin PL = %v, want 100", users[adminUserID.String()])
	}

	// Default user PL is 0.
	if powerLevels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", powerLevels["users_default"])
	}

	// No member-writable event types: all artifact metadata lives in
	// the artifact service, not Matrix state events.
	events := powerLevels["events"].(map[ref.EventType]any)

	// Default event PL is 100 (unknown event types require admin).
	if powerLevels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", powerLevels["events_default"])
	}

	// Default state PL is 100.
	if powerLevels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", powerLevels["state_default"])
	}

	// Admin-protected Matrix events are all PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.power_levels"), ref.EventType("m.room.name"), ref.EventType("m.room.topic"),
		ref.EventType("m.room.canonical_alias"), ref.EventType("m.room.join_rules"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s PL = %v, want 100", eventType, events[eventType])
		}
	}

	// Administrative actions require PL 100.
	for _, action := range []string{"ban", "kick", "invite", "redact"} {
		if powerLevels[action] != 100 {
			t.Errorf("%s = %v, want 100", action, powerLevels[action])
		}
	}
}
