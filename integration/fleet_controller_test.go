// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/template"
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
	// to read/write MachineConfig for place/unplace. The admin grants
	// this explicitly: invite + PL 50.
	grantFleetControllerConfigAccess(t, admin, &fleetController{
		PrincipalName: controllerName,
		UserID:        account.UserID,
	}, machine)

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
// If token is non-nil, the client authenticates with the given
// daemon-minted service token. If token is nil, the client is
// unauthenticated (for status checks).
func fleetClient(t *testing.T, fc *fleetController, token []byte) *service.ServiceClient {
	t.Helper()
	return service.NewServiceClientFromToken(fc.SocketPath, token)
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

// grantFleetControllerConfigAccess invites a fleet controller to a machine's
// config room and grants it PL 50 so it can read/write MachineConfig for
// placement. The admin (PL 100) reads current power levels, adds the fleet
// controller at PL 50, and writes the updated power levels back.
func grantFleetControllerConfigAccess(t *testing.T, admin *messaging.Session, fc *fleetController, machine *testMachine) {
	t.Helper()
	ctx := t.Context()

	if machine.ConfigRoomID == "" {
		t.Fatal("machine has no config room ID — was startMachine called?")
	}

	userID := principal.MatrixUserID(fc.PrincipalName, testServerName)

	// Invite (idempotent — M_FORBIDDEN means already a member).
	if err := admin.InviteUser(ctx, machine.ConfigRoomID, userID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite fleet controller to config room %s: %v", machine.ConfigRoomID, err)
		}
	}

	// Read current power levels, add fleet controller at PL 50, write back.
	powerLevelJSON, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		schema.MatrixEventTypePowerLevels, "")
	if err != nil {
		t.Fatalf("read config room power levels: %v", err)
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(powerLevelJSON, &powerLevels); err != nil {
		t.Fatalf("unmarshal config room power levels: %v", err)
	}

	users, _ := powerLevels["users"].(map[string]any)
	if users == nil {
		users = make(map[string]any)
		powerLevels["users"] = users
	}
	users[userID] = 50

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.MatrixEventTypePowerLevels, "", powerLevels); err != nil {
		t.Fatalf("grant fleet controller PL 50 in config room: %v", err)
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

// publishFleetOperatorTemplate publishes a test agent template with
// RequiredServices: ["fleet"]. Principals deployed with this template
// get a daemon-minted fleet service token, which the test uses to
// authenticate fleet controller API calls. Returns the template
// reference string.
func publishFleetOperatorTemplate(t *testing.T, admin *messaging.Session, machine *testMachine) string {
	t.Helper()

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")
	grantTemplateAccess(t, admin, machine)

	ref, err := schema.ParseTemplateRef("bureau/template:fleet-operator")
	if err != nil {
		t.Fatalf("parse fleet operator template ref: %v", err)
	}

	_, err = template.Push(t.Context(), admin, ref, schema.TemplateContent{
		Description:      "Fleet operator with fleet service token",
		Command:          []string{testAgentBinary},
		RequiredServices: []string{"fleet"},
		Namespaces:       &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Source: testAgentBinary, Dest: testAgentBinary, Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME":                "/workspace",
			"TERM":                "xterm-256color",
			"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
			"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
			"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
		},
	}, testServerName)
	if err != nil {
		t.Fatalf("push fleet operator template: %v", err)
	}

	return ref.String()
}

// deployFleetOperator registers a principal, pushes credentials, deploys
// it with the given fleet grants, waits for the proxy socket (proving
// the daemon minted service tokens), and returns the daemon-minted fleet
// token. The principal uses the fleet operator template which has
// RequiredServices: ["fleet"], so the daemon mints a fleet token with
// the filtered grants.
func deployFleetOperator(t *testing.T, admin *messaging.Session, machine *testMachine, localpart, templateRef string, grants []string) []byte {
	t.Helper()

	account := registerPrincipal(t, localpart, localpart+"-password")
	pushCredentials(t, admin, machine, account)
	joinConfigRoom(t, admin, machine.ConfigRoomID, account)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  account,
			Template: templateRef,
			Authorization: &schema.AuthorizationPolicy{
				Grants: []schema.Grant{{Actions: grants}},
			},
		}},
	})

	waitForFile(t, machine.PrincipalSocketPath(localpart))
	return readDaemonMintedToken(t, machine, localpart, "fleet")
}

// publishFleetServiceBinding publishes an m.bureau.room_service state
// event for the "fleet" role in the machine's config room. The daemon
// reads this binding on demand when resolving RequiredServices during
// sandbox creation — the binding must exist before any principal with
// RequiredServices: ["fleet"] is deployed.
func publishFleetServiceBinding(t *testing.T, admin *messaging.Session, machine *testMachine, fc *fleetController) {
	t.Helper()

	_, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		schema.EventTypeRoomService, "fleet",
		schema.RoomServiceContent{Principal: fc.UserID})
	if err != nil {
		t.Fatalf("publish fleet service binding in config room: %v", err)
	}
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
	fleetRoomID := createFleetRoom(t, admin)

	machine := newTestMachine(t, "machine/fleet-lifecycle")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	operatorTemplateRef := publishFleetOperatorTemplate(t, admin, machine)

	// Start the fleet controller. It performs initial /sync before
	// opening the socket, so by the time we can call APIs the fleet
	// model should already contain the machine. Must start before
	// deploying the operator because the operator's template has
	// RequiredServices: ["fleet"] — the daemon needs the fleet
	// controller's socket and binding to resolve that dependency.
	controllerName := "service/fleet/lifecycle"
	fc := startFleetController(t, admin, machine, controllerName, fleetRoomID)
	publishFleetServiceBinding(t, admin, machine, fc)

	// Deploy a fleet operator to obtain a daemon-minted fleet token.
	// The daemon resolves the fleet service binding (published above),
	// mounts the fleet controller socket, and mints a fleet token
	// with the operator's grants.
	operatorToken := deployFleetOperator(t, admin, machine,
		"agent/fleet-lifecycle-operator", operatorTemplateRef, []string{"fleet/**"})

	ctx := t.Context()

	// --- Sub-test: unauthenticated status ---
	t.Run("Status", func(t *testing.T) {
		unauthClient := fleetClient(t, fc, nil)
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
		authClient := fleetClient(t, fc, operatorToken)
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
		authClient := fleetClient(t, fc, operatorToken)
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
		authClient := fleetClient(t, fc, operatorToken)

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

		authClient := fleetClient(t, fc, operatorToken)
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

		authClient := fleetClient(t, fc, operatorToken)
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

		authClient := fleetClient(t, fc, operatorToken)
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
		authClient := fleetClient(t, fc, operatorToken)

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

	fleetRoomID := createFleetRoom(t, admin)

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
	operatorTemplateRef := publishFleetOperatorTemplate(t, admin, machine)

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
	// the sandbox starts.
	joinConfigRoom(t, admin, machine.ConfigRoomID, serviceAccount)

	// Push an empty MachineConfig so the fleet controller can discover
	// the config room. The fleet controller discovers config room IDs
	// from MachineConfig state events.
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
	publishFleetServiceBinding(t, admin, machine, fc)

	// Deploy a fleet operator to obtain a daemon-minted fleet token.
	// The operator template has RequiredServices: ["fleet"], so the
	// daemon resolves the fleet service binding, mounts the fleet
	// controller socket, and mints a fleet token.
	operatorToken := deployFleetOperator(t, admin, machine,
		"agent/fleet-place-operator", operatorTemplateRef, []string{"fleet/**"})

	authClient := fleetClient(t, fc, operatorToken)
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
	fleetRoomID := createFleetRoom(t, admin)

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
	operatorTemplateRef := publishFleetOperatorTemplate(t, admin, machineA)

	// Register the service principal and push credentials to both
	// machines so either can create a sandbox after reconciliation.
	serviceLocalpart := "service/stt/reconcile-test"
	serviceAccount := registerPrincipal(t, serviceLocalpart, "fleet-recon-password")
	pushCredentials(t, admin, machineA, serviceAccount)
	pushCredentials(t, admin, machineB, serviceAccount)

	// Push empty MachineConfig to both machines so the fleet controller
	// can discover their config rooms.
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

	// Grant the fleet controller access to machineB's config room.
	// startFleetController only grants access for the machine it receives
	// (machineA); additional machines need explicit grants.
	grantFleetControllerConfigAccess(t, admin, fc, machineB)

	// Publish the fleet service binding and deploy a fleet operator on
	// machineA to obtain a daemon-minted fleet token.
	publishFleetServiceBinding(t, admin, machineA, fc)
	operatorToken := deployFleetOperator(t, admin, machineA,
		"agent/fleet-recon-operator", operatorTemplateRef, []string{"fleet/**"})

	// Wait for the fleet controller to discover both config rooms.
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineA.Name)
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineB.Name)

	// Publish the fleet service with Min=2. The fleet controller
	// discovers the service via /sync, runs reconcile, detects a
	// deficit of 2, scores both machines, and calls place() for each.
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
	authClient := fleetClient(t, fc, operatorToken)
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

// TestFleetAuthorizationDenied verifies the fleet controller's security
// boundary: unauthenticated requests are rejected for authenticated
// endpoints, tokens with empty grants deny everything, and narrow grants
// only authorize their specific actions.
//
// This is the authorization complement to TestFleetControllerLifecycle
// (which proves the happy path with full grants). Together they verify
// that the fleet API is both functional and secure.
func TestFleetAuthorizationDenied(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleetRoomID := createFleetRoom(t, admin)

	machine := newTestMachine(t, "machine/fleet-auth")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	operatorTemplateRef := publishFleetOperatorTemplate(t, admin, machine)

	// Start the fleet controller first. The operator template has
	// RequiredServices: ["fleet"], so the daemon needs the fleet
	// controller's socket and binding before it can create sandboxes.
	controllerName := "service/fleet/auth-test"
	fc := startFleetController(t, admin, machine, controllerName, fleetRoomID)
	publishFleetServiceBinding(t, admin, machine, fc)

	// Deploy 3 principals with different grant scopes. Each uses the
	// fleet operator template (RequiredServices: ["fleet"]) so the
	// daemon mints a fleet token for each. The daemon's
	// filterGrantsForService determines which grants end up in each
	// token:
	//   - narrowExact: "fleet/info" → token has one exact fleet grant
	//   - narrowWildcard: "fleet/list-*" → token has one wildcard fleet grant
	//   - noFleetGrants: "command/**" → no fleet-relevant grants, so the
	//     token is valid but has zero grants (default-deny)
	narrowExact := registerPrincipal(t, "agent/fleet-auth-exact", "fleet-auth-exact-password")
	narrowWildcard := registerPrincipal(t, "agent/fleet-auth-wild", "fleet-auth-wild-password")
	noFleetGrants := registerPrincipal(t, "agent/fleet-auth-denied", "fleet-auth-denied-password")

	pushCredentials(t, admin, machine, narrowExact)
	pushCredentials(t, admin, machine, narrowWildcard)
	pushCredentials(t, admin, machine, noFleetGrants)

	joinConfigRoom(t, admin, machine.ConfigRoomID, narrowExact)
	joinConfigRoom(t, admin, machine.ConfigRoomID, narrowWildcard)
	joinConfigRoom(t, admin, machine.ConfigRoomID, noFleetGrants)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{
			{
				Account:  narrowExact,
				Template: operatorTemplateRef,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{"fleet/info"}}},
				},
			},
			{
				Account:  narrowWildcard,
				Template: operatorTemplateRef,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{"fleet/list-*"}}},
				},
			},
			{
				Account:  noFleetGrants,
				Template: operatorTemplateRef,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{"command/**"}}},
				},
			},
		},
	})

	waitForFile(t, machine.PrincipalSocketPath("agent/fleet-auth-exact"))
	waitForFile(t, machine.PrincipalSocketPath("agent/fleet-auth-wild"))
	waitForFile(t, machine.PrincipalSocketPath("agent/fleet-auth-denied"))

	narrowExactToken := readDaemonMintedToken(t, machine, "agent/fleet-auth-exact", "fleet")
	narrowWildcardToken := readDaemonMintedToken(t, machine, "agent/fleet-auth-wild", "fleet")
	noFleetGrantsToken := readDaemonMintedToken(t, machine, "agent/fleet-auth-denied", "fleet")

	ctx := t.Context()

	// assertServiceError verifies that err is a *service.ServiceError
	// whose Message contains the expected substring. This distinguishes
	// auth errors from connection errors or parameter validation errors.
	assertServiceError := func(t *testing.T, err error, action, expectedSubstring string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error, got nil", action)
		}
		var serviceErr *service.ServiceError
		if !errors.As(err, &serviceErr) {
			t.Fatalf("%s: expected *service.ServiceError, got %T: %v", action, err, err)
		}
		if !strings.Contains(serviceErr.Message, expectedSubstring) {
			t.Errorf("%s: expected %q in error, got %q", action, expectedSubstring, serviceErr.Message)
		}
	}

	// All authenticated fleet actions. Status is deliberately excluded
	// because it is the only unauthenticated endpoint.
	authenticatedActions := []string{
		"info", "list-machines", "list-services", "show-machine",
		"show-service", "place", "unplace", "plan", "machine-health",
	}

	t.Run("Unauthenticated", func(t *testing.T) {
		client := fleetClient(t, fc, nil)

		// Status is the only unauthenticated endpoint — it must
		// succeed without a token.
		var status fleetStatusResponse
		if err := client.Call(ctx, "status", nil, &status); err != nil {
			t.Fatalf("status should succeed without auth: %v", err)
		}

		// Every authenticated endpoint rejects a missing token.
		for _, action := range authenticatedActions {
			err := client.Call(ctx, action, nil, nil)
			assertServiceError(t, err, action, "authentication required")
		}
	})

	t.Run("NoFleetGrants", func(t *testing.T) {
		// This principal has grants=["command/**"] which
		// filterGrantsForService excludes from the fleet token.
		// The token is valid (signed by the daemon) but has zero
		// fleet-relevant grants — default-deny rejects everything.
		client := fleetClient(t, fc, noFleetGrantsToken)

		for _, action := range authenticatedActions {
			err := client.Call(ctx, action, nil, nil)
			assertServiceError(t, err, action, "access denied")
		}
	})

	t.Run("NarrowExactGrant", func(t *testing.T) {
		// This principal has grants=["fleet/info"] — an exact match
		// for a single action.
		client := fleetClient(t, fc, narrowExactToken)

		// The granted action succeeds.
		var info fleetInfoResponse
		if err := client.Call(ctx, "info", nil, &info); err != nil {
			t.Fatalf("info with fleet/info grant: %v", err)
		}

		// Actions outside the exact grant are denied.
		for _, action := range []string{"list-machines", "list-services",
			"show-machine", "place"} {
			err := client.Call(ctx, action, nil, nil)
			assertServiceError(t, err, action, "access denied")
		}
	})

	t.Run("NarrowWildcardGrant", func(t *testing.T) {
		// This principal has grants=["fleet/list-*"] — single-segment
		// wildcard that matches list-machines and list-services but
		// not show-machine or info.
		client := fleetClient(t, fc, narrowWildcardToken)

		// Both list actions match the wildcard pattern.
		var machines fleetListMachinesResponse
		if err := client.Call(ctx, "list-machines", nil, &machines); err != nil {
			t.Fatalf("list-machines with fleet/list-* grant: %v", err)
		}
		var services fleetListServicesResponse
		if err := client.Call(ctx, "list-services", nil, &services); err != nil {
			t.Fatalf("list-services with fleet/list-* grant: %v", err)
		}

		// Actions outside the wildcard pattern are denied.
		for _, action := range []string{"info", "show-machine", "place"} {
			err := client.Call(ctx, action, nil, nil)
			assertServiceError(t, err, action, "access denied")
		}
	})
}
