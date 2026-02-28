// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func mustUserID(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("parse user ID %q: %v", raw, err)
	}
	return userID
}

func TestMachineRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := MachineRoomPowerLevels(adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Members must be able to write machine presence events.
	for _, eventType := range []ref.EventType{
		EventTypeMachineKey,
		EventTypeMachineInfo,
		EventTypeMachineStatus,
		EventTypeWebRTCOffer,
		EventTypeWebRTCAnswer,
	} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Admin-protected events must require PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.encryption"), ref.EventType("m.room.server_acl"),
		ref.EventType("m.room.tombstone"), ref.EventType("m.space.child"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}
}

func TestServiceRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := ServiceRoomPowerLevels(adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Members must be able to register services.
	if events[EventTypeService] != 0 {
		t.Errorf("%s power level = %v, want 0", EventTypeService, events[EventTypeService])
	}

	// Admin-protected events must require PL 100.
	for _, eventType := range []ref.EventType{
		ref.EventType("m.room.encryption"), ref.EventType("m.room.server_acl"),
		ref.EventType("m.room.tombstone"), ref.EventType("m.space.child"),
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Invite threshold is 50 so machine daemons (PL 50) can invite
	// service principals for HA failover and fleet placement.
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}
