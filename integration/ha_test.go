// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestHALeaseAcquisition verifies the full HA lease lifecycle for a single
// machine: the daemon detects a critical fleet service, acquires the HA
// lease via the random-backoff-plus-verify protocol, writes a
// PrincipalAssignment to its own config room, and the reconcile loop
// creates a sandbox. This proves the complete path from fleet service
// publication through HA acquisition to a running sandbox.
func TestHALeaseAcquisition(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ha-acq")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register the service principal and push credentials so the daemon
	// can create a sandbox after acquiring the lease.
	serviceLocalpart := "service/ha/acquire"
	serviceAccount := registerFleetPrincipal(t, fleet, serviceLocalpart, "ha-acquire-password")
	pushCredentials(t, admin, machine, serviceAccount)

	// Publish a template for the service.
	templateRef := publishTestAgentTemplate(t, admin, machine, "ha-acquire-agent")

	// Watch the fleet room before publishing the service so we catch
	// the daemon's lease acquisition event.
	fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)
	configWatch := watchRoom(t, admin, machine.ConfigRoomID)

	publishFleetService(t, admin, fleet.FleetRoomID, serviceLocalpart, fleetschema.FleetServiceContent{
		Template: templateRef,
		HAClass:  "critical",
		Replicas: fleetschema.ReplicaSpec{Min: 1},
		Placement: fleetschema.PlacementConstraints{
			PreferredMachines: []string{machine.Name},
			AllowedMachines:   []string{machine.Name},
		},
		Failover: "migrate",
	})

	// Wait for the daemon to acquire the HA lease.
	leaseJSON := fleetWatch.WaitForStateEvent(t,
		schema.EventTypeHALease, serviceLocalpart)
	var lease fleetschema.HALeaseContent
	if err := json.Unmarshal(leaseJSON, &lease); err != nil {
		t.Fatalf("unmarshal HA lease: %v", err)
	}
	if lease.Holder != machine.Name {
		t.Errorf("lease holder = %q, want %q", lease.Holder, machine.Name)
	}
	if lease.Service != serviceLocalpart {
		t.Errorf("lease service = %q, want %q", lease.Service, serviceLocalpart)
	}

	// Wait for the daemon to write the HA-generated PrincipalAssignment.
	configJSON := configWatch.WaitForStateEvent(t,
		schema.EventTypeMachineConfig, machine.Name)
	var config schema.MachineConfig
	if err := json.Unmarshal(configJSON, &config); err != nil {
		t.Fatalf("unmarshal machine config: %v", err)
	}
	var foundAssignment bool
	for _, assignment := range config.Principals {
		if assignment.Principal.AccountLocalpart() == serviceLocalpart {
			foundAssignment = true
			if assignment.Template != templateRef {
				t.Errorf("assignment template = %q, want %q", assignment.Template, templateRef)
			}
			if !assignment.AutoStart {
				t.Error("assignment auto_start should be true")
			}
			break
		}
	}
	if !foundAssignment {
		t.Fatalf("PrincipalAssignment for %q not found in config (got %d principals)",
			serviceLocalpart, len(config.Principals))
	}

	// Wait for the proxy socket — proves the full end-to-end path:
	// HA acquisition → PrincipalAssignment → reconcile → sandbox creation.
	proxySocket := machine.PrincipalSocketPath(t, serviceLocalpart)
	waitForFile(t, proxySocket)

	// Verify the proxy serves the correct identity.
	proxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, proxyClient); whoami != serviceAccount.UserID.String() {
		t.Errorf("whoami = %q, want %q", whoami, serviceAccount.UserID)
	}
}

// TestHALeaseFailover verifies the HA failover path: two machines compete
// for a critical fleet service lease, one acquires it, and when that
// daemon is killed the other detects the expired lease and takes over.
//
// The test does not force which machine wins the initial race (with
// baseDelay=0 both machines acquire instantly and the winner is
// whichever Matrix write lands last). Instead it observes the winner,
// kills that daemon, and verifies the other machine acquires the lease
// and creates a sandbox.
func TestHALeaseFailover(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	// Boot two machines with manual daemon lifecycle so we can kill
	// the winner mid-test. Both use startMachineLauncher +
	// startMachineDaemonManual so we get *exec.Cmd handles.
	machineA := newTestMachine(t, fleet, "ha-fo-a")
	machineB := newTestMachine(t, fleet, "ha-fo-b")

	options := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	}

	startMachineLauncher(t, admin, machineA, options)
	startMachineLauncher(t, admin, machineB, options)

	daemonA := startMachineDaemonManual(t, admin, machineA, options)
	daemonADone := make(chan struct{})
	go func() {
		daemonA.Wait()
		close(daemonADone)
	}()
	t.Cleanup(func() {
		_ = daemonA.Process.Signal(syscall.SIGTERM)
		select {
		case <-daemonADone:
		case <-time.After(5 * time.Second): //nolint:realclock // process cleanup deadline; t.Context() may already be cancelled
		}
	})

	daemonB := startMachineDaemonManual(t, admin, machineB, options)
	daemonBDone := make(chan struct{})
	go func() {
		daemonB.Wait()
		close(daemonBDone)
	}()
	t.Cleanup(func() {
		_ = daemonB.Process.Signal(syscall.SIGTERM)
		select {
		case <-daemonBDone:
		case <-time.After(5 * time.Second): //nolint:realclock // process cleanup deadline; t.Context() may already be cancelled
		}
	})

	// Register the service principal and push credentials to both
	// machines so either can create a sandbox after acquiring the lease.
	serviceLocalpart := "service/ha/failover"
	serviceAccount := registerFleetPrincipal(t, fleet, serviceLocalpart, "ha-failover-password")
	pushCredentials(t, admin, machineA, serviceAccount)
	pushCredentials(t, admin, machineB, serviceAccount)

	// Publish a template and grant access to both machines.
	templateRef := publishTestAgentTemplate(t, admin, machineA, "ha-failover-agent")
	grantTemplateAccess(t, admin, machineB)

	// --- Phase 1: Initial acquisition ---
	fleetWatch := watchRoom(t, admin, fleet.FleetRoomID)

	publishFleetService(t, admin, fleet.FleetRoomID, serviceLocalpart, fleetschema.FleetServiceContent{
		Template: templateRef,
		HAClass:  "critical",
		Replicas: fleetschema.ReplicaSpec{Min: 1},
		Placement: fleetschema.PlacementConstraints{
			PreferredMachines: []string{machineA.Name, machineB.Name},
			AllowedMachines:   []string{machineA.Name, machineB.Name},
		},
		Failover: "migrate",
	})

	// Wait for someone to acquire the lease. With baseDelay=0 both
	// machines race and the winner is non-deterministic. We identify
	// the winner and loser from the lease holder.
	leaseEvent := fleetWatch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeHALease {
			return false
		}
		if event.StateKey == nil || *event.StateKey != serviceLocalpart {
			return false
		}
		// Accept only a lease with a valid holder that isn't expired.
		holder, _ := event.Content["holder"].(string)
		return holder == machineA.Name || holder == machineB.Name
	}, "initial HA lease acquisition")

	holder, _ := leaseEvent.Content["holder"].(string)
	t.Logf("initial lease holder: %s", holder)

	var winner, loser *testMachine
	var winnerDone chan struct{}
	if holder == machineA.Name {
		winner = machineA
		loser = machineB
		winnerDone = daemonADone
	} else {
		winner = machineB
		loser = machineA
		winnerDone = daemonBDone
	}

	// Wait for the proxy socket on the winner.
	winnerProxySocket := winner.PrincipalSocketPath(t, serviceLocalpart)
	waitForFile(t, winnerProxySocket)

	proxyClient := proxyHTTPClient(winnerProxySocket)
	if whoami := proxyWhoami(t, proxyClient); whoami != serviceAccount.UserID.String() {
		t.Fatalf("winner whoami = %q, want %q", whoami, serviceAccount.UserID)
	}

	// --- Phase 2: Kill the winner's daemon ---
	failoverWatch := watchRoom(t, admin, fleet.FleetRoomID)

	if holder == machineA.Name {
		daemonA.Process.Signal(syscall.SIGTERM)
	} else {
		daemonB.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-winnerDone:
	case <-t.Context().Done():
		t.Fatal("winner daemon did not exit after SIGTERM")
	}
	t.Logf("winner daemon (%s) exited", winner.Name)

	// --- Phase 3: Verify failover ---
	// The dying daemon writes an expired lease (releaseAllLeases).
	// The loser's daemon detects the expired lease via /sync and
	// acquires it. Wait for the loser's lease, skipping the expired
	// lease from the winner's shutdown.
	failoverWatch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeHALease {
			return false
		}
		if event.StateKey == nil || *event.StateKey != serviceLocalpart {
			return false
		}
		eventHolder, _ := event.Content["holder"].(string)
		return eventHolder == loser.Name
	}, "HA lease failover to "+loser.Name)
	t.Logf("failover lease acquired by %s", loser.Name)

	// Wait for the proxy socket on the loser (now the new holder).
	loserProxySocket := loser.PrincipalSocketPath(t, serviceLocalpart)
	waitForFile(t, loserProxySocket)

	loserProxyClient := proxyHTTPClient(loserProxySocket)
	if whoami := proxyWhoami(t, loserProxyClient); whoami != serviceAccount.UserID.String() {
		t.Errorf("failover whoami = %q, want %q", whoami, serviceAccount.UserID)
	}
}
