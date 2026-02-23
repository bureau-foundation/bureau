// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestProxyCrashRecovery verifies that when a proxy process dies unexpectedly,
// the daemon detects it via watchProxyExit, destroys the orphaned sandbox,
// and re-creates the principal automatically. This validates:
//
//   - Event-driven detection via the launcher's wait-proxy IPC (milliseconds,
//     not dependent on health-check polling)
//   - Correct state cleanup (exit watcher cancelled, running map cleared)
//   - Successful re-reconciliation (new proxy serves the same identity)
//   - CRITICAL message posted to the config room
func TestProxyCrashRecovery(t *testing.T) {
	t.Parallel()
	const principalLocalpart = "agent/proxy-crash"

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)
	machine := newTestMachine(t, fleet, "proxy-crash")

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Deploy a principal ---
	deployment := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Localpart:    principalLocalpart,
			MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
		}},
	})

	proxySocket := deployment.ProxySockets[principalLocalpart]
	t.Log("principal deployed, proxy socket exists")

	// Verify the proxy works before the crash.
	proxyClient := proxyHTTPClient(proxySocket)
	initialWhoami := proxyWhoami(t, proxyClient)
	if initialWhoami != deployment.Accounts[principalLocalpart].UserID.String() {
		t.Fatalf("initial whoami = %q, want %q", initialWhoami, deployment.Accounts[principalLocalpart].UserID)
	}
	t.Log("proxy serving correctly")

	// --- Phase 3: Get the proxy PID via launcher IPC ---
	proxyPID := launcherListProxyPID(t, machine.LauncherSocket, principalLocalpart)
	t.Logf("proxy PID = %d", proxyPID)

	// --- Phase 4: Kill the proxy with SIGKILL ---
	// Set up a room watch BEFORE the kill so we only see messages that
	// arrive after the crash event.
	watch := watchRoom(t, admin, machine.ConfigRoomID)

	t.Log("killing proxy with SIGKILL")
	if err := syscall.Kill(proxyPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill proxy (pid %d): %v", proxyPID, err)
	}

	// --- Phase 5: Verify the daemon detects the death and recovers ---
	// watchProxyExit detects the crash, calls reconcile() to recreate
	// the principal, and posts a proxy_crash notification with status
	// "recovered". This is the definitive signal that the full cycle
	// (detection → cleanup → re-creation) completed.
	waitForNotification[schema.ProxyCrashMessage](
		t, &watch, schema.MsgTypeProxyCrash, machine.UserID,
		func(m schema.ProxyCrashMessage) bool {
			return m.Principal == principalLocalpart && m.Status == "recovered"
		}, "proxy crash recovery for "+principalLocalpart)
	t.Log("recovery message found in config room")

	// --- Phase 6: Verify the new proxy serves the correct identity ---
	// The recovery message guarantees the new sandbox exists and the
	// proxy is accepting connections. A fresh HTTP client is needed
	// because the old transport has a broken connection to the dead proxy.
	newProxyClient := proxyHTTPClient(proxySocket)
	recoveredWhoami := proxyWhoami(t, newProxyClient)
	if recoveredWhoami != deployment.Accounts[principalLocalpart].UserID.String() {
		t.Fatalf("recovered whoami = %q, want %q", recoveredWhoami, deployment.Accounts[principalLocalpart].UserID)
	}
	t.Log("new proxy serves correct identity")

	t.Log("proxy crash recovery verified: detection, cleanup, re-creation, identity preserved")
}

// launcherListProxyPID sends a list-sandboxes IPC request to the launcher
// socket and returns the proxy PID for the named principal. Fails the test
// if the principal is not found in the launcher's sandbox list.
func launcherListProxyPID(t *testing.T, launcherSocket, principalLocalpart string) int {
	t.Helper()

	conn, err := net.Dial("unix", launcherSocket)
	if err != nil {
		t.Fatalf("dial launcher socket: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := ipc.Request{Action: "list-sandboxes"}
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("encode list-sandboxes request: %v", err)
	}

	var response ipc.Response
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decode list-sandboxes response: %v", err)
	}
	if !response.OK {
		t.Fatalf("list-sandboxes failed: %s", response.Error)
	}

	for _, entry := range response.Sandboxes {
		if entry.Localpart == principalLocalpart {
			if entry.ProxyPID == 0 {
				t.Fatalf("proxy PID for %s is 0", principalLocalpart)
			}
			return entry.ProxyPID
		}
	}

	t.Fatalf("principal %s not found in list-sandboxes response (have %d sandboxes)",
		principalLocalpart, len(response.Sandboxes))
	return 0
}
