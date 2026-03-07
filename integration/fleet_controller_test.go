// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
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
	Localpart  string                           `json:"localpart"`
	Definition *fleetschema.FleetServiceContent `json:"definition"`
	Instances  []fleetServiceInstance           `json:"instances"`
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

type fleetMachineHealthEntry struct {
	Localpart        string `json:"localpart"`
	HealthState      string `json:"health_state"`
	PresenceState    string `json:"presence_state"`
	LastHeartbeat    string `json:"last_heartbeat"`
	StalenessSeconds int    `json:"staleness_seconds"`
}

type fleetMachineHealthResponse struct {
	Machines []fleetMachineHealthEntry `json:"machines"`
}

// --- Fleet controller test infrastructure ---

// fleetController holds the runtime state of a fleet controller started
// for integration testing. The SocketPath is the CBOR API endpoint.
type fleetController struct {
	PrincipalName string
	UserID        ref.UserID
	SocketPath    string
}

// startFleetController deploys a fleet controller using the production
// principal.Create() path and waits for daemon discovery. The fleet
// controller runs as a machine-level service (outside the sandbox) and
// needs access to fleet-scoped rooms plus elevated power in the config
// room for MachineConfig writes.
func startFleetController(t *testing.T, admin *messaging.DirectSession, machine *testMachine, controllerName string, fleet *testFleet, extraEnv ...map[string]string) *fleetController {
	t.Helper()

	var environmentVariables map[string]string
	if len(extraEnv) > 0 {
		environmentVariables = extraEnv[0]
	}

	svc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:                    resolvedBinary(t, "FLEET_CONTROLLER_BINARY"),
		Name:                      "fleet-controller",
		Localpart:                 controllerName,
		ExtraRooms:                []ref.RoomID{fleet.FleetRoomID, fleet.MachineRoomID},
		ExtraEnvironmentVariables: environmentVariables,
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
	})

	controller := &fleetController{
		PrincipalName: controllerName,
		UserID:        svc.Account.UserID,
		SocketPath:    svc.SocketPath,
	}

	// Grant PL 50 in the config room for MachineConfig read/write.
	// principal.Create() handles basic membership; this grants the
	// elevated power level needed for fleet controller operations.
	grantFleetControllerConfigAccess(t, admin, controller, machine)

	return controller
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
// the fleet room. The state key is the service's federation-safe state
// key (localpart:server, without the @ prefix).
func publishFleetService(t *testing.T, admin *messaging.DirectSession, fleetRoomID ref.RoomID, serviceStateKey string, definition fleetschema.FleetServiceContent) {
	t.Helper()

	_, err := admin.SendStateEvent(t.Context(), fleetRoomID, schema.EventTypeFleetService, serviceStateKey, definition)
	if err != nil {
		t.Fatalf("publish fleet service %s: %v", serviceStateKey, err)
	}
}

// clearFleetService publishes empty content for a FleetServiceContent state
// event, removing the service from the fleet controller's tracking map.
// This matches the production clearFleetRegistration() behavior in
// cmd/bureau/service/destroy.go.
func clearFleetService(t *testing.T, admin *messaging.DirectSession, fleetRoomID ref.RoomID, serviceStateKey string) {
	t.Helper()

	_, err := admin.SendStateEvent(t.Context(), fleetRoomID, schema.EventTypeFleetService, serviceStateKey, struct{}{})
	if err != nil {
		t.Fatalf("clear fleet service %s: %v", serviceStateKey, err)
	}
}

// grantFleetControllerConfigAccess invites a fleet controller to a machine's
// config room and grants it PL 50 so it can read/write MachineConfig for
// placement.
func grantFleetControllerConfigAccess(t *testing.T, admin *messaging.DirectSession, fc *fleetController, machine *testMachine) {
	t.Helper()
	ctx := t.Context()

	if machine.ConfigRoomID.IsZero() {
		t.Fatal("machine has no config room ID — was startMachine called?")
	}

	// Invite (idempotent — M_FORBIDDEN means already a member).
	if err := admin.InviteUser(ctx, machine.ConfigRoomID, fc.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite fleet controller to config room %s: %v", machine.ConfigRoomID, err)
		}
	}

	// Grant fleet controller PL 50 in the config room.
	if err := schema.GrantPowerLevels(ctx, admin, machine.ConfigRoomID, schema.PowerLevelGrants{
		Users: map[ref.UserID]int{fc.UserID: schema.PowerLevelOperator},
	}); err != nil {
		t.Fatalf("grant fleet controller PL in config room: %v", err)
	}
}

// assertFleetMachine calls the fleet controller's list-machines endpoint
// and asserts the given machine localpart is present. The fleet controller
// builds its fleet model before creating its service socket, so after
// startFleetController returns the machine is guaranteed to be in the model.
// If the machine is missing, it's a bug in the fleet controller startup
// sequence, not a timing issue.
func assertFleetMachine(t *testing.T, client *service.ServiceClient, machineIdentifier string) {
	t.Helper()

	var response fleetListMachinesResponse
	if err := client.Call(t.Context(), "list-machines", nil, &response); err != nil {
		t.Fatalf("list-machines: %v", err)
	}

	for _, machine := range response.Machines {
		if machine.Localpart == machineIdentifier {
			return
		}
	}

	t.Fatalf("machine %q not found in fleet controller model (model has %d machines)", machineIdentifier, len(response.Machines))
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
func waitForFleetConfigRoom(t *testing.T, fleetWatch *roomWatch, fc *fleetController, machineIdentifier string) {
	t.Helper()

	waitForNotification[fleetschema.FleetConfigRoomDiscoveredMessage](
		t, fleetWatch, fleetschema.MsgTypeFleetConfigRoomDiscovered, fc.UserID,
		func(m fleetschema.FleetConfigRoomDiscoveredMessage) bool {
			return m.Machine == machineIdentifier
		},
		"fleet controller discovers config room for "+machineIdentifier,
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
func waitForFleetService(t *testing.T, fleetWatch *roomWatch, fc *fleetController, serviceIdentifier string) {
	t.Helper()

	waitForNotification[fleetschema.FleetServiceDiscoveredMessage](
		t, fleetWatch, fleetschema.MsgTypeFleetServiceDiscovered, fc.UserID,
		func(m fleetschema.FleetServiceDiscoveredMessage) bool {
			return m.Service == serviceIdentifier
		},
		"fleet controller discovers service "+serviceIdentifier,
	)
}

// fleetServiceUserID constructs a ref.UserID for a fleet service given
// the fleet reference and the service name (e.g., "stt/lifecycle").
// The service name is the part after "service/" in the account localpart.
func fleetServiceUserID(t *testing.T, fleet *testFleet, name string) ref.UserID {
	t.Helper()
	svcRef, err := ref.NewService(fleet.Ref, name)
	if err != nil {
		t.Fatalf("create service ref for %q: %v", name, err)
	}
	return svcRef.UserID()
}

// mintFleetToken creates a fleet service token signed by the machine's
// Ed25519 key pair. The token is valid for 5 minutes and carries the
// given grants scoped to the "fleet" audience.
//
// This replaces the old deployFleetOperator pattern, which deployed a
// sandbox purely to read a daemon-minted token. That pattern was broken:
// the test agent exited immediately, the daemon revoked the token, and
// the test used a revoked token.
func mintFleetToken(t *testing.T, fleet *testFleet, machine *testMachine, grants []string) []byte {
	t.Helper()

	// Use a synthetic entity as the token subject. No sandbox or Matrix
	// account is needed — the fleet controller validates the token
	// signature and grants, not the subject's existence.
	entity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/fleet-test-operator")
	if err != nil {
		t.Fatalf("construct entity for fleet token: %v", err)
	}
	tokenGrants := make([]servicetoken.Grant, len(grants))
	for i, pattern := range grants {
		tokenGrants[i] = servicetoken.Grant{Actions: []string{pattern}}
	}
	return mintTestServiceToken(t, machine, entity, "fleet", tokenGrants)
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	// Boot a machine. The daemon publishes MachineInfo and MachineStatus
	// to #bureau/machine, and creates the per-machine config room.
	// startMachine blocks until the first status heartbeat arrives, so
	// the fleet controller's initial /sync will see this machine.
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "fleet-lifecycle")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	controllerName := "service/fleet/lifecycle"
	fc := startFleetController(t, admin, machine, controllerName, fleet)

	operatorToken := mintFleetToken(t, fleet, machine, []string{fleetschema.ActionAll})

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

	// --- Sub-test: fleet service binding auto-published ---
	t.Run("ServiceBinding", func(t *testing.T) {
		// The fleet controller publishes its service binding to config
		// rooms during initialSync. Since startFleetController blocks
		// until the socket is ready (after initialSync completes), the
		// binding should already be present.
		raw, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
			schema.EventTypeServiceBinding, "fleet")
		if err != nil {
			t.Fatalf("get fleet service binding: %v", err)
		}

		var binding schema.ServiceBindingContent
		if err := json.Unmarshal(raw, &binding); err != nil {
			t.Fatalf("unmarshal service binding: %v", err)
		}

		// The binding's principal should be the fleet controller's
		// entity (fleet-scoped, with the controller's principal name).
		if binding.Principal.IsZero() {
			t.Fatal("service binding principal is zero")
		}
		expectedLocalpart := controllerName
		if actualLocalpart := binding.Principal.AccountLocalpart(); actualLocalpart != expectedLocalpart {
			t.Errorf("service binding principal localpart = %q, want %q",
				actualLocalpart, expectedLocalpart)
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
		assertFleetMachine(t, authClient, machine.UserID.String())

		var response fleetListMachinesResponse
		if err := authClient.Call(ctx, "list-machines", nil, &response); err != nil {
			t.Fatalf("list-machines: %v", err)
		}

		if len(response.Machines) == 0 {
			t.Fatal("list-machines returned 0 machines")
		}

		var found bool
		for _, summary := range response.Machines {
			if summary.Localpart == machine.UserID.String() {
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
			map[string]any{"machine": machine.UserID.String()}, &response); err != nil {
			t.Fatalf("show-machine: %v", err)
		}

		if response.Localpart != machine.UserID.String() {
			t.Errorf("show-machine localpart = %q, want %q", response.Localpart, machine.UserID.String())
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
		svcUserID := fleetServiceUserID(t, fleet, "stt/lifecycle")
		svcStateKey := svcUserID.StateKey()
		svcID := svcUserID.String()

		// Create a fleet room watch before publishing so we can
		// event-wait for the service discovered notification.
		fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)

		publishFleetService(t, admin, fleet.FleetRoomID, svcStateKey, fleetschema.FleetServiceContent{
			Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":whisper-stt",
			Replicas: fleetschema.ReplicaSpec{Min: 0},
			Failover: "migrate",
			Priority: 10,
		})

		authClient := fleetClient(t, fc, operatorToken)
		waitForFleetService(t, &fleetWatch, fc, svcID)

		// Verify the service appears in list-services with correct fields.
		var listResponse fleetListServicesResponse
		if err := authClient.Call(ctx, "list-services", nil, &listResponse); err != nil {
			t.Fatalf("list-services: %v", err)
		}

		var found bool
		for _, svc := range listResponse.Services {
			if svc.Localpart == svcID {
				found = true
				expectedTemplate := ns.Namespace.TemplateRoomAliasLocalpart() + ":whisper-stt"
				if svc.Template != expectedTemplate {
					t.Errorf("service template = %q, want %q", svc.Template, expectedTemplate)
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
			t.Errorf("service %q not found in list-services response", svcID)
		}
	})

	// --- Sub-test: show service ---
	t.Run("ShowService", func(t *testing.T) {
		svcID := fleetServiceUserID(t, fleet, "stt/lifecycle").String()

		authClient := fleetClient(t, fc, operatorToken)
		// The service was published in the ServiceDiscovery sub-test.
		// Since sub-tests run sequentially within the parent, the fleet
		// controller has already processed it.
		var response fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": svcID}, &response); err != nil {
			t.Fatalf("show-service: %v", err)
		}

		if response.Localpart != svcID {
			t.Errorf("show-service localpart = %q, want %q", response.Localpart, svcID)
		}
		if response.Definition == nil {
			t.Fatal("show-service definition is nil")
		}
		expectedTemplate := ns.Namespace.TemplateRoomAliasLocalpart() + ":whisper-stt"
		if response.Definition.Template != expectedTemplate {
			t.Errorf("definition template = %q, want %q",
				response.Definition.Template, expectedTemplate)
		}
		if len(response.Instances) != 0 {
			t.Errorf("show-service instances = %d, want 0 (no placement performed)",
				len(response.Instances))
		}
	})

	// --- Sub-test: plan (dry-run scoring) ---
	t.Run("Plan", func(t *testing.T) {
		svcID := fleetServiceUserID(t, fleet, "stt/lifecycle").String()

		authClient := fleetClient(t, fc, operatorToken)
		var response fleetPlanResponse
		if err := authClient.Call(ctx, "plan",
			map[string]any{"service": svcID}, &response); err != nil {
			t.Fatalf("plan: %v", err)
		}

		if response.Service != svcID {
			t.Errorf("plan service = %q, want %q", response.Service, svcID)
		}
		// The machine should appear as a candidate (it's the only one
		// and has no constraints to violate).
		if len(response.Candidates) == 0 {
			t.Fatal("plan returned 0 candidates, want >= 1")
		}
		var foundCandidate bool
		for _, candidate := range response.Candidates {
			if candidate.Machine == machine.UserID.String() {
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

		var response fleetMachineHealthResponse
		if err := authClient.Call(ctx, "machine-health",
			map[string]any{"machine": machine.UserID.String()}, &response); err != nil {
			t.Fatalf("machine-health: %v", err)
		}

		if len(response.Machines) != 1 {
			t.Fatalf("machine-health returned %d entries, want 1", len(response.Machines))
		}
		entry := response.Machines[0]
		if entry.Localpart != machine.UserID.String() {
			t.Errorf("health localpart = %q, want %q", entry.Localpart, machine.UserID.String())
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

	// Boot a machine with proxy support. The daemon needs the proxy
	// binary to create sandboxes when the fleet controller places a
	// service.
	machine := newTestMachine(t, fleet, "fleet-place")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Publish a test template so the daemon can create sandboxes when
	// the fleet controller places a service.
	templateRef := publishTestAgentTemplate(t, admin, machine, "fleet-place-agent")

	// Register a Matrix account for the service principal. The fleet
	// controller will reference this localpart when it creates a
	// PrincipalAssignment.
	serviceLocalpart := "service/stt/place-test"
	serviceAccount := registerFleetPrincipal(t, fleet, serviceLocalpart, "fleet-place-test-password")

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
	// the config room.
	pushMachineConfig(t, admin, machine, deploymentConfig{})

	// Create a fleet room watch BEFORE starting the fleet controller.
	configDiscoverWatch := watchRoom(t, admin, fleet.FleetRoomID)

	controllerName := "service/fleet/place-test"
	fc := startFleetController(t, admin, machine, controllerName, fleet)

	operatorToken := mintFleetToken(t, fleet, machine, []string{fleetschema.ActionAll})
	authClient := fleetClient(t, fc, operatorToken)
	ctx := t.Context()

	// Wait for the fleet controller to discover the config room.
	waitForFleetConfigRoom(t, &configDiscoverWatch, fc, machine.UserID.String())

	// Construct the service user ID for FC API interactions.
	svcUserID := fleetServiceUserID(t, fleet, "stt/place-test")
	svcStateKey := svcUserID.StateKey()
	svcID := svcUserID.String()

	// Create a fleet room watch before publishing the service so we
	// can event-wait for the service discovered notification.
	serviceDiscoverWatch := watchRoom(t, admin, fleet.FleetRoomID)

	// Publish a fleet service definition with Min=0 so the reconcile
	// loop does not auto-place it. This test exercises explicit place
	// and unplace calls; auto-placement is tested separately in
	// TestFleetAutoPlacement.
	publishFleetService(t, admin, fleet.FleetRoomID, svcStateKey, fleetschema.FleetServiceContent{
		Template: templateRef,
		Replicas: fleetschema.ReplicaSpec{Min: 0},
		Failover: "migrate",
		Priority: 10,
	})
	waitForFleetService(t, &serviceDiscoverWatch, fc, svcID)

	// --- Place the service ---
	t.Run("Place", func(t *testing.T) {
		var placeResponse fleetPlaceResponse
		if err := authClient.Call(ctx, "place", map[string]any{
			"service": svcID,
			"machine": machine.UserID.String(),
		}, &placeResponse); err != nil {
			t.Fatalf("place: %v", err)
		}

		if placeResponse.Service != svcID {
			t.Errorf("place response service = %q, want %q", placeResponse.Service, svcID)
		}
		if placeResponse.Machine != machine.UserID.String() {
			t.Errorf("place response machine = %q, want %q", placeResponse.Machine, machine.UserID.String())
		}

		// Wait for the daemon to create the proxy. The daemon detects
		// the MachineConfig change (written by the fleet controller),
		// reads credentials, and creates a proxy sandbox.
		proxySocket := machine.PrincipalProxySocketPath(t, serviceLocalpart)
		waitForFile(t, proxySocket)

		// Verify the proxy serves the correct identity.
		proxyClient := proxyHTTPClient(proxySocket)
		whoamiUserID := proxyWhoami(t, proxyClient)
		if whoamiUserID != serviceAccount.UserID.String() {
			t.Errorf("proxy whoami = %q, want %q", whoamiUserID, serviceAccount.UserID)
		}

		// Verify show-service reflects the placement.
		var showService fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": svcID}, &showService); err != nil {
			t.Fatalf("show-service: %v", err)
		}
		if len(showService.Instances) != 1 {
			t.Fatalf("show-service instances = %d, want 1", len(showService.Instances))
		}
		if showService.Instances[0].Machine != machine.UserID.String() {
			t.Errorf("instance machine = %q, want %q",
				showService.Instances[0].Machine, machine.UserID.String())
		}

		// Verify show-machine reflects the assignment.
		var showMachine fleetShowMachineResponse
		if err := authClient.Call(ctx, "show-machine",
			map[string]any{"machine": machine.UserID.String()}, &showMachine); err != nil {
			t.Fatalf("show-machine: %v", err)
		}
		var foundAssignment bool
		for _, assignment := range showMachine.Assignments {
			if assignment.Principal.AccountLocalpart() == serviceLocalpart {
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
		proxySocket := machine.PrincipalProxySocketPath(t, serviceLocalpart)

		var unplaceResponse fleetUnplaceResponse
		if err := authClient.Call(ctx, "unplace", map[string]any{
			"service": svcID,
			"machine": machine.UserID.String(),
		}, &unplaceResponse); err != nil {
			t.Fatalf("unplace: %v", err)
		}

		if unplaceResponse.Service != svcID {
			t.Errorf("unplace response service = %q, want %q",
				unplaceResponse.Service, svcID)
		}
		if unplaceResponse.Machine != machine.UserID.String() {
			t.Errorf("unplace response machine = %q, want %q",
				unplaceResponse.Machine, machine.UserID.String())
		}

		// Wait for the daemon to tear down the proxy. The daemon
		// detects the MachineConfig change (fleet controller removed
		// the assignment) and destroys the sandbox.
		waitForFileGone(t, proxySocket)

		// Verify show-service reflects zero instances.
		var showService fleetShowServiceResponse
		if err := authClient.Call(ctx, "show-service",
			map[string]any{"service": svcID}, &showService); err != nil {
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

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	// Boot two machines with proxy support.
	machineA := newTestMachine(t, fleet, "fleet-recon-a")
	machineB := newTestMachine(t, fleet, "fleet-recon-b")
	options := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	}
	startMachine(t, admin, machineA, options)
	startMachine(t, admin, machineB, options)

	// Publish a test template and grant both machines access.
	templateRef := publishTestAgentTemplate(t, admin, machineA, "fleet-recon-agent")
	grantTemplateAccess(t, admin, machineB)

	// Register the service principal and push credentials to both
	// machines so either can create a sandbox after reconciliation.
	serviceLocalpart := "service/stt/reconcile-test"
	serviceAccount := registerFleetPrincipal(t, fleet, serviceLocalpart, "fleet-recon-password")
	pushCredentials(t, admin, machineA, serviceAccount)
	pushCredentials(t, admin, machineB, serviceAccount)

	// Push empty MachineConfig to both machines so the fleet controller
	// can discover their config rooms.
	pushMachineConfig(t, admin, machineA, deploymentConfig{})
	pushMachineConfig(t, admin, machineB, deploymentConfig{})

	discoverWatch := watchRoom(t, admin, fleet.FleetRoomID)

	controllerName := "service/fleet/reconcile-test"
	fc := startFleetController(t, admin, machineA, controllerName, fleet)
	grantFleetControllerConfigAccess(t, admin, fc, machineB)

	operatorToken := mintFleetToken(t, fleet, machineA, []string{fleetschema.ActionAll})

	// Wait for the fleet controller to discover both config rooms.
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineA.UserID.String())
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineB.UserID.String())

	// Construct service user ID for FC API interactions.
	svcUserID := fleetServiceUserID(t, fleet, "stt/reconcile-test")
	svcStateKey := svcUserID.StateKey()
	svcID := svcUserID.String()

	// Publish the fleet service with Min=2. The fleet controller
	// discovers the service via /sync, runs reconcile, detects a
	// deficit of 2, scores both machines, and calls place() for each.
	publishFleetService(t, admin, fleet.FleetRoomID, svcStateKey, fleetschema.FleetServiceContent{
		Template: templateRef,
		Replicas: fleetschema.ReplicaSpec{Min: 2},
		Placement: fleetschema.PlacementConstraints{
			AllowedMachines: []string{machineA.Name, machineB.Name},
		},
		Failover: "migrate",
		Priority: 10,
	})
	waitForFleetService(t, &discoverWatch, fc, svcID)

	// Wait for proxy sockets on both machines. Proxy socket existence
	// proves the full chain: fleet controller reconcile → place() writes
	// MachineConfig → daemon /sync → launcher sandbox creation.
	proxySocketA := machineA.PrincipalProxySocketPath(t, serviceLocalpart)
	proxySocketB := machineB.PrincipalProxySocketPath(t, serviceLocalpart)
	waitForFile(t, proxySocketA)
	waitForFile(t, proxySocketB)

	// Verify both proxies serve the correct identity.
	proxyClientA := proxyHTTPClient(proxySocketA)
	if whoami := proxyWhoami(t, proxyClientA); whoami != serviceAccount.UserID.String() {
		t.Errorf("machine A proxy whoami = %q, want %q", whoami, serviceAccount.UserID)
	}
	proxyClientB := proxyHTTPClient(proxySocketB)
	if whoami := proxyWhoami(t, proxyClientB); whoami != serviceAccount.UserID.String() {
		t.Errorf("machine B proxy whoami = %q, want %q", whoami, serviceAccount.UserID)
	}

	// Verify the fleet model reflects both placements.
	authClient := fleetClient(t, fc, operatorToken)
	ctx := t.Context()

	var showService fleetShowServiceResponse
	if err := authClient.Call(ctx, "show-service",
		map[string]any{"service": svcID}, &showService); err != nil {
		t.Fatalf("show-service: %v", err)
	}
	if len(showService.Instances) != 2 {
		t.Fatalf("show-service instances = %d, want 2", len(showService.Instances))
	}
	instanceMachines := make(map[string]bool, len(showService.Instances))
	for _, instance := range showService.Instances {
		instanceMachines[instance.Machine] = true
	}
	if !instanceMachines[machineA.UserID.String()] {
		t.Errorf("show-service missing instance on %s", machineA.Name)
	}
	if !instanceMachines[machineB.UserID.String()] {
		t.Errorf("show-service missing instance on %s", machineB.Name)
	}

	// Verify show-machine for both machines: each should have a
	// PrincipalAssignment with the correct template, AutoStart=true,
	// and fleet_managed label matching the controller name.
	for _, machine := range []*testMachine{machineA, machineB} {
		var showMachine fleetShowMachineResponse
		if err := authClient.Call(ctx, "show-machine",
			map[string]any{"machine": machine.UserID.String()}, &showMachine); err != nil {
			t.Fatalf("show-machine %s: %v", machine.Name, err)
		}
		var foundAssignment bool
		for _, assignment := range showMachine.Assignments {
			if assignment.Principal.AccountLocalpart() == serviceLocalpart {
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

	// Verify the fleet controller published its service binding to both
	// config rooms. MachineA's binding comes from initial sync (the fleet
	// controller discovers its own machine's config room during startup).
	// MachineB's binding comes from incremental sync (the fleet controller
	// accepts the invite to B's config room after grantFleetControllerConfigAccess).
	for _, machine := range []*testMachine{machineA, machineB} {
		raw, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
			schema.EventTypeServiceBinding, "fleet")
		if err != nil {
			t.Fatalf("get fleet binding for %s: %v", machine.Name, err)
		}

		var binding schema.ServiceBindingContent
		if err := json.Unmarshal(raw, &binding); err != nil {
			t.Fatalf("unmarshal binding for %s: %v", machine.Name, err)
		}

		if binding.Principal.IsZero() {
			t.Errorf("machine %s: fleet binding principal is zero", machine.Name)
		} else if localpart := binding.Principal.AccountLocalpart(); localpart != controllerName {
			t.Errorf("machine %s: fleet binding principal localpart = %q, want %q",
				machine.Name, localpart, controllerName)
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

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "fleet-auth")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	controllerName := "service/fleet/auth-test"
	fc := startFleetController(t, admin, machine, controllerName, fleet)

	// Mint tokens with different grant scopes directly. No sandbox
	// deployment needed — the fleet controller validates the token
	// signature and grants, not the principal's sandbox state.
	narrowExactToken := mintFleetToken(t, fleet, machine, []string{fleetschema.ActionInfo})
	narrowWildcardToken := mintFleetToken(t, fleet, machine, []string{"fleet/list-*"})
	noFleetGrantsToken := mintFleetToken(t, fleet, machine, nil)

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

// TestFleetEligibilityConstraints verifies the fleet controller's placement
// constraint enforcement end-to-end. The placement engine (scoreMachine in
// placement.go) evaluates candidate machines against a service's
// PlacementConstraints and returns ineligible (-1) for machines that fail
// any constraint. The plan API exposes this as a dry-run: scorePlacement
// filters out ineligible machines and returns only eligible candidates.
//
// This test verifies the full path through real Matrix state: daemon
// publishes MachineInfo with labels → fleet controller reads via /sync →
// plan API filters correctly based on constraints.
//
// Three sub-tests:
//   - RequiredLabels: key=value label matching (gpu=h100 vs gpu=t4)
//   - LabelPresenceOnly: key presence without value matching
//   - AntiAffinity: machine assignment exclusion
func TestFleetEligibilityConstraints(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)
	ctx := t.Context()

	// Boot two machines with proxy support. Proxy is needed for the
	// place call in the AntiAffinity sub-test (place writes MachineConfig
	// which triggers daemon reconciliation).
	machineA := newTestMachine(t, fleet, "fleet-elig-a")
	machineB := newTestMachine(t, fleet, "fleet-elig-b")
	machineOpts := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	}
	startMachine(t, admin, machineA, machineOpts)
	startMachine(t, admin, machineB, machineOpts)

	// Set labels on both machines. SetMachineLabels reads the
	// daemon-published MachineInfo, replaces the Labels field, and
	// writes back — preserving the daemon's hardware inventory while
	// adding test-specific labels. Both machines' MachineInfo live
	// in the same fleet-scoped machine room, differentiated by state
	// key (machine localpart).
	for _, labelEntry := range []struct {
		machine *testMachine
		labels  map[string]string
	}{
		{machineA, map[string]string{"gpu": "h100", "tier": "production"}},
		{machineB, map[string]string{"gpu": "t4", "tier": "production"}},
	} {
		if err := schema.SetMachineLabels(ctx, admin, labelEntry.machine.MachineRoomID,
			labelEntry.machine.UserID.StateKey(), labelEntry.labels); err != nil {
			t.Fatalf("set labels for %s: %v", labelEntry.machine.Name, err)
		}
	}

	// Push empty MachineConfig to both machines so the fleet controller
	// can discover their config rooms.
	pushMachineConfig(t, admin, machineA, deploymentConfig{})
	pushMachineConfig(t, admin, machineB, deploymentConfig{})

	// Create a fleet room watch before starting the fleet controller so
	// the admin's sync checkpoint captures config room discovery events.
	discoverWatch := watchRoom(t, admin, fleet.FleetRoomID)

	controllerName := "service/fleet/eligibility-test"
	fc := startFleetController(t, admin, machineA, controllerName, fleet)
	grantFleetControllerConfigAccess(t, admin, fc, machineB)

	operatorToken := mintFleetToken(t, fleet, machineA, []string{fleetschema.ActionAll})
	authClient := fleetClient(t, fc, operatorToken)

	// Wait for the fleet controller to discover both config rooms.
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineA.UserID.String())
	waitForFleetConfigRoom(t, &discoverWatch, fc, machineB.UserID.String())

	// planCandidateMachines calls the plan API and returns the set of
	// candidate machine identifiers (full user ID strings).
	planCandidateMachines := func(t *testing.T, serviceIdentifier string) map[string]bool {
		t.Helper()
		var response fleetPlanResponse
		if err := authClient.Call(ctx, "plan",
			map[string]any{"service": serviceIdentifier}, &response); err != nil {
			t.Fatalf("plan %s: %v", serviceIdentifier, err)
		}
		candidates := make(map[string]bool, len(response.Candidates))
		for _, candidate := range response.Candidates {
			candidates[candidate.Machine] = true
		}
		return candidates
	}

	// publishAndDiscover publishes a fleet service and waits for the
	// fleet controller to process it via /sync.
	publishAndDiscover := func(t *testing.T, serviceStateKey string, svcID string, definition fleetschema.FleetServiceContent) {
		t.Helper()
		serviceWatch := watchRoom(t, admin, fleet.FleetRoomID)
		publishFleetService(t, admin, fleet.FleetRoomID, serviceStateKey, definition)
		waitForFleetService(t, &serviceWatch, fc, svcID)
	}

	// --- RequiredLabels: key=value matching ---
	// Service requires gpu=h100. MachineA has gpu:h100, machineB has
	// gpu:t4. Only machineA should be eligible.
	t.Run("RequiredLabels", func(t *testing.T) {
		svcUserID := fleetServiceUserID(t, fleet, "stt/elig-required")
		svcStateKey := svcUserID.StateKey()
		svcID := svcUserID.String()
		publishAndDiscover(t, svcStateKey, svcID, fleetschema.FleetServiceContent{
			Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":whisper-stt",
			Replicas: fleetschema.ReplicaSpec{Min: 0},
			Placement: fleetschema.PlacementConstraints{
				Requires:        []string{"gpu=h100"},
				AllowedMachines: []string{machineA.Name, machineB.Name},
			},
			Failover: "none",
		})

		candidates := planCandidateMachines(t, svcID)
		if !candidates[machineA.UserID.String()] {
			t.Errorf("machineA (%s) should be eligible (has gpu=h100)", machineA.Name)
		}
		if candidates[machineB.UserID.String()] {
			t.Errorf("machineB (%s) should be ineligible (has gpu=t4, not h100)", machineB.Name)
		}
	})

	// --- LabelPresenceOnly: key presence without value matching ---
	// Service requires "gpu" (no =value). Both machines have a gpu
	// label (different values). Both should be eligible.
	t.Run("LabelPresenceOnly", func(t *testing.T) {
		svcUserID := fleetServiceUserID(t, fleet, "stt/elig-presence")
		svcStateKey := svcUserID.StateKey()
		svcID := svcUserID.String()
		publishAndDiscover(t, svcStateKey, svcID, fleetschema.FleetServiceContent{
			Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":whisper-stt",
			Replicas: fleetschema.ReplicaSpec{Min: 0},
			Placement: fleetschema.PlacementConstraints{
				Requires:        []string{"gpu"},
				AllowedMachines: []string{machineA.Name, machineB.Name},
			},
			Failover: "none",
		})

		candidates := planCandidateMachines(t, svcID)
		if !candidates[machineA.UserID.String()] {
			t.Errorf("machineA (%s) should be eligible (has gpu label)", machineA.Name)
		}
		if !candidates[machineB.UserID.String()] {
			t.Errorf("machineB (%s) should be eligible (has gpu label)", machineB.Name)
		}
	})

	// --- AntiAffinity: machine assignment exclusion ---
	// Place a baseline service on machineA, then publish a second
	// service with AntiAffinity pointing to the baseline. The plan
	// API should exclude machineA (hosts the baseline) and return
	// only machineB.
	t.Run("AntiAffinity", func(t *testing.T) {
		// Publish and discover the baseline service.
		baselineUserID := fleetServiceUserID(t, fleet, "stt/elig-baseline")
		baselineStateKey := baselineUserID.StateKey()
		baselineID := baselineUserID.String()
		publishAndDiscover(t, baselineStateKey, baselineID, fleetschema.FleetServiceContent{
			Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":baseline-stt",
			Replicas: fleetschema.ReplicaSpec{Min: 0},
			Placement: fleetschema.PlacementConstraints{
				AllowedMachines: []string{machineA.Name, machineB.Name},
			},
			Failover: "none",
		})

		// Place the baseline on machineA. The fleet controller updates
		// its in-memory machine.assignments map synchronously during the
		// place call (execute.go:162), so the subsequent plan call will
		// see the assignment without any async delay. The daemon will
		// attempt to reconcile the placed service but will skip it
		// (no template or credentials provisioned) — that's fine, we
		// only need the fleet controller's in-memory state.
		var placeResponse fleetPlaceResponse
		if err := authClient.Call(ctx, "place", map[string]any{
			"service": baselineID,
			"machine": machineA.UserID.String(),
		}, &placeResponse); err != nil {
			t.Fatalf("place baseline on machineA: %v", err)
		}

		// Publish a second service with anti-affinity to the baseline.
		// AntiAffinity values are parsed with ref.ParseUserID(), so they
		// need the full @localpart:server format.
		antiUserID := fleetServiceUserID(t, fleet, "stt/elig-anti")
		antiStateKey := antiUserID.StateKey()
		antiID := antiUserID.String()
		publishAndDiscover(t, antiStateKey, antiID, fleetschema.FleetServiceContent{
			Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":anti-stt",
			Replicas: fleetschema.ReplicaSpec{Min: 0},
			Placement: fleetschema.PlacementConstraints{
				AntiAffinity:    []string{baselineID},
				AllowedMachines: []string{machineA.Name, machineB.Name},
			},
			Failover: "none",
		})

		candidates := planCandidateMachines(t, antiID)
		if candidates[machineA.UserID.String()] {
			t.Errorf("machineA (%s) should be ineligible (hosts baseline service)", machineA.Name)
		}
		if !candidates[machineB.UserID.String()] {
			t.Errorf("machineB (%s) should be eligible (does not host baseline)", machineB.Name)
		}
	})
}

// TestFleetPresenceDetection verifies that the fleet controller receives
// m.presence events from machine daemons and uses them to accelerate
// health detection. The sequence:
//
//   - Machine daemon starts, sets presence to "online"
//   - Fleet controller receives presence via /sync, updates model
//   - Daemon is killed (SIGTERM triggers graceful shutdown with
//     SetPresence("offline", "shutting down"))
//   - Fleet controller receives the presence change, escalates health
//     from "online" to "suspect"
//   - Fleet controller publishes a FleetPresenceChanged notification
//
// This tests the presence fast-path: the fleet controller detects the
// daemon going offline via presence (seconds) rather than waiting for
// heartbeat staleness (30-90 seconds). The heartbeat is still fresh
// when presence reports offline, so checkMachineHealth escalates to
// "suspect" via the presence fast-path rather than the heartbeat
// staleness path.
func TestFleetPresenceDetection(t *testing.T) {
	t.Parallel()

	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)
	machine := newTestMachine(t, fleet, "presence")

	options := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   daemonBinary,
		Fleet:          fleet,
	}

	// Start launcher and daemon manually — we kill the daemon mid-test.
	startMachineLauncher(t, admin, machine, options)
	daemon := startMachineDaemonManual(t, admin, machine, options)
	t.Cleanup(func() {
		// If the test exits early (e.g., fatal), kill the daemon.
		// After SIGTERM in the main test body, Process.Signal returns
		// "os: process already finished" which is harmless.
		daemon.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- daemon.Wait() }()
		testutil.RequireReceive(t, done, 5*time.Second, "daemon cleanup")
	})

	// Push empty MachineConfig so the fleet controller can discover
	// the machine's config room.
	pushMachineConfig(t, admin, machine, deploymentConfig{})

	// Create fleet room watch BEFORE starting the fleet controller
	// so we capture all fleet notifications from initial startup.
	fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)

	controllerName := "service/fleet/presence-test"
	fc := startFleetController(t, admin, machine, controllerName, fleet)

	// Wait for the fleet controller to discover the machine's config
	// room. After this, the fleet controller has the machine in its
	// model and is processing presence events for it.
	waitForFleetConfigRoom(t, &fleetWatch, fc, machine.UserID.String())

	operatorToken := mintFleetToken(t, fleet, machine, []string{fleetschema.ActionAll})
	authClient := fleetClient(t, fc, operatorToken)

	// Verify initial health: machine should be online with a fresh
	// heartbeat. The daemon set presence to "online" on startup, and
	// the fleet controller received it via /sync.
	var initialHealth fleetMachineHealthResponse
	if err := authClient.Call(t.Context(), "machine-health",
		map[string]any{"machine": machine.UserID.String()}, &initialHealth); err != nil {
		t.Fatalf("machine-health (initial): %v", err)
	}
	if length := len(initialHealth.Machines); length != 1 {
		t.Fatalf("machine-health returned %d entries, want 1", length)
	}
	initialEntry := initialHealth.Machines[0]
	if initialEntry.HealthState != "online" {
		t.Fatalf("initial health state = %q, want %q", initialEntry.HealthState, "online")
	}
	t.Log("initial machine health: online")

	// Set up watch for the presence change notification BEFORE killing
	// the daemon so the admin's sync checkpoint captures it.
	presenceWatch := watchRoom(t, admin, fleet.FleetRoomID)

	// Kill the daemon gracefully. SIGTERM triggers the shutdown path
	// which calls SetPresence("offline", "shutting down") before exit.
	t.Log("killing daemon (SIGTERM)")
	if err := daemon.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- daemon.Wait() }()
	testutil.RequireReceive(t, waitDone, 5*time.Second, "daemon did not exit after SIGTERM")
	t.Log("daemon exited")

	// Trigger a fleet controller sync cycle. Conduwuit accumulates
	// presence events but doesn't wake /sync long-polls for
	// presence-only changes. Sending a room event to a shared room
	// wakes the long-poll, and the accumulated presence events are
	// included in the same sync response. In production, other
	// activity (heartbeats, config changes) triggers syncs frequently,
	// so presence events piggyback without needing an explicit trigger.
	_, err := admin.SendEvent(t.Context(), fleet.FleetRoomID, schema.MatrixEventTypeMessage,
		map[string]any{
			"msgtype": "m.text",
			"body":    "sync trigger after daemon shutdown",
		})
	if err != nil {
		t.Fatalf("trigger fleet sync: %v", err)
	}

	// Wait for the fleet controller to process the presence event.
	// The fleet controller publishes a FleetPresenceChanged notification
	// to the fleet room when it detects a presence state transition.
	presenceChanged := waitForNotification[fleetschema.FleetPresenceChangedMessage](
		t, &presenceWatch, fleetschema.MsgTypeFleetPresenceChanged, fc.UserID,
		func(message fleetschema.FleetPresenceChangedMessage) bool {
			return message.Machine == machine.UserID.String() && message.Current == "offline"
		}, "presence changed to offline for "+machine.Name)

	t.Logf("fleet controller received presence offline (previous=%q)", presenceChanged.Previous)

	// Query machine-health. The fleet controller has processed the
	// presence event (the notification proves it), so the model
	// reflects the updated state.
	var afterHealth fleetMachineHealthResponse
	if err := authClient.Call(t.Context(), "machine-health",
		map[string]any{"machine": machine.UserID.String()}, &afterHealth); err != nil {
		t.Fatalf("machine-health (after presence offline): %v", err)
	}

	afterEntry := afterHealth.Machines[0]
	if afterEntry.PresenceState != "offline" {
		t.Errorf("presence state = %q, want %q", afterEntry.PresenceState, "offline")
	}
	// Health should be "suspect": the heartbeat is still fresh (the daemon
	// just exited), but presence reported offline, so checkMachineHealth
	// escalated from "online" to "suspect" via the presence fast-path.
	if afterEntry.HealthState != "suspect" {
		t.Errorf("health state = %q, want %q (expected presence-based suspect escalation)", afterEntry.HealthState, "suspect")
	}

	t.Log("fleet controller: presence offline, health escalated to suspect")
}

// TestServiceFleetRegistration verifies the full fleet registration lifecycle:
// deploying a service with FleetRegister publishes a FleetServiceContent state
// event that the fleet controller discovers, and clearing the registration
// removes it from the fleet controller's tracking.
//
// This exercises the production code paths added by the fleet ergonomics work:
//   - registerServiceWithFleet (cmd/bureau/service/create.go)
//   - clearFleetRegistration (cmd/bureau/service/destroy.go)
//
// The test uses the integration test's deployService helper with
// FleetRegister:true, which publishes the same FleetServiceContent that the
// production CLI publishes.
func TestServiceFleetRegistration(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "fleet-reg")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Start the fleet controller. Set up a fleet room watch BEFORE
	// starting so we can synchronize on discovery notifications.
	controllerName := "service/fleet/reg"
	fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)
	fc := startFleetController(t, admin, machine, controllerName, fleet)
	waitForFleetConfigRoom(t, &fleetWatch, fc, machine.UserID.String())

	operatorToken := mintFleetToken(t, fleet, machine, []string{fleetschema.ActionAll})
	authClient := fleetClient(t, fc, operatorToken)

	ctx := t.Context()

	// Construct service user IDs for FC API interactions.
	svcUserID := fleetServiceUserID(t, fleet, "test/fleet-reg")
	svcStateKey := svcUserID.StateKey()
	svcID := svcUserID.String()

	sentinelUserID := fleetServiceUserID(t, fleet, "test/fleet-reg-sentinel")
	sentinelStateKey := sentinelUserID.StateKey()
	sentinelID := sentinelUserID.String()

	// Phase 1: Deploy a service with fleet registration.
	// Set up the fleet watch BEFORE deploying so we capture the
	// discovery notification.
	serviceFleetWatch := watchRoom(t, admin, fleet.FleetRoomID)

	svc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:        resolvedBinary(t, "TEST_SERVICE_BINARY"),
		Name:          "fleet-reg-svc",
		Localpart:     "service/test/fleet-reg",
		FleetRegister: true,
	})
	_ = svc // service socket not needed for this test

	// Wait for the fleet controller to discover the service definition.
	waitForFleetService(t, &serviceFleetWatch, fc, svcID)

	// Phase 2: Verify the fleet controller has the correct service fields.
	var listResponse fleetListServicesResponse
	if err := authClient.Call(ctx, "list-services", nil, &listResponse); err != nil {
		t.Fatalf("list-services after registration: %v", err)
	}

	var found bool
	for _, service := range listResponse.Services {
		if service.Localpart == svcID {
			found = true
			if service.Replicas != 1 {
				t.Errorf("service replicas = %d, want 1", service.Replicas)
			}
			if service.Failover != string(fleetschema.FailoverNone) {
				t.Errorf("service failover = %q, want %q", service.Failover, fleetschema.FailoverNone)
			}
			if service.Priority != 50 {
				t.Errorf("service priority = %d, want 50", service.Priority)
			}
			break
		}
	}
	if !found {
		t.Fatalf("service %q not found in list-services after registration", svcID)
	}

	// Verify show-service returns the full definition with placement.
	var showResponse fleetShowServiceResponse
	if err := authClient.Call(ctx, "show-service",
		map[string]any{"service": svcID}, &showResponse); err != nil {
		t.Fatalf("show-service: %v", err)
	}
	if showResponse.Definition == nil {
		t.Fatal("show-service definition is nil")
	}
	if len(showResponse.Definition.Placement.PreferredMachines) == 0 {
		t.Error("show-service placement preferred_machines is empty, want machine name")
	} else if showResponse.Definition.Placement.PreferredMachines[0] != machine.Name {
		t.Errorf("preferred_machines[0] = %q, want %q",
			showResponse.Definition.Placement.PreferredMachines[0], machine.Name)
	}

	// Phase 3: Clear fleet registration (matches service destroy behavior).
	// The fleet controller does not emit a removal notification, so use a
	// sentinel service to synchronize: publish a second fleet service
	// definition after the clear. Since the fleet controller's /sync
	// processes events in order, when the sentinel's discovery
	// notification arrives, the clear has already been processed.
	sentinelWatch := watchRoom(t, admin, fleet.FleetRoomID)

	clearFleetService(t, admin, fleet.FleetRoomID, svcStateKey)
	publishFleetService(t, admin, fleet.FleetRoomID, sentinelStateKey, fleetschema.FleetServiceContent{
		Template: ns.Namespace.TemplateRoomAliasLocalpart() + ":sentinel",
		Replicas: fleetschema.ReplicaSpec{Min: 0},
	})

	waitForFleetService(t, &sentinelWatch, fc, sentinelID)

	// Phase 4: Verify the original service is gone from the fleet model.
	var afterListResponse fleetListServicesResponse
	if err := authClient.Call(ctx, "list-services", nil, &afterListResponse); err != nil {
		t.Fatalf("list-services after clear: %v", err)
	}

	for _, service := range afterListResponse.Services {
		if service.Localpart == svcID {
			t.Fatalf("service %q still present in fleet controller after clearing registration", svcID)
		}
	}

	// Clean up sentinel.
	clearFleetService(t, admin, fleet.FleetRoomID, sentinelStateKey)

	t.Log("fleet registration lifecycle: registered, verified, cleared, verified removal")
}
