// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
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

	const machineName = "machine/restart"
	const principalLocalpart = "agent/restart"
	machineUserID := "@machine/restart:" + testServerName
	principalUserID := "@agent/restart:" + testServerName

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
		"--output", bootstrapPath,
	)

	bootstrapConfig, err := bootstrap.ReadConfig(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap config: %v", err)
	}

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
	_ = bootstrapConfig // Used for reference; password rotation tested elsewhere.

	publicKeyBytes, err := os.ReadFile(filepath.Join(stateDir, "machine-key.pub"))
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))

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
	waitForFile(t, launcherSocket, 15*time.Second)

	// Start the first daemon as a manageable process (not via startProcess,
	// since we need to kill it mid-test without subtest cleanup).
	initialStatusWatch := watchRoom(t, admin, machineRoomID)
	daemon1 := startDaemonProcess(t, daemonBinary, machineName, runDir, stateDir)

	// Wait for the daemon to come alive.
	initialStatusWatch.WaitForStateEvent(t,
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

	// Register the principal and encrypt credentials.
	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create matrix client: %v", err)
	}

	passwordBuffer, err := secret.NewFromString("pass-restart-agent")
	if err != nil {
		t.Fatalf("create password buffer: %v", err)
	}
	registrationTokenBuffer, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	principalSession, err := matrixClient.Register(ctx, messaging.RegisterRequest{
		Username:          principalLocalpart,
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	passwordBuffer.Close()
	registrationTokenBuffer.Close()
	if err != nil {
		t.Fatalf("register principal: %v", err)
	}
	principalToken := principalSession.AccessToken()
	principalSession.Close()

	credentialBundle := map[string]string{
		"MATRIX_TOKEN":          principalToken,
		"MATRIX_USER_ID":        principalUserID,
		"MATRIX_HOMESERVER_URL": testHomeserverURL,
	}
	credentialJSON, err := json.Marshal(credentialBundle)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	ciphertext, err := sealed.Encrypt(credentialJSON, []string{publicKey})
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}

	_, err = admin.SendStateEvent(ctx, configRoomID, schema.EventTypeCredentials,
		principalLocalpart, map[string]any{
			"version":        1,
			"principal":      principalUserID,
			"encrypted_for":  []string{machineUserID},
			"keys":           []string{"MATRIX_TOKEN", "MATRIX_USER_ID", "MATRIX_HOMESERVER_URL"},
			"ciphertext":     ciphertext,
			"provisioned_by": "@bureau-admin:" + testServerName,
			"provisioned_at": time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("push credentials: %v", err)
	}

	_, err = admin.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig,
		machineName, map[string]any{
			"principals": []map[string]any{
				{
					"localpart":  principalLocalpart,
					"template":   "",
					"auto_start": true,
					"matrix_policy": map[string]any{
						"allow_join": true,
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for the proxy socket — proves the sandbox was created.
	proxySocket := principal.RunDirSocketPath(runDir, principalLocalpart)
	waitForFile(t, proxySocket, 15*time.Second)
	t.Log("principal deployed, proxy socket exists")

	// Verify the proxy works before daemon restart.
	proxyClient := proxyHTTPClient(proxySocket)
	initialWhoami := proxyWhoami(t, proxyClient)
	if initialWhoami != principalUserID {
		t.Fatalf("initial whoami = %q, want %q", initialWhoami, principalUserID)
	}

	// Verify initial heartbeat reports 1 running sandbox. The daemon
	// publishes status on its interval; use a watch from the current
	// sync position to catch the next heartbeat.
	runningWatch := watchRoom(t, admin, machineRoomID)
	runningWatch.WaitForMachineStatus(t, machineName, func(status schema.MachineStatus) bool {
		return status.Sandboxes.Running == 1
	}, "initial heartbeat with Running=1")

	// --- Phase 3: Kill the daemon ---
	t.Log("killing daemon (SIGTERM)")
	daemon1.Process.Signal(syscall.SIGTERM)
	waitDone := make(chan error, 1)
	go func() { waitDone <- daemon1.Wait() }()
	select {
	case <-waitDone:
		t.Log("daemon exited")
	case <-time.After(5 * time.Second):
		daemon1.Process.Kill()
		<-waitDone
		t.Log("daemon killed after timeout")
	}

	// Verify the launcher socket and proxy socket still exist.
	if _, err := os.Stat(launcherSocket); err != nil {
		t.Fatalf("launcher socket disappeared after daemon kill: %v", err)
	}
	if _, err := os.Stat(proxySocket); err != nil {
		t.Fatalf("proxy socket disappeared after daemon kill: %v", err)
	}
	t.Log("launcher and proxy survived daemon kill")

	// Verify the proxy is still functional (not just the socket file).
	midWhoami := proxyWhoami(t, proxyClient)
	if midWhoami != principalUserID {
		t.Fatalf("proxy whoami after daemon kill = %q, want %q", midWhoami, principalUserID)
	}

	// --- Phase 4: Clear the heartbeat, start a new daemon ---
	// Publish a sentinel status event so we can distinguish the old
	// daemon's heartbeat from the new one's. The sentinel has an empty
	// principal field; the new daemon will overwrite it with the real one.
	_, err = admin.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineStatus,
		machineName, map[string]any{
			"principal": "",
			"sandboxes": map[string]any{"running": -1},
		})
	if err != nil {
		t.Fatalf("clear machine status sentinel: %v", err)
	}

	// Set up room watches before starting the new daemon so we can detect
	// the adoption message and heartbeat without matching stale events.
	adoptionWatch := watchRoom(t, admin, configRoomID)
	recoveryWatch := watchRoom(t, admin, machineRoomID)

	t.Log("starting new daemon")
	daemon2 := startDaemonProcess(t, daemonBinary, machineName, runDir, stateDir)
	t.Cleanup(func() {
		daemon2.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- daemon2.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			daemon2.Process.Kill()
			<-done
		}
	})

	// --- Phase 5: Verify recovery ---
	// Wait for the new daemon to publish a heartbeat with the correct
	// running count. The sentinel has running=-1, so any valid heartbeat
	// with running >= 0 is from the new daemon.
	recoveryWatch.WaitForMachineStatus(t, machineName, func(status schema.MachineStatus) bool {
		return status.Principal == machineUserID && status.Sandboxes.Running == 1
	}, "heartbeat from new daemon with Running=1")
	t.Log("new daemon adopted sandbox and published correct heartbeat")

	// Verify the proxy is still functional after daemon restart.
	finalWhoami := proxyWhoami(t, proxyClient)
	if finalWhoami != principalUserID {
		t.Fatalf("proxy whoami after daemon restart = %q, want %q", finalWhoami, principalUserID)
	}
	t.Log("proxy survived daemon restart, identity preserved")

	// Verify the adoption was logged to the config room. The watch was set
	// up before starting daemon2, so only messages from the new daemon match.
	adoptionWatch.WaitForMessage(t, "Adopted "+principalLocalpart, machineUserID)
	t.Log("adoption message found in config room")

	t.Log("daemon restart recovery verified: proxy undisturbed, heartbeat correct, adoption logged")
}

// startDaemonProcess starts a daemon binary and returns the *exec.Cmd handle
// for manual lifecycle management. Unlike startProcess, the caller is
// responsible for killing the process. This is needed for tests that kill
// and restart the daemon mid-test.
func startDaemonProcess(t *testing.T, binary, machineName, runDir, stateDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--admin-user", "bureau-admin",
		"--status-interval", "2s",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Logf("daemon started (pid %d)", cmd.Process.Pid)
	return cmd
}
