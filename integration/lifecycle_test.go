// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestMachineLifecycle exercises the full production bootstrap path:
//
//   - Admin provisions a machine via "bureau machine provision" (CLI)
//   - Launcher boots with --bootstrap-file --first-boot-only (registers,
//     rotates password, publishes key)
//   - Verify one-time password was rotated (login with old password fails)
//   - Launcher + daemon start normally from saved session
//   - Verify machine key and status appear in Matrix
//   - Stop the daemon + launcher (SIGTERM)
//   - Restart both from saved session — no re-registration needed
//   - Verify new status heartbeat appears (proves session persistence)
//   - Decommission the machine via "bureau machine decommission" (CLI)
//   - Verify state events cleared from Matrix
func TestMachineLifecycle(t *testing.T) {
	t.Parallel()

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)
	machineRef, err := ref.NewMachine(fleet.Ref, "lifecycle")
	if err != nil {
		t.Fatalf("create machine ref: %v", err)
	}
	machineName := machineRef.Localpart()
	machineUserID := machineRef.UserID()
	machineRoomID := fleet.MachineRoomID

	// --- Phase 1: Provision via CLI ---
	stateDir := t.TempDir()
	bootstrapPath := filepath.Join(stateDir, "bootstrap.json")

	runBureauOrFail(t, "machine", "provision", fleet.Prefix, "lifecycle",
		"--credential-file", credentialFile,
		"--output", bootstrapPath,
	)

	// Read the bootstrap config to capture the one-time password.
	bootstrapConfig, err := bootstrap.ReadConfig(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap config: %v", err)
	}
	if bootstrapConfig.MachineName != machineName {
		t.Fatalf("bootstrap config machine_name = %q, want %q", bootstrapConfig.MachineName, machineName)
	}
	if bootstrapConfig.Password == "" {
		t.Fatal("bootstrap config has empty password")
	}
	oneTimePassword := bootstrapConfig.Password

	// --- Phase 2: First boot with --bootstrap-file --first-boot-only ---
	runDir := tempSocketDir(t)
	launcherSocket := principal.LauncherSocketPath(runDir)
	workspaceRoot := filepath.Join(stateDir, "workspace")
	cacheRoot := filepath.Join(stateDir, "cache")

	// Run the launcher in first-boot-only mode. It should register, rotate
	// the password, publish the key, and exit 0.
	firstBootCmd := exec.Command(launcherBinary,
		"--bootstrap-file", bootstrapPath,
		"--first-boot-only",
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--fleet", fleet.Prefix,
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
	t.Log("first boot completed successfully")

	// Verify the bootstrap file was deleted by the launcher.
	if _, err := os.Stat(bootstrapPath); !os.IsNotExist(err) {
		t.Errorf("bootstrap file should have been deleted after first boot, but still exists")
	}

	// Verify keypair was generated.
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")
	publicKeyBytes, err := os.ReadFile(publicKeyPath)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))
	if publicKey == "" {
		t.Fatal("public key file is empty")
	}

	// Verify the machine key was published to #bureau/machine.
	// The launcher published this during first boot (which already completed),
	// so the event exists in the room state — read it directly.
	machineKeyJSON, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		t.Fatalf("get machine key: %v", err)
	}
	var machineKey struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}
	if machineKey.Algorithm != "age-x25519" {
		t.Errorf("machine key algorithm = %q, want age-x25519", machineKey.Algorithm)
	}
	if machineKey.PublicKey != publicKey {
		t.Errorf("published key = %q, local key = %q", machineKey.PublicKey, publicKey)
	}

	// --- Phase 3: Verify password rotation ---
	// The one-time password should no longer work. Try to log in with it.
	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create matrix client: %v", err)
	}
	oneTimePasswordBuffer, err := secret.NewFromString(oneTimePassword)
	if err != nil {
		t.Fatalf("create one-time password buffer: %v", err)
	}
	_, loginError := matrixClient.Login(ctx, machineName, oneTimePasswordBuffer)
	oneTimePasswordBuffer.Close()
	if loginError == nil {
		t.Error("login with one-time password should have failed after rotation, but succeeded")
	} else {
		t.Logf("one-time password correctly rejected: %v", loginError)
	}

	// --- Phase 4: Normal startup (launcher + daemon from saved session) ---
	// Use a subtest so we get clean process cleanup before Phase 6.
	t.Run("RunningPhase", func(t *testing.T) {
		startProcess(t, "launcher", launcherBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machineName,
			"--server-name", testServerName,
			"--fleet", fleet.Prefix,
			"--run-dir", runDir,
			"--state-dir", stateDir,
			"--workspace-root", workspaceRoot,
			"--cache-root", cacheRoot,
			"--proxy-binary", proxyBinary,
		)
		waitForFile(t, launcherSocket)

		statusWatch := watchRoom(t, admin, fleet.MachineRoomID)

		startProcess(t, "daemon", daemonBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machineName,
			"--server-name", testServerName,
			"--run-dir", runDir,
			"--state-dir", stateDir,
			"--admin-user", "bureau-admin",
			"--status-interval", "2s",
			"--fleet", fleet.Prefix,
		)

		// Wait for MachineStatus heartbeat.
		statusJSON := statusWatch.WaitForStateEvent(t,
			schema.EventTypeMachineStatus, machineName)
		var status struct {
			Principal string `json:"principal"`
		}
		if err := json.Unmarshal(statusJSON, &status); err != nil {
			t.Fatalf("unmarshal machine status: %v", err)
		}
		if status.Principal != machineUserID {
			t.Errorf("machine status principal = %q, want %q", status.Principal, machineUserID)
		}

		// Verify the config room was created.
		configAlias := machineRef.RoomAlias()
		configRoomID, err := admin.ResolveAlias(ctx, configAlias)
		if err != nil {
			t.Fatalf("config room not created: %v", err)
		}
		if configRoomID == "" {
			t.Fatal("config room resolved to empty room ID")
		}

		// Verify machine appears in "bureau machine list".
		listOutput := runBureauOrFail(t, "machine", "list", fleet.Prefix,
			"--homeserver", testHomeserverURL,
			"--token", admin.AccessToken(),
			"--user-id", "@bureau-admin:"+testServerName,
		)
		if !strings.Contains(listOutput, machineName) {
			t.Errorf("bureau machine list output does not contain %q:\n%s", machineName, listOutput)
		}
	})
	// Subtest cleanup stops daemon and launcher (LIFO).

	// --- Phase 5: Restart and verify session persistence ---
	t.Run("RestartPhase", func(t *testing.T) {
		// Clean up the old launcher socket (it was removed by cleanup,
		// but the path may have been partially cleaned).
		os.Remove(launcherSocket)

		startProcess(t, "launcher-restart", launcherBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machineName,
			"--server-name", testServerName,
			"--fleet", fleet.Prefix,
			"--run-dir", runDir,
			"--state-dir", stateDir,
			"--workspace-root", workspaceRoot,
			"--cache-root", cacheRoot,
			"--proxy-binary", proxyBinary,
		)
		waitForFile(t, launcherSocket)

		statusWatch := watchRoom(t, admin, fleet.MachineRoomID)

		startProcess(t, "daemon-restart", daemonBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machineName,
			"--server-name", testServerName,
			"--run-dir", runDir,
			"--state-dir", stateDir,
			"--admin-user", "bureau-admin",
			"--status-interval", "2s",
			"--fleet", fleet.Prefix,
		)

		// Wait for a fresh MachineStatus heartbeat after restart.
		statusWatch.WaitForStateEvent(t,
			schema.EventTypeMachineStatus, machineName)
		t.Log("machine reconnected and published status after restart")
	})

	// --- Phase 6: Decommission ---
	runBureauOrFail(t, "machine", "decommission", fleet.Prefix, "lifecycle",
		"--credential-file", credentialFile,
	)

	// Verify machine key was cleared (empty content).
	clearedKeyJSON, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		t.Fatalf("get machine key after decommission: %v", err)
	}
	var clearedKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(clearedKeyJSON, &clearedKey); err == nil && clearedKey.PublicKey != "" {
		t.Errorf("machine key public_key should be empty after decommission, got %q", clearedKey.PublicKey)
	}

	// Verify machine status was cleared.
	clearedStatusJSON, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineStatus, machineName)
	if err != nil {
		t.Fatalf("get machine status after decommission: %v", err)
	}
	var clearedStatus struct {
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal(clearedStatusJSON, &clearedStatus); err == nil && clearedStatus.Principal != "" {
		t.Errorf("machine status should be empty after decommission, got principal=%q", clearedStatus.Principal)
	}

	t.Log("machine lifecycle complete: provision → bootstrap → run → restart → decommission")
}

// TestTwoMachineFleet provisions two machines, bootstraps both, verifies
// they can see each other in the fleet, assigns a principal to each, and
// verifies the principals can exchange messages through their respective
// proxies. This is the proof point that the multi-machine system works.
func TestTwoMachineFleet(t *testing.T) {
	t.Parallel()

	const principalALocalpart = "agent/fleet-a"
	const principalBLocalpart = "agent/fleet-b"
	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	twoMachineFleet := createTestFleet(t, admin)
	machineARef, err := ref.NewMachine(twoMachineFleet.Ref, "fleet-a")
	if err != nil {
		t.Fatalf("create machine ref: %v", err)
	}
	machineBRef, err := ref.NewMachine(twoMachineFleet.Ref, "fleet-b")
	if err != nil {
		t.Fatalf("create machine ref: %v", err)
	}
	machineAName := machineARef.Localpart()
	machineBName := machineBRef.Localpart()
	machineRoomID := twoMachineFleet.MachineRoomID

	// --- Provision both machines ---
	stateDirA := t.TempDir()
	stateDirB := t.TempDir()
	bootstrapPathA := filepath.Join(stateDirA, "bootstrap.json")
	bootstrapPathB := filepath.Join(stateDirB, "bootstrap.json")

	runBureauOrFail(t, "machine", "provision", twoMachineFleet.Prefix, "fleet-a",
		"--credential-file", credentialFile,
		"--output", bootstrapPathA,
	)
	runBureauOrFail(t, "machine", "provision", twoMachineFleet.Prefix, "fleet-b",
		"--credential-file", credentialFile,
		"--output", bootstrapPathB,
	)
	t.Log("both machines provisioned")

	// --- First boot both machines ---
	runDirA := tempSocketDir(t)
	runDirB := tempSocketDir(t)

	type machineSetup struct {
		name          string
		machineRef    ref.Machine
		stateDir      string
		runDir        string
		bootstrapPath string
		workspaceRoot string
		cacheRoot     string
	}

	setupMachine := func(name string, machineRef ref.Machine, stateDir, runDir, bootstrapPath string) machineSetup {
		return machineSetup{
			name:          name,
			machineRef:    machineRef,
			stateDir:      stateDir,
			runDir:        runDir,
			bootstrapPath: bootstrapPath,
			workspaceRoot: filepath.Join(stateDir, "workspace"),
			cacheRoot:     filepath.Join(stateDir, "cache"),
		}
	}

	machineA := setupMachine(machineAName, machineARef, stateDirA, runDirA, bootstrapPathA)
	machineB := setupMachine(machineBName, machineBRef, stateDirB, runDirB, bootstrapPathB)

	// First-boot both in sequence (each is fast — just login + rotate + publish).
	for _, machine := range []machineSetup{machineA, machineB} {
		firstBootCmd := exec.Command(launcherBinary,
			"--bootstrap-file", machine.bootstrapPath,
			"--first-boot-only",
			"--machine-name", machine.name,
			"--server-name", testServerName,
			"--fleet", twoMachineFleet.Prefix,
			"--run-dir", machine.runDir,
			"--state-dir", machine.stateDir,
			"--workspace-root", machine.workspaceRoot,
			"--cache-root", machine.cacheRoot,
		)
		firstBootCmd.Stdout = os.Stderr
		firstBootCmd.Stderr = os.Stderr
		if err := firstBootCmd.Run(); err != nil {
			t.Fatalf("first boot %s failed: %v", machine.name, err)
		}
	}
	t.Log("both machines completed first boot")

	// Verify both machine keys are published. First boot already completed,
	// so these events exist in room state — read them directly.
	if _, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineKey, machineAName); err != nil {
		t.Fatalf("get machine A key: %v", err)
	}
	if _, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineKey, machineBName); err != nil {
		t.Fatalf("get machine B key: %v", err)
	}

	// --- Start both launcher+daemon pairs ---
	startMachineProcesses := func(t *testing.T, machine machineSetup) {
		t.Helper()
		startProcess(t, machine.name+"-launcher", launcherBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machine.name,
			"--server-name", testServerName,
			"--fleet", twoMachineFleet.Prefix,
			"--run-dir", machine.runDir,
			"--state-dir", machine.stateDir,
			"--workspace-root", machine.workspaceRoot,
			"--cache-root", machine.cacheRoot,
			"--proxy-binary", proxyBinary,
		)
		waitForFile(t, principal.LauncherSocketPath(machine.runDir))

		startProcess(t, machine.name+"-daemon", daemonBinary,
			"--homeserver", testHomeserverURL,
			"--machine-name", machine.name,
			"--server-name", testServerName,
			"--run-dir", machine.runDir,
			"--state-dir", machine.stateDir,
			"--admin-user", "bureau-admin",
			"--status-interval", "2s",
			"--fleet", twoMachineFleet.Prefix,
		)
	}

	// Set up a watch before starting daemons to detect their first heartbeats.
	statusWatch := watchRoom(t, admin, twoMachineFleet.MachineRoomID)

	startMachineProcesses(t, machineA)
	startMachineProcesses(t, machineB)

	// Wait for both daemons to publish MachineStatus heartbeats.
	statusWatch.WaitForStateEvent(t,
		schema.EventTypeMachineStatus, machineAName)
	statusWatch.WaitForStateEvent(t,
		schema.EventTypeMachineStatus, machineBName)
	t.Log("both daemons running and publishing status")

	// --- Verify mutual visibility ---
	// Both machines should see each other's keys in #bureau/machine.
	events, err := admin.GetRoomState(ctx, machineRoomID)
	if err != nil {
		t.Fatalf("get machine room state: %v", err)
	}
	keyCount := 0
	for _, event := range events {
		if event.Type == schema.EventTypeMachineKey && event.StateKey != nil {
			contentBytes, _ := json.Marshal(event.Content)
			var key struct {
				PublicKey string `json:"public_key"`
			}
			if json.Unmarshal(contentBytes, &key) == nil && key.PublicKey != "" {
				keyCount++
			}
		}
	}
	// At least our two machines should have keys (there may be others from
	// other tests that ran earlier in the same homeserver, but at minimum
	// these two must be present).
	if keyCount < 2 {
		t.Errorf("expected at least 2 machine keys in #bureau/machine, got %d", keyCount)
	}

	// --- Register principals and provision credentials ---
	principalAAccount := registerPrincipal(t, principalALocalpart, "pass-fleet-a")
	principalBAccount := registerPrincipal(t, principalBLocalpart, "pass-fleet-b")

	type principalSetup struct {
		account      principalAccount
		machineSetup machineSetup
	}

	principals := []principalSetup{
		{principalAAccount, machineA},
		{principalBAccount, machineB},
	}

	// Resolve config rooms (created by daemons) and push credentials + config.
	for _, p := range principals {
		configAlias := p.machineSetup.machineRef.RoomAlias()
		configRoomID, err := admin.ResolveAlias(ctx, configAlias)
		if err != nil {
			t.Fatalf("config room %s not created: %v", configAlias, err)
		}
		if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
			t.Fatalf("admin join config room %s: %v", configAlias, err)
		}

		_, err = credential.Provision(ctx, admin, credential.ProvisionParams{
			Machine:       p.machineSetup.machineRef,
			Principal:     p.account.Localpart,
			MachineRoomID: twoMachineFleet.MachineRoomID,
			Credentials: map[string]string{
				"MATRIX_TOKEN":          p.account.Token,
				"MATRIX_USER_ID":        p.account.UserID,
				"MATRIX_HOMESERVER_URL": testHomeserverURL,
			},
		})
		if err != nil {
			t.Fatalf("provision credentials for %s: %v", p.account.Localpart, err)
		}

		_, err = admin.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig,
			p.machineSetup.name, schema.MachineConfig{
				Principals: []schema.PrincipalAssignment{
					{
						Localpart: p.account.Localpart,
						AutoStart: true,
						MatrixPolicy: &schema.MatrixPolicy{
							AllowJoin: true,
						},
					},
				},
			})
		if err != nil {
			t.Fatalf("push machine config for %s: %v", p.machineSetup.name, err)
		}
	}

	// --- Wait for both proxy sockets ---
	proxySocketA := principal.RunDirSocketPath(machineA.runDir, principalAAccount.Localpart)
	proxySocketB := principal.RunDirSocketPath(machineB.runDir, principalBAccount.Localpart)
	waitForFile(t, proxySocketA)
	waitForFile(t, proxySocketB)
	t.Log("both proxies spawned")

	// Verify proxy identities.
	clientA := proxyHTTPClient(proxySocketA)
	clientB := proxyHTTPClient(proxySocketB)
	whoamiA := proxyWhoami(t, clientA)
	whoamiB := proxyWhoami(t, clientB)
	if whoamiA != principalAAccount.UserID {
		t.Errorf("proxy A whoami = %q, want %q", whoamiA, principalAAccount.UserID)
	}
	if whoamiB != principalBAccount.UserID {
		t.Errorf("proxy B whoami = %q, want %q", whoamiB, principalBAccount.UserID)
	}

	// --- Create a shared room and exchange messages ---
	// Admin creates the room and invites both principals.
	sharedRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Fleet Test Room",
		Preset:     "private_chat",
		Invite:     []string{principalAAccount.UserID, principalBAccount.UserID},
		Visibility: "private",
	})
	if err != nil {
		t.Fatalf("create shared room: %v", err)
	}

	// Both principals join through their proxies.
	proxyJoinRoom(t, clientA, sharedRoom.RoomID)
	proxyJoinRoom(t, clientB, sharedRoom.RoomID)

	// Principal A sends a message.
	messageFromA := testutil.UniqueID("hello-from-fleet-a")
	proxySendMessage(t, clientA, sharedRoom.RoomID, messageFromA)

	// Principal B sends a message.
	messageFromB := testutil.UniqueID("hello-from-fleet-b")
	proxySendMessage(t, clientB, sharedRoom.RoomID, messageFromB)

	// Verify principal B can see A's message (and vice versa).
	// No sleep needed: proxySendMessage returned 200 OK with event_id,
	// meaning the homeserver has persisted the events. A subsequent /sync
	// sees them immediately.
	eventsB := proxySyncRoomTimeline(t, clientB, sharedRoom.RoomID)
	assertMessagePresent(t, eventsB, principalAAccount.UserID, messageFromA)

	eventsA := proxySyncRoomTimeline(t, clientA, sharedRoom.RoomID)
	assertMessagePresent(t, eventsA, principalBAccount.UserID, messageFromB)

	t.Log("cross-machine message exchange verified")

	// --- Decommission both machines ---
	runBureauOrFail(t, "machine", "decommission", twoMachineFleet.Prefix, "fleet-a",
		"--credential-file", credentialFile,
	)
	runBureauOrFail(t, "machine", "decommission", twoMachineFleet.Prefix, "fleet-b",
		"--credential-file", credentialFile,
	)

	// Verify both machine keys are cleared.
	for _, name := range []string{machineAName, machineBName} {
		keyJSON, err := admin.GetStateEvent(ctx, machineRoomID,
			schema.EventTypeMachineKey, name)
		if err != nil {
			t.Fatalf("get machine key for %s after decommission: %v", name, err)
		}
		var key struct {
			PublicKey string `json:"public_key"`
		}
		if json.Unmarshal(keyJSON, &key) == nil && key.PublicKey != "" {
			t.Errorf("machine %s key should be cleared after decommission, got %q", name, key.PublicKey)
		}
	}

	t.Log("two-machine fleet lifecycle complete: provision → boot → message → decommission")
}
