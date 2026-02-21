// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// testClockEpoch is the fixed time used by the fake clock in socket
// tests. Token timestamps and the fleet controller clock share this
// epoch so token validation succeeds deterministically.
var testClockEpoch = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// --- Test infrastructure ---

// testServer creates a FleetController with a running socket server and
// returns a ServiceClient connected via a real minted token. The token
// carries fleet/* grants so all query actions are authorized. Call the
// returned cleanup function to shut down the server.
func testServer(t *testing.T, fc *FleetController) (*service.ServiceClient, func()) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(testClockEpoch)
	fc.clock = testClock
	fc.startedAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "fleet",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "fleet.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	fc.logger = logger
	fc.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintToken(t, privateKey, "agent/tester", []servicetoken.Grant{
		{Actions: []string{"fleet/*"}},
	})
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	cleanup := func() {
		cancel()
		waitGroup.Wait()
	}
	return client, cleanup
}

// testServerNoGrants is like testServer but the token carries no
// grants — all grant checks should fail.
func testServerNoGrants(t *testing.T, fc *FleetController) (*service.ServiceClient, func()) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(testClockEpoch)
	fc.clock = testClock
	fc.startedAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "fleet",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "fleet.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	fc.logger = logger
	fc.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintToken(t, privateKey, "agent/unauthorized", nil)
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	cleanup := func() {
		cancel()
		waitGroup.Wait()
	}
	return client, cleanup
}

// mintToken creates a signed test token with specific grants. The
// subject is an account localpart (e.g. "agent/tester"), expanded
// to a full user ID under the standard test fleet.
// Timestamps are relative to testClockEpoch.
func mintToken(t *testing.T, privateKey ed25519.PrivateKey, subject string, grants []servicetoken.Grant) []byte {
	t.Helper()
	fleet := testFleet(t)
	subjectEntity := testEntity(t, subject)
	machine, err := ref.NewMachine(fleet, "test")
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	token := &servicetoken.Token{
		Subject:   subjectEntity.UserID(),
		Machine:   machine,
		Audience:  "fleet",
		Grants:    grants,
		ID:        "test-token",
		IssuedAt:  testClockEpoch.Add(-5 * time.Minute).Unix(),
		ExpiresAt: testClockEpoch.Add(5 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tokenBytes
}

// waitForSocket polls until the socket file exists.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for range 500 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond) //nolint:realclock — filesystem polling for socket existence
	}
	t.Fatalf("socket %s did not appear within timeout", path)
}

// requireServiceError asserts that err is a *service.ServiceError.
func requireServiceError(t *testing.T, err error) *service.ServiceError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var serviceErr *service.ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	return serviceErr
}

// sampleFleetController creates a FleetController populated with test
// data for socket API tests: two machines, two services, one definition.
// Uses a fakeConfigStore so mutation handlers (place/unplace) can read
// and write machine configs without a real homeserver.
func sampleFleetController(t *testing.T) *FleetController {
	t.Helper()
	fc := newTestFleetController(t)
	fc.principalName = "service/fleet/prod"
	fc.configStore = newFakeConfigStore()

	// Machine 1: workstation with GPU, one fleet-managed assignment.
	fc.machines["machine/workstation"] = &machineState{
		info: &schema.MachineInfo{
			Principal:     "@machine/workstation:bureau.local",
			Hostname:      "workstation",
			MemoryTotalMB: 65536,
			GPUs: []schema.GPUInfo{
				{Vendor: "NVIDIA", ModelName: "RTX 4090", VRAMTotalBytes: 25769803776},
			},
			Labels: map[string]string{"gpu": "rtx4090"},
		},
		status: &schema.MachineStatus{
			Principal:    "@machine/workstation:bureau.local",
			CPUPercent:   42,
			MemoryUsedMB: 32000,
		},
		assignments: map[string]*schema.PrincipalAssignment{
			"service/stt/whisper": {
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
		configRoomID: mustRoomID("!config-ws:local"),
	}
	fc.configRooms["machine/workstation"] = mustRoomID("!config-ws:local")

	// Machine 2: server with no GPU, no assignments.
	fc.machines["machine/server"] = &machineState{
		info: &schema.MachineInfo{
			Principal:     "@machine/server:bureau.local",
			Hostname:      "server",
			MemoryTotalMB: 131072,
			Labels:        map[string]string{},
		},
		status: &schema.MachineStatus{
			Principal:    "@machine/server:bureau.local",
			CPUPercent:   15,
			MemoryUsedMB: 16000,
		},
		assignments:  make(map[string]*schema.PrincipalAssignment),
		configRoomID: mustRoomID("!config-srv:local"),
	}
	fc.configRooms["machine/server"] = mustRoomID("!config-srv:local")

	// Fleet service 1: whisper STT.
	fc.services["service/stt/whisper"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: schema.ReplicaSpec{Min: 1},
			Failover: "migrate",
			Priority: 10,
		},
		instances: map[string]*schema.PrincipalAssignment{
			"machine/workstation": {
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
			},
		},
	}

	// Fleet service 2: worker with no instances.
	fc.services["service/batch/worker"] = &fleetServiceState{
		definition: &schema.FleetServiceContent{
			Template: "bureau/template:worker",
			Replicas: schema.ReplicaSpec{Min: 2},
			Failover: "alert",
			Priority: 50,
		},
		instances: make(map[string]*schema.PrincipalAssignment),
	}

	// One machine definition.
	fc.definitions["gpu-cloud-pool"] = &schema.MachineDefinitionContent{
		Provider: "gcloud",
		Resources: schema.MachineResources{
			CPUCores: 8,
			MemoryMB: 32768,
		},
	}

	return fc
}

// --- Status tests ---

func TestStatusUnauthenticated(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	// Status should work with an authenticated client too.
	var response statusResponse
	if err := client.Call(context.Background(), "status", nil, &response); err != nil {
		t.Fatalf("status call failed: %v", err)
	}

	// startedAt is 2026-01-01 12:00, clock epoch is 2026-01-15 12:00.
	// That's 14 days = 1209600 seconds.
	if response.UptimeSeconds != 1209600 {
		t.Errorf("uptime = %d, want 1209600", response.UptimeSeconds)
	}
}

func TestStatusWithoutToken(t *testing.T) {
	fc := sampleFleetController(t)

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(testClockEpoch)
	fc.clock = testClock
	fc.startedAt = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "fleet",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "fleet.sock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)
	fc.logger = logger
	fc.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// No token — unauthenticated client.
	unauthClient := service.NewServiceClientFromToken(socketPath, nil)
	var response statusResponse
	if err := unauthClient.Call(context.Background(), "status", nil, &response); err != nil {
		t.Fatalf("unauthenticated status should work: %v", err)
	}
	if response.UptimeSeconds != 1209600 {
		t.Errorf("uptime = %d, want 1209600", response.UptimeSeconds)
	}
}

// --- Info tests ---

func TestInfoReturnsModelCounts(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response infoResponse
	if err := client.Call(context.Background(), "info", nil, &response); err != nil {
		t.Fatalf("info call failed: %v", err)
	}

	if response.Machines != 2 {
		t.Errorf("machines = %d, want 2", response.Machines)
	}
	if response.Services != 2 {
		t.Errorf("services = %d, want 2", response.Services)
	}
	if response.Definitions != 1 {
		t.Errorf("definitions = %d, want 1", response.Definitions)
	}
	if response.ConfigRooms != 2 {
		t.Errorf("config_rooms = %d, want 2", response.ConfigRooms)
	}
	if response.UptimeSeconds != 1209600 {
		t.Errorf("uptime = %d, want 1209600", response.UptimeSeconds)
	}
}

func TestInfoDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response infoResponse
	err := client.Call(context.Background(), "info", nil, &response)
	requireServiceError(t, err)
}

// --- List machines tests ---

func TestListMachinesReturnsSorted(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response listMachinesResponse
	if err := client.Call(context.Background(), "list-machines", nil, &response); err != nil {
		t.Fatalf("list-machines call failed: %v", err)
	}

	if len(response.Machines) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(response.Machines))
	}

	// Should be sorted by localpart.
	if response.Machines[0].Localpart != "machine/server" {
		t.Errorf("first machine = %q, want machine/server", response.Machines[0].Localpart)
	}
	if response.Machines[1].Localpart != "machine/workstation" {
		t.Errorf("second machine = %q, want machine/workstation", response.Machines[1].Localpart)
	}

	// Verify workstation details.
	workstation := response.Machines[1]
	if workstation.Hostname != "workstation" {
		t.Errorf("hostname = %q, want workstation", workstation.Hostname)
	}
	if workstation.CPUPercent != 42 {
		t.Errorf("cpu_percent = %d, want 42", workstation.CPUPercent)
	}
	if workstation.MemoryUsedMB != 32000 {
		t.Errorf("memory_used_mb = %d, want 32000", workstation.MemoryUsedMB)
	}
	if workstation.MemoryTotalMB != 65536 {
		t.Errorf("memory_total_mb = %d, want 65536", workstation.MemoryTotalMB)
	}
	if workstation.GPUCount != 1 {
		t.Errorf("gpu_count = %d, want 1", workstation.GPUCount)
	}
	if workstation.Assignments != 1 {
		t.Errorf("assignments = %d, want 1", workstation.Assignments)
	}
}

func TestListMachinesIncludesPartialMachines(t *testing.T) {
	fc := sampleFleetController(t)
	// Add a machine with nil info.
	fc.machines["machine/booting"] = &machineState{
		status:      &schema.MachineStatus{CPUPercent: 5},
		assignments: make(map[string]*schema.PrincipalAssignment),
	}

	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response listMachinesResponse
	if err := client.Call(context.Background(), "list-machines", nil, &response); err != nil {
		t.Fatalf("list-machines call failed: %v", err)
	}

	if len(response.Machines) != 3 {
		t.Fatalf("expected 3 machines (including partial), got %d", len(response.Machines))
	}

	// The partial machine should have zero-value fields for info.
	booting := response.Machines[0] // "machine/booting" sorts first
	if booting.Localpart != "machine/booting" {
		t.Fatalf("expected machine/booting first, got %s", booting.Localpart)
	}
	if booting.Hostname != "" {
		t.Errorf("hostname for partial machine = %q, want empty", booting.Hostname)
	}
	if booting.MemoryTotalMB != 0 {
		t.Errorf("memory_total_mb for partial machine = %d, want 0", booting.MemoryTotalMB)
	}
	// Status is present, so CPUPercent should be populated.
	if booting.CPUPercent != 5 {
		t.Errorf("cpu_percent for partial machine = %d, want 5", booting.CPUPercent)
	}
}

func TestListMachinesDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response listMachinesResponse
	err := client.Call(context.Background(), "list-machines", nil, &response)
	requireServiceError(t, err)
}

// --- List services tests ---

func TestListServicesReturnsSorted(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response listServicesResponse
	if err := client.Call(context.Background(), "list-services", nil, &response); err != nil {
		t.Fatalf("list-services call failed: %v", err)
	}

	if len(response.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(response.Services))
	}

	// Should be sorted by localpart.
	if response.Services[0].Localpart != "service/batch/worker" {
		t.Errorf("first service = %q, want service/batch/worker", response.Services[0].Localpart)
	}
	if response.Services[1].Localpart != "service/stt/whisper" {
		t.Errorf("second service = %q, want service/stt/whisper", response.Services[1].Localpart)
	}
}

func TestListServicesIncludesInstanceCount(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response listServicesResponse
	if err := client.Call(context.Background(), "list-services", nil, &response); err != nil {
		t.Fatalf("list-services call failed: %v", err)
	}

	// whisper has 1 instance, worker has 0.
	for _, svc := range response.Services {
		switch svc.Localpart {
		case "service/stt/whisper":
			if svc.Instances != 1 {
				t.Errorf("whisper instances = %d, want 1", svc.Instances)
			}
			if svc.Template != "bureau/template:whisper-stt" {
				t.Errorf("whisper template = %q, want bureau/template:whisper-stt", svc.Template)
			}
			if svc.Replicas != 1 {
				t.Errorf("whisper replicas = %d, want 1", svc.Replicas)
			}
			if svc.Failover != "migrate" {
				t.Errorf("whisper failover = %q, want migrate", svc.Failover)
			}
			if svc.Priority != 10 {
				t.Errorf("whisper priority = %d, want 10", svc.Priority)
			}
		case "service/batch/worker":
			if svc.Instances != 0 {
				t.Errorf("worker instances = %d, want 0", svc.Instances)
			}
		}
	}
}

func TestListServicesDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response listServicesResponse
	err := client.Call(context.Background(), "list-services", nil, &response)
	requireServiceError(t, err)
}

// --- Show machine tests ---

func TestShowMachineReturnsDetail(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showMachineResponse
	fields := map[string]any{"machine": "machine/workstation"}
	if err := client.Call(context.Background(), "show-machine", fields, &response); err != nil {
		t.Fatalf("show-machine call failed: %v", err)
	}

	if response.Localpart != "machine/workstation" {
		t.Errorf("localpart = %q, want machine/workstation", response.Localpart)
	}
	if response.Info == nil {
		t.Fatal("info should not be nil")
	}
	if response.Info.Hostname != "workstation" {
		t.Errorf("hostname = %q, want workstation", response.Info.Hostname)
	}
	if response.Status == nil {
		t.Fatal("status should not be nil")
	}
	if response.Status.CPUPercent != 42 {
		t.Errorf("cpu_percent = %d, want 42", response.Status.CPUPercent)
	}
	if len(response.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(response.Assignments))
	}
	if response.Assignments[0].Principal.AccountLocalpart() != "service/stt/whisper" {
		t.Errorf("assignment localpart = %q, want service/stt/whisper", response.Assignments[0].Principal.AccountLocalpart())
	}
	if response.ConfigRoomID != "!config-ws:local" {
		t.Errorf("config_room_id = %q, want !config-ws:local", response.ConfigRoomID)
	}
}

func TestShowMachineNotFound(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showMachineResponse
	fields := map[string]any{"machine": "machine/nonexistent"}
	err := client.Call(context.Background(), "show-machine", fields, &response)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Message == "" {
		t.Error("expected non-empty error message")
	}
}

func TestShowMachineMissingField(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showMachineResponse
	err := client.Call(context.Background(), "show-machine", nil, &response)
	requireServiceError(t, err)
}

func TestShowMachineDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response showMachineResponse
	fields := map[string]any{"machine": "machine/workstation"}
	err := client.Call(context.Background(), "show-machine", fields, &response)
	requireServiceError(t, err)
}

// --- Show service tests ---

func TestShowServiceReturnsDetail(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showServiceResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	if err := client.Call(context.Background(), "show-service", fields, &response); err != nil {
		t.Fatalf("show-service call failed: %v", err)
	}

	if response.Localpart != "service/stt/whisper" {
		t.Errorf("localpart = %q, want service/stt/whisper", response.Localpart)
	}
	if response.Definition == nil {
		t.Fatal("definition should not be nil")
	}
	if response.Definition.Template != "bureau/template:whisper-stt" {
		t.Errorf("template = %q, want bureau/template:whisper-stt", response.Definition.Template)
	}
	if len(response.Instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(response.Instances))
	}
	if response.Instances[0].Machine != "machine/workstation" {
		t.Errorf("instance machine = %q, want machine/workstation", response.Instances[0].Machine)
	}
}

func TestShowServiceNotFound(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showServiceResponse
	fields := map[string]any{"service": "service/nonexistent"}
	err := client.Call(context.Background(), "show-service", fields, &response)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Message == "" {
		t.Error("expected non-empty error message")
	}
}

func TestShowServiceMissingField(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response showServiceResponse
	err := client.Call(context.Background(), "show-service", nil, &response)
	requireServiceError(t, err)
}

func TestShowServiceDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetController(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response showServiceResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	err := client.Call(context.Background(), "show-service", fields, &response)
	requireServiceError(t, err)
}

// --- Place tests ---

// sampleFleetControllerForMutation creates a fleet controller with
// seeded config store data suitable for place/unplace tests. The
// machines have configs in the fake store that the mutation handlers
// will read and write.
func sampleFleetControllerForMutation(t *testing.T) *FleetController {
	t.Helper()
	fc := sampleFleetController(t)
	store := fc.configStore.(*fakeConfigStore)

	// Seed the workstation config with its current assignment.
	store.seedConfig("!config-ws:local", "machine/workstation", &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, "service/stt/whisper"),
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
			},
		},
	})

	// Server config is empty (no assignments).
	store.seedConfig("!config-srv:local", "machine/server", &schema.MachineConfig{})

	return fc
}

func TestPlaceAutoSelectsMachine(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	// Place worker service — should auto-select a machine.
	var response placeResponse
	fields := map[string]any{"service": "service/batch/worker"}
	if err := client.Call(context.Background(), "place", fields, &response); err != nil {
		t.Fatalf("place call failed: %v", err)
	}

	if response.Service != "service/batch/worker" {
		t.Errorf("service = %q, want service/batch/worker", response.Service)
	}
	if response.Machine == "" {
		t.Error("machine should be set after auto-selection")
	}
	if response.Score < 0 {
		t.Errorf("score should be non-negative for auto-selected machine, got %d", response.Score)
	}

	// Verify the service now has an instance.
	var showResponse showServiceResponse
	showFields := map[string]any{"service": "service/batch/worker"}
	if err := client.Call(context.Background(), "show-service", showFields, &showResponse); err != nil {
		t.Fatalf("show-service call failed: %v", err)
	}
	if len(showResponse.Instances) != 1 {
		t.Errorf("expected 1 instance after place, got %d", len(showResponse.Instances))
	}
}

func TestPlaceManualMachine(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response placeResponse
	fields := map[string]any{
		"service": "service/batch/worker",
		"machine": "machine/server",
	}
	if err := client.Call(context.Background(), "place", fields, &response); err != nil {
		t.Fatalf("place call failed: %v", err)
	}

	if response.Machine != "machine/server" {
		t.Errorf("machine = %q, want machine/server", response.Machine)
	}
	// Manual placement doesn't run scoring, so score should be -1.
	if response.Score != -1 {
		t.Errorf("score for manual placement = %d, want -1", response.Score)
	}
}

func TestPlaceNoEligibleMachine(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)

	// Place whisper on both machines so no eligible machines remain.
	// (workstation already has it via sampleFleetController)
	fc.machines["machine/server"].assignments["service/stt/whisper"] = &schema.PrincipalAssignment{
		Principal: testEntity(t, "service/stt/whisper"),
		Labels:    map[string]string{"fleet_managed": "service/fleet/prod"},
	}
	fc.services["service/stt/whisper"].instances["machine/server"] = fc.machines["machine/server"].assignments["service/stt/whisper"]

	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response placeResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	err := client.Call(context.Background(), "place", fields, &response)
	requireServiceError(t, err)
}

func TestPlaceMissingServiceField(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response placeResponse
	err := client.Call(context.Background(), "place", nil, &response)
	requireServiceError(t, err)
}

func TestPlaceDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response placeResponse
	fields := map[string]any{"service": "service/batch/worker"}
	err := client.Call(context.Background(), "place", fields, &response)
	requireServiceError(t, err)
}

// --- Unplace tests ---

func TestUnplaceRemovesAssignment(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response unplaceResponse
	fields := map[string]any{
		"service": "service/stt/whisper",
		"machine": "machine/workstation",
	}
	if err := client.Call(context.Background(), "unplace", fields, &response); err != nil {
		t.Fatalf("unplace call failed: %v", err)
	}

	if response.Service != "service/stt/whisper" {
		t.Errorf("service = %q, want service/stt/whisper", response.Service)
	}
	if response.Machine != "machine/workstation" {
		t.Errorf("machine = %q, want machine/workstation", response.Machine)
	}

	// Verify the service now has 0 instances.
	var showResponse showServiceResponse
	showFields := map[string]any{"service": "service/stt/whisper"}
	if err := client.Call(context.Background(), "show-service", showFields, &showResponse); err != nil {
		t.Fatalf("show-service call failed: %v", err)
	}
	if len(showResponse.Instances) != 0 {
		t.Errorf("expected 0 instances after unplace, got %d", len(showResponse.Instances))
	}

	// Verify machine has 0 assignments.
	var machineResponse showMachineResponse
	machineFields := map[string]any{"machine": "machine/workstation"}
	if err := client.Call(context.Background(), "show-machine", machineFields, &machineResponse); err != nil {
		t.Fatalf("show-machine call failed: %v", err)
	}
	if len(machineResponse.Assignments) != 0 {
		t.Errorf("expected 0 assignments after unplace, got %d", len(machineResponse.Assignments))
	}
}

func TestUnplaceNotPlacedReturnsError(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response unplaceResponse
	fields := map[string]any{
		"service": "service/batch/worker",
		"machine": "machine/workstation",
	}
	err := client.Call(context.Background(), "unplace", fields, &response)
	requireServiceError(t, err)
}

func TestUnplaceMissingFields(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	// Missing machine.
	var response unplaceResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	err := client.Call(context.Background(), "unplace", fields, &response)
	requireServiceError(t, err)

	// Missing service.
	fields2 := map[string]any{"machine": "machine/workstation"}
	err2 := client.Call(context.Background(), "unplace", fields2, &response)
	requireServiceError(t, err2)
}

func TestUnplaceDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response unplaceResponse
	fields := map[string]any{
		"service": "service/stt/whisper",
		"machine": "machine/workstation",
	}
	err := client.Call(context.Background(), "unplace", fields, &response)
	requireServiceError(t, err)
}

// --- Plan tests ---

func TestPlanReturnsScores(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response planResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	if err := client.Call(context.Background(), "plan", fields, &response); err != nil {
		t.Fatalf("plan call failed: %v", err)
	}

	if response.Service != "service/stt/whisper" {
		t.Errorf("service = %q, want service/stt/whisper", response.Service)
	}

	// Should have candidates (at least the machines with GPU labels).
	if len(response.Candidates) == 0 {
		t.Error("expected at least one candidate")
	}

	// All candidates should have non-negative scores.
	for _, candidate := range response.Candidates {
		if candidate.Score < 0 {
			t.Errorf("candidate %q has negative score %d", candidate.Machine, candidate.Score)
		}
	}

	// Current machines should list where whisper is placed.
	if len(response.CurrentMachines) != 1 {
		t.Fatalf("expected 1 current machine, got %d", len(response.CurrentMachines))
	}
	if response.CurrentMachines[0] != "machine/workstation" {
		t.Errorf("current machine = %q, want machine/workstation", response.CurrentMachines[0])
	}
}

func TestPlanServiceNotFound(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response planResponse
	fields := map[string]any{"service": "service/nonexistent"}
	err := client.Call(context.Background(), "plan", fields, &response)
	requireServiceError(t, err)
}

func TestPlanMissingField(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServer(t, fc)
	defer cleanup()

	var response planResponse
	err := client.Call(context.Background(), "plan", nil, &response)
	requireServiceError(t, err)
}

func TestPlanDeniedWithoutGrant(t *testing.T) {
	fc := sampleFleetControllerForMutation(t)
	client, cleanup := testServerNoGrants(t, fc)
	defer cleanup()

	var response planResponse
	fields := map[string]any{"service": "service/stt/whisper"}
	err := client.Call(context.Background(), "plan", fields, &response)
	requireServiceError(t, err)
}
