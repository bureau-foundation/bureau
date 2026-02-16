// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// newTestFleetController creates a FleetController suitable for unit
// testing sync logic. The session is nil, which is safe for code paths
// that don't make network calls.
func newTestFleetController() *FleetController {
	return &FleetController{
		clock:         clock.Real(),
		startedAt:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		machines:      make(map[string]*machineState),
		services:      make(map[string]*fleetServiceState),
		definitions:   make(map[string]*schema.MachineDefinitionContent),
		config:        make(map[string]*schema.FleetConfigContent),
		leases:        make(map[string]*schema.HALeaseContent),
		configRooms:   make(map[string]string),
		fleetRoomID:   "!fleet:local",
		machineRoomID: "!machine:local",
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// toContentMap converts a typed struct to the map[string]any form used
// by messaging.Event.Content. This round-trips through JSON, matching
// how the Matrix homeserver delivers events.
func toContentMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	return result
}

func stringPtr(s string) *string { return &s }

func TestBuildSyncFilterIncludesFleetTypes(t *testing.T) {
	filter := buildSyncFilter()

	expectedTypes := []string{
		schema.EventTypeFleetService,
		schema.EventTypeMachineDefinition,
		schema.EventTypeFleetConfig,
		schema.EventTypeHALease,
		schema.EventTypeMachineStatus,
		schema.EventTypeMachineInfo,
		schema.EventTypeService,
		schema.EventTypeServiceStatus,
		schema.EventTypeMachineConfig,
		schema.EventTypeFleetAlert,
	}

	for _, eventType := range expectedTypes {
		if !strings.Contains(filter, eventType) {
			t.Errorf("sync filter missing event type %q", eventType)
		}
	}

	// Verify the filter is valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(filter), &parsed); err != nil {
		t.Fatalf("sync filter is not valid JSON: %v", err)
	}

	// Verify the filter has the expected structure.
	room, ok := parsed["room"].(map[string]any)
	if !ok {
		t.Fatal("filter missing 'room' section")
	}
	state, ok := room["state"].(map[string]any)
	if !ok {
		t.Fatal("filter missing 'room.state' section")
	}
	types, ok := state["types"].([]any)
	if !ok {
		t.Fatal("filter missing 'room.state.types'")
	}
	if len(types) != len(expectedTypes) {
		t.Errorf("state types has %d entries, want %d", len(types), len(expectedTypes))
	}
}

func TestProcessFleetRoomState(t *testing.T) {
	fc := newTestFleetController()

	fleetServiceContent := toContentMap(t, schema.FleetServiceContent{
		Template: "bureau/template:whisper-stt",
		Replicas: schema.ReplicaSpec{Min: 1},
		Placement: schema.PlacementConstraints{
			Requires: []string{"gpu"},
		},
		Failover: "migrate",
		Priority: 10,
	})

	machineDefinitionContent := toContentMap(t, schema.MachineDefinitionContent{
		Provider: "local",
		Labels:   map[string]string{"gpu": "rtx4090"},
		Resources: schema.MachineResources{
			CPUCores: 16,
			MemoryMB: 65536,
			GPU:      "rtx4090",
			GPUCount: 1,
		},
		Provisioning: schema.ProvisioningConfig{
			MACAddress: "aa:bb:cc:dd:ee:ff",
		},
		Scaling: schema.ScalingConfig{
			MinInstances: 0,
			MaxInstances: 1,
		},
		Lifecycle: schema.MachineLifecycleConfig{
			ProvisionOn: "demand",
			WakeMethod:  "wol",
		},
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeFleetService,
			StateKey: stringPtr("service/stt/whisper"),
			Content:  fleetServiceContent,
		},
		{
			Type:     schema.EventTypeMachineDefinition,
			StateKey: stringPtr("gpu-workstation"),
			Content:  machineDefinitionContent,
		},
	}

	fc.processRoomState("!fleet:local", stateEvents, nil)

	// Verify fleet service was indexed.
	serviceState, exists := fc.services["service/stt/whisper"]
	if !exists {
		t.Fatal("fleet service 'service/stt/whisper' should be in the services map")
	}
	if serviceState.definition.Template != "bureau/template:whisper-stt" {
		t.Errorf("service template = %q, want %q", serviceState.definition.Template, "bureau/template:whisper-stt")
	}
	if serviceState.definition.Replicas.Min != 1 {
		t.Errorf("service replicas.min = %d, want 1", serviceState.definition.Replicas.Min)
	}

	// Verify machine definition was indexed.
	definition, exists := fc.definitions["gpu-workstation"]
	if !exists {
		t.Fatal("machine definition 'gpu-workstation' should be in the definitions map")
	}
	if definition.Provider != "local" {
		t.Errorf("definition provider = %q, want %q", definition.Provider, "local")
	}
	if definition.Resources.GPUCount != 1 {
		t.Errorf("definition gpu_count = %d, want 1", definition.Resources.GPUCount)
	}
}

func TestProcessMachineRoomState(t *testing.T) {
	fc := newTestFleetController()

	machineInfoContent := toContentMap(t, schema.MachineInfo{
		Principal: "@machine/workstation:bureau.local",
		Hostname:  "workstation",
		CPU: schema.CPUInfo{
			Model:          "AMD Ryzen 9 7950X",
			Sockets:        1,
			CoresPerSocket: 16,
			ThreadsPerCore: 2,
		},
		MemoryTotalMB: 65536,
	})

	machineStatusContent := toContentMap(t, schema.MachineStatus{
		Principal:     "@machine/workstation:bureau.local",
		CPUPercent:    42,
		MemoryUsedMB:  32000,
		UptimeSeconds: 86400,
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeMachineInfo,
			StateKey: stringPtr("machine/workstation"),
			Content:  machineInfoContent,
		},
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: stringPtr("machine/workstation"),
			Content:  machineStatusContent,
		},
	}

	fc.processRoomState("!machine:local", stateEvents, nil)

	machine, exists := fc.machines["machine/workstation"]
	if !exists {
		t.Fatal("machine 'machine/workstation' should be in the machines map")
	}
	if machine.info == nil {
		t.Fatal("machine info should not be nil")
	}
	if machine.info.Hostname != "workstation" {
		t.Errorf("machine hostname = %q, want %q", machine.info.Hostname, "workstation")
	}
	if machine.info.CPU.CoresPerSocket != 16 {
		t.Errorf("machine cores_per_socket = %d, want 16", machine.info.CPU.CoresPerSocket)
	}
	if machine.status == nil {
		t.Fatal("machine status should not be nil")
	}
	if machine.status.CPUPercent != 42 {
		t.Errorf("machine cpu_percent = %d, want 42", machine.status.CPUPercent)
	}
	if machine.status.MemoryUsedMB != 32000 {
		t.Errorf("machine memory_used_mb = %d, want 32000", machine.status.MemoryUsedMB)
	}
}

func TestProcessConfigRoomState(t *testing.T) {
	fc := newTestFleetController()

	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "service/stt/whisper",
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels: map[string]string{
					"fleet_managed": "service/fleet/prod",
				},
			},
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:llm-agent",
				AutoStart: true,
			},
		},
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeMachineConfig,
			StateKey: stringPtr("machine/workstation"),
			Content:  machineConfigContent,
		},
	}

	fc.processRoomState("!config-ws:local", stateEvents, nil)

	// Verify the config room mapping was recorded.
	configRoomID, exists := fc.configRooms["machine/workstation"]
	if !exists {
		t.Fatal("config room mapping should exist for machine/workstation")
	}
	if configRoomID != "!config-ws:local" {
		t.Errorf("config room ID = %q, want %q", configRoomID, "!config-ws:local")
	}

	// Verify the fleet-managed assignment was tracked.
	machine, exists := fc.machines["machine/workstation"]
	if !exists {
		t.Fatal("machine 'machine/workstation' should be in the machines map")
	}
	if machine.configRoomID != "!config-ws:local" {
		t.Errorf("machine config room ID = %q, want %q", machine.configRoomID, "!config-ws:local")
	}
	assignment, exists := machine.assignments["service/stt/whisper"]
	if !exists {
		t.Fatal("fleet-managed assignment 'service/stt/whisper' should be tracked")
	}
	if assignment.Template != "bureau/template:whisper-stt" {
		t.Errorf("assignment template = %q, want %q", assignment.Template, "bureau/template:whisper-stt")
	}
}

func TestProcessConfigRoomIgnoresNonFleetAssignments(t *testing.T) {
	fc := newTestFleetController()

	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:llm-agent",
				AutoStart: true,
				// No fleet_managed label.
			},
		},
	})

	stateEvents := []messaging.Event{
		{
			Type:     schema.EventTypeMachineConfig,
			StateKey: stringPtr("machine/workstation"),
			Content:  machineConfigContent,
		},
	}

	fc.processRoomState("!config-ws:local", stateEvents, nil)

	machine, exists := fc.machines["machine/workstation"]
	if !exists {
		t.Fatal("machine should exist (created by config room processing)")
	}
	if len(machine.assignments) != 0 {
		t.Fatalf("expected 0 fleet-managed assignments, got %d", len(machine.assignments))
	}
}

func TestHandleSyncLeaveRemovesMachine(t *testing.T) {
	fc := newTestFleetController()

	// Set up a machine with a config room.
	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{
			Principal: "@machine/workstation:bureau.local",
			Hostname:  "workstation",
		},
		status: &schema.MachineStatus{
			Principal:  "@machine/workstation:bureau.local",
			CPUPercent: 42,
		},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": {
				Localpart: "service/stt/whisper",
				Template:  "bureau/template:whisper-stt",
			},
		},
		configRoomID: "!config-ws:local",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!config-ws:local": {},
			},
		},
	}

	fc.handleSync(context.Background(), response)

	// Config room mapping should be removed.
	if _, exists := fc.configRooms["machine/workstation"]; exists {
		t.Fatal("config room mapping should have been removed after leave")
	}

	// Machine should still exist but with empty assignments.
	machine, exists := fc.machines["machine/workstation"]
	if !exists {
		t.Fatal("machine should still exist after config room leave")
	}
	if len(machine.assignments) != 0 {
		t.Fatalf("machine assignments should be empty after config room leave, got %d", len(machine.assignments))
	}
	if machine.configRoomID != "" {
		t.Errorf("machine config room ID should be empty after leave, got %q", machine.configRoomID)
	}
}

func TestHandleSyncUpdatesExistingMachine(t *testing.T) {
	fc := newTestFleetController()

	// Pre-populate with initial machine state.
	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{
			Principal: "@machine/workstation:bureau.local",
			Hostname:  "workstation",
		},
		status: &schema.MachineStatus{
			Principal:    "@machine/workstation:bureau.local",
			CPUPercent:   42,
			MemoryUsedMB: 32000,
		},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	// Simulate an incremental sync with an updated machine status.
	updatedStatus := toContentMap(t, schema.MachineStatus{
		Principal:     "@machine/workstation:bureau.local",
		CPUPercent:    85,
		MemoryUsedMB:  58000,
		UptimeSeconds: 172800,
	})

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				"!machine:local": {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeMachineStatus,
								StateKey: stringPtr("machine/workstation"),
								Content:  updatedStatus,
							},
						},
					},
				},
			},
		},
	}

	fc.handleSync(context.Background(), response)

	machine, exists := fc.machines["machine/workstation"]
	if !exists {
		t.Fatal("machine should still exist after sync update")
	}
	if machine.status.CPUPercent != 85 {
		t.Errorf("machine cpu_percent = %d, want 85", machine.status.CPUPercent)
	}
	if machine.status.MemoryUsedMB != 58000 {
		t.Errorf("machine memory_used_mb = %d, want 58000", machine.status.MemoryUsedMB)
	}
	if machine.status.UptimeSeconds != 172800 {
		t.Errorf("machine uptime_seconds = %d, want 172800", machine.status.UptimeSeconds)
	}
	// Info should be preserved from the original state.
	if machine.info == nil {
		t.Fatal("machine info should still be present after status-only update")
	}
	if machine.info.Hostname != "workstation" {
		t.Errorf("machine hostname = %q, want %q", machine.info.Hostname, "workstation")
	}
}
