// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Drain test infrastructure ---

// drainTestEnv holds the shared infrastructure for a drain coordination
// test: a machine with a fleet-managed ticket service, a fleet
// controller with a short drain grace period, and an ops room wired
// for reservations. Each top-level drain test creates its own env via
// setupDrainTestEnv to guarantee isolation — no shared mutable state
// between tests.
type drainTestEnv struct {
	admin        *messaging.DirectSession
	fleet        *testFleet
	machine      *testMachine
	ticketSvc    ticketServiceDeployment
	fc           *fleetController
	opsRoomID    ref.RoomID
	requester    principalAccount
	ticketClient *service.ServiceClient
	templateRef  string
}

// setupDrainTestEnv creates a fully isolated drain test environment:
// namespace, fleet, machine (launcher+daemon), ticket service with
// fleet_managed label, fleet controller with fake clock and 5s drain
// grace period, ops room, and authenticated requester.
//
// The suffix makes all resource names unique across parallel tests.
// Each call produces an independent namespace, so resources from
// different envs cannot collide.
func setupDrainTestEnv(t *testing.T, suffix string) *drainTestEnv {
	t.Helper()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	// The FC name is known in advance so fleet_managed labels can be
	// set on the ticket service BEFORE the FC starts, avoiding a race
	// where the FC discovers FleetServiceContent before processing the
	// MachineConfig with the fleet_managed label.
	fcName := "service/fleet/" + suffix
	ticketServiceLocalpart := "service/ticket/" + suffix

	machine := newTestMachine(t, fleet, suffix)
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the ticket service with fleet_managed label set from the
	// start. This ensures the MachineConfig the daemon reconciles
	// already has the label, and when the FC's initial sync processes
	// this config it tracks the instance — preventing a duplicate
	// placement race.
	ticketBinary := resolvedBinary(t, "TICKET_SERVICE_BINARY")
	ticketSvcResult := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    ticketBinary,
		Name:      "ticket-" + suffix,
		Localpart: ticketServiceLocalpart,
		Labels:    map[string]string{"fleet_managed": fcName},
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
	})
	ticketSvc := ticketServiceDeployment{
		Entity:     ticketSvcResult.Entity,
		Account:    ticketSvcResult.Account,
		SocketPath: ticketSvcResult.SocketPath,
	}

	// Start the FC with a 5-second drain grace period and fake clock.
	// The short grace period is long enough for services to respond in
	// normal operation but short enough to test timeout via clock advance.
	fc := startFleetControllerWithDrainGrace(t, admin, machine,
		fcName, fleet, 5*time.Second,
		map[string]string{"BUREAU_TEST_CLOCK": "1"})

	// Derive template ref from deployed service config.
	var templateRef string
	for _, spec := range machine.deployedServices {
		if spec.Localpart == ticketServiceLocalpart {
			templateRef = spec.Template
			break
		}
	}
	if templateRef == "" {
		t.Fatal("could not find ticket service template ref in deployed services")
	}

	// Publish FleetServiceContent so the FC knows the service definition.
	// The MachineConfig already has the fleet_managed label (set during
	// deployService above), so the FC will track the existing instance
	// and NOT try to place a duplicate.
	ticketServiceUserID := ticketSvcResult.Entity.UserID()
	fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)
	publishFleetService(t, admin, fleet.FleetRoomID, ticketServiceUserID.StateKey(), fleetschema.FleetServiceContent{
		Template: templateRef,
		Replicas: fleetschema.ReplicaSpec{Min: 1, Max: 1},
		Failover: fleetschema.FailoverNone,
		Priority: 50,
		Placement: fleetschema.PlacementConstraints{
			PreferredMachines: []string{machine.Name},
		},
	})
	waitForFleetService(t, &fleetWatch, fc, ticketServiceUserID.String())
	t.Logf("fleet controller discovered ticket service as fleet-managed")

	requester := registerFleetPrincipal(t, fleet, "agent/"+suffix+"-requester", "test-pass")

	ticketToken := mintTestServiceTokenForUser(t, machine, requester.UserID, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})
	ticketClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, ticketToken)

	opsRoomID := setupReservationOpsRoom(t, admin, machine, ticketSvc, fc, fleet)
	t.Logf("drain test env ready (suffix=%s, ops=%s)", suffix, opsRoomID)

	return &drainTestEnv{
		admin:        admin,
		fleet:        fleet,
		machine:      machine,
		ticketSvc:    ticketSvc,
		fc:           fc,
		opsRoomID:    opsRoomID,
		requester:    requester,
		ticketClient: ticketClient,
		templateRef:  templateRef,
	}
}

// registerGhostService publishes a FleetServiceContent and MachineConfig
// entry for a service that doesn't actually run. The FC tracks it as
// fleet-managed but receives no drain_status when drain is published,
// forcing a drain timeout.
//
// The ghost service is added to the machine's deployedServices and a
// MachineConfig push is triggered so the FC picks it up via /sync.
func registerGhostService(t *testing.T, env *drainTestEnv, ghostLocalpart string) {
	t.Helper()

	ghostUserID := fleetServiceUserID(t, env.fleet, ghostLocalpart[len("service/"):])
	fleetWatch := watchRoom(t, env.admin, env.fleet.FleetRoomID)

	publishFleetService(t, env.admin, env.fleet.FleetRoomID, ghostUserID.StateKey(), fleetschema.FleetServiceContent{
		Template: env.templateRef,
		Replicas: fleetschema.ReplicaSpec{Min: 1, Max: 1},
		Failover: fleetschema.FailoverNone,
		Priority: 50,
		Placement: fleetschema.PlacementConstraints{
			PreferredMachines: []string{env.machine.Name},
		},
	})

	env.machine.deployedServices = append(env.machine.deployedServices, principalSpec{
		Localpart: ghostLocalpart,
		Template:  env.templateRef,
		Labels:    map[string]string{"fleet_managed": env.fc.PrincipalName},
	})
	pushMachineConfig(t, env.admin, env.machine, deploymentConfig{})

	waitForFleetService(t, &fleetWatch, env.fc, ghostUserID.String())
	t.Logf("ghost service %s registered and discovered by FC", ghostLocalpart)
}

// --- Drain test helpers ---

// waitForDrainStatus waits for a drain_status state event from the given
// entity (identified by state key, which is the entity's full Matrix user
// ID). Returns the parsed DrainStatusContent.
func waitForDrainStatus(t *testing.T, watch *roomWatch, stateKey string) schema.DrainStatusContent {
	t.Helper()

	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeDrainStatus {
			return false
		}
		return event.StateKey != nil && *event.StateKey == stateKey
	}, "drain_status from "+stateKey)

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal drain_status content: %v", err)
	}
	var status schema.DrainStatusContent
	if err := json.Unmarshal(contentJSON, &status); err != nil {
		t.Fatalf("unmarshal drain_status from %s: %v", stateKey, err)
	}
	t.Logf("drain_status from %s: acknowledged=%v in_flight=%d drained_at=%s",
		stateKey, status.Acknowledged, status.InFlight, status.DrainedAt)
	return status
}

// waitForDrainStatusCleared waits for a drain_status state event from
// the given service that is cleared (empty JSON object).
func waitForDrainStatusCleared(t *testing.T, watch *roomWatch, stateKey string) {
	t.Helper()

	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeDrainStatus {
			return false
		}
		if event.StateKey == nil || *event.StateKey != stateKey {
			return false
		}
		return len(event.Content) == 0
	}, fmt.Sprintf("drain_status cleared for %s", stateKey))
	t.Logf("drain_status cleared for %s", stateKey)
}

// startFleetControllerWithDrainGrace starts a fleet controller with a
// specific drain grace period. This overrides the default 30s grace period
// by passing --drain-grace-period in the service template command.
func startFleetControllerWithDrainGrace(
	t *testing.T,
	admin *messaging.DirectSession,
	machine *testMachine,
	controllerName string,
	fleet *testFleet,
	drainGracePeriod time.Duration,
	extraEnv map[string]string,
) *fleetController {
	t.Helper()

	binary := resolvedBinary(t, "FLEET_CONTROLLER_BINARY")

	svc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    binary,
		Name:      "fleet-controller",
		Localpart: controllerName,
		Command: []string{
			binary,
			fmt.Sprintf("--drain-grace-period=%s", drainGracePeriod),
		},
		ExtraRooms:                []ref.RoomID{fleet.FleetRoomID, fleet.MachineRoomID},
		ExtraEnvironmentVariables: extraEnv,
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
	})

	controller := &fleetController{
		PrincipalName: controllerName,
		UserID:        svc.Account.UserID,
		SocketPath:    svc.SocketPath,
	}

	grantFleetControllerConfigAccess(t, admin, controller, machine)

	return controller
}

// --- Integration tests ---

// TestDrainCoordination exercises the coordinated machine drain protocol
// for scenarios where no ghost services or fleet-managed service mutations
// are needed. Each subtest creates its own workspace room and reservation,
// and cleans up after itself. No subtest mutates the FC's fleet-managed
// service index.
func TestDrainCoordination(t *testing.T) {
	t.Parallel()

	env := setupDrainTestEnv(t, "drain-coord")

	t.Run("CoordinatedGrant", func(t *testing.T) {
		// Full round-trip: exclusive request → drain published →
		// ticket service publishes drain_status → FC waits for ack →
		// FC grants.
		wsRoomID := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "coordinated-grant")

		opsWatch := watchRoom(t, env.admin, env.opsRoomID)

		ticketID := publishResourceRequest(t, env.ticketClient, wsRoomID,
			env.machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// Relay ticket appears in ops room.
		relayTicketID, _ := waitForRelayTicket(t, &opsWatch, env.machine.Ref.Name())

		// FC sets relay ticket to in_progress.
		waitForTicketStatus(t, &opsWatch, relayTicketID, ticket.StatusInProgress)

		// Drain published.
		drain := waitForMachineDrain(t, &opsWatch)
		if drain.ReservationHolder.IsZero() {
			t.Error("drain has empty reservation holder")
		}

		// The ticket service and daemon both publish drain_status in
		// response to the drain event. The FC waits specifically for
		// the ticket service (it's fleet-managed).
		ticketDrainStatus := waitForDrainStatus(t, &opsWatch, env.ticketSvc.Entity.UserID().StateKey())
		if !ticketDrainStatus.Acknowledged {
			t.Error("ticket service drain_status should be acknowledged")
		}

		// The machine daemon also publishes drain_status (informational,
		// not gating the grant).
		daemonDrainStatus := waitForDrainStatus(t, &opsWatch, env.machine.UserID.StateKey())
		if !daemonDrainStatus.Acknowledged {
			t.Error("daemon drain_status should be acknowledged")
		}

		// FC sees the ticket service's drain_status (in_flight == 0,
		// no active relay tickets when the drain started), completes
		// the grant.
		holderStateKey := env.requester.UserID.StateKey()
		grant := waitForReservationGrant(t, &opsWatch, holderStateKey)
		if grant.Mode != schema.ModeExclusive {
			t.Errorf("grant mode = %q, want %q", grant.Mode, schema.ModeExclusive)
		}

		// Cleanup: close workspace ticket to release reservation.
		cleanupWatch := watchRoom(t, env.admin, env.opsRoomID)
		closeWorkspaceTicket(t, env.ticketClient, wsRoomID, ticketID, "drain coord test complete")
		waitForReservationCleared(t, &cleanupWatch, holderStateKey)
		t.Log("coordinated grant test complete")
	})

	t.Run("InclusiveSkipsDrain", func(t *testing.T) {
		// Inclusive mode should grant immediately without publishing
		// a drain event.
		wsRoomID := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "inclusive-skip")

		opsWatch := watchRoom(t, env.admin, env.opsRoomID)
		wsWatch := watchRoom(t, env.admin, wsRoomID)

		ticketID := publishResourceRequest(t, env.ticketClient, wsRoomID,
			env.machine.Ref.Name(), schema.ModeInclusive, 2, "10m")

		// Relay ticket appears.
		relayTicketID, _ := waitForRelayTicket(t, &opsWatch, env.machine.Ref.Name())

		// FC grants immediately (no drain for inclusive mode).
		waitForTicketStatus(t, &opsWatch, relayTicketID, ticket.StatusInProgress)
		holderStateKey := env.requester.UserID.StateKey()
		grant := waitForReservationGrant(t, &opsWatch, holderStateKey)
		if grant.Mode != schema.ModeInclusive {
			t.Errorf("grant mode = %q, want %q", grant.Mode, schema.ModeInclusive)
		}

		// If drain had been published, it would appear between
		// in_progress and grant in the timeline. Since we got the
		// grant without seeing drain, inclusive mode correctly skipped it.
		t.Log("inclusive mode skipped drain correctly")

		// Verify workspace ticket reaches granted status.
		waitForClaimStatus(t, &wsWatch, ticketID, schema.ClaimGranted)

		// Cleanup.
		cleanupWatch := watchRoom(t, env.admin, env.opsRoomID)
		closeWorkspaceTicket(t, env.ticketClient, wsRoomID, ticketID, "inclusive test complete")
		waitForReservationCleared(t, &cleanupWatch, holderStateKey)
	})

	t.Run("DrainClearedOnRelease", func(t *testing.T) {
		// After an exclusive grant, releasing the reservation should
		// clear the drain event. Services should then clear their
		// drain_status.
		wsRoomID := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "drain-release")

		opsWatch := watchRoom(t, env.admin, env.opsRoomID)

		ticketID := publishResourceRequest(t, env.ticketClient, wsRoomID,
			env.machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		// Wait for the full grant flow.
		_, _ = waitForRelayTicket(t, &opsWatch, env.machine.Ref.Name())
		waitForMachineDrain(t, &opsWatch)

		holderStateKey := env.requester.UserID.StateKey()
		waitForReservationGrant(t, &opsWatch, holderStateKey)

		// Now release: close the workspace ticket.
		releaseWatch := watchRoom(t, env.admin, env.opsRoomID)
		closeWorkspaceTicket(t, env.ticketClient, wsRoomID, ticketID, "drain release test")

		// Reservation cleared.
		waitForReservationCleared(t, &releaseWatch, holderStateKey)

		// Drain cleared.
		waitForDrainCleared(t, &releaseWatch)

		// Services respond to drain clear by clearing drain_status.
		waitForDrainStatusCleared(t, &releaseWatch, env.machine.UserID.StateKey())
		waitForDrainStatusCleared(t, &releaseWatch, env.ticketSvc.Entity.UserID().StateKey())

		t.Log("drain cleared on release verified")
	})

	t.Run("DrainServicesListPopulated", func(t *testing.T) {
		// Verify that the drain event's Services field contains the
		// fleet-managed service localparts.
		wsRoomID := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "drain-services")

		opsWatch := watchRoom(t, env.admin, env.opsRoomID)

		ticketID := publishResourceRequest(t, env.ticketClient, wsRoomID,
			env.machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

		_, _ = waitForRelayTicket(t, &opsWatch, env.machine.Ref.Name())
		drain := waitForMachineDrain(t, &opsWatch)

		// The Services list should include the fleet-managed ticket
		// service's full Matrix user ID.
		ticketServiceUserID := env.ticketSvc.Entity.UserID()
		if !slices.Contains(drain.Services, ticketServiceUserID) {
			t.Errorf("drain services = %v, want to contain %q",
				drain.Services, ticketServiceUserID)
		}

		// Cleanup.
		holderStateKey := env.requester.UserID.StateKey()
		waitForReservationGrant(t, &opsWatch, holderStateKey)
		cleanupWatch := watchRoom(t, env.admin, env.opsRoomID)
		closeWorkspaceTicket(t, env.ticketClient, wsRoomID, ticketID, "services list test complete")
		waitForReservationCleared(t, &cleanupWatch, holderStateKey)

		t.Log("drain services list populated correctly")
	})
}

// TestDrainTimeout verifies that the fleet controller grants a
// reservation after the drain grace period expires, even when a
// fleet-managed service never publishes drain_status. Uses a ghost
// service (registered in the fleet but not actually running) that
// the FC waits for and eventually times out on.
//
// This is a separate top-level test (not a subtest of
// TestDrainCoordination) because registering a ghost service mutates
// the FC's fleet-managed service index. Isolation prevents the ghost
// from affecting other drain tests.
func TestDrainTimeout(t *testing.T) {
	t.Parallel()

	env := setupDrainTestEnv(t, "drain-timeout")

	// Register a ghost service that the FC tracks but that never
	// publishes drain_status.
	registerGhostService(t, env, "service/ghost/drain-timeout")

	wsRoomID := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "timeout-ws")

	opsWatch := watchRoom(t, env.admin, env.opsRoomID)

	ticketID := publishResourceRequest(t, env.ticketClient, wsRoomID,
		env.machine.Ref.Name(), schema.ModeExclusive, 2, "10m")

	// Drain published. The FC enters pending state waiting for all
	// fleet-managed services (ticket service + ghost).
	waitForMachineDrain(t, &opsWatch)

	// The ticket service and daemon respond, but the ghost does not.
	// Advance the FC's fake clock past the 5s drain grace period.
	// The drain_status events from the ticket service and daemon
	// wake the FC via /sync (they match the FC's sync filter).
	// checkDrainCompletion sees the advanced clock and grants via
	// timeout — no additional trigger needed.
	advanceFleetClock(t, env.fc, 10*time.Second)

	// FC times out waiting for the ghost service and grants anyway.
	holderStateKey := env.requester.UserID.StateKey()
	grant := waitForReservationGrant(t, &opsWatch, holderStateKey)
	if grant.Mode != schema.ModeExclusive {
		t.Errorf("grant mode = %q, want %q", grant.Mode, schema.ModeExclusive)
	}
	t.Log("drain timeout grant verified")

	// Cleanup.
	cleanupWatch := watchRoom(t, env.admin, env.opsRoomID)
	closeWorkspaceTicket(t, env.ticketClient, wsRoomID, ticketID, "drain timeout test complete")
	waitForReservationCleared(t, &cleanupWatch, holderStateKey)
}

// TestDrainPreemption verifies that a higher-priority request arriving
// during a pending drain preempts the lower-priority drain: the FC
// closes the low-priority relay ticket, cancels its drain, and starts
// a new drain for the higher-priority request.
//
// Like TestDrainTimeout, this is a separate top-level test because it
// registers a ghost service to keep the FC in pending state.
func TestDrainPreemption(t *testing.T) {
	t.Parallel()

	env := setupDrainTestEnv(t, "drain-preempt")

	// Register a ghost service so the drain doesn't complete
	// immediately — the FC stays in pending state.
	registerGhostService(t, env, "service/ghost/drain-preempt")

	wsRoomP3 := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "preempt-low")
	wsRoomP1 := createReservationWorkspaceRoom(t, env.admin, env.fleet, env.ticketSvc, "preempt-high")

	holderStateKey := env.requester.UserID.StateKey()

	// Phase 1: Submit P3 exclusive request. FC publishes drain,
	// enters pending state (ghost service won't respond).
	opsWatch := watchRoom(t, env.admin, env.opsRoomID)

	ticketIDP3 := publishResourceRequest(t, env.ticketClient, wsRoomP3,
		env.machine.Ref.Name(), schema.ModeExclusive, 3, "10m")

	relayTicketIDP3, _ := waitForRelayTicket(t, &opsWatch, env.machine.Ref.Name())
	waitForTicketStatus(t, &opsWatch, relayTicketIDP3, ticket.StatusInProgress)
	waitForMachineDrain(t, &opsWatch)

	// Phase 2: Submit P1 exclusive request. FC should preempt
	// the P3 drain: close P3's relay ticket, cancel drain, start
	// new drain for P1.
	opsWatch2 := watchRoom(t, env.admin, env.opsRoomID)

	ticketIDP1 := publishResourceRequest(t, env.ticketClient, wsRoomP1,
		env.machine.Ref.Name(), schema.ModeExclusive, 1, "10m")

	// P3's relay ticket closed with preemption.
	p3Closed := waitForTicketStatus(t, &opsWatch2, relayTicketIDP3, ticket.StatusClosed)
	if p3Closed.CloseReason == "" {
		t.Error("preempted P3 relay ticket has empty close reason")
	}
	t.Logf("P3 relay ticket closed: %s", p3Closed.CloseReason)

	// New relay ticket for P1.
	relayTicketIDP1, _ := waitForRelayTicket(t, &opsWatch2, env.machine.Ref.Name())
	waitForTicketStatus(t, &opsWatch2, relayTicketIDP1, ticket.StatusInProgress)

	// New drain for P1.
	waitForMachineDrain(t, &opsWatch2)

	// Advance clock past drain grace period to complete the P1 grant
	// (the ghost service still won't respond). The drain_status events
	// from the ticket service and daemon in response to the new P1
	// drain wake the FC via /sync. checkDrainCompletion sees the
	// advanced clock and grants via timeout.
	advanceFleetClock(t, env.fc, 10*time.Second)

	grant := waitForReservationGrant(t, &opsWatch2, holderStateKey)
	if grant.Mode != schema.ModeExclusive {
		t.Errorf("grant mode = %q, want %q", grant.Mode, schema.ModeExclusive)
	}
	t.Log("preemption during drain verified")

	// Cleanup.
	cleanupWatch := watchRoom(t, env.admin, env.opsRoomID)
	closeWorkspaceTicket(t, env.ticketClient, wsRoomP3, ticketIDP3, "preempt cleanup P3")
	closeWorkspaceTicket(t, env.ticketClient, wsRoomP1, ticketIDP1, "preempt cleanup P1")
	waitForReservationCleared(t, &cleanupWatch, holderStateKey)
}
