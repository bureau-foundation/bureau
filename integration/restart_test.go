// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TestDaemonRestartRecovery verifies that restarting the daemon while the
// launcher continues running does not disrupt active principals:
//
//   - Launcher + daemon are started, a principal is deployed
//   - The daemon is killed (SIGTERM) — launcher and proxy survive
//   - A new daemon is started
//   - The new daemon discovers the pre-existing sandbox via list-sandboxes,
//     adopts it, and publishes a heartbeat with the correct running count
//   - The proxy remains functional throughout (verified via whoami)
func TestDaemonRestartRecovery(t *testing.T) {
	t.Parallel()

	const principalLocalpart = "agent/restart"

	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)
	machine := newTestMachine(t, fleet, "restart")

	options := machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   daemonBinary,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	}

	// --- Phase 1+2: Provision, first boot, start launcher + daemon ---
	startMachineLauncher(t, admin, machine, options)
	daemon1 := startMachineDaemonManual(t, admin, machine, options)

	// --- Deploy a principal ---
	account := registerFleetPrincipal(t, fleet, principalLocalpart, "pass-restart-agent")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:      account,
			MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
		}},
	})

	proxySocket := proxySockets[principalLocalpart]
	t.Log("principal deployed, proxy socket exists")

	// Verify the proxy works before daemon restart.
	proxyClient := proxyHTTPClient(proxySocket)
	initialWhoami := proxyWhoami(t, proxyClient)
	if initialWhoami != account.UserID.String() {
		t.Fatalf("initial whoami = %q, want %q", initialWhoami, account.UserID)
	}

	// Verify initial heartbeat reports 1 running sandbox. The daemon
	// publishes status on its interval; use a watch from the current
	// sync position to catch the next heartbeat.
	runningWatch := watchRoom(t, admin, fleet.MachineRoomID)
	runningWatch.WaitForMachineStatus(t, machine.Name, func(status schema.MachineStatus) bool {
		return status.Sandboxes.Running == 1
	}, "initial heartbeat with Running=1")

	// --- Phase 3: Kill the daemon ---
	t.Log("killing daemon (SIGTERM)")
	daemon1.Process.Signal(syscall.SIGTERM)
	waitDone := make(chan error, 1)
	go func() { waitDone <- daemon1.Wait() }()
	testutil.RequireReceive(t, waitDone, 5*time.Second, "daemon did not exit after SIGTERM")
	t.Log("daemon exited")

	// Verify the launcher socket and proxy socket still exist.
	if _, err := os.Stat(machine.LauncherSocket); err != nil {
		t.Fatalf("launcher socket disappeared after daemon kill: %v", err)
	}
	if _, err := os.Stat(proxySocket); err != nil {
		t.Fatalf("proxy socket disappeared after daemon kill: %v", err)
	}
	t.Log("launcher and proxy survived daemon kill")

	// Verify the proxy is still functional (not just the socket file).
	midWhoami := proxyWhoami(t, proxyClient)
	if midWhoami != account.UserID.String() {
		t.Fatalf("proxy whoami after daemon kill = %q, want %q", midWhoami, account.UserID)
	}

	// --- Phase 4: Clear the heartbeat, start a new daemon ---
	// Publish a sentinel status event so we can distinguish the old
	// daemon's heartbeat from the new one's. The sentinel has an empty
	// principal field; the new daemon will overwrite it with the real one.
	ctx := t.Context()
	_, err := admin.SendStateEvent(ctx, fleet.MachineRoomID, schema.EventTypeMachineStatus,
		machine.Name, map[string]any{
			"principal": "",
			"sandboxes": map[string]any{"running": -1},
		})
	if err != nil {
		t.Fatalf("clear machine status sentinel: %v", err)
	}

	// Set up room watches before starting the new daemon so we can detect
	// the adoption message and heartbeat without matching stale events.
	adoptionWatch := watchRoom(t, admin, machine.ConfigRoomID)
	recoveryWatch := watchRoom(t, admin, fleet.MachineRoomID)

	t.Log("starting new daemon")
	daemon2 := startMachineDaemonManual(t, admin, machine, options)
	t.Cleanup(func() {
		daemon2.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- daemon2.Wait() }()
		testutil.RequireReceive(t, done, 5*time.Second, "daemon2 did not exit during cleanup")
	})

	// --- Phase 5: Verify recovery ---
	// Wait for the new daemon to publish a heartbeat with the correct
	// running count. The sentinel has running=-1, so any valid heartbeat
	// with running >= 0 is from the new daemon.
	recoveryWatch.WaitForMachineStatus(t, machine.Name, func(status schema.MachineStatus) bool {
		return status.Principal == machine.UserID.String() && status.Sandboxes.Running == 1
	}, "heartbeat from new daemon with Running=1")
	t.Log("new daemon adopted sandbox and published correct heartbeat")

	// Verify the proxy is still functional after daemon restart.
	finalWhoami := proxyWhoami(t, proxyClient)
	if finalWhoami != account.UserID.String() {
		t.Fatalf("proxy whoami after daemon restart = %q, want %q", finalWhoami, account.UserID)
	}
	t.Log("proxy survived daemon restart, identity preserved")

	// Verify the adoption was logged to the config room. The watch was set
	// up before starting daemon2, so only messages from the new daemon match.
	adoption := waitForNotification[schema.PrincipalAdoptedMessage](
		t, &adoptionWatch, schema.MsgTypePrincipalAdopted, machine.UserID,
		func(m schema.PrincipalAdoptedMessage) bool {
			return m.Principal == principalLocalpart
		}, "adoption of "+principalLocalpart)
	_ = adoption
	t.Log("adoption message found in config room")

	t.Log("daemon restart recovery verified: proxy undisturbed, heartbeat correct, adoption logged")
}
