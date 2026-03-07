// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- heartbeatInterval ---

func TestHeartbeatIntervalDefault(t *testing.T) {
	fc := newTestFleetController(t)
	if got := fc.heartbeatInterval(); got != defaultHeartbeatInterval {
		t.Errorf("heartbeatInterval() = %v, want %v", got, defaultHeartbeatInterval)
	}
}

func TestHeartbeatIntervalFromConfig(t *testing.T) {
	fc := newTestFleetController(t)
	fc.config[testConfigUserID()] = &fleet.FleetConfigContent{
		HeartbeatIntervalSeconds: 10,
	}
	if got := fc.heartbeatInterval(); got != 10*time.Second {
		t.Errorf("heartbeatInterval() = %v, want 10s", got)
	}
}

func TestHeartbeatIntervalIgnoresZero(t *testing.T) {
	fc := newTestFleetController(t)
	fc.config[testConfigUserID()] = &fleet.FleetConfigContent{
		HeartbeatIntervalSeconds: 0,
	}
	if got := fc.heartbeatInterval(); got != defaultHeartbeatInterval {
		t.Errorf("heartbeatInterval() = %v, want default %v", got, defaultHeartbeatInterval)
	}
}

// --- checkMachineHealth ---

func TestCheckMachineHealthOnline(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	fc.machines[testMachineUserID("a")] = &machineState{
		info:          &schema.MachineInfo{Hostname: "a"},
		status:        &schema.MachineStatus{},
		assignments:   make(map[ref.UserID]*schema.PrincipalAssignment),
		lastHeartbeat: fakeClock.Now().Add(-10 * time.Second),
		healthState:   healthOnline,
	}

	fc.checkMachineHealth(context.Background())

	if fc.machines[testMachineUserID("a")].healthState != healthOnline {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthOnline)
	}
}

func TestCheckMachineHealthSuspect(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	// Heartbeat older than 1x interval (30s) but within 3x (90s).
	fc.machines[testMachineUserID("a")] = &machineState{
		info:          &schema.MachineInfo{Hostname: "a"},
		status:        &schema.MachineStatus{},
		assignments:   make(map[ref.UserID]*schema.PrincipalAssignment),
		lastHeartbeat: fakeClock.Now().Add(-60 * time.Second),
		healthState:   healthOnline,
	}

	fc.checkMachineHealth(context.Background())

	if fc.machines[testMachineUserID("a")].healthState != healthSuspect {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthSuspect)
	}
}

func TestCheckMachineHealthOfflineTriggersFailover(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	store := newFakeConfigStore()
	fc.configStore = store
	fc.principalName = "service/fleet/prod"

	// Seed a machine with a fleet-managed service.
	webEntity := testEntity(t, "service/web")
	fc.machines[testMachineUserID("a")] = &machineState{
		info:   &schema.MachineInfo{Hostname: "a"},
		status: &schema.MachineStatus{},
		assignments: map[ref.UserID]*schema.PrincipalAssignment{
			testServiceUserID("service/web"): {
				Principal: webEntity,
				Template:  "bureau/template:web",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
		configRoomID:  mustRoomID("!config-a:local"),
		lastHeartbeat: fakeClock.Now().Add(-120 * time.Second), // 4x interval: offline
		healthState:   healthSuspect,
	}
	fc.configRooms[testMachineUserID("a")] = mustRoomID("!config-a:local")

	fc.services[testServiceUserID("service/web")] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:web",
			Replicas: fleet.ReplicaSpec{Min: 1, Max: 3},
		},
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			testMachineUserID("a"): fc.machines[testMachineUserID("a")].assignments[testServiceUserID("service/web")],
		},
	}

	// Seed the config store so unplace can read/write.
	store.seedConfig("!config-a:local", testMachineUserID("a").StateKey(), &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: webEntity,
				Template:  "bureau/template:web",
				AutoStart: true,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	})

	fc.checkMachineHealth(context.Background())

	// Machine should be offline.
	if fc.machines[testMachineUserID("a")].healthState != healthOffline {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthOffline)
	}

	// Service should be unplaced from machine/a.
	if len(fc.machines[testMachineUserID("a")].assignments) != 0 {
		t.Errorf("assignments count = %d, want 0", len(fc.machines[testMachineUserID("a")].assignments))
	}
	if len(fc.services[testServiceUserID("service/web")].instances) != 0 {
		t.Errorf("service instances count = %d, want 0", len(fc.services[testServiceUserID("service/web")].instances))
	}

	// A fleet alert should have been published.
	alertKey := storeKey("!fleet:local", "failover/"+testServiceUserID("service/web").String()+"/"+testMachineUserID("a").String())
	if _, exists := store.configs[alertKey]; !exists {
		t.Error("expected fleet alert to be published")
	}
}

func TestCheckMachineHealthOfflineNotReTriggered(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	// Machine already offline with no assignments (failover already happened).
	fc.machines[testMachineUserID("a")] = &machineState{
		info:          &schema.MachineInfo{Hostname: "a"},
		status:        &schema.MachineStatus{},
		assignments:   make(map[ref.UserID]*schema.PrincipalAssignment),
		lastHeartbeat: fakeClock.Now().Add(-120 * time.Second),
		healthState:   healthOffline,
	}

	// Should not trigger failover again (no assignments to move).
	fc.checkMachineHealth(context.Background())

	if fc.machines[testMachineUserID("a")].healthState != healthOffline {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthOffline)
	}
}

func TestCheckMachineHealthSkipsNoHeartbeat(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	// Machine with no heartbeat (discovered through config room only).
	fc.machines[testMachineUserID("a")] = &machineState{
		info:        &schema.MachineInfo{Hostname: "a"},
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}

	fc.checkMachineHealth(context.Background())

	// Health state should remain at its zero value (empty string),
	// not be changed to offline.
	if fc.machines[testMachineUserID("a")].healthState != "" {
		t.Errorf("healthState = %q, want empty (unchanged)", fc.machines[testMachineUserID("a")].healthState)
	}
}

func TestCheckMachineHealthCustomInterval(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	// Set a 10-second heartbeat interval.
	fc.config[testConfigUserID()] = &fleet.FleetConfigContent{
		HeartbeatIntervalSeconds: 10,
	}

	// 25 seconds stale: within 3x of 10s, so suspect.
	fc.machines[testMachineUserID("a")] = &machineState{
		info:          &schema.MachineInfo{Hostname: "a"},
		status:        &schema.MachineStatus{},
		assignments:   make(map[ref.UserID]*schema.PrincipalAssignment),
		lastHeartbeat: fakeClock.Now().Add(-25 * time.Second),
		healthState:   healthOnline,
	}

	fc.checkMachineHealth(context.Background())

	if fc.machines[testMachineUserID("a")].healthState != healthSuspect {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthSuspect)
	}
}

func TestCheckMachineHealthCustomIntervalOffline(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	fc.configStore = newFakeConfigStore()

	// Set a 10-second heartbeat interval.
	fc.config[testConfigUserID()] = &fleet.FleetConfigContent{
		HeartbeatIntervalSeconds: 10,
	}

	// 35 seconds stale: beyond 3x of 10s = 30s, so offline.
	fc.machines[testMachineUserID("a")] = &machineState{
		info:          &schema.MachineInfo{Hostname: "a"},
		status:        &schema.MachineStatus{},
		assignments:   make(map[ref.UserID]*schema.PrincipalAssignment),
		lastHeartbeat: fakeClock.Now().Add(-35 * time.Second),
		healthState:   healthSuspect,
	}

	fc.checkMachineHealth(context.Background())

	if fc.machines[testMachineUserID("a")].healthState != healthOffline {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthOffline)
	}
}

// --- executeFailover ---

func TestExecuteFailoverMultipleServices(t *testing.T) {
	fc := newTestFleetController(t)
	fakeClock := clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	fc.clock = fakeClock
	store := newFakeConfigStore()
	fc.configStore = store
	fc.principalName = "service/fleet/prod"

	webEntity := testEntity(t, "service/web")
	apiEntity := testEntity(t, "service/api")
	fc.machines[testMachineUserID("a")] = &machineState{
		info:   &schema.MachineInfo{Hostname: "a"},
		status: &schema.MachineStatus{},
		assignments: map[ref.UserID]*schema.PrincipalAssignment{
			testServiceUserID("service/web"): {
				Principal: webEntity,
				Template:  "bureau/template:web",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
			testServiceUserID("service/api"): {
				Principal: apiEntity,
				Template:  "bureau/template:api",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
		configRoomID: mustRoomID("!config-a:local"),
	}
	fc.configRooms[testMachineUserID("a")] = mustRoomID("!config-a:local")

	fc.services[testServiceUserID("service/web")] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:web",
			Replicas: fleet.ReplicaSpec{Min: 1, Max: 3},
		},
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			testMachineUserID("a"): fc.machines[testMachineUserID("a")].assignments[testServiceUserID("service/web")],
		},
	}
	fc.services[testServiceUserID("service/api")] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:api",
			Replicas: fleet.ReplicaSpec{Min: 1, Max: 3},
		},
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			testMachineUserID("a"): fc.machines[testMachineUserID("a")].assignments[testServiceUserID("service/api")],
		},
	}

	store.seedConfig("!config-a:local", testMachineUserID("a").StateKey(), &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: webEntity, Template: "bureau/template:web", AutoStart: true, Labels: map[string]string{"fleet_managed": "service/fleet/prod"}},
			{Principal: apiEntity, Template: "bureau/template:api", AutoStart: true, Labels: map[string]string{"fleet_managed": "service/fleet/prod"}},
		},
	})

	fc.executeFailover(context.Background(), testMachineUserID("a"), fc.machines[testMachineUserID("a")])

	if len(fc.machines[testMachineUserID("a")].assignments) != 0 {
		t.Errorf("assignments count = %d, want 0", len(fc.machines[testMachineUserID("a")].assignments))
	}
	if len(fc.services[testServiceUserID("service/web")].instances) != 0 {
		t.Errorf("web instances count = %d, want 0", len(fc.services[testServiceUserID("service/web")].instances))
	}
	if len(fc.services[testServiceUserID("service/api")].instances) != 0 {
		t.Errorf("api instances count = %d, want 0", len(fc.services[testServiceUserID("service/api")].instances))
	}

	// Both alerts should be published.
	webAlert := storeKey("!fleet:local", "failover/"+testServiceUserID("service/web").String()+"/"+testMachineUserID("a").String())
	apiAlert := storeKey("!fleet:local", "failover/"+testServiceUserID("service/api").String()+"/"+testMachineUserID("a").String())
	if _, exists := store.configs[webAlert]; !exists {
		t.Error("expected web failover alert")
	}
	if _, exists := store.configs[apiAlert]; !exists {
		t.Error("expected api failover alert")
	}
}

func TestExecuteFailoverNoAssignments(t *testing.T) {
	fc := newTestFleetController(t)
	fc.clock = clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	store := newFakeConfigStore()
	fc.configStore = store

	fc.machines[testMachineUserID("a")] = &machineState{
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
	}

	// Should be a no-op.
	fc.executeFailover(context.Background(), testMachineUserID("a"), fc.machines[testMachineUserID("a")])

	if len(store.writes) != 0 {
		t.Errorf("writes count = %d, want 0", len(store.writes))
	}
}

// --- publishFleetAlert ---

func TestPublishFleetAlert(t *testing.T) {
	fc := newTestFleetController(t)
	store := newFakeConfigStore()
	fc.configStore = store

	alert := fleet.FleetAlertContent{
		AlertType: "failover",
		Fleet:     "service/fleet/prod",
		Service:   "service/web",
		Machine:   "bureau/fleet/prod/machine/a",
		Message:   "machine offline",
	}

	fc.publishFleetAlert(context.Background(), alert)

	expected := storeKey("!fleet:local", "failover/service/web/bureau/fleet/prod/machine/a")
	raw, exists := store.configs[expected]
	if !exists {
		t.Fatal("expected alert event in config store")
	}

	var written fleet.FleetAlertContent
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("unmarshaling alert: %v", err)
	}
	if written.AlertType != "failover" {
		t.Errorf("AlertType = %q, want failover", written.AlertType)
	}
	if written.Service != "service/web" {
		t.Errorf("Service = %q, want service/web", written.Service)
	}
}

// --- alertStateKey ---

func TestAlertStateKey(t *testing.T) {
	tests := []struct {
		name  string
		alert fleet.FleetAlertContent
		want  string
	}{
		{
			name:  "all fields",
			alert: fleet.FleetAlertContent{AlertType: "failover", Service: "svc/a", Machine: "bureau/fleet/prod/machine/x"},
			want:  "failover/svc/a/bureau/fleet/prod/machine/x",
		},
		{
			name:  "no machine",
			alert: fleet.FleetAlertContent{AlertType: "capacity_request", Service: "svc/a"},
			want:  "capacity_request/svc/a",
		},
		{
			name:  "type only",
			alert: fleet.FleetAlertContent{AlertType: "rollback"},
			want:  "rollback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := alertStateKey(tt.alert); got != tt.want {
				t.Errorf("alertStateKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- processMachineStatusEvent recovery ---

func TestProcessMachineStatusEventSetsHeartbeat(t *testing.T) {
	fc := newTestFleetController(t)
	epoch := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	fc.clock = clock.Fake(epoch)

	event := makeEvent(schema.EventTypeMachineStatus, testMachineUserID("a").StateKey(), map[string]any{
		"principal":      "@bureau/fleet/prod/machine/a:bureau.local",
		"cpu_percent":    42,
		"memory_used_mb": 1000,
	})

	fc.processMachineStatusEvent(event)

	machine := fc.machines[testMachineUserID("a")]
	if machine == nil {
		t.Fatal("expected machine to be created")
	}
	if !machine.lastHeartbeat.Equal(epoch) {
		t.Errorf("lastHeartbeat = %v, want %v", machine.lastHeartbeat, epoch)
	}
	if machine.healthState != healthOnline {
		t.Errorf("healthState = %q, want %q", machine.healthState, healthOnline)
	}
}

func TestProcessMachineStatusEventRecovery(t *testing.T) {
	fc := newTestFleetController(t)
	epoch := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	fc.clock = clock.Fake(epoch)

	// Pre-populate machine as offline.
	fc.machines[testMachineUserID("a")] = &machineState{
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
		healthState: healthOffline,
	}

	// Record logs to verify recovery is logged.
	var logBuffer testLogBuffer
	fc.logger = slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelInfo}))

	event := makeEvent(schema.EventTypeMachineStatus, testMachineUserID("a").StateKey(), map[string]any{
		"principal":      "@bureau/fleet/prod/machine/a:bureau.local",
		"cpu_percent":    10,
		"memory_used_mb": 500,
	})

	fc.processMachineStatusEvent(event)

	if fc.machines[testMachineUserID("a")].healthState != healthOnline {
		t.Errorf("healthState = %q, want %q", fc.machines[testMachineUserID("a")].healthState, healthOnline)
	}
	if !logBuffer.contains("machine recovered") {
		t.Error("expected recovery log message")
	}
}

func TestProcessMachineStatusEventNoRecoveryLogWhenAlreadyOnline(t *testing.T) {
	fc := newTestFleetController(t)
	fc.clock = clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	fc.machines[testMachineUserID("a")] = &machineState{
		assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
		healthState: healthOnline,
	}

	var logBuffer testLogBuffer
	fc.logger = slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelInfo}))

	event := makeEvent(schema.EventTypeMachineStatus, testMachineUserID("a").StateKey(), map[string]any{
		"principal":      "@bureau/fleet/prod/machine/a:bureau.local",
		"cpu_percent":    10,
		"memory_used_mb": 500,
	})

	fc.processMachineStatusEvent(event)

	if logBuffer.contains("machine recovered") {
		t.Error("should not log recovery when already online")
	}
}

// --- Machine health socket action ---

func TestMachineHealthAllMachines(t *testing.T) {
	fc := sampleFleetController(t)
	// Set health states on the sample machines.
	epoch := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	fc.machines[testMachineUserID("workstation")].lastHeartbeat = epoch.Add(-10 * time.Second)
	fc.machines[testMachineUserID("workstation")].healthState = healthOnline
	fc.machines[testMachineUserID("server")].lastHeartbeat = epoch.Add(-60 * time.Second)
	fc.machines[testMachineUserID("server")].healthState = healthSuspect

	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response machineHealthResponse
	if err := client.Call(context.Background(), "machine-health", nil, &response); err != nil {
		t.Fatalf("machine-health call failed: %v", err)
	}

	if len(response.Machines) != 2 {
		t.Fatalf("machines count = %d, want 2", len(response.Machines))
	}
	// Should be sorted by localpart.
	if response.Machines[0].Localpart != testMachineUserID("server").String() {
		t.Errorf("first machine = %q, want %q", response.Machines[0].Localpart, testMachineUserID("server").String())
	}
	if response.Machines[1].Localpart != testMachineUserID("workstation").String() {
		t.Errorf("second machine = %q, want %q", response.Machines[1].Localpart, testMachineUserID("workstation").String())
	}
}

func TestMachineHealthSingleMachine(t *testing.T) {
	fc := sampleFleetController(t)
	epoch := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	fc.machines[testMachineUserID("workstation")].lastHeartbeat = epoch.Add(-10 * time.Second)
	fc.machines[testMachineUserID("workstation")].healthState = healthOnline

	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response machineHealthResponse
	fields := map[string]any{"machine": testMachineUserID("workstation").String()}
	if err := client.Call(context.Background(), "machine-health", fields, &response); err != nil {
		t.Fatalf("machine-health call failed: %v", err)
	}

	if len(response.Machines) != 1 {
		t.Fatalf("machines count = %d, want 1", len(response.Machines))
	}
	if response.Machines[0].HealthState != healthOnline {
		t.Errorf("health_state = %q, want %q", response.Machines[0].HealthState, healthOnline)
	}
}

func TestMachineHealthNotFound(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response machineHealthResponse
	fields := map[string]any{"machine": testMachineUserID("nonexistent").String()}
	err := client.Call(context.Background(), "machine-health", fields, &response)
	if err == nil {
		t.Fatal("expected error for nonexistent machine")
	}
}

func TestMachineHealthDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response machineHealthResponse
	err := client.Call(context.Background(), "machine-health", nil, &response)
	if err == nil {
		t.Fatal("expected error for unauthorized request")
	}
}

func TestMachineHealthUnknownState(t *testing.T) {
	fc := sampleFleetController(t)
	// Machine with no heartbeat should show "unknown".
	fc.machines[testMachineUserID("workstation")].lastHeartbeat = time.Time{}
	fc.machines[testMachineUserID("workstation")].healthState = ""

	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response machineHealthResponse
	fields := map[string]any{"machine": testMachineUserID("workstation").String()}
	if err := client.Call(context.Background(), "machine-health", fields, &response); err != nil {
		t.Fatalf("machine-health call failed: %v", err)
	}

	if response.Machines[0].HealthState != "unknown" {
		t.Errorf("health_state = %q, want unknown", response.Machines[0].HealthState)
	}
	if response.Machines[0].LastHeartbeat != "" {
		t.Errorf("last_heartbeat = %q, want empty", response.Machines[0].LastHeartbeat)
	}
}

// --- Test helpers ---

// testLogBuffer captures log output for assertions.
type testLogBuffer struct {
	data []byte
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *testLogBuffer) contains(substring string) bool {
	return strings.Contains(string(b.data), substring)
}

// makeEvent constructs a messaging.Event for testing.
func makeEvent(eventType ref.EventType, stateKey string, content map[string]any) messaging.Event {
	return messaging.Event{
		Type:     eventType,
		StateKey: &stateKey,
		Content:  content,
	}
}
