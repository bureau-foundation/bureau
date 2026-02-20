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
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// testFleet constructs the ref.Fleet used by all fleet controller tests.
// Namespace "bureau", fleet "prod", server "bureau.local" —
// localpart "bureau/fleet/prod".
func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	ns, err := ref.NewNamespace("bureau.local", "bureau")
	if err != nil {
		t.Fatalf("creating test namespace: %v", err)
	}
	fleet, err := ref.NewFleet(ns, "prod")
	if err != nil {
		t.Fatalf("creating test fleet: %v", err)
	}
	return fleet
}

// testEntity constructs a ref.Entity from a bare account localpart
// (e.g., "service/stt/whisper") using the standard test fleet
// (bureau/fleet/prod on bureau.local). The resulting user ID is
// "@bureau/fleet/prod/service/stt/whisper:bureau.local".
func testEntity(t *testing.T, accountLocalpart string) ref.Entity {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(testFleet(t), accountLocalpart)
	if err != nil {
		t.Fatalf("creating test entity %q: %v", accountLocalpart, err)
	}
	return entity
}

// newTestFleetController creates a FleetController suitable for unit
// testing sync logic. The session is nil, which is safe for code paths
// that don't make network calls.
func newTestFleetController(t *testing.T) *FleetController {
	t.Helper()
	fleetRoomID, err := ref.ParseRoomID("!fleet:local")
	if err != nil {
		t.Fatalf("invalid test room ID: %v", err)
	}
	machineRoomID, err := ref.ParseRoomID("!machine:local")
	if err != nil {
		t.Fatalf("invalid test room ID: %v", err)
	}
	return &FleetController{
		clock:         clock.Real(),
		startedAt:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		fleet:         testFleet(t),
		machines:      make(map[string]*machineState),
		services:      make(map[string]*fleetServiceState),
		definitions:   make(map[string]*schema.MachineDefinitionContent),
		config:        make(map[string]*schema.FleetConfigContent),
		leases:        make(map[string]*schema.HALeaseContent),
		configRooms:   make(map[string]string),
		fleetRoomID:   fleetRoomID,
		machineRoomID: machineRoomID,
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
	fc := newTestFleetController(t)

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
	fc := newTestFleetController(t)

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
	fc := newTestFleetController(t)

	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels: map[string]string{
					"fleet_managed": "service/fleet/prod",
				},
			},
			{
				Principal: testEntity(t, "agent/iree/amdgpu-pm"),
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
	fc := newTestFleetController(t)

	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "agent/iree/amdgpu-pm"),
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
	fc := newTestFleetController(t)

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
				Principal: testEntity(t, "service/stt/whisper"),
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

func TestHandleSyncLeaveConfigRoomCleansUpServiceInstances(t *testing.T) {
	fc := newTestFleetController(t)

	assignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}

	fc.machines["machine/workstation"] = &machineState{
		info:   &schema.MachineInfo{Hostname: "workstation"},
		status: &schema.MachineStatus{CPUPercent: 42},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": assignment,
		},
		configRoomID: "!config-ws:local",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: schema.ReplicaSpec{Min: 1},
		},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": assignment,
		},
	}

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!config-ws:local": {},
			},
		},
	}

	fc.handleSync(context.Background(), response)

	// Service instances should be cleaned up.
	serviceState := fc.services["service/stt/whisper"]
	if len(serviceState.instances) != 0 {
		t.Errorf("expected 0 service instances after config room leave, got %d", len(serviceState.instances))
	}
}

func TestHandleSyncLeaveMachineRoomClearsAllState(t *testing.T) {
	fc := newTestFleetController(t)

	assignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/worker"),
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}

	fc.machines["machine/alpha"] = &machineState{
		info:   &schema.MachineInfo{Hostname: "alpha"},
		status: &schema.MachineStatus{},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/worker": assignment,
		},
	}
	fc.machines["machine/beta"] = &machineState{
		info:        &schema.MachineInfo{Hostname: "beta"},
		status:      &schema.MachineStatus{},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	fc.services["service/worker"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/alpha": assignment,
		},
	}

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Leave: map[string]messaging.LeftRoom{
				"!machine:local": {},
			},
		},
	}

	fc.handleSync(context.Background(), response)

	if len(fc.machines) != 0 {
		t.Errorf("expected 0 machines after machine room leave, got %d", len(fc.machines))
	}
	if len(fc.services["service/worker"].instances) != 0 {
		t.Errorf("expected 0 service instances after machine room leave, got %d",
			len(fc.services["service/worker"].instances))
	}
}

func TestProcessMachineConfigTracksServiceInstances(t *testing.T) {
	fc := newTestFleetController(t)

	// Pre-populate a service definition so instance tracking works.
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: schema.ReplicaSpec{Min: 1},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	// Process a machine config with a fleet-managed assignment.
	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
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

	// The service should now track an instance on this machine.
	serviceState := fc.services["service/stt/whisper"]
	if len(serviceState.instances) != 1 {
		t.Fatalf("expected 1 service instance, got %d", len(serviceState.instances))
	}
	if _, exists := serviceState.instances["machine/workstation"]; !exists {
		t.Error("service should have instance on machine/workstation")
	}
}

func TestProcessMachineConfigRemovesStaleInstances(t *testing.T) {
	fc := newTestFleetController(t)

	oldAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}

	// Pre-populate machine with an existing assignment.
	fc.machines["machine/workstation"] = &machineState{
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": oldAssignment,
		},
		configRoomID: "!config-ws:local",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	// Pre-populate service instances.
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": oldAssignment,
		},
	}

	// Process an updated config that removes the whisper assignment.
	machineConfigContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			// No fleet-managed assignments left.
			{
				Principal: testEntity(t, "agent/coding/main"),
				Template:  "bureau/template:coding-agent",
			},
		},
	})

	event := messaging.Event{
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  machineConfigContent,
	}
	fc.processMachineConfigEvent("!config-ws:local", event)

	// Service instance should be removed.
	serviceState := fc.services["service/stt/whisper"]
	if len(serviceState.instances) != 0 {
		t.Errorf("expected 0 instances after assignment removal, got %d", len(serviceState.instances))
	}

	// Machine should have no fleet-managed assignments.
	machine := fc.machines["machine/workstation"]
	if len(machine.assignments) != 0 {
		t.Errorf("expected 0 fleet-managed assignments, got %d", len(machine.assignments))
	}
}

func TestHandleSyncUpdatesExistingMachine(t *testing.T) {
	fc := newTestFleetController(t)

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

// --- Pending echo tests ---

// TestPendingEchoSkipsStaleConfigEvent verifies that a /sync event
// arriving while a write is pending does not overwrite the optimistic
// local state set by place() or unplace().
func TestPendingEchoSkipsStaleConfigEvent(t *testing.T) {
	fc := newTestFleetController(t)

	// Set up a machine with a placed service and a pending echo.
	placedAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines["machine/workstation"] = &machineState{
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": placedAssignment,
		},
		configRoomID:       "!config-ws:local",
		pendingEchoEventID: "$echo-abc",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": placedAssignment,
		},
	}

	// Simulate a stale /sync event with no fleet assignments (the
	// state before our place() write). The event ID does NOT match
	// the pending echo.
	staleContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{},
	})
	staleEvent := messaging.Event{
		EventID:  "$stale-old-event",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  staleContent,
	}

	fc.processMachineConfigEvent("!config-ws:local", staleEvent)

	// Optimistic state from place() should be preserved.
	machine := fc.machines["machine/workstation"]
	if len(machine.assignments) != 1 {
		t.Fatalf("expected 1 assignment (preserved), got %d", len(machine.assignments))
	}
	if _, exists := machine.assignments["service/stt/whisper"]; !exists {
		t.Error("placed assignment should still be present")
	}

	// Pending echo should still be set.
	if machine.pendingEchoEventID != "$echo-abc" {
		t.Errorf("pending echo should still be %q, got %q", "$echo-abc", machine.pendingEchoEventID)
	}

	// Service instance should be preserved too.
	serviceState := fc.services["service/stt/whisper"]
	if len(serviceState.instances) != 1 {
		t.Fatalf("expected 1 service instance (preserved), got %d", len(serviceState.instances))
	}
}

// TestPendingEchoClearsOnEchoArrival verifies that when the echo of
// our own write arrives via /sync, the pending echo is cleared and
// the event is applied normally.
func TestPendingEchoClearsOnEchoArrival(t *testing.T) {
	fc := newTestFleetController(t)

	placedAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines["machine/workstation"] = &machineState{
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": placedAssignment,
		},
		configRoomID:       "!config-ws:local",
		pendingEchoEventID: "$echo-abc",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": placedAssignment,
		},
	}

	// The echo event — same event ID as the pending echo, with the
	// assignment we placed.
	echoContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	})
	echoEvent := messaging.Event{
		EventID:  "$echo-abc",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  echoContent,
	}

	fc.processMachineConfigEvent("!config-ws:local", echoEvent)

	// Pending echo should be cleared.
	machine := fc.machines["machine/workstation"]
	if machine.pendingEchoEventID != "" {
		t.Errorf("pending echo should be cleared after echo arrival, got %q", machine.pendingEchoEventID)
	}

	// Assignment should still be present (echo confirms it).
	if len(machine.assignments) != 1 {
		t.Fatalf("expected 1 assignment after echo, got %d", len(machine.assignments))
	}
}

// TestPendingEchoAllowsSubsequentEvents verifies that after the echo
// clears the pending state, subsequent /sync events apply normally.
func TestPendingEchoAllowsSubsequentEvents(t *testing.T) {
	fc := newTestFleetController(t)

	fc.machines["machine/workstation"] = &machineState{
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": {
				Principal: testEntity(t, "service/stt/whisper"),
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
		configRoomID:       "!config-ws:local",
		pendingEchoEventID: "$echo-abc",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	// Process the echo to clear the pending state.
	echoContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	})
	fc.processMachineConfigEvent("!config-ws:local", messaging.Event{
		EventID:  "$echo-abc",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  echoContent,
	})

	// Now process a subsequent event that removes all fleet assignments.
	emptyContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{},
	})
	fc.processMachineConfigEvent("!config-ws:local", messaging.Event{
		EventID:  "$later-event",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  emptyContent,
	})

	// The subsequent event should have been applied — assignments empty.
	machine := fc.machines["machine/workstation"]
	if len(machine.assignments) != 0 {
		t.Fatalf("expected 0 assignments after subsequent event, got %d", len(machine.assignments))
	}
}

// TestPendingEchoLatestWriteWins verifies that if multiple writes
// happen before any echo arrives, the latest write's event ID
// supersedes the earlier one. Only the latest echo clears the
// pending state.
func TestPendingEchoLatestWriteWins(t *testing.T) {
	fc := newTestFleetController(t)

	fc.machines["machine/workstation"] = &machineState{
		assignments:        make(map[string]*schema.PrincipalAssignment),
		configRoomID:       "!config-ws:local",
		pendingEchoEventID: "$echo-first",
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	// Simulate a second write overwriting the pending echo.
	fc.machines["machine/workstation"].pendingEchoEventID = "$echo-second"

	// The echo of the first write arrives — should still be skipped
	// because we're waiting for the second echo.
	firstContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{},
	})
	fc.processMachineConfigEvent("!config-ws:local", messaging.Event{
		EventID:  "$echo-first",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  firstContent,
	})

	machine := fc.machines["machine/workstation"]
	if machine.pendingEchoEventID != "$echo-second" {
		t.Errorf("pending echo should still be %q after first echo, got %q",
			"$echo-second", machine.pendingEchoEventID)
	}

	// The echo of the second write arrives — should clear and apply.
	secondContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{},
	})
	fc.processMachineConfigEvent("!config-ws:local", messaging.Event{
		EventID:  "$echo-second",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  secondContent,
	})

	if machine.pendingEchoEventID != "" {
		t.Errorf("pending echo should be cleared after second echo, got %q", machine.pendingEchoEventID)
	}
}

// TestNoPendingEchoPassesThrough verifies that when no write is
// pending, /sync events are applied normally (no regression).
func TestNoPendingEchoPassesThrough(t *testing.T) {
	fc := newTestFleetController(t)

	fc.machines["machine/workstation"] = &machineState{
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: "!config-ws:local",
		// No pending echo.
	}
	fc.configRooms["machine/workstation"] = "!config-ws:local"

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	configContent := toContentMap(t, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	})
	fc.processMachineConfigEvent("!config-ws:local", messaging.Event{
		EventID:  "$normal-event",
		Type:     schema.EventTypeMachineConfig,
		StateKey: stringPtr("machine/workstation"),
		Content:  configContent,
	})

	machine := fc.machines["machine/workstation"]
	if len(machine.assignments) != 1 {
		t.Fatalf("expected 1 assignment from normal event, got %d", len(machine.assignments))
	}
	if _, exists := machine.assignments["service/stt/whisper"]; !exists {
		t.Error("assignment should be present after normal event processing")
	}
}
