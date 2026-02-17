// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
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
	const machineName = "machine/proxy-crash"
	const principalLocalpart = "agent/proxy-crash"
	machineUserID := "@machine/proxy-crash:" + testServerName
	principalUserID := "@agent/proxy-crash:" + testServerName

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	machineRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasMachine, testServerName))
	if err != nil {
		t.Fatalf("resolve machine room: %v", err)
	}

	fleetRoomID := createFleetRoom(t, admin)

	// --- Phase 1: Provision and first boot ---
	stateDir := t.TempDir()
	bootstrapPath := filepath.Join(stateDir, "bootstrap.json")
	runDir := tempSocketDir(t)
	workspaceRoot := filepath.Join(stateDir, "workspace")
	cacheRoot := filepath.Join(stateDir, "cache")
	launcherSocket := principal.LauncherSocketPath(runDir)

	runBureauOrFail(t, "machine", "provision", machineName,
		"--credential-file", credentialFile,
		"--server-name", testServerName,
		"--fleet-room", fleetRoomID,
		"--output", bootstrapPath,
	)

	bootstrapConfig, err := bootstrap.ReadConfig(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap config: %v", err)
	}
	_ = bootstrapConfig

	firstBootCmd := exec.Command(launcherBinary,
		"--bootstrap-file", bootstrapPath,
		"--first-boot-only",
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--workspace-root", workspaceRoot,
		"--cache-root", cacheRoot,
	)
	firstBootCmd.Stdout = os.Stderr
	firstBootCmd.Stderr = os.Stderr
	if err := firstBootCmd.Run(); err != nil {
		t.Fatalf("first boot failed: %v", err)
	}

	// --- Phase 2: Start launcher + daemon, deploy a principal ---
	startProcess(t, "launcher", launcherBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--workspace-root", workspaceRoot,
		"--cache-root", cacheRoot,
		"--proxy-binary", proxyBinary,
	)
	waitForFile(t, launcherSocket)

	statusWatch := watchRoom(t, admin, machineRoomID)

	startProcess(t, "daemon", daemonBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--admin-user", "bureau-admin",
		"--status-interval", "2s",
		"--fleet-room", fleetRoomID,
	)

	// Wait for the daemon to come alive.
	statusWatch.WaitForStateEvent(t,
		schema.EventTypeMachineStatus, machineName)

	// Resolve config room and push credentials + config.
	configAlias := schema.FullRoomAlias(schema.ConfigRoomAlias(machineName), testServerName)
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room not created: %v", err)
	}
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}

	// Register the principal, provision credentials, and deploy.
	account := registerPrincipal(t, principalLocalpart, "pass-proxy-crash-agent")

	_, err = credential.Provision(ctx, admin, credential.ProvisionParams{
		MachineName: machineName,
		Principal:   principalLocalpart,
		ServerName:  testServerName,
		Credentials: map[string]string{
			"MATRIX_TOKEN":          account.Token,
			"MATRIX_USER_ID":        account.UserID,
			"MATRIX_HOMESERVER_URL": testHomeserverURL,
		},
	})
	if err != nil {
		t.Fatalf("provision credentials: %v", err)
	}

	_, err = admin.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig,
		machineName, schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: principalLocalpart,
					AutoStart: true,
					MatrixPolicy: &schema.MatrixPolicy{
						AllowJoin: true,
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for the proxy socket — proves the sandbox was created.
	proxySocket := principal.RunDirSocketPath(runDir, principalLocalpart)
	waitForFile(t, proxySocket)
	t.Log("principal deployed, proxy socket exists")

	// Verify the proxy works before the crash.
	proxyClient := proxyHTTPClient(proxySocket)
	initialWhoami := proxyWhoami(t, proxyClient)
	if initialWhoami != principalUserID {
		t.Fatalf("initial whoami = %q, want %q", initialWhoami, principalUserID)
	}
	t.Log("proxy serving correctly")

	// --- Phase 3: Get the proxy PID via launcher IPC ---
	proxyPID := launcherListProxyPID(t, launcherSocket, principalLocalpart)
	t.Logf("proxy PID = %d", proxyPID)

	// --- Phase 4: Kill the proxy with SIGKILL ---
	// Set up a room watch BEFORE the kill so we only see messages that
	// arrive after the crash event.
	watch := watchRoom(t, admin, configRoomID)

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
		t, &watch, schema.MsgTypeProxyCrash, machineUserID,
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
	if recoveredWhoami != principalUserID {
		t.Fatalf("recovered whoami = %q, want %q", recoveredWhoami, principalUserID)
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
