// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestGetNumericField(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		key  string
		want float64
	}{
		{
			name: "present integer-valued float",
			data: map[string]any{"state_default": float64(100)},
			key:  "state_default",
			want: 100,
		},
		{
			name: "present zero",
			data: map[string]any{"events_default": float64(0)},
			key:  "events_default",
			want: 0,
		},
		{
			name: "missing key",
			data: map[string]any{},
			key:  "state_default",
			want: 0,
		},
		{
			name: "wrong type (string)",
			data: map[string]any{"state_default": "100"},
			key:  "state_default",
			want: 0,
		},
		{
			name: "wrong type (int)",
			data: map[string]any{"state_default": 100},
			key:  "state_default",
			want: 0,
		},
		{
			name: "nil map value",
			data: map[string]any{"state_default": nil},
			key:  "state_default",
			want: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getNumericField(test.data, test.key)
			if got != test.want {
				t.Errorf("getNumericField(%v, %q) = %v, want %v", test.data, test.key, got, test.want)
			}
		})
	}
}

func TestGetUserPowerLevel(t *testing.T) {
	tests := []struct {
		name        string
		powerLevels map[string]any
		userID      string
		want        float64
	}{
		{
			name: "admin user at 100",
			powerLevels: map[string]any{
				"users_default": float64(0),
				"users": map[string]any{
					"@admin:local": float64(100),
				},
			},
			userID: "@admin:local",
			want:   100,
		},
		{
			name: "user not in map gets users_default",
			powerLevels: map[string]any{
				"users_default": float64(0),
				"users": map[string]any{
					"@admin:local": float64(100),
				},
			},
			userID: "@somebody:local",
			want:   0,
		},
		{
			name: "no users map gets users_default",
			powerLevels: map[string]any{
				"users_default": float64(50),
			},
			userID: "@admin:local",
			want:   50,
		},
		{
			name:        "empty map returns 0",
			powerLevels: map[string]any{},
			userID:      "@admin:local",
			want:        0,
		},
		{
			name: "users_default missing returns 0",
			powerLevels: map[string]any{
				"users": map[string]any{},
			},
			userID: "@somebody:local",
			want:   0,
		},
		{
			name: "user with non-numeric value gets users_default",
			powerLevels: map[string]any{
				"users_default": float64(10),
				"users": map[string]any{
					"@admin:local": "not-a-number",
				},
			},
			userID: "@admin:local",
			want:   10,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getUserPowerLevel(test.powerLevels, test.userID)
			if got != test.want {
				t.Errorf("getUserPowerLevel(%q) = %v, want %v", test.userID, got, test.want)
			}
		})
	}
}

func TestGetStateEventPowerLevel(t *testing.T) {
	tests := []struct {
		name        string
		powerLevels map[string]any
		eventType   string
		want        float64
	}{
		{
			name: "explicitly set event type",
			powerLevels: map[string]any{
				"state_default": float64(100),
				"events": map[string]any{
					"m.bureau.ha_lease": float64(0),
				},
			},
			eventType: "m.bureau.ha_lease",
			want:      0,
		},
		{
			name: "unlisted event falls back to state_default",
			powerLevels: map[string]any{
				"state_default": float64(100),
				"events": map[string]any{
					"m.bureau.ha_lease": float64(0),
				},
			},
			eventType: "m.bureau.fleet_config",
			want:      100,
		},
		{
			name: "no events map falls back to state_default",
			powerLevels: map[string]any{
				"state_default": float64(50),
			},
			eventType: "m.bureau.ha_lease",
			want:      50,
		},
		{
			name:        "empty map returns 0",
			powerLevels: map[string]any{},
			eventType:   "m.bureau.ha_lease",
			want:        0,
		},
		{
			name: "admin-only event at 100",
			powerLevels: map[string]any{
				"state_default": float64(100),
				"events": map[string]any{
					"m.bureau.fleet_service":      float64(100),
					"m.bureau.machine_definition": float64(100),
					"m.bureau.fleet_config":       float64(100),
					"m.bureau.fleet_cache":        float64(100),
					"m.bureau.ha_lease":           float64(0),
					"m.bureau.service_status":     float64(0),
					"m.bureau.fleet_alert":        float64(0),
				},
			},
			eventType: "m.bureau.fleet_service",
			want:      100,
		},
		{
			name: "non-numeric event value falls back to state_default",
			powerLevels: map[string]any{
				"state_default": float64(100),
				"events": map[string]any{
					"m.bureau.ha_lease": "zero",
				},
			},
			eventType: "m.bureau.ha_lease",
			want:      100,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getStateEventPowerLevel(test.powerLevels, test.eventType)
			if got != test.want {
				t.Errorf("getStateEventPowerLevel(%q) = %v, want %v", test.eventType, got, test.want)
			}
		})
	}
}

func TestFleetRoomDefinitions(t *testing.T) {
	// Verify that the fleetRoom definitions in checkFleet match the schema
	// power level functions. This catches drift between the doctor's check
	// list and the canonical power level definitions.

	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")

	t.Run("fleet config room member events at PL 0", func(t *testing.T) {
		expected := schema.FleetRoomPowerLevels(adminUserID)
		events := expected["events"].(map[ref.EventType]any)

		memberEventTypes := []ref.EventType{
			schema.EventTypeHALease,
			schema.EventTypeServiceStatus,
			schema.EventTypeFleetAlert,
		}
		for _, eventType := range memberEventTypes {
			level, ok := events[eventType]
			if !ok {
				t.Errorf("FleetRoomPowerLevels missing event type %s", eventType)
				continue
			}
			if level != 0 {
				t.Errorf("FleetRoomPowerLevels[%s] = %v, want 0", eventType, level)
			}
		}
	})

	t.Run("fleet config room admin events at PL 100", func(t *testing.T) {
		expected := schema.FleetRoomPowerLevels(adminUserID)
		events := expected["events"].(map[ref.EventType]any)

		adminEventTypes := []ref.EventType{
			schema.EventTypeFleetService,
			schema.EventTypeMachineDefinition,
			schema.EventTypeFleetConfig,
			schema.EventTypeFleetCache,
		}
		for _, eventType := range adminEventTypes {
			level, ok := events[eventType]
			if !ok {
				t.Errorf("FleetRoomPowerLevels missing event type %s", eventType)
				continue
			}
			if level != 100 {
				t.Errorf("FleetRoomPowerLevels[%s] = %v, want 100", eventType, level)
			}
		}
	})

	t.Run("machine room member events at PL 0", func(t *testing.T) {
		expected := schema.MachineRoomPowerLevels(adminUserID)
		events := expected["events"].(map[ref.EventType]any)

		memberEventTypes := []ref.EventType{
			schema.EventTypeMachineKey,
			schema.EventTypeMachineInfo,
			schema.EventTypeMachineStatus,
			schema.EventTypeWebRTCOffer,
			schema.EventTypeWebRTCAnswer,
		}
		for _, eventType := range memberEventTypes {
			level, ok := events[eventType]
			if !ok {
				t.Errorf("MachineRoomPowerLevels missing event type %s", eventType)
				continue
			}
			if level != 0 {
				t.Errorf("MachineRoomPowerLevels[%s] = %v, want 0", eventType, level)
			}
		}
	})

	t.Run("service room member events at PL 0", func(t *testing.T) {
		expected := schema.ServiceRoomPowerLevels(adminUserID)
		events := expected["events"].(map[ref.EventType]any)

		memberEventTypes := []ref.EventType{
			schema.EventTypeService,
		}
		for _, eventType := range memberEventTypes {
			level, ok := events[eventType]
			if !ok {
				t.Errorf("ServiceRoomPowerLevels missing event type %s", eventType)
				continue
			}
			if level != 0 {
				t.Errorf("ServiceRoomPowerLevels[%s] = %v, want 0", eventType, level)
			}
		}
	})
}
