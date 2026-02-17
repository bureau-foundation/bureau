// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Fleet controller response types ---
//
// These mirror the CBOR response structs in cmd/bureau-fleet-controller/socket.go.
// Defined here because the fleet controller is a separate binary — there is
// no importable Go package.

type fleetStatusResponse struct {
	UptimeSeconds int `json:"uptime_seconds"`
}

type fleetInfoResponse struct {
	UptimeSeconds int `json:"uptime_seconds"`
	Machines      int `json:"machines"`
	Services      int `json:"services"`
	Definitions   int `json:"definitions"`
	ConfigRooms   int `json:"config_rooms"`
}

type fleetMachineSummary struct {
	Localpart     string            `json:"localpart"`
	Hostname      string            `json:"hostname"`
	CPUPercent    int               `json:"cpu_percent"`
	MemoryUsedMB  int               `json:"memory_used_mb"`
	MemoryTotalMB int               `json:"memory_total_mb"`
	GPUCount      int               `json:"gpu_count"`
	Labels        map[string]string `json:"labels"`
	Assignments   int               `json:"assignments"`
	ConfigRoomID  string            `json:"config_room_id"`
}

type fleetListMachinesResponse struct {
	Machines []fleetMachineSummary `json:"machines"`
}

type fleetServiceSummary struct {
	Localpart string `json:"localpart"`
	Template  string `json:"template"`
	Replicas  int    `json:"replicas_min"`
	Instances int    `json:"instances"`
	Failover  string `json:"failover"`
	Priority  int    `json:"priority"`
}

type fleetListServicesResponse struct {
	Services []fleetServiceSummary `json:"services"`
}

type fleetShowMachineResponse struct {
	Localpart    string                       `json:"localpart"`
	Info         *schema.MachineInfo          `json:"info"`
	Status       *schema.MachineStatus        `json:"status"`
	Assignments  []schema.PrincipalAssignment `json:"assignments"`
	ConfigRoomID string                       `json:"config_room_id"`
}

type fleetServiceInstance struct {
	Machine    string                      `json:"machine"`
	Assignment *schema.PrincipalAssignment `json:"assignment"`
}

type fleetShowServiceResponse struct {
	Localpart  string                      `json:"localpart"`
	Definition *schema.FleetServiceContent `json:"definition"`
	Instances  []fleetServiceInstance      `json:"instances"`
}

type fleetPlaceResponse struct {
	Service string `json:"service"`
	Machine string `json:"machine"`
	Score   int    `json:"score"`
}

type fleetUnplaceResponse struct {
	Service string `json:"service"`
	Machine string `json:"machine"`
}

type fleetPlanCandidate struct {
	Machine string `json:"machine"`
	Score   int    `json:"score"`
}

type fleetPlanResponse struct {
	Service         string               `json:"service"`
	Candidates      []fleetPlanCandidate `json:"candidates"`
	CurrentMachines []string             `json:"current_machines"`
}

// --- Fleet controller test infrastructure ---

// fleetController holds the runtime state of a fleet controller started
// for integration testing. The SocketPath is the CBOR API endpoint.
type fleetController struct {
	PrincipalName string
	UserID        string
	SocketPath    string
	StateDir      string
}

// startFleetController starts a fleet controller binary as an externally-
// managed service (same pattern as the ticket service). It registers the
// Matrix account, writes session credentials, invites the service principal
// to the required rooms (fleet, machine, service, system), and starts the
// binary. Returns after the socket file appears on disk.
func startFleetController(t *testing.T, admin *messaging.Session, machine *testMachine, controllerName, fleetRoomID string) *fleetController {
	t.Helper()

	ctx := t.Context()

	// Register the fleet controller's Matrix account.
	account := registerPrincipal(t, controllerName, "fleet-controller-password")

	// Write session.json for the fleet controller.
	stateDir := t.TempDir()
	sessionData := service.SessionData{
		HomeserverURL: testHomeserverURL,
		UserID:        account.UserID,
		AccessToken:   account.Token,
	}
	sessionJSON, err := json.Marshal(sessionData)
	if err != nil {
		t.Fatalf("marshal fleet controller session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("write fleet controller session.json: %v", err)
	}

	// Resolve and invite to required rooms.
	systemRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasSystem, testServerName))
	if err != nil {
		t.Fatalf("resolve system room: %v", err)
	}
	serviceRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasService, testServerName))
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}
	machineRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasMachine, testServerName))
	if err != nil {
		t.Fatalf("resolve machine room: %v", err)
	}

	for _, invite := range []struct {
		roomID string
		name   string
	}{
		{systemRoomID, "system"},
		{serviceRoomID, "service"},
		{fleetRoomID, "fleet"},
		{machineRoomID, "machine"},
	} {
		if err := admin.InviteUser(ctx, invite.roomID, account.UserID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite fleet controller to %s room: %v", invite.name, err)
			}
		}
	}

	// The fleet controller needs access to each machine's config room
	// to read/write MachineConfig for place/unplace. In production, the
	// daemon detects fleet controllers in the fleet room and invites
	// them to the config room with PL 50. In this test, the daemon's
	// sync loop handles this automatically after the fleet controller
	// joins the fleet room.

	// Start the fleet controller binary.
	socketPath := principal.RunDirSocketPath(machine.RunDir, controllerName)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatalf("create fleet controller socket directory: %v", err)
	}

	fleetBinary := resolvedBinary(t, "FLEET_CONTROLLER_BINARY")
	startProcess(t, "fleet-controller", fleetBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--principal-name", controllerName,
		"--server-name", testServerName,
		"--fleet-room", fleetRoomID,
		"--run-dir", machine.RunDir,
		"--state-dir", stateDir,
	)

	waitForFile(t, socketPath)

	return &fleetController{
		PrincipalName: controllerName,
		UserID:        account.UserID,
		SocketPath:    socketPath,
		StateDir:      stateDir,
	}
}

// fleetClient creates a service.ServiceClient for the fleet controller.
// If signingKey is non-nil, the client authenticates with a minted
// service token carrying the given grants. If signingKey is nil, the
// client is unauthenticated (for status checks).
func fleetClient(t *testing.T, fc *fleetController, signingKey ed25519.PrivateKey, grants []servicetoken.Grant) *service.ServiceClient {
	t.Helper()

	if signingKey == nil {
		return service.NewServiceClientFromToken(fc.SocketPath, nil)
	}

	// Use a fixed far-future timestamp for test tokens. The fleet
	// controller validates expiry against its own clock, and integration
	// tests always run well within this window.
	const testIssuedAt = 1735689600  // 2025-01-01T00:00:00Z
	const testExpiresAt = 4070908800 // 2099-01-01T00:00:00Z
	token := &servicetoken.Token{
		Subject:   "test/fleet-operator",
		Machine:   "machine/test",
		Audience:  "fleet",
		Grants:    grants,
		ID:        "test-fleet-token",
		IssuedAt:  testIssuedAt,
		ExpiresAt: testExpiresAt,
	}

	tokenBytes, err := servicetoken.Mint(signingKey, token)
	if err != nil {
		t.Fatalf("mint fleet service token: %v", err)
	}

	return service.NewServiceClientFromToken(fc.SocketPath, tokenBytes)
}

// fleetAllGrants returns grants that authorize all fleet API actions.
func fleetAllGrants() []servicetoken.Grant {
	return []servicetoken.Grant{
		{Actions: []string{"fleet/**"}},
	}
}

// publishFleetService publishes a FleetServiceContent state event to
// the fleet room. The state key is the service localpart.
func publishFleetService(t *testing.T, admin *messaging.Session, fleetRoomID, serviceLocalpart string, definition schema.FleetServiceContent) {
	t.Helper()

	_, err := admin.SendStateEvent(t.Context(), fleetRoomID, schema.EventTypeFleetService, serviceLocalpart, definition)
	if err != nil {
		t.Fatalf("publish fleet service %s: %v", serviceLocalpart, err)
	}
}

// loadDaemonSigningKey reads the daemon's token signing private key from
// the machine's state directory. The daemon generates this keypair at
// startup and publishes the public key to #bureau/system.
func loadDaemonSigningKey(t *testing.T, machine *testMachine) ed25519.PrivateKey {
	t.Helper()

	_, privateKey, err := servicetoken.LoadKeypair(machine.StateDir)
	if err != nil {
		t.Fatalf("load daemon signing keypair from %s: %v", machine.StateDir, err)
	}

	return privateKey
}

// inviteFleetControllerToConfigRoom invites a fleet controller to a
// machine's config room so it can read/write MachineConfig for placement.
func inviteFleetControllerToConfigRoom(t *testing.T, admin *messaging.Session, fc *fleetController, machine *testMachine) {
	t.Helper()

	if machine.ConfigRoomID == "" {
		t.Fatal("machine has no config room ID — was startMachine called?")
	}

	userID := principal.MatrixUserID(fc.PrincipalName, testServerName)
	if err := admin.InviteUser(t.Context(), machine.ConfigRoomID, userID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite fleet controller to config room %s: %v", machine.ConfigRoomID, err)
		}
	}
}

// assertFleetMachine calls the fleet controller's list-machines endpoint
// and asserts the given machine localpart is present. The fleet controller
// builds its fleet model before creating its service socket, so after
// startFleetController returns the machine is guaranteed to be in the model.
// If the machine is missing, it's a bug in the fleet controller startup
// sequence, not a timing issue.
func assertFleetMachine(t *testing.T, client *service.ServiceClient, machineLocalpart string) {
	t.Helper()

	var response fleetListMachinesResponse
	if err := client.Call(t.Context(), "list-machines", nil, &response); err != nil {
		t.Fatalf("list-machines: %v", err)
	}

	for _, machine := range response.Machines {
		if machine.Localpart == machineLocalpart {
			return
		}
	}

	t.Fatalf("machine %q not found in fleet controller model (model has %d machines)", machineLocalpart, len(response.Machines))
}

// waitForFleetConfigRoom waits for the fleet controller to discover and
// process a machine's config room. The fleet controller posts a
// MsgTypeFleetConfigRoomDiscovered notification to the fleet room after
// it joins and processes the MachineConfig state event. The sequence is:
//   - Daemon's sync detects the fleet controller in the fleet room
//   - Daemon invites the fleet controller to the config room (PL 50)
//   - Fleet controller's sync detects the invite and joins
//   - Fleet controller calls GetRoomState and processes MachineConfig
//   - Fleet controller emits a config room discovered notification
//
// The fleetWatch must be created on the fleet room BEFORE starting the
// fleet controller so the admin's sync checkpoint captures the
// notification event.
func waitForFleetConfigRoom(t *testing.T, fleetWatch *roomWatch, fc *fleetController, machineLocalpart string) {
	t.Helper()

	waitForNotification[schema.FleetConfigRoomDiscoveredMessage](
		t, fleetWatch, schema.MsgTypeFleetConfigRoomDiscovered, fc.UserID,
		func(m schema.FleetConfigRoomDiscoveredMessage) bool {
			return m.Machine == machineLocalpart
		},
		"fleet controller discovers config room for "+machineLocalpart,
	)
}

// waitForFleetService waits for the fleet controller to process a fleet
// service definition. The fleet controller posts a
// MsgTypeFleetServiceDiscovered notification to the fleet room after it
// processes a new FleetServiceContent state event from the fleet room.
//
// The fleetWatch must be created on the fleet room BEFORE publishing the
// service definition so the admin's sync checkpoint captures the
// notification event.
func waitForFleetService(t *testing.T, fleetWatch *roomWatch, fc *fleetController, serviceLocalpart string) {
	t.Helper()

	waitForNotification[schema.FleetServiceDiscoveredMessage](
		t, fleetWatch, schema.MsgTypeFleetServiceDiscovered, fc.UserID,
		func(m schema.FleetServiceDiscoveredMessage) bool {
			return m.Service == serviceLocalpart
		},
		"fleet controller discovers service "+serviceLocalpart,
	)
}

// --- Fleet controller integration tests ---

// TestFleetControllerLifecycle verifies the full fleet controller startup
// path: boots a machine (launcher + daemon), starts the fleet controller
// binary, and exercises every query API endpoint. This is the smoke test
// that proves the fleet controller can:
//   - Start up and complete initial /sync
//   - Discover machines from #bureau/machine state events
//   - Respond to unauthenticated status checks
//   - Authenticate and authorize service token requests
//   - Track fleet service definitions published to #bureau/fleet
//   - Return correct machine and service detail views
func TestFleetControllerLifecycle(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	// Boot a machine. The daemon publishes MachineInfo and MachineStatus
	// to #bureau/machine, and creates the per-machine config room.
	// startMachine blocks until the first status heartbeat arrives, so
	// the fleet controller's initial /sync will see this machine.
	fleetRoomID := defaultFleetRoomID(t)

	machine := newTestMachine(t, "machine/fleet-lifecycle")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Start the fleet controller. It performs initial /sync before
	// opening the socket, so by the time we can call APIs the fleet
	// model should already contain the machine.
	controllerName := "service/fleet/lifecycle"
	fc := startFleetController(t, admin, machine, controllerName, fleetRoomID)

	// Load the daemon's token signing key. The daemon generated this
	// keypair at startup and published the public key to #bureau/system.
	// The fleet controller loaded that public key during startup to
	// verify incoming tokens.
	signingKey := loadDaemonSigningKey(t, machine)

	ctx := t.Context()

	// --- Sub-test: unauthenticated status ---
	t.Run("Status", func(t *testing.T) {
		unauthClient := fleetClient(t, fc, nil, nil)
		var status fleetStatusResponse
		if err := unauthClient.Call(ctx, "status", nil, &status); err != nil {
			t.Fatalf("status: %v", err)
		}
		// The fleet controller just started — uptime should be small but
		// non-negative. We don't assert an exact value because test
		// execution time varies.
		if status.UptimeSeconds < 0 {
			t.Errorf("uptime = %d, want >= 0", status.UptimeSeconds)
		}
	})

	// --- Sub-test: authenticated info ---
	t.Run("Info", func(t *testing.T) {
		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
		var info fleetInfoResponse
		if err := authClient.Call(ctx, "info", nil, &info); err != nil {
			t.Fatalf("info: %v", err)
		}
		if info.Machines < 1 {
			t.Errorf("info.Machines = %d, want >= 1", info.Machines)
		}
		if info.UptimeSeconds < 0 {
			t.Errorf("info.UptimeSeconds = %d, want >= 0", info.UptimeSeconds)
		}
	})

	// --- Sub-test: list machines ---
	t.Run("ListMachines", func(t *testing.T) {
		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
		assertFleetMachine(t, authClient, machine.Name)

		var response fleetListMachinesResponse
		if err := authClient.Call(ctx, "list-machines", nil, &response); err != nil {
			t.Fatalf("list-machines: %v", err)
		}

		if len(response.Machines) == 0 {
			t.Fatal("list-machines returned 0 machines")
		}

		var found bool
		for _, summary := range response.Machines {
			if summary.Localpart == machine.Name {
				found = true
				// The daemon publishes MachineInfo with hostname from
				// hwinfo.Probe. In a test environment the hostname
				// should be non-empty.
				if summary.Hostname == "" {
					t.Error("machine hostname is empty (expected from MachineInfo)")
				}
				// MemoryTotalMB comes from MachineInfo. The test host
				// always has some memory.
				if summary.MemoryTotalMB == 0 {
					t.Error("machine MemoryTotalMB is 0 (expected from MachineInfo)")
				}
				break
			}
		}
		if !found {
			t.Errorf("machine %q not found in list-machines response", machine.Name)
		}
	})

	// --- Sub-test: show machine ---
	t.Run("ShowMachine", func(t *testing.T) {
		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())

		var response fleetShowMachineResponse
		if err := authClient.Call(ctx, "show-machine",
			map[string]any{"machine": machine.Name}, &response); err != nil {
			t.Fatalf("show-machine: %v", err)
		}

		if response.Localpart != machine.Name {
			t.Errorf("show-machine localpart = %q, want %q", response.Localpart, machine.Name)
		}
		if response.Info == nil {
			t.Error("show-machine Info is nil (expected MachineInfo from daemon)")
		}
		if response.Status == nil {
			t.Error("show-machine Status is nil (expected MachineStatus from daemon heartbeat)")
		}
	})

	// --- Sub-test: publish and discover a fleet service ---
	t.Run("ServiceDiscovery", func(t *testing.T) {
		serviceLocalpart := "service/stt/lifecycle"

		// Create a fleet room watch before publishing so we can
		// event-wait for the service discovered notification.
		fleetWatch := watchRoom(t, admin, fleetRoomID)

		publishFleetService(t, admin, fleetRoomID, serviceLocalpart, schema.FleetServiceContent{
			Template: "bureau/template:whisper-stt",
			Replicas: schema.ReplicaSpec{Min: 0},
			Failover: "migrate",
			Priority: 10,
		})

		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
		waitForFleetService(t, &fleetWatch, fc, serviceLocalpart)

		// Verify the service appears in list-services with correct fields.
		var listResponse fleetListServicesResponse
		if err := authClient.Call(ctx, "list-services", nil, &listResponse); err != nil {
			t.Fatalf("list-services: %v", err)
		}

		var found bool
		for _, svc := range listResponse.Services {
			if svc.Localpart == serviceLocalpart {
				found = true
				if svc.Template != "bureau/template:whisper-stt" {
					t.Errorf("service template = %q, want %q", svc.Template, "bureau/template:whisper-stt")
				}
				if svc.Replicas != 0 {
					t.Errorf("service replicas = %d, want 0", svc.Replicas)
				}
				if svc.Failover != "migrate" {
					t.Errorf("service failover = %q, want %q", svc.Failover, "migrate")
				}
				if svc.Priority != 10 {
					t.Errorf("service priority = %d, want 10", svc.Priority)
				}
				// No instances placed yet.
				if svc.Instances != 0 {
					t.Errorf("service instances = %d, want 0 (no placement performed)", svc.Instances)
				}
				break
			}
		}
		if !found {
			t.Errorf("service %q not found in list-services response", serviceLocalpart)
		}
	})

	// --- Sub-test: show service ---
	t.Run("ShowService", func(t *testing.T) {
		serviceLocalpart := "service/stt/lifecycle"

		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
		// The service was published in the ServiceDiscovery sub-test.
		// Since sub-tests run sequentially within the parent, the fleet
		// controller has already processed it.
		var response fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": serviceLocalpart}, &response); err != nil {
			t.Fatalf("show-service: %v", err)
		}

		if response.Localpart != serviceLocalpart {
			t.Errorf("show-service localpart = %q, want %q", response.Localpart, serviceLocalpart)
		}
		if response.Definition == nil {
			t.Fatal("show-service definition is nil")
		}
		if response.Definition.Template != "bureau/template:whisper-stt" {
			t.Errorf("definition template = %q, want %q",
				response.Definition.Template, "bureau/template:whisper-stt")
		}
		if len(response.Instances) != 0 {
			t.Errorf("show-service instances = %d, want 0 (no placement performed)",
				len(response.Instances))
		}
	})

	// --- Sub-test: plan (dry-run scoring) ---
	t.Run("Plan", func(t *testing.T) {
		serviceLocalpart := "service/stt/lifecycle"

		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
		var response fleetPlanResponse
		if err := authClient.Call(ctx, "plan",
			map[string]any{"service": serviceLocalpart}, &response); err != nil {
			t.Fatalf("plan: %v", err)
		}

		if response.Service != serviceLocalpart {
			t.Errorf("plan service = %q, want %q", response.Service, serviceLocalpart)
		}
		// The machine should appear as a candidate (it's the only one
		// and has no constraints to violate).
		if len(response.Candidates) == 0 {
			t.Fatal("plan returned 0 candidates, want >= 1")
		}
		var foundCandidate bool
		for _, candidate := range response.Candidates {
			if candidate.Machine == machine.Name {
				foundCandidate = true
				if candidate.Score <= 0 {
					t.Errorf("candidate score = %d, want > 0", candidate.Score)
				}
				break
			}
		}
		if !foundCandidate {
			t.Errorf("machine %q not found in plan candidates", machine.Name)
		}
		// No instances yet, so current_machines should be empty.
		if len(response.CurrentMachines) != 0 {
			t.Errorf("plan current_machines = %v, want empty", response.CurrentMachines)
		}
	})

	// --- Sub-test: machine health ---
	t.Run("MachineHealth", func(t *testing.T) {
		authClient := fleetClient(t, fc, signingKey, fleetAllGrants())

		var response struct {
			Machines []struct {
				Localpart        string `json:"localpart"`
				HealthState      string `json:"health_state"`
				LastHeartbeat    string `json:"last_heartbeat"`
				StalenessSeconds int    `json:"staleness_seconds"`
			} `json:"machines"`
		}
		if err := authClient.Call(ctx, "machine-health",
			map[string]any{"machine": machine.Name}, &response); err != nil {
			t.Fatalf("machine-health: %v", err)
		}

		if len(response.Machines) != 1 {
			t.Fatalf("machine-health returned %d entries, want 1", len(response.Machines))
		}
		entry := response.Machines[0]
		if entry.Localpart != machine.Name {
			t.Errorf("health localpart = %q, want %q", entry.Localpart, machine.Name)
		}
		// The machine published a heartbeat during startMachine, so the
		// fleet controller should have a recent heartbeat timestamp and
		// mark it as online.
		if entry.HealthState != "online" {
			t.Errorf("health state = %q, want %q", entry.HealthState, "online")
		}
		if entry.LastHeartbeat == "" {
			t.Error("last_heartbeat is empty (expected timestamp from MachineStatus)")
		}
	})
}

// TestFleetPlaceAndUnplace verifies the fleet controller's core mutation
// path end-to-end. This is the critical control loop:
//
//   - Publish a FleetServiceContent definition
//   - Call place with an explicit machine target
//   - Fleet controller writes a PrincipalAssignment to MachineConfig
//   - Daemon detects the config change via /sync and starts the proxy
//   - Verify show-service and show-machine reflect the placement
//   - Call unplace to remove the service
//   - Daemon detects the removal and tears down the proxy
//   - Verify the service instance count returns to zero
//
// This proves the full control loop: fleet controller → Matrix state event
// → daemon /sync → launcher IPC → proxy lifecycle.
func TestFleetPlaceAndUnplace(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := defaultFleetRoomID(t)

	// Boot a machine with proxy support. The daemon needs the proxy
	// binary to create sandboxes when the fleet controller places a
	// service.
	machine := newTestMachine(t, "machine/fleet-place")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Publish a test template so the daemon can create sandboxes when
	// the fleet controller places a service.
	templateRef := publishTestAgentTemplate(t, admin, machine, "fleet-place-agent")

	// Register a Matrix account for the service principal. The fleet
	// controller will reference this localpart when it creates a
	// PrincipalAssignment.
	serviceLocalpart := "service/stt/place-test"
	serviceAccount := registerPrincipal(t, serviceLocalpart, "fleet-place-test-password")

	// Push encrypted credentials for the service principal. The daemon
	// needs these to start the proxy — without credentials, the daemon
	// skips the principal with a warning.
	pushCredentials(t, admin, machine, serviceAccount)

	// The test agent sends a ready signal to the config room. For that
	// to work, the service account must be a member. The proxy's
	// default-deny grants block JoinRoom, so handle membership before
	// the sandbox starts: admin invites, principal joins via direct
	// session (outside the sandbox).
	if err := admin.InviteUser(t.Context(), machine.ConfigRoomID, serviceAccount.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite service to config room: %v", err)
		}
	}
	serviceSession := principalSession(t, serviceAccount)
	if _, err := serviceSession.JoinRoom(t.Context(), machine.ConfigRoomID); err != nil {
		t.Fatalf("service join config room: %v", err)
	}
	serviceSession.Close()

	// Push an empty MachineConfig so the fleet controller can discover
	// the config room once it joins. The fleet controller learns config
	// room IDs from MachineConfig state events inside config rooms.
	pushMachineConfig(t, admin, machine, deploymentConfig{})

	// Create a fleet room watch BEFORE starting the fleet controller.
	// The fleet controller posts config room discovered notifications
	// to the fleet room after it joins and processes each config room.
	configDiscoverWatch := watchRoom(t, admin, fleetRoomID)

	// Start the fleet controller. The daemon detects the fleet controller
	// joining the fleet room via /sync, invites it to the config room,
	// and grants it PL 50. The fleet controller then accepts the config
	// room invite and processes the MachineConfig event.
	controllerName := "service/fleet/place-test"
	fc := startFleetController(t, admin, machine, controllerName, fleetRoomID)

	signingKey := loadDaemonSigningKey(t, machine)
	authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
	ctx := t.Context()

	// Wait for the fleet controller to discover the config room. The
	// fleet controller emits a structured notification to the fleet
	// room after processing the MachineConfig state event.
	waitForFleetConfigRoom(t, &configDiscoverWatch, fc, machine.Name)

	// Create a fleet room watch before publishing the service so we
	// can event-wait for the service discovered notification.
	serviceDiscoverWatch := watchRoom(t, admin, fleetRoomID)

	// Publish a fleet service definition with Min=0 so the reconcile
	// loop does not auto-place it. This test exercises explicit place
	// and unplace calls; auto-placement is tested separately in
	// TestFleetAutoPlacement.
	publishFleetService(t, admin, fleetRoomID, serviceLocalpart, schema.FleetServiceContent{
		Template: templateRef,
		Replicas: schema.ReplicaSpec{Min: 0},
		Failover: "migrate",
		Priority: 10,
	})
	waitForFleetService(t, &serviceDiscoverWatch, fc, serviceLocalpart)

	// --- Place the service ---
	t.Run("Place", func(t *testing.T) {
		var placeResponse fleetPlaceResponse
		if err := authClient.Call(ctx, "place", map[string]any{
			"service": serviceLocalpart,
			"machine": machine.Name,
		}, &placeResponse); err != nil {
			t.Fatalf("place: %v", err)
		}

		if placeResponse.Service != serviceLocalpart {
			t.Errorf("place response service = %q, want %q", placeResponse.Service, serviceLocalpart)
		}
		if placeResponse.Machine != machine.Name {
			t.Errorf("place response machine = %q, want %q", placeResponse.Machine, machine.Name)
		}

		// Wait for the daemon to create the proxy. The daemon detects
		// the MachineConfig change (written by the fleet controller),
		// reads credentials, and creates a proxy sandbox.
		proxySocket := machine.PrincipalSocketPath(serviceLocalpart)
		waitForFile(t, proxySocket)

		// Verify the proxy serves the correct identity.
		proxyClient := proxyHTTPClient(proxySocket)
		whoamiUserID := proxyWhoami(t, proxyClient)
		if whoamiUserID != serviceAccount.UserID {
			t.Errorf("proxy whoami = %q, want %q", whoamiUserID, serviceAccount.UserID)
		}

		// Verify show-service reflects the placement.
		var showService fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": serviceLocalpart}, &showService); err != nil {
			t.Fatalf("show-service: %v", err)
		}
		if len(showService.Instances) != 1 {
			t.Fatalf("show-service instances = %d, want 1", len(showService.Instances))
		}
		if showService.Instances[0].Machine != machine.Name {
			t.Errorf("instance machine = %q, want %q",
				showService.Instances[0].Machine, machine.Name)
		}

		// Verify show-machine reflects the assignment.
		var showMachine fleetShowMachineResponse
		if err := authClient.Call(ctx, "show-machine",
			map[string]any{"machine": machine.Name}, &showMachine); err != nil {
			t.Fatalf("show-machine: %v", err)
		}
		var foundAssignment bool
		for _, assignment := range showMachine.Assignments {
			if assignment.Localpart == serviceLocalpart {
				foundAssignment = true
				if assignment.Labels["fleet_managed"] != controllerName {
					t.Errorf("assignment fleet_managed label = %q, want %q",
						assignment.Labels["fleet_managed"], controllerName)
				}
				break
			}
		}
		if !foundAssignment {
			t.Errorf("assignment for %q not found in show-machine response", serviceLocalpart)
		}
	})

	// --- Unplace the service ---
	t.Run("Unplace", func(t *testing.T) {
		proxySocket := machine.PrincipalSocketPath(serviceLocalpart)

		var unplaceResponse fleetUnplaceResponse
		if err := authClient.Call(ctx, "unplace", map[string]any{
			"service": serviceLocalpart,
			"machine": machine.Name,
		}, &unplaceResponse); err != nil {
			t.Fatalf("unplace: %v", err)
		}

		if unplaceResponse.Service != serviceLocalpart {
			t.Errorf("unplace response service = %q, want %q",
				unplaceResponse.Service, serviceLocalpart)
		}
		if unplaceResponse.Machine != machine.Name {
			t.Errorf("unplace response machine = %q, want %q",
				unplaceResponse.Machine, machine.Name)
		}

		// Wait for the daemon to tear down the proxy. The daemon
		// detects the MachineConfig change (fleet controller removed
		// the assignment) and destroys the sandbox.
		waitForFileGone(t, proxySocket)

		// Verify show-service reflects zero instances.
		var showService fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": serviceLocalpart}, &showService); err != nil {
			t.Fatalf("show-service: %v", err)
		}
		if len(showService.Instances) != 0 {
			t.Errorf("show-service instances = %d, want 0 after unplace",
				len(showService.Instances))
		}
	})
}

// TestFleetReconciliation verifies the fleet controller's autonomous
// reconciliation loop. Unlike TestFleetPlaceAndUnplace (which exercises
// explicit place/unplace API calls), this test publishes a service with
// Replicas.Min=2 and lets the reconcile loop place it on both machines
// without any explicit placement commands.
//
// This proves the complete autonomous path:
//   - Fleet controller detects a service with insufficient replicas
//   - reconcile() scores machines and calls place() for each deficit
//   - place() writes PrincipalAssignment to each machine's config room
//   - Each daemon detects the config change via /sync
//   - Each launcher creates a proxy sandbox
//   - Both proxy sockets appear (end-to-end proof)
func TestFleetReconciliation(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleetRoomID := defaultFleetRoomID(t)

	// Boot two machines with proxy support.
	machineA := newTestMachine(t, "machine/fleet-recon-a")
	machineB := newTestMachine(t, "machine/fleet-recon-b")
	options := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	}
	startMachine(t, admin, machineA, options)
	startMachine(t, admin, machineB, options)

	// Publish a test template and grant both machines access.
	templateRef := publishTestAgentTemplate(t, admin, machineA, "fleet-recon-agent")
	grantTemplateAccess(t, admin, machineB)

	// Register the service principal and push credentials to both
	// machines so either can create a sandbox after reconciliation.
	serviceLocalpart := "service/stt/reconcile-test"
	serviceAccount := registerPrincipal(t, serviceLocalpart, "fleet-recon-password")
	pushCredentials(t, admin, machineA, serviceAccount)
	pushCredentials(t, admin, machineB, serviceAccount)

	// Push empty MachineConfig to both machines so the fleet controller
	// can discover their config room IDs. The daemon does not publish
	// MachineConfig at startup — it reads existing config during its
	// reconcile loop. The fleet controller discovers config room IDs
	// from MachineConfig state events (processMachineConfigEvent).
	pushMachineConfig(t, admin, machineA, deploymentConfig{})
	pushMachineConfig(t, admin, machineB, deploymentConfig{})

	// Watch the fleet room BEFORE starting the fleet controller. The
	// fleet controller posts config room and service discovery
	// notifications to the fleet room. A single watch serves all
	// discovery waits because WaitForEvent preserves non-matching
	// events in its pending buffer.
	discoverWatch := watchRoom(t, admin, fleetRoomID)

	controllerName := "service/fleet/reconcile-test"
	fc := startFleetController(t, admin, machineA, controllerName, fleetRoomID)

	// Wait for the fleet controller to discover both config rooms.
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineA.Name)
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineB.Name)

	// Publish the fleet service with Min=2. The fleet controller
	// discovers the service via /sync, runs reconcile, detects a
	// deficit of 2, scores both machines, and calls place() for each.
	// AllowedMachines isolates this test from parallel tests sharing
	// the fleet room.
	publishFleetService(t, admin, fleetRoomID, serviceLocalpart, schema.FleetServiceContent{
		Template: templateRef,
		Replicas: schema.ReplicaSpec{Min: 2},
		Placement: schema.PlacementConstraints{
			AllowedMachines: []string{machineA.Name, machineB.Name},
		},
		Failover: "migrate",
		Priority: 10,
	})
	waitForFleetService(t, &discoverWatch, fc, serviceLocalpart)

	// Wait for proxy sockets on both machines. Proxy socket existence
	// proves the full chain: fleet controller reconcile → place() writes
	// MachineConfig → daemon /sync → launcher sandbox creation.
	proxySocketA := machineA.PrincipalSocketPath(serviceLocalpart)
	proxySocketB := machineB.PrincipalSocketPath(serviceLocalpart)
	waitForFile(t, proxySocketA)
	waitForFile(t, proxySocketB)

	// Verify both proxies serve the correct identity.
	proxyClientA := proxyHTTPClient(proxySocketA)
	if whoami := proxyWhoami(t, proxyClientA); whoami != serviceAccount.UserID {
		t.Errorf("machine A proxy whoami = %q, want %q", whoami, serviceAccount.UserID)
	}
	proxyClientB := proxyHTTPClient(proxySocketB)
	if whoami := proxyWhoami(t, proxyClientB); whoami != serviceAccount.UserID {
		t.Errorf("machine B proxy whoami = %q, want %q", whoami, serviceAccount.UserID)
	}

	// Verify the fleet model reflects both placements.
	signingKey := loadDaemonSigningKey(t, machineA)
	authClient := fleetClient(t, fc, signingKey, fleetAllGrants())
	ctx := t.Context()

	var showService fleetShowServiceResponse
	if err := authClient.Call(ctx, "show-service",
		map[string]any{"service": serviceLocalpart}, &showService); err != nil {
		t.Fatalf("show-service: %v", err)
	}
	if len(showService.Instances) != 2 {
		t.Fatalf("show-service instances = %d, want 2", len(showService.Instances))
	}
	instanceMachines := make(map[string]bool, len(showService.Instances))
	for _, instance := range showService.Instances {
		instanceMachines[instance.Machine] = true
	}
	if !instanceMachines[machineA.Name] {
		t.Errorf("show-service missing instance on %s", machineA.Name)
	}
	if !instanceMachines[machineB.Name] {
		t.Errorf("show-service missing instance on %s", machineB.Name)
	}

	// Verify show-machine for both machines: each should have a
	// PrincipalAssignment with the correct template, AutoStart=true,
	// and fleet_managed label matching the controller name.
	for _, machine := range []*testMachine{machineA, machineB} {
		var showMachine fleetShowMachineResponse
		if err := authClient.Call(ctx, "show-machine",
			map[string]any{"machine": machine.Name}, &showMachine); err != nil {
			t.Fatalf("show-machine %s: %v", machine.Name, err)
		}
		var foundAssignment bool
		for _, assignment := range showMachine.Assignments {
			if assignment.Localpart == serviceLocalpart {
				foundAssignment = true
				if assignment.Template != templateRef {
					t.Errorf("machine %s assignment template = %q, want %q",
						machine.Name, assignment.Template, templateRef)
				}
				if !assignment.AutoStart {
					t.Errorf("machine %s assignment AutoStart should be true",
						machine.Name)
				}
				if assignment.Labels["fleet_managed"] != controllerName {
					t.Errorf("machine %s fleet_managed label = %q, want %q",
						machine.Name, assignment.Labels["fleet_managed"], controllerName)
				}
				break
			}
		}
		if !foundAssignment {
			t.Errorf("machine %s: assignment for %q not found (got %d assignments)",
				machine.Name, serviceLocalpart, len(showMachine.Assignments))
		}
	}
}
