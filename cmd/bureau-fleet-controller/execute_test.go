// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

// fakeConfigStore implements configStore for testing placement execution
// without a real Matrix homeserver. Configs are stored in memory, keyed
// by "roomID|stateKey". Missing keys return a Matrix M_NOT_FOUND error,
// matching the real homeserver behavior.
type fakeConfigStore struct {
	mu      sync.Mutex
	configs map[string]json.RawMessage
	writes  []configWrite
}

// configWrite records a single SendStateEvent call for test assertions.
type configWrite struct {
	RoomID    string
	EventType ref.EventType
	StateKey  string
	Content   any
}

func newFakeConfigStore() *fakeConfigStore {
	return &fakeConfigStore{
		configs: make(map[string]json.RawMessage),
	}
}

// storeKey builds the lookup key from roomID and stateKey.
func storeKey(roomID, stateKey string) string {
	return roomID + "|" + stateKey
}

func (f *fakeConfigStore) GetStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, exists := f.configs[storeKey(roomID.String(), stateKey)]
	if !exists {
		return nil, &messaging.MatrixError{
			Code:       messaging.ErrCodeNotFound,
			Message:    "State event not found",
			StatusCode: 404,
		}
	}
	return raw, nil
}

func (f *fakeConfigStore) SendStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := json.Marshal(content)
	if err != nil {
		return ref.EventID{}, fmt.Errorf("marshaling content: %w", err)
	}
	f.configs[storeKey(roomID.String(), stateKey)] = data
	f.writes = append(f.writes, configWrite{
		RoomID:    roomID.String(),
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
	return ref.MustParseEventID("$event-" + stateKey), nil
}

// seedConfig stores a MachineConfig in the fake so that subsequent reads
// will return it.
func (f *fakeConfigStore) seedConfig(roomID, stateKey string, config *schema.MachineConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := json.Marshal(config)
	if err != nil {
		panic("seedConfig: " + err.Error())
	}
	f.configs[storeKey(roomID, stateKey)] = data
}

// latestConfig reads the most recently stored MachineConfig for a given
// room and state key. Panics if no config exists — call only when a
// write is expected to have occurred.
func (f *fakeConfigStore) latestConfig(roomID, stateKey string) *schema.MachineConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, exists := f.configs[storeKey(roomID, stateKey)]
	if !exists {
		panic("latestConfig: no config for " + storeKey(roomID, stateKey))
	}
	var config schema.MachineConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		panic("latestConfig: " + err.Error())
	}
	return &config
}

// newExecuteTestController creates a FleetController with a
// fakeConfigStore and principalName set for execute tests.
func newExecuteTestController(t *testing.T) (*FleetController, *fakeConfigStore) {
	t.Helper()
	fc := newTestFleetController(t)
	store := newFakeConfigStore()
	fc.configStore = store
	fc.principalName = "service/fleet/prod"
	return fc, store
}

// --- readMachineConfig tests ---

func TestReadMachineConfigExisting(t *testing.T) {
	fc, store := newExecuteTestController(t)

	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")
	store.seedConfig("!config-ws:local", "machine/workstation", &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: testEntity(t, "service/stt/whisper"), Template: "bureau/template:whisper-stt"},
		},
	})

	config, err := fc.readMachineConfig(context.Background(), "machine/workstation")
	if err != nil {
		t.Fatalf("readMachineConfig: %v", err)
	}
	if len(config.Principals) != 1 {
		t.Fatalf("expected 1 principal, got %d", len(config.Principals))
	}
	if config.Principals[0].Principal.AccountLocalpart() != "service/stt/whisper" {
		t.Errorf("principal localpart = %q, want service/stt/whisper", config.Principals[0].Principal.AccountLocalpart())
	}
}

func TestReadMachineConfigNotFound(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")
	// Don't seed any config — should return empty MachineConfig.

	config, err := fc.readMachineConfig(context.Background(), "machine/workstation")
	if err != nil {
		t.Fatalf("readMachineConfig should succeed with empty config: %v", err)
	}
	if len(config.Principals) != 0 {
		t.Errorf("expected empty principals on M_NOT_FOUND, got %d", len(config.Principals))
	}
}

func TestReadMachineConfigNoConfigRoom(t *testing.T) {
	fc, _ := newExecuteTestController(t)
	// Don't add any config room mapping.

	_, err := fc.readMachineConfig(context.Background(), "machine/workstation")
	if err == nil {
		t.Fatal("expected error for missing config room")
	}
}

// --- writeMachineConfig tests ---

func TestWriteMachineConfig(t *testing.T) {
	fc, store := newExecuteTestController(t)

	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	config := &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: testEntity(t, "service/stt/whisper"), Template: "bureau/template:whisper-stt"},
		},
	}
	eventID, err := fc.writeMachineConfig(context.Background(), "machine/workstation", config)
	if err != nil {
		t.Fatalf("writeMachineConfig: %v", err)
	}
	if eventID.IsZero() {
		t.Error("writeMachineConfig should return a non-empty event ID")
	}

	if len(store.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(store.writes))
	}
	if store.writes[0].RoomID != "!config-ws:local" {
		t.Errorf("write room ID = %q, want !config-ws:local", store.writes[0].RoomID)
	}
	if store.writes[0].EventType != schema.EventTypeMachineConfig {
		t.Errorf("write event type = %q, want %q", store.writes[0].EventType, schema.EventTypeMachineConfig)
	}
	if store.writes[0].StateKey != "machine/workstation" {
		t.Errorf("write state key = %q, want machine/workstation", store.writes[0].StateKey)
	}
}

func TestWriteMachineConfigNoConfigRoom(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	_, err := fc.writeMachineConfig(context.Background(), "machine/workstation", &schema.MachineConfig{})
	if err == nil {
		t.Fatal("expected error for missing config room")
	}
}

// --- buildAssignment tests ---

func TestBuildAssignment(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:whisper-stt",
		Replicas: fleet.ReplicaSpec{Min: 2},
		Payload:  json.RawMessage(`{"model":"whisper-large","language":"en"}`),
	}

	assignment, err := fc.buildAssignment("service/stt/whisper", definition)
	if err != nil {
		t.Fatalf("buildAssignment: %v", err)
	}

	if assignment.Principal.AccountLocalpart() != "service/stt/whisper" {
		t.Errorf("localpart = %q, want service/stt/whisper", assignment.Principal.AccountLocalpart())
	}
	if assignment.Template != "bureau/template:whisper-stt" {
		t.Errorf("template = %q, want bureau/template:whisper-stt", assignment.Template)
	}
	if !assignment.AutoStart {
		t.Error("auto_start should be true")
	}
	if assignment.Labels["fleet_managed"] != "service/fleet/prod" {
		t.Errorf("fleet_managed label = %q, want service/fleet/prod", assignment.Labels["fleet_managed"])
	}
	if assignment.Payload == nil {
		t.Fatal("Payload should be propagated from definition")
	}
	if assignment.Payload["model"] != "whisper-large" {
		t.Errorf("Payload[model] = %v, want whisper-large", assignment.Payload["model"])
	}
	if assignment.Payload["language"] != "en" {
		t.Errorf("Payload[language] = %v, want en", assignment.Payload["language"])
	}
}

func TestBuildAssignmentMalformedPayload(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:worker",
		Replicas: fleet.ReplicaSpec{Min: 1},
		Payload:  json.RawMessage(`not valid json`),
	}

	_, err := fc.buildAssignment("service/worker", definition)
	if err == nil {
		t.Fatal("expected error for malformed payload")
	}
}

func TestBuildAssignmentPropagatesAuthorization(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:ticket-service",
		Replicas: fleet.ReplicaSpec{Min: 1},
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin:   true,
			AllowInvite: true,
		},
		ServiceVisibility: []string{schema.ActionServiceAll},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{schema.ActionTicketCreate, schema.ActionTicketUpdate}},
			},
		},
	}

	assignment, err := fc.buildAssignment("service/ticket", definition)
	if err != nil {
		t.Fatalf("buildAssignment: %v", err)
	}

	if assignment.MatrixPolicy == nil {
		t.Fatal("MatrixPolicy should be propagated from definition")
	}
	if !assignment.MatrixPolicy.AllowJoin {
		t.Error("MatrixPolicy.AllowJoin should be true")
	}
	if !assignment.MatrixPolicy.AllowInvite {
		t.Error("MatrixPolicy.AllowInvite should be true")
	}

	if len(assignment.ServiceVisibility) != 1 || assignment.ServiceVisibility[0] != schema.ActionServiceAll {
		t.Errorf("ServiceVisibility = %v, want [%s]", assignment.ServiceVisibility, schema.ActionServiceAll)
	}

	if assignment.Authorization == nil {
		t.Fatal("Authorization should be propagated from definition")
	}
	if len(assignment.Authorization.Grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(assignment.Authorization.Grants))
	}
	if assignment.Authorization.Grants[0].Actions[0] != schema.ActionTicketCreate {
		t.Errorf("grant action = %q, want %s", assignment.Authorization.Grants[0].Actions[0], schema.ActionTicketCreate)
	}
}

func TestBuildAssignmentNilAuthorization(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:worker",
		Replicas: fleet.ReplicaSpec{Min: 1},
	}

	assignment, err := fc.buildAssignment("service/worker", definition)
	if err != nil {
		t.Fatalf("buildAssignment: %v", err)
	}

	if assignment.MatrixPolicy != nil {
		t.Errorf("MatrixPolicy should be nil when not set on definition, got %+v", assignment.MatrixPolicy)
	}
	if assignment.ServiceVisibility != nil {
		t.Errorf("ServiceVisibility should be nil when not set on definition, got %v", assignment.ServiceVisibility)
	}
	if assignment.Authorization != nil {
		t.Errorf("Authorization should be nil when not set on definition, got %+v", assignment.Authorization)
	}
}

// --- place tests ---

func TestPlaceHappyPath(t *testing.T) {
	fc, store := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:         &schema.MachineInfo{Hostname: "workstation", MemoryTotalMB: 65536},
		status:       &schema.MachineStatus{CPUPercent: 42},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: fleet.ReplicaSpec{Min: 1},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	// No existing config — readMachineConfig will return empty.
	err := fc.place(context.Background(), "service/stt/whisper", "machine/workstation")
	if err != nil {
		t.Fatalf("place: %v", err)
	}

	// Verify Matrix write happened.
	if len(store.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(store.writes))
	}
	written := store.latestConfig("!config-ws:local", "machine/workstation")
	if len(written.Principals) != 1 {
		t.Fatalf("expected 1 principal in written config, got %d", len(written.Principals))
	}
	if written.Principals[0].Principal.AccountLocalpart() != "service/stt/whisper" {
		t.Errorf("written principal = %q, want service/stt/whisper", written.Principals[0].Principal.AccountLocalpart())
	}
	if written.Principals[0].Labels["fleet_managed"] != "service/fleet/prod" {
		t.Errorf("written fleet_managed = %q, want service/fleet/prod", written.Principals[0].Labels["fleet_managed"])
	}

	// Verify in-memory model was updated.
	machine := fc.machines["machine/workstation"]
	if _, exists := machine.assignments["service/stt/whisper"]; !exists {
		t.Error("machine assignments should contain the placed service")
	}
	serviceState := fc.services["service/stt/whisper"]
	if _, exists := serviceState.instances["machine/workstation"]; !exists {
		t.Error("service instances should contain the target machine")
	}

	// Verify pending echo was recorded.
	if machine.pendingEchoEventID.IsZero() {
		t.Error("machine should have a pending echo event ID after place")
	}
}

func TestPlacePreservesExistingPrincipals(t *testing.T) {
	fc, store := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:         &schema.MachineInfo{Hostname: "workstation", MemoryTotalMB: 65536},
		status:       &schema.MachineStatus{CPUPercent: 42},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: fleet.ReplicaSpec{Min: 1},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	// Seed an existing non-fleet principal.
	store.seedConfig("!config-ws:local", "machine/workstation", &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "agent/coding/main"),
				Template:  "bureau/template:coding-agent",
				AutoStart: true,
			},
		},
	})

	err := fc.place(context.Background(), "service/stt/whisper", "machine/workstation")
	if err != nil {
		t.Fatalf("place: %v", err)
	}

	written := store.latestConfig("!config-ws:local", "machine/workstation")
	if len(written.Principals) != 2 {
		t.Fatalf("expected 2 principals (existing + new), got %d", len(written.Principals))
	}

	// Verify the existing principal is preserved.
	found := false
	for _, principal := range written.Principals {
		if principal.Principal.AccountLocalpart() == "agent/coding/main" {
			found = true
			break
		}
	}
	if !found {
		t.Error("existing non-fleet principal should be preserved after place")
	}
}

func TestPlaceServiceNotFound(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:         &schema.MachineInfo{},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-ws:local"),
	}

	err := fc.place(context.Background(), "service/nonexistent", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for nonexistent service")
	}
}

func TestPlaceServiceNoDefinition(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:         &schema.MachineInfo{},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.services["service/pending"] = &fleetServiceState{
		definition: nil,
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	err := fc.place(context.Background(), "service/pending", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for service with no definition")
	}
}

func TestPlaceMachineNotFound(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	err := fc.place(context.Background(), "service/stt/whisper", "machine/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent machine")
	}
}

func TestPlaceMachineNoConfigRoom(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:        &schema.MachineInfo{},
		assignments: make(map[string]*schema.PrincipalAssignment),
		// configRoomID left at zero value: no config room.
	}
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	err := fc.place(context.Background(), "service/stt/whisper", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for machine with no config room")
	}
}

func TestPlaceAlreadyPlaced(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	existingAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": existingAssignment,
		},
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	err := fc.place(context.Background(), "service/stt/whisper", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for already-placed service")
	}
}

// --- unplace tests ---

func TestUnplaceHappyPath(t *testing.T) {
	fc, store := newExecuteTestController(t)

	existingAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		AutoStart: true,
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}

	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{Hostname: "workstation"},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": existingAssignment,
		},
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: fleet.ReplicaSpec{Min: 1},
		},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": existingAssignment,
		},
	}

	// Seed the Matrix config with the fleet assignment and a non-fleet one.
	store.seedConfig("!config-ws:local", "machine/workstation", &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
			{
				Principal: testEntity(t, "agent/coding/main"),
				Template:  "bureau/template:coding-agent",
				AutoStart: true,
			},
		},
	})

	err := fc.unplace(context.Background(), "service/stt/whisper", "machine/workstation")
	if err != nil {
		t.Fatalf("unplace: %v", err)
	}

	// Verify the fleet assignment was removed but the non-fleet one preserved.
	written := store.latestConfig("!config-ws:local", "machine/workstation")
	if len(written.Principals) != 1 {
		t.Fatalf("expected 1 principal after unplace, got %d", len(written.Principals))
	}
	if written.Principals[0].Principal.AccountLocalpart() != "agent/coding/main" {
		t.Errorf("remaining principal = %q, want agent/coding/main", written.Principals[0].Principal.AccountLocalpart())
	}

	// Verify in-memory model was updated.
	machine := fc.machines["machine/workstation"]
	if _, exists := machine.assignments["service/stt/whisper"]; exists {
		t.Error("machine assignments should not contain the unplaced service")
	}
	serviceState := fc.services["service/stt/whisper"]
	if _, exists := serviceState.instances["machine/workstation"]; exists {
		t.Error("service instances should not contain the target machine")
	}

	// Verify pending echo was recorded.
	if machine.pendingEchoEventID.IsZero() {
		t.Error("machine should have a pending echo event ID after unplace")
	}
}

func TestUnplaceNotPlaced(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	fc.machines["machine/workstation"] = &machineState{
		info:         &schema.MachineInfo{},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-ws:local"),
	}

	err := fc.unplace(context.Background(), "service/stt/whisper", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for service not placed on machine")
	}
}

func TestUnplaceWrongFleetController(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	// Assignment managed by a different fleet controller.
	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": {
				Principal: testEntity(t, "service/stt/whisper"),
				Labels:    map[string]string{"fleet_managed": "service/fleet/staging"},
			},
		},
		configRoomID: mustRoomID("!config-ws:local"),
	}

	err := fc.unplace(context.Background(), "service/stt/whisper", "machine/workstation")
	if err == nil {
		t.Fatal("expected error for assignment managed by different fleet controller")
	}
}

func TestUnplaceMachineNotFound(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	err := fc.unplace(context.Background(), "service/stt/whisper", "machine/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent machine")
	}
}

// --- reconcile tests ---

func TestReconcileUnderReplicated(t *testing.T) {
	fc, store := newExecuteTestController(t)

	// Two machines eligible for placement.
	for _, name := range []string{"machine/alpha", "machine/beta"} {
		machine := standardMachine()
		machine.configRoomID = mustRoomID("!config-" + name + ":local")
		fc.machines[name] = machine
		fc.configRooms[name] = machine.configRoomID
	}

	// Service needs min 2 replicas but has 0 instances.
	fc.services["service/worker"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:worker",
			Replicas: fleet.ReplicaSpec{Min: 2},
			Resources: fleet.ResourceRequirements{
				MemoryMB: 2048,
				GPU:      true,
			},
			Placement: fleet.PlacementConstraints{
				Requires: []string{"gpu"},
			},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	fc.reconcile(context.Background())

	// Both machines should have received a placement.
	serviceState := fc.services["service/worker"]
	if len(serviceState.instances) != 2 {
		t.Errorf("expected 2 instances after reconcile, got %d", len(serviceState.instances))
	}

	// Verify writes went to the config store.
	if len(store.writes) != 2 {
		t.Errorf("expected 2 config writes, got %d", len(store.writes))
	}
}

func TestReconcileOverReplicated(t *testing.T) {
	fc, store := newExecuteTestController(t)

	// Service has max 1 but is on 2 machines.
	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:worker",
		Replicas: fleet.ReplicaSpec{Min: 1, Max: 1},
		Resources: fleet.ResourceRequirements{
			MemoryMB: 2048,
			GPU:      true,
		},
		Placement: fleet.PlacementConstraints{
			Requires: []string{"gpu"},
		},
	}

	workerEntity := testEntity(t, "service/worker")
	fleetAssignment := func(name string) *schema.PrincipalAssignment {
		return &schema.PrincipalAssignment{
			Principal: workerEntity,
			Template:  "bureau/template:worker",
			AutoStart: true,
			Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
		}
	}

	// Machine alpha: low utilization (high score — should be kept).
	machineAlpha := standardMachine()
	machineAlpha.status.CPUPercent = 20
	machineAlpha.status.MemoryUsedMB = 16000
	machineAlpha.configRoomID = mustRoomID("!config-alpha:local")
	machineAlpha.assignments = map[string]*schema.PrincipalAssignment{
		"service/worker": fleetAssignment("alpha"),
	}
	fc.machines["machine/alpha"] = machineAlpha
	fc.configRooms["machine/alpha"] = mustRoomID("!config-alpha:local")

	// Machine beta: high utilization (low score — should be removed).
	machineBeta := standardMachine()
	machineBeta.status.CPUPercent = 80
	machineBeta.status.MemoryUsedMB = 55000
	machineBeta.configRoomID = mustRoomID("!config-beta:local")
	machineBeta.assignments = map[string]*schema.PrincipalAssignment{
		"service/worker": fleetAssignment("beta"),
	}
	fc.machines["machine/beta"] = machineBeta
	fc.configRooms["machine/beta"] = mustRoomID("!config-beta:local")

	fc.services["service/worker"] = &fleetServiceState{
		definition: definition,
		instances: map[string]*schema.PrincipalAssignment{
			"machine/alpha": fleetAssignment("alpha"),
			"machine/beta":  fleetAssignment("beta"),
		},
	}

	// Seed configs so unplace can read them.
	for _, name := range []string{"machine/alpha", "machine/beta"} {
		store.seedConfig(fc.configRooms[name].String(), name, &schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				*fleetAssignment(name),
			},
		})
	}

	fc.reconcile(context.Background())

	// Should have removed the lowest-scored machine (beta, higher CPU).
	serviceState := fc.services["service/worker"]
	if len(serviceState.instances) != 1 {
		t.Fatalf("expected 1 instance after reconcile, got %d", len(serviceState.instances))
	}
	if _, exists := serviceState.instances["machine/alpha"]; !exists {
		t.Error("machine/alpha should be the remaining instance (higher score)")
	}
	if _, exists := serviceState.instances["machine/beta"]; exists {
		t.Error("machine/beta should have been removed (lower score)")
	}

	// Verify the write removed the assignment from beta.
	if len(store.writes) != 1 {
		t.Fatalf("expected 1 write (unplace from beta), got %d", len(store.writes))
	}
	if store.writes[0].RoomID != "!config-beta:local" {
		t.Errorf("write room = %q, want !config-beta:local", store.writes[0].RoomID)
	}
}

func TestReconcileAlreadySatisfied(t *testing.T) {
	fc, store := newExecuteTestController(t)

	machine := standardMachine()
	machine.configRoomID = mustRoomID("!config-ws:local")
	existingAssignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/worker"),
		Template:  "bureau/template:worker",
		AutoStart: true,
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	machine.assignments = map[string]*schema.PrincipalAssignment{
		"service/worker": existingAssignment,
	}
	fc.machines["machine/workstation"] = machine
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	fc.services["service/worker"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:worker",
			Replicas: fleet.ReplicaSpec{Min: 1},
			Resources: fleet.ResourceRequirements{
				MemoryMB: 2048,
				GPU:      true,
			},
			Placement: fleet.PlacementConstraints{
				Requires: []string{"gpu"},
			},
		},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": existingAssignment,
		},
	}

	fc.reconcile(context.Background())

	// No writes should have occurred — the service is already at min.
	if len(store.writes) != 0 {
		t.Errorf("expected 0 writes when already satisfied, got %d", len(store.writes))
	}
}

func TestReconcileSkipsServicesWithoutDefinition(t *testing.T) {
	fc, store := newExecuteTestController(t)

	fc.services["service/orphan"] = &fleetServiceState{
		definition: nil,
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	fc.reconcile(context.Background())

	if len(store.writes) != 0 {
		t.Errorf("expected 0 writes for service without definition, got %d", len(store.writes))
	}
}

func TestReconcileInsufficientMachines(t *testing.T) {
	fc, store := newExecuteTestController(t)

	// One eligible machine, but service wants min 3.
	machine := standardMachine()
	machine.configRoomID = mustRoomID("!config-ws:local")
	fc.machines["machine/workstation"] = machine
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	fc.services["service/worker"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:worker",
			Replicas: fleet.ReplicaSpec{Min: 3},
			Resources: fleet.ResourceRequirements{
				MemoryMB: 2048,
				GPU:      true,
			},
			Placement: fleet.PlacementConstraints{
				Requires: []string{"gpu"},
			},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	fc.reconcile(context.Background())

	// Should have placed on the 1 available machine despite wanting 3.
	serviceState := fc.services["service/worker"]
	if len(serviceState.instances) != 1 {
		t.Errorf("expected 1 instance (placed on only available machine), got %d", len(serviceState.instances))
	}
	if len(store.writes) != 1 {
		t.Errorf("expected 1 write, got %d", len(store.writes))
	}
}

// --- rebuildServiceInstances tests ---

func TestRebuildServiceInstances(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	assignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Template:  "bureau/template:whisper-stt",
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}

	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{Hostname: "workstation"},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": assignment,
		},
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.machines["machine/server"] = &machineState{
		info: &schema.MachineInfo{Hostname: "server"},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": assignment,
		},
		configRoomID: mustRoomID("!config-srv:local"),
	}

	// Service exists but instances are empty (simulating post-initial-sync
	// before rebuild).
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: fleet.ReplicaSpec{Min: 2},
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	fc.rebuildServiceInstances()

	serviceState := fc.services["service/stt/whisper"]
	if len(serviceState.instances) != 2 {
		t.Fatalf("expected 2 instances after rebuild, got %d", len(serviceState.instances))
	}
	if _, exists := serviceState.instances["machine/workstation"]; !exists {
		t.Error("instances should contain machine/workstation")
	}
	if _, exists := serviceState.instances["machine/server"]; !exists {
		t.Error("instances should contain machine/server")
	}
}

func TestRebuildServiceInstancesIgnoresUnknownServices(t *testing.T) {
	fc, _ := newExecuteTestController(t)

	// Machine has an assignment for a service not in fc.services.
	fc.machines["machine/workstation"] = &machineState{
		assignments: map[string]*schema.PrincipalAssignment{
			"service/unknown": {
				Principal: testEntity(t, "service/unknown"),
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	}

	fc.services["service/worker"] = &fleetServiceState{
		definition: &fleet.FleetServiceContent{Template: "t"},
		instances:  make(map[string]*schema.PrincipalAssignment),
	}

	// Should not panic or add stale entries.
	fc.rebuildServiceInstances()

	if len(fc.services["service/worker"].instances) != 0 {
		t.Error("worker should have 0 instances (machine has unrelated assignment)")
	}
}

// --- isFleetManaged tests ---

func TestIsFleetManaged(t *testing.T) {
	whisperEntity := testEntity(t, "service/stt/whisper")
	agentEntity := testEntity(t, "agent/coding/main")

	tests := []struct {
		name       string
		assignment *schema.PrincipalAssignment
		want       bool
	}{
		{
			name: "fleet-managed assignment",
			assignment: &schema.PrincipalAssignment{
				Principal: whisperEntity,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
			want: true,
		},
		{
			name: "non-fleet assignment with labels",
			assignment: &schema.PrincipalAssignment{
				Principal: agentEntity,
				Labels:    map[string]string{"purpose": "coding"},
			},
			want: false,
		},
		{
			name: "assignment with nil labels",
			assignment: &schema.PrincipalAssignment{
				Principal: agentEntity,
				Labels:    nil,
			},
			want: false,
		},
		{
			name: "assignment with empty labels",
			assignment: &schema.PrincipalAssignment{
				Principal: agentEntity,
				Labels:    map[string]string{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFleetManaged(tt.assignment)
			if got != tt.want {
				t.Errorf("isFleetManaged() = %v, want %v", got, tt.want)
			}
		})
	}
}
