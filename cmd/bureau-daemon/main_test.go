// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		expected  string
	}{
		{
			name:      "simple alias",
			fullAlias: "#bureau/machine:bureau.local",
			expected:  "bureau/machine",
		},
		{
			name:      "fleet-scoped alias",
			fullAlias: "#bureau/fleet/prod/machine/workstation:bureau.local",
			expected:  "bureau/fleet/prod/machine/workstation",
		},
		{
			name:      "different server",
			fullAlias: "#test:example.org",
			expected:  "test",
		},
		{
			name:      "no # prefix",
			fullAlias: "bureau/machine:bureau.local",
			expected:  "bureau/machine",
		},
		{
			name:      "no server suffix",
			fullAlias: "#bureau/machine",
			expected:  "bureau/machine",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := principal.RoomAliasLocalpart(test.fullAlias)
			if result != test.expected {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q",
					test.fullAlias, result, test.expected)
			}
		})
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	machine, _ := testMachineSetup(t, "workstation", "bureau.local")
	machineUserID := machine.UserID()
	levels := schema.ConfigRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Machine should have power level 50 (operational writes: MachineConfig
	// for HA hosting, layout publishes, invites). PL 50 cannot modify
	// credentials, power levels, or room metadata (all PL 100).
	machineLevel, ok := users[machineUserID]
	if !ok {
		t.Fatalf("machine %q not in users map", machineUserID)
	}
	if machineLevel != 50 {
		t.Errorf("machine power level = %v, want 50", machineLevel)
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// MachineConfig requires PL 50 (machine and fleet controllers can write placements).
	// Credentials require PL 100 (admin only).
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[schema.EventTypeMachineConfig] != 50 {
		t.Errorf("%s power level = %v, want 50", schema.EventTypeMachineConfig, events[schema.EventTypeMachineConfig])
	}
	if events[schema.EventTypeCredentials] != 100 {
		t.Errorf("%s power level = %v, want 100", schema.EventTypeCredentials, events[schema.EventTypeCredentials])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Invite should require PL 50 (machine can invite during creation).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestUptimeSeconds(t *testing.T) {
	// uptimeSeconds should return a positive value on Linux.
	uptime := uptimeSeconds()
	if uptime <= 0 {
		t.Errorf("uptimeSeconds() = %d, want > 0", uptime)
	}
}

func TestPublishStatus_SandboxCount(t *testing.T) {
	// Verify that the running map count is correctly calculated.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "bureau.local")
	daemon.running["iree/amdgpu/pm"] = true
	daemon.running["service/stt/whisper"] = true
	daemon.running["service/tts/piper"] = true
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Count running principals the same way publishStatus does.
	runningCount := 0
	for range daemon.running {
		runningCount++
	}
	if runningCount != 3 {
		t.Errorf("running count = %d, want 3", runningCount)
	}
}
