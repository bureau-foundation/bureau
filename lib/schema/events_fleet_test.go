// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
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

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
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

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
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

func TestFleetRoomPowerLevels(t *testing.T) {
	adminUserID := mustUserID(t, "@bureau-admin:bureau.local")
	levels := FleetRoomPowerLevels(adminUserID)

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

	// Members must be able to write runtime operational events.
	for _, eventType := range []ref.EventType{
		EventTypeHALease,
		EventTypeServiceStatus,
		EventTypeFleetAlert,
	} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Administrative configuration events are admin-only (PL 100).
	// These are written by operators through the CLI, not by runtime
	// components. A compromised member could inject phantom services
	// (FleetService), manipulate controller behavior (FleetConfig),
	// or compromise binary provenance (FleetCache).
	for _, eventType := range []ref.EventType{
		EventTypeFleetService,
		EventTypeMachineDefinition,
		EventTypeFleetConfig,
		EventTypeFleetCache,
		EventTypeEnvironmentBuild,
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100 (admin-only)", eventType, events[eventType])
		}
	}

	// Dev team metadata is admin-only — explicit regardless of state_default.
	if events[EventTypeDevTeam] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeDevTeam, events[EventTypeDevTeam])
	}

	if levels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", levels["state_default"])
	}
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Fleet rooms are admin-invite-only.
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}

func TestFleetCacheContentComposeFields(t *testing.T) {
	original := FleetCacheContent{
		URL:             "https://cache.infra.bureau.foundation",
		Name:            "main",
		PublicKeys:      []string{"cache-1:key1"},
		ComposeTemplate: "bureau/template:nix-builder",
		DefaultSystem:   "x86_64-linux",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded FleetCacheContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ComposeTemplate != original.ComposeTemplate {
		t.Errorf("ComposeTemplate = %q, want %q", decoded.ComposeTemplate, original.ComposeTemplate)
	}
	if decoded.DefaultSystem != original.DefaultSystem {
		t.Errorf("DefaultSystem = %q, want %q", decoded.DefaultSystem, original.DefaultSystem)
	}

	// Omitempty: absent compose fields should not appear in JSON.
	minimal := FleetCacheContent{
		URL:        "https://example.com",
		PublicKeys: []string{"key"},
	}
	minData, _ := json.Marshal(minimal)
	var raw map[string]any
	json.Unmarshal(minData, &raw)
	if _, exists := raw["compose_template"]; exists {
		t.Error("compose_template should be omitted when empty")
	}
	if _, exists := raw["default_system"]; exists {
		t.Error("default_system should be omitted when empty")
	}
}

func TestEnvironmentBuildContentRoundTrip(t *testing.T) {
	original := EnvironmentBuildContent{
		Profile:   "sysadmin-runner-env",
		FlakeRef:  "github:bureau-foundation/bureau/abc123",
		System:    "x86_64-linux",
		StorePath: "/nix/store/xyz-bureau-sysadmin-runner-env",
		Machine:   "bureau/fleet/prod/machine/workstation",
		Timestamp: "2026-03-04T10:30:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded EnvironmentBuildContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Profile != original.Profile {
		t.Errorf("Profile = %q, want %q", decoded.Profile, original.Profile)
	}
	if decoded.FlakeRef != original.FlakeRef {
		t.Errorf("FlakeRef = %q, want %q", decoded.FlakeRef, original.FlakeRef)
	}
	if decoded.System != original.System {
		t.Errorf("System = %q, want %q", decoded.System, original.System)
	}
	if decoded.StorePath != original.StorePath {
		t.Errorf("StorePath = %q, want %q", decoded.StorePath, original.StorePath)
	}
	if decoded.Machine != original.Machine {
		t.Errorf("Machine = %q, want %q", decoded.Machine, original.Machine)
	}
	if decoded.Timestamp != original.Timestamp {
		t.Errorf("Timestamp = %q, want %q", decoded.Timestamp, original.Timestamp)
	}
}
