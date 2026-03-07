// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/observe"
)

// TestFleetCacheConfig exercises the bureau fleet cache CLI command
// end-to-end against a real homeserver: empty read, write with
// validation, populated read, and Matrix API verification.
func TestFleetCacheConfig(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)
	ctx := t.Context()

	credentialFile := writeTestCredentialFile(t,
		testHomeserverURL, admin.UserID().String(), admin.AccessToken())

	// Step 1: Read mode with no cache configured.
	readEmptyOutput := runBureauOrFail(t, "fleet", "cache", fleet.Prefix,
		"--credential-file", credentialFile, "--json")

	var readEmptyResult struct {
		Cache schema.FleetCacheContent `json:"cache"`
	}
	if err := json.Unmarshal([]byte(readEmptyOutput), &readEmptyResult); err != nil {
		t.Fatalf("unmarshal empty read: %v\noutput: %s", err, readEmptyOutput)
	}
	if readEmptyResult.Cache.URL != "" {
		t.Errorf("expected empty URL before configuration, got %q", readEmptyResult.Cache.URL)
	}

	// Step 2: Write mode — configure the fleet cache.
	cacheURL := "http://test-cache.local:5580/main"
	cacheName := "main"
	publicKey := "test-key:abc123def456"

	writeOutput := runBureauOrFail(t, "fleet", "cache", fleet.Prefix,
		"--url", cacheURL,
		"--name", cacheName,
		"--public-key", publicKey,
		"--credential-file", credentialFile, "--json")

	var writeResult struct {
		Cache schema.FleetCacheContent `json:"cache"`
	}
	if err := json.Unmarshal([]byte(writeOutput), &writeResult); err != nil {
		t.Fatalf("unmarshal write: %v\noutput: %s", err, writeOutput)
	}
	if writeResult.Cache.URL != cacheURL {
		t.Errorf("write result URL = %q, want %q", writeResult.Cache.URL, cacheURL)
	}
	if writeResult.Cache.Name != cacheName {
		t.Errorf("write result Name = %q, want %q", writeResult.Cache.Name, cacheName)
	}
	if len(writeResult.Cache.PublicKeys) != 1 || writeResult.Cache.PublicKeys[0] != publicKey {
		t.Errorf("write result PublicKeys = %v, want [%q]", writeResult.Cache.PublicKeys, publicKey)
	}

	// Step 3: Read mode — verify persisted values.
	readOutput := runBureauOrFail(t, "fleet", "cache", fleet.Prefix,
		"--credential-file", credentialFile, "--json")

	var readResult struct {
		Cache schema.FleetCacheContent `json:"cache"`
	}
	if err := json.Unmarshal([]byte(readOutput), &readResult); err != nil {
		t.Fatalf("unmarshal read: %v\noutput: %s", err, readOutput)
	}
	if readResult.Cache.URL != cacheURL {
		t.Errorf("read result URL = %q, want %q", readResult.Cache.URL, cacheURL)
	}
	if readResult.Cache.Name != cacheName {
		t.Errorf("read result Name = %q, want %q", readResult.Cache.Name, cacheName)
	}
	if len(readResult.Cache.PublicKeys) != 1 || readResult.Cache.PublicKeys[0] != publicKey {
		t.Errorf("read result PublicKeys = %v, want [%q]", readResult.Cache.PublicKeys, publicKey)
	}

	// Step 4: Verify via Matrix API directly.
	cacheContent, err := messaging.GetState[schema.FleetCacheContent](
		ctx, admin, fleet.FleetRoomID, schema.EventTypeFleetCache, "")
	if err != nil {
		t.Fatalf("GetState fleet cache: %v", err)
	}
	if cacheContent.URL != cacheURL {
		t.Errorf("Matrix state URL = %q, want %q", cacheContent.URL, cacheURL)
	}
	if cacheContent.Name != cacheName {
		t.Errorf("Matrix state Name = %q, want %q", cacheContent.Name, cacheName)
	}
	if len(cacheContent.PublicKeys) != 1 || cacheContent.PublicKeys[0] != publicKey {
		t.Errorf("Matrix state PublicKeys = %v, want [%q]", cacheContent.PublicKeys, publicKey)
	}
}

// TestDaemonSyncsFleetCache verifies the daemon→doctor data flow:
// when a fleet cache event is published to the fleet room, the daemon
// picks it up via /sync and writes it to daemon-status.json.
func TestDaemonSyncsFleetCache(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "cache")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
	})

	statusPath := filepath.Join(machine.RunDir, schema.DaemonStatusFilename)

	// Step 1: Verify daemon-status.json exists with no fleet cache.
	statusData, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read daemon status: %v", err)
	}
	var initialStatus schema.DaemonStatus
	if err := json.Unmarshal(statusData, &initialStatus); err != nil {
		t.Fatalf("unmarshal initial status: %v", err)
	}
	if initialStatus.FleetCache != nil {
		t.Fatalf("expected nil FleetCache before publishing, got %+v", initialStatus.FleetCache)
	}

	// Step 2: Publish fleet cache event to the fleet room.
	cacheURL := "http://test-daemon-cache.local:5580/main"
	publicKey := "daemon-test-key:xyz789"
	cacheContent := schema.FleetCacheContent{
		URL:        cacheURL,
		Name:       "main",
		PublicKeys: []string{publicKey},
	}
	_, err = admin.SendStateEvent(t.Context(), fleet.FleetRoomID,
		schema.EventTypeFleetCache, "", cacheContent)
	if err != nil {
		t.Fatalf("publish fleet cache event: %v", err)
	}

	// Step 3: Wait for daemon to sync the fleet cache to daemon-status.json.
	// The daemon's incremental /sync picks up the fleet room state change,
	// calls syncFleetCache(), and rewrites daemon-status.json.
	waitForFileContent(t, statusPath, func(data []byte) bool {
		var status schema.DaemonStatus
		if err := json.Unmarshal(data, &status); err != nil {
			return false
		}
		return status.FleetCache != nil && status.FleetCache.URL == cacheURL
	})

	// Step 4: Full verification of the synced content.
	finalData, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read final daemon status: %v", err)
	}
	var finalStatus schema.DaemonStatus
	if err := json.Unmarshal(finalData, &finalStatus); err != nil {
		t.Fatalf("unmarshal final status: %v", err)
	}
	if finalStatus.FleetCache == nil {
		t.Fatal("FleetCache still nil after sync")
	}
	if finalStatus.FleetCache.URL != cacheURL {
		t.Errorf("synced URL = %q, want %q", finalStatus.FleetCache.URL, cacheURL)
	}
	if finalStatus.FleetCache.Name != "main" {
		t.Errorf("synced Name = %q, want %q", finalStatus.FleetCache.Name, "main")
	}
	if len(finalStatus.FleetCache.PublicKeys) != 1 || finalStatus.FleetCache.PublicKeys[0] != publicKey {
		t.Errorf("synced PublicKeys = %v, want [%q]", finalStatus.FleetCache.PublicKeys, publicKey)
	}

	t.Logf("daemon synced fleet cache: url=%s keys=%d",
		finalStatus.FleetCache.URL, len(finalStatus.FleetCache.PublicKeys))
}

// TestMachineJoinsFleet verifies the complete machine bootstrap path:
// launcher registers with the homeserver and publishes its key, daemon
// starts, performs initial sync, creates the config room, and publishes
// a MachineStatus heartbeat. This proves the full launcher→daemon→Matrix
// lifecycle works end-to-end.
func TestMachineJoinsFleet(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
	})

	// startMachine already proved: key published, status heartbeat received,
	// config room created. The assertions below verify the content details
	// that startMachine doesn't check.
	ctx := t.Context()

	// Verify machine key algorithm and value.
	machineKeyJSON, err := admin.GetStateEvent(ctx, machine.MachineRoomID,
		schema.EventTypeMachineKey, machine.UserID.StateKey())
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
	if machineKey.PublicKey != machine.PublicKey {
		t.Errorf("published key = %q, startMachine captured = %q", machineKey.PublicKey, machine.PublicKey)
	}

	// Verify MachineStatus contents.
	statusJSON, err := admin.GetStateEvent(ctx, machine.MachineRoomID,
		schema.EventTypeMachineStatus, machine.UserID.StateKey())
	if err != nil {
		t.Fatalf("get machine status: %v", err)
	}
	var status struct {
		Principal string `json:"principal"`
		Sandboxes struct {
			Running int `json:"running"`
		} `json:"sandboxes"`
		UptimeSeconds int64 `json:"uptime_seconds"`
	}
	if err := json.Unmarshal(statusJSON, &status); err != nil {
		t.Fatalf("unmarshal machine status: %v", err)
	}
	if status.Principal != machine.UserID.String() {
		t.Errorf("machine status principal = %q, want %q", status.Principal, machine.UserID)
	}
	if status.Sandboxes.Running != 0 {
		t.Errorf("expected 0 running sandboxes, got %d", status.Sandboxes.Running)
	}
	if status.UptimeSeconds == 0 {
		t.Error("machine status has zero uptime")
	}

	// Verify config room power levels.
	powerLevelJSON, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		schema.MatrixEventTypePowerLevels, "")
	if err != nil {
		t.Fatalf("get config room power levels: %v", err)
	}
	var powerLevels struct {
		Users map[string]int `json:"users"`
	}
	if err := json.Unmarshal(powerLevelJSON, &powerLevels); err != nil {
		t.Fatalf("unmarshal power levels: %v", err)
	}
	adminUserID := admin.UserID().String()
	if level, ok := powerLevels.Users[adminUserID]; !ok || level != 100 {
		t.Errorf("admin power level = %d (present=%v), want 100", level, ok)
	}
	if level, ok := powerLevels.Users[machine.UserID.String()]; !ok || level != 50 {
		t.Errorf("machine power level = %d (present=%v), want 50", level, ok)
	}

	// Verify fleet service binding from provisioning. Provision()
	// publishes an m.bureau.service_binding with state key "fleet" so the
	// machine can discover the fleet controller immediately when its daemon
	// starts, even before any fleet controller is deployed. No fleet
	// controller is running in this test — the binding comes purely from
	// machine.Provision() calling fleet.PublishFleetBindingToRoom().
	bindingJSON, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "fleet")
	if err != nil {
		t.Fatalf("get fleet service binding from config room: %v", err)
	}
	var binding schema.ServiceBindingContent
	if err := json.Unmarshal(bindingJSON, &binding); err != nil {
		t.Fatalf("unmarshal fleet service binding: %v", err)
	}
	if binding.Principal.IsZero() {
		t.Fatal("fleet service binding principal is zero")
	}
	// The binding should reference the fleet controller identity:
	// service/fleet within the test fleet.
	if accountLocalpart := binding.Principal.AccountLocalpart(); accountLocalpart != "service/fleet" {
		t.Errorf("fleet binding principal account localpart = %q, want %q",
			accountLocalpart, "service/fleet")
	}

	// Verify dev team metadata on fleet rooms and config room.
	// EnsureFleetRooms publishes m.bureau.dev_team on all three fleet
	// rooms, and machine.Provision publishes it on the config room.
	// All should point to the namespace's conventional dev team alias.
	expectedDevTeam := schema.DevTeamRoomAlias(fleet.Ref.Namespace())
	devTeamRooms := []struct {
		name   string
		roomID ref.RoomID
	}{
		{"fleet config", fleet.FleetRoomID},
		{"fleet machine", fleet.MachineRoomID},
		{"fleet service", fleet.ServiceRoomID},
		{"machine config", machine.ConfigRoomID},
	}
	for _, room := range devTeamRooms {
		devTeam, err := messaging.GetState[schema.DevTeamContent](ctx, admin, room.roomID, schema.EventTypeDevTeam, "")
		if err != nil {
			t.Errorf("%s room: %v", room.name, err)
			continue
		}
		if devTeam.Room != expectedDevTeam {
			t.Errorf("%s room: dev team = %s, want %s", room.name, devTeam.Room, expectedDevTeam)
		}
	}
}

// TestPrincipalAssignment verifies the full sandbox lifecycle: admin writes
// a MachineConfig assigning a principal, the daemon reconciles, the launcher
// creates a proxy, and the proxy correctly serves the principal's Matrix
// identity. This proves credential encryption, IPC, proxy spawning, and
// reconciliation work end-to-end.
func TestPrincipalAssignment(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "sandbox")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	deployment := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: "test/agent"}},
	})

	// Verify the proxy serves the principal's identity.
	proxyClient := proxyHTTPClient(deployment.ProxySockets["test/agent"])
	whoamiUserID := proxyWhoami(t, proxyClient)
	if whoamiUserID != deployment.Accounts["test/agent"].UserID.String() {
		t.Errorf("whoami user_id = %q, want %q", whoamiUserID, deployment.Accounts["test/agent"].UserID)
	}

	// Verify MachineStatus reflects the running sandbox. The daemon publishes
	// status on its interval; watch from the current sync position.
	sandboxWatch := watchRoom(t, admin, machine.MachineRoomID)
	sandboxWatch.WaitForMachineStatus(t, machine.UserID.StateKey(), func(status schema.MachineStatus) bool {
		return status.Sandboxes.Running > 0
	}, "MachineStatus with running sandboxes")
}

// TestOperatorFlow verifies the operator-facing observation pipeline:
// list targets, observe a principal's terminal, and confirm authorization
// enforcement. This builds a full stack (launcher + daemon + principal with
// observation allowances), then exercises the daemon's observe socket as
// both a Go library client and through the bureau CLI binary.
func TestOperatorFlow(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "observe")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:     resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:       resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:              fleet,
		ProxyBinary:        resolvedBinary(t, "PROXY_BINARY"),
		ObserveRelayBinary: resolvedBinary(t, "OBSERVE_RELAY_BINARY"),
	})

	// Publish a room-level authorization policy on the config room with
	// observation allowances. This exercises the production path: workspace
	// and config rooms carry RoomAuthorizationPolicy state events with
	// MemberAllowances, and the daemon merges these into each principal's
	// effective allowances during reconcile. Every principal belongs to
	// the config room, so MemberAllowances here apply to all principals
	// on this machine.
	_, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		schema.EventTypeAuthorization, "", schema.RoomAuthorizationPolicy{
			MemberAllowances: []schema.Allowance{
				{Actions: []string{observation.ActionObserve}, Actors: []string{"**:**"}},
				{Actions: []string{observation.ActionReadWrite}, Actors: []string{admin.UserID().Localpart() + ":**"}},
			},
		})
	if err != nil {
		t.Fatalf("publish room authorization policy: %v", err)
	}

	deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: "test/observed"}},
	})

	// Wait for the observe socket to be ready (separate from proxy sockets).
	waitForFile(t, machine.ObserveSocket)

	// Use the per-test admin's credentials for observe socket authentication.
	adminToken := admin.AccessToken()
	adminUserID := admin.UserID().String()

	// --- Sub-test: list targets via Go library ---
	t.Run("ListTargets", func(t *testing.T) {
		// Wait for the daemon's heartbeat to report running sandboxes. This
		// proves d.running has been updated (the daemon counts sandboxes from
		// its running map when publishing the heartbeat). ListTargets reads
		// from the same running map, so a single call suffices after this.
		listWatch := watchRoom(t, admin, machine.MachineRoomID)
		listWatch.WaitForMachineStatus(t, machine.UserID.StateKey(), func(status schema.MachineStatus) bool {
			return status.Sandboxes.Running > 0
		}, "MachineStatus with running sandboxes for ListTargets")

		response, err := observe.ListTargets(machine.ObserveSocket, observe.ListRequest{
			Observer: adminUserID,
			Token:    adminToken,
		})
		if err != nil {
			t.Fatalf("ListTargets: %v", err)
		}

		// Verify the running principal appears in the list.
		var foundPrincipal bool
		for _, entry := range response.Principals {
			if entry.Localpart == "test/observed" {
				foundPrincipal = true
				if !entry.Local {
					t.Error("expected principal to be local")
				}
				if !entry.Observable {
					t.Error("expected principal to be observable")
				}
				if entry.Machine != machine.Name {
					t.Errorf("principal machine = %q, want %q", entry.Machine, machine.Name)
				}
				break
			}
		}
		if !foundPrincipal {
			t.Errorf("principal %q not found in list response (got %d principals)",
				"test/observed", len(response.Principals))
		}

		// Verify the local machine appears.
		var foundMachine bool
		for _, machineEntry := range response.Machines {
			if machineEntry.Name == machine.Name {
				foundMachine = true
				if !machineEntry.Self {
					t.Error("expected machine to be self")
				}
				if !machineEntry.Reachable {
					t.Error("expected machine to be reachable")
				}
				break
			}
		}
		if !foundMachine {
			t.Errorf("machine %q not found in list response (got %d machines)",
				machine.Name, len(response.Machines))
		}
	})

	// --- Sub-test: list targets via CLI binary ---
	t.Run("ListCLI", func(t *testing.T) {
		// Write an operator session file so the CLI can authenticate.
		configHome := t.TempDir()
		sessionDir := filepath.Join(configHome, "bureau")
		os.MkdirAll(sessionDir, 0755)
		sessionFile := filepath.Join(sessionDir, "session.json")
		sessionJSON, _ := json.Marshal(map[string]string{
			"user_id":      adminUserID,
			"access_token": adminToken,
			"homeserver":   testHomeserverURL,
		})
		if err := os.WriteFile(sessionFile, sessionJSON, 0600); err != nil {
			t.Fatalf("write session file: %v", err)
		}

		cmd := exec.Command(bureauBinary, "list", "--json", "--socket", machine.ObserveSocket)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configHome)
		output, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				t.Fatalf("bureau list --json failed: %v\nstderr: %s", err, exitErr.Stderr)
			}
			t.Fatalf("bureau list --json failed: %v", err)
		}

		var listResponse observe.ListResponse
		if err := json.Unmarshal(output, &listResponse); err != nil {
			t.Fatalf("parse list JSON: %v\noutput: %s", err, output)
		}

		var foundPrincipal bool
		for _, entry := range listResponse.Principals {
			if entry.Localpart == "test/observed" {
				foundPrincipal = true
				break
			}
		}
		if !foundPrincipal {
			t.Errorf("CLI: principal %q not found in list output", "test/observed")
		}
	})

	// --- Sub-test: observe handshake via Go library ---
	t.Run("ObserveHandshake", func(t *testing.T) {
		session, err := observe.Connect(machine.ObserveSocket, observe.ObserveRequest{
			Principal: "test/observed",
			Mode:      "readwrite",
			Observer:  adminUserID,
			Token:     adminToken,
		})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer session.Close()

		if session.Metadata.Principal != "test/observed" {
			t.Errorf("metadata principal = %q, want %q",
				session.Metadata.Principal, "test/observed")
		}
		if session.Metadata.Machine != machine.Name {
			t.Errorf("metadata machine = %q, want %q",
				session.Metadata.Machine, machine.Name)
		}
	})

	// --- Sub-test: observe denied without authentication ---
	t.Run("ObserveDeniedUnauthorized", func(t *testing.T) {
		_, err := observe.Connect(machine.ObserveSocket, observe.ObserveRequest{
			Principal: "test/observed",
			Mode:      "readwrite",
			Observer:  "@nobody:" + testServerName,
			Token:     "invalid-token-that-should-be-rejected",
		})
		if err == nil {
			t.Fatal("expected observe to be denied with invalid token")
		}
		if !strings.Contains(err.Error(), "denied") && !strings.Contains(err.Error(), "authentication") {
			t.Logf("observe error (expected auth failure): %v", err)
		}
	})
}

// TestCredentialRotation verifies that the daemon detects credential changes
// and restarts the affected principal's sandbox. The flow:
//   - Deploy a principal with the original registration token
//   - Verify the proxy serves the correct identity
//   - Log in again to obtain a new access token
//   - Push new encrypted credentials with the new token
//   - Verify the proxy socket disappears (old sandbox destroyed) and
//     reappears (new sandbox created with updated credentials)
//   - Verify the restarted proxy still serves the correct identity
func TestCredentialRotation(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "rotate")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register and deploy the principal with its initial token.
	password := "rotate-test-password"
	agent := registerFleetPrincipal(t, fleet, "test/rotate", password)
	pushCredentials(t, admin, machine, agent)
	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: agent.Localpart}},
	})

	// Verify the proxy serves the correct identity with the original token.
	proxySocket := machine.PrincipalProxySocketPath(t, agent.Localpart)
	waitForFile(t, proxySocket)
	proxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, proxyClient); whoami != agent.UserID.String() {
		t.Fatalf("initial whoami = %q, want %q", whoami, agent.UserID)
	}

	// Log in again to get a fresh access token. This creates a new device
	// on the homeserver — the original token remains valid but the new one
	// is what we'll push as updated credentials.
	rotated := loginFleetPrincipal(t, fleet, agent.Localpart, password)
	if rotated.Token == agent.Token {
		t.Fatal("login returned the same token as registration (expected a new device token)")
	}

	// Push new credentials. The ciphertext will differ because:
	//   (a) the token payload is different, and
	//   (b) age encryption uses a fresh ephemeral key per Encrypt call.
	// The daemon compares ciphertext on each reconcile cycle and updates
	// the proxy's credentials in-place via the admin socket.
	watch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// Wait for the daemon to update credentials in-place. The daemon
	// pushes decrypted credentials to the proxy admin socket and posts
	// a credentials_rotated notification with status "live_updated".
	waitForNotification[schema.CredentialsRotatedMessage](
		t, &watch, schema.MsgTypeCredentialsRotated, machine.UserID,
		func(m schema.CredentialsRotatedMessage) bool {
			return m.Principal == agent.Localpart && m.Status == schema.CredRotationLiveUpdated
		}, "credentials live-updated for "+agent.Localpart)

	// Verify the proxy serves the correct identity after in-place update.
	// The proxy process is the same (no sandbox restart), but its credential
	// map has been swapped to hold the rotated token.
	if whoami := proxyWhoami(t, proxyClient); whoami != agent.UserID.String() {
		t.Fatalf("post-rotation whoami = %q, want %q", whoami, agent.UserID)
	}

	// Prove the proxy is using the NEW token, not the old one. Log out the
	// original device (invalidates the old token on the homeserver), then
	// verify the proxy's whoami still works — which can only succeed if the
	// proxy's credential injection is using the rotated token.
	logoutToken(t, agent.Token)

	if whoami := proxyWhoami(t, proxyClient); whoami != agent.UserID.String() {
		t.Fatalf("whoami after old-token logout = %q, want %q (proxy should be using new token)", whoami, agent.UserID)
	}
}

// TestCrossMachineObservation verifies the full cross-machine observation
// pipeline over WebRTC transport. Two machines boot with their own launcher
// and daemon. A principal is deployed on machine A (provider). Machine B
// (consumer) discovers the principal via the service directory, discovers
// machine A's transport address via MachineStatus, and uses the WebRTC
// transport to forward an observation request to machine A.
//
// This exercises the production path end-to-end:
//   - MatrixSignaler SDP exchange through Matrix state events
//   - pion/webrtc PeerConnection establishment over loopback
//   - Data channel multiplexing (SCTP)
//   - HTTP observation protocol forwarding through the data channel
//   - Relay fork on the provider machine
//   - Bidirectional byte bridge through the consumer daemon
//   - Provider-side authorization via the target's observation allowances
func TestCrossMachineObservation(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	ctx := t.Context()

	// Boot two machines. The provider hosts the principal and needs the
	// proxy binary (to create a sandbox) and the observe relay binary
	// (to fork a relay for the observation session). The consumer just
	// needs a daemon — it routes observation requests but doesn't host
	// any principals.
	provider := newTestMachine(t, fleet, "xm-prov")
	consumer := newTestMachine(t, fleet, "xm-cons")

	startMachine(t, admin, provider, machineOptions{
		LauncherBinary:     resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:       resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:              fleet,
		ProxyBinary:        resolvedBinary(t, "PROXY_BINARY"),
		ObserveRelayBinary: resolvedBinary(t, "OBSERVE_RELAY_BINARY"),
	})
	startMachine(t, admin, consumer, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
	})

	// Publish a room-level authorization policy on the provider's config
	// room with observation allowances. This exercises the production
	// path: room-level MemberAllowances are merged into each principal's
	// effective allowances during daemon reconcile. The provider daemon
	// performs authorization locally when forwarding observation sessions.
	_, err := admin.SendStateEvent(ctx, provider.ConfigRoomID,
		schema.EventTypeAuthorization, "", schema.RoomAuthorizationPolicy{
			MemberAllowances: []schema.Allowance{
				{Actions: []string{observation.ActionObserve}, Actors: []string{"**:**"}},
				{Actions: []string{observation.ActionReadWrite}, Actors: []string{admin.UserID().Localpart() + ":**"}},
			},
		})
	if err != nil {
		t.Fatalf("publish room authorization policy on provider: %v", err)
	}

	deployPrincipals(t, admin, provider, deploymentConfig{
		Principals: []principalSpec{{Localpart: "test/xm-obs"}},
	})

	// Push a MachineConfig on the consumer. The consumer doesn't host
	// any principals — it routes observation requests to the provider,
	// which performs its own authorization. No observation allowances
	// are needed on the consumer side.
	pushMachineConfig(t, admin, consumer, deploymentConfig{})

	// Publish a service entry in the fleet service room so the consumer
	// daemon can discover the principal on the provider machine. In
	// production, services are registered by the daemon or admin; here
	// we simulate that by pushing the state event directly.
	observedEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "test/xm-obs")
	if err != nil {
		t.Fatalf("construct observed principal entity ref: %v", err)
	}
	_, err = admin.SendStateEvent(ctx, fleet.ServiceRoomID, schema.EventTypeService,
		"test/xm-obs", schema.Service{
			Principal:   observedEntity,
			Machine:     provider.Ref,
			Endpoints:   map[string]string{"cbor": "service.sock"},
			Description: "test principal for cross-machine observation",
		})
	if err != nil {
		t.Fatalf("publish service event: %v", err)
	}

	// Wait for the observe socket on the consumer machine.
	waitForFile(t, consumer.ObserveSocket)

	// Use the per-test admin's credentials for observe socket authentication.
	adminToken := admin.AccessToken()
	adminUserID := admin.UserID().String()

	// --- Sub-test: list targets from consumer shows remote principal ---
	t.Run("ListRemoteTargets", func(t *testing.T) {
		// Poll until the remote principal appears as observable. This is
		// the convergence point that proves two independent subsystems
		// have synced:
		//   (a) Service directory: consumer synced m.bureau.service events
		//   (b) Peer discovery: consumer read provider's MachineStatus
		//       and extracted its transport address
		var response *observe.ListResponse
		for {
			var listError error
			response, listError = observe.ListTargets(consumer.ObserveSocket, observe.ListRequest{
				Observer: adminUserID,
				Token:    adminToken,
			})
			if listError != nil {
				t.Fatalf("ListTargets: %v", listError)
			}

			for _, entry := range response.Principals {
				if entry.Localpart == "test/xm-obs" && entry.Observable {
					t.Logf("remote principal %q discovered (machine=%s, observable=%v)",
						entry.Localpart, entry.Machine, entry.Observable)
					goto discovered
				}
			}

			if t.Context().Err() != nil {
				t.Logf("last list response: %d principals, %d machines",
					len(response.Principals), len(response.Machines))
				for _, principal := range response.Principals {
					t.Logf("  principal: %s (machine=%s local=%v observable=%v)",
						principal.Localpart, principal.Machine, principal.Local, principal.Observable)
				}
				for _, machine := range response.Machines {
					t.Logf("  machine: %s (self=%v reachable=%v)",
						machine.Name, machine.Self, machine.Reachable)
				}
				t.Fatal("timed out waiting for remote principal to appear as observable")
			}
			runtime.Gosched()
		}
	discovered:

		// Verify the principal is remote and observable.
		for _, entry := range response.Principals {
			if entry.Localpart == "test/xm-obs" {
				if entry.Local {
					t.Error("expected principal to be remote, not local")
				}
				if !entry.Observable {
					t.Error("expected principal to be observable")
				}
				if entry.Machine != provider.Name {
					t.Errorf("principal machine = %q, want %q", entry.Machine, provider.Name)
				}
				break
			}
		}

		// Verify the provider machine appears as a reachable peer.
		var foundPeer bool
		for _, machineEntry := range response.Machines {
			if machineEntry.Name == provider.Name {
				foundPeer = true
				if machineEntry.Self {
					t.Error("provider should not be marked as self on consumer's list")
				}
				if !machineEntry.Reachable {
					t.Error("provider should be reachable")
				}
				break
			}
		}
		if !foundPeer {
			t.Errorf("provider machine %q not found in list response", provider.Name)
		}
	})

	// --- Sub-test: observe handshake through WebRTC transport ---
	t.Run("RemoteObserveHandshake", func(t *testing.T) {
		// This is the big one: the operator connects to the consumer's
		// observe socket, the consumer dials the provider via WebRTC,
		// the provider forks a relay, and the observation stream flows
		// back through the data channel to the operator.
		session, err := observe.Connect(consumer.ObserveSocket, observe.ObserveRequest{
			Principal: "test/xm-obs",
			Mode:      "readwrite",
			Observer:  adminUserID,
			Token:     adminToken,
		})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer session.Close()

		// The metadata comes from the relay on the provider machine,
		// bridged through the WebRTC data channel and the consumer daemon.
		if session.Metadata.Principal != "test/xm-obs" {
			t.Errorf("metadata principal = %q, want %q",
				session.Metadata.Principal, "test/xm-obs")
		}
		if session.Metadata.Machine != provider.Name {
			t.Errorf("metadata machine = %q, want %q (should be the provider)",
				session.Metadata.Machine, provider.Name)
		}
	})
}

// TestConfigReconciliation verifies the daemon's core reconciliation loop:
// detecting MachineConfig changes via /sync, creating and destroying
// sandboxes through launcher IPC, and correctly tracking running state.
// Each phase pushes a new config and verifies the daemon converges:
//   - Add one principal → proxy appears with correct identity
//   - Add a second principal → new proxy appears, first still running
//   - Remove the first → its proxy is destroyed, second still running
//   - Remove all → all proxies destroyed, sandbox count returns to zero
func TestConfigReconciliation(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "reconcile")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register both principals upfront (Matrix account creation only —
	// no sandboxes until credentials and config are pushed).
	alpha := registerFleetPrincipal(t, fleet, "test/alpha", "alpha-password")
	beta := registerFleetPrincipal(t, fleet, "test/beta", "beta-password")

	// Push encrypted credentials for both principals. Credentials are
	// inert until a MachineConfig references the principal — the daemon
	// only acts on principals listed in the config with auto_start=true.
	pushCredentials(t, admin, machine, alpha)
	pushCredentials(t, admin, machine, beta)

	alphaSocket := machine.PrincipalProxySocketPath(t, alpha.Localpart)
	betaSocket := machine.PrincipalProxySocketPath(t, beta.Localpart)

	// --- Phase 1: deploy alpha ---
	t.Run("AddFirstPrincipal", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{{Localpart: alpha.Localpart}},
		})
		waitForFile(t, alphaSocket)

		alphaClient := proxyHTTPClient(alphaSocket)
		if whoami := proxyWhoami(t, alphaClient); whoami != alpha.UserID.String() {
			t.Errorf("alpha whoami = %q, want %q", whoami, alpha.UserID)
		}

		// Beta should not be running — no config references it yet.
		if _, err := os.Stat(betaSocket); err == nil {
			t.Error("beta proxy socket exists before being configured")
		}
	})

	// --- Phase 2: add beta alongside alpha ---
	t.Run("AddSecondPrincipal", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{
				{Localpart: alpha.Localpart},
				{Localpart: beta.Localpart},
			},
		})
		waitForFile(t, betaSocket)

		// Beta should now be reachable with the correct identity.
		betaClient := proxyHTTPClient(betaSocket)
		if whoami := proxyWhoami(t, betaClient); whoami != beta.UserID.String() {
			t.Errorf("beta whoami = %q, want %q", whoami, beta.UserID)
		}

		// Alpha should still be running and unaffected by the config update.
		alphaClient := proxyHTTPClient(alphaSocket)
		if whoami := proxyWhoami(t, alphaClient); whoami != alpha.UserID.String() {
			t.Errorf("alpha whoami = %q, want %q (should survive config update)", whoami, alpha.UserID)
		}
	})

	// --- Phase 3: remove alpha, keep beta ---
	t.Run("RemoveFirstPrincipal", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{{Localpart: beta.Localpart}},
		})

		// Alpha's proxy should be torn down: launcher sends SIGTERM, proxy
		// cleans up and removes its socket.
		waitForFileGone(t, alphaSocket)

		// Beta should still be running and unaffected.
		betaClient := proxyHTTPClient(betaSocket)
		if whoami := proxyWhoami(t, betaClient); whoami != beta.UserID.String() {
			t.Errorf("beta whoami = %q, want %q (should survive alpha removal)", whoami, beta.UserID)
		}
	})

	// --- Phase 4: remove all principals ---
	t.Run("RemoveAllPrincipals", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{})

		waitForFileGone(t, betaSocket)

		// Verify MachineStatus reflects zero running sandboxes.
		zeroWatch := watchRoom(t, admin, machine.MachineRoomID)
		zeroWatch.WaitForMachineStatus(t, machine.UserID.StateKey(), func(status schema.MachineStatus) bool {
			return status.Sandboxes.Running == 0
		}, "MachineStatus with 0 running sandboxes")
	})
}

// TestServiceReEnable verifies that a service can be re-deployed via
// principal.AssignPrincipals when its Matrix account already exists.
// This is the production path for "bureau fleet enable" and "bureau ticket
// enable" when re-run: principal.Create fails with M_USER_IN_USE, and the
// enable command calls AssignPrincipals to ensure the MachineConfig
// assignment exists so the daemon starts the sandbox.
//
// Unlike a proxy-level test, this deploys bureau-test-service which
// exercises the full service lifecycle: BootstrapViaProxy, CBOR socket
// server, fleet service directory registration, and token authentication.
//
// The test exercises the exact sequence:
//   - Deploy a service via deployTestService (production principal.Create path)
//   - Verify the service socket responds to status queries
//   - Remove the MachineConfig assignment (daemon tears down the sandbox)
//   - Call principal.AssignPrincipals to re-publish the assignment
//   - Verify the daemon reconciles, restarts the sandbox, and the service
//     socket becomes responsive again
func TestServiceReEnable(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "reenable")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Phase 1: Deploy via the production path. deployTestService calls
	// principal.Create(AutoStart: true), which registers the Matrix
	// account, provisions credentials, and publishes a MachineConfig
	// assignment. The daemon creates the sandbox, the service calls
	// BootstrapViaProxy, registers in the fleet service directory,
	// and creates a CBOR socket.
	svc := deployTestService(t, admin, fleet, machine, "reenable")

	// Verify the service is alive via the unauthenticated status action.
	statusClient := service.NewServiceClientFromToken(svc.SocketPath, nil)
	var status struct {
		UptimeSeconds float64 `cbor:"uptime_seconds"`
		Principal     string  `cbor:"principal"`
	}
	if err := statusClient.Call(t.Context(), "status", nil, &status); err != nil {
		t.Fatalf("initial status call: %v", err)
	}
	if status.Principal == "" {
		t.Fatal("status returned empty principal")
	}

	// Phase 2: Remove the MachineConfig assignment → daemon tears down.
	// Clear deployedServices so pushMachineConfig doesn't re-include
	// the service we're intentionally removing.
	machine.deployedServices = nil
	pushMachineConfig(t, admin, machine, deploymentConfig{})

	// The launcher removes the service socket symlink on sandbox
	// destruction, so wait for the symlink itself to disappear.
	waitForFileGone(t, svc.SocketPath)

	// Phase 3: Re-publish via AssignPrincipals. This is the code path
	// that fleet/ticket enable use when principal.Create returns
	// M_USER_IN_USE. The test verifies the daemon reconciles and
	// restarts the sandbox.
	templateName := strings.ReplaceAll(svc.Account.Localpart, "/", "-")
	templateRef, err := schema.ParseTemplateRef(ns.Namespace.TemplateRoomAliasLocalpart() + ":" + templateName)
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, assignErr := principal.AssignPrincipals(t.Context(), admin, machine.ConfigRoomID, []principal.CreateParams{{
		Machine:     machine.Ref,
		Principal:   svc.Entity,
		TemplateRef: templateRef,
		AutoStart:   true,
	}})
	if assignErr != nil {
		t.Fatalf("AssignPrincipals: %v", assignErr)
	}

	// Phase 4: Wait for the service to come back up. The launcher
	// creates a new symlink on sandbox re-creation.
	waitForFile(t, svc.SocketPath)

	// Resolve the symlink target (new socket inside the new sandbox)
	// and wait for the service binary to create it.
	socketTarget, err := os.Readlink(svc.SocketPath)
	if err != nil {
		t.Fatalf("readlink service socket after re-enable: %v", err)
	}
	waitForFile(t, socketTarget)

	// Verify the re-created service responds to status queries.
	reenabledClient := service.NewServiceClientFromToken(svc.SocketPath, nil)
	var reenabledStatus struct {
		UptimeSeconds float64 `cbor:"uptime_seconds"`
		Principal     string  `cbor:"principal"`
	}
	if err := reenabledClient.Call(t.Context(), "status", nil, &reenabledStatus); err != nil {
		t.Fatalf("status after re-enable: %v", err)
	}
	if reenabledStatus.Principal == "" {
		t.Fatal("status after re-enable returned empty principal")
	}
}

// TestCrossMachineRequiredService verifies that a principal on one machine
// can declare a RequiredService that runs on a different machine. The daemon
// creates a transport tunnel to bridge the local sandbox socket to the
// remote service, proving the full cross-machine service resolution path:
//
//   - Service deployed on provider machine and registered in fleet directory
//   - Admin publishes a service binding on the consumer's config room
//   - Agent deployed on consumer with RequiredServices referencing the binding
//   - Consumer daemon's first MachineConfig triggers a pre-reconcile service
//     directory sync (discovering the provider's service via GetRoomState)
//   - Consumer daemon resolves the binding → finds service is remote →
//     creates a WebRTC transport tunnel → mounts tunnel socket into sandbox
//   - Proxy socket appearing proves sandbox creation succeeded, which
//     requires successful tunnel creation and service mount resolution
func TestCrossMachineRequiredService(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	ctx := t.Context()

	// Boot two machines. The provider hosts the service and needs
	// ProxyBinary to create the service sandbox. The consumer needs
	// ProxyBinary to create the agent sandbox that depends on the
	// remote service.
	provider := newTestMachine(t, fleet, "xm-svc-prov")
	consumer := newTestMachine(t, fleet, "xm-svc-cons")

	startMachine(t, admin, provider, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})
	startMachine(t, admin, consumer, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Phase 1: Deploy the service on the provider. deployTestService
	// returns after the provider daemon has synced the registration, so
	// the service event is in the fleet service room. The consumer daemon
	// discovers the service via its pre-reconcile service directory sync
	// (triggered when the first MachineConfig arrives in Phase 3).
	svc := deployTestService(t, admin, fleet, provider, "xm-svc")

	// Phase 2: Publish a service binding on the consumer's config room.
	// RequiredServices entries resolve through bindings: the daemon reads
	// m.bureau.service_binding state events to find the principal entity,
	// then checks d.services to determine whether the service is local
	// or remote.
	_, err := admin.SendStateEvent(ctx, consumer.ConfigRoomID,
		schema.EventTypeServiceBinding, "test-svc",
		schema.ServiceBindingContent{Principal: svc.Entity})
	if err != nil {
		t.Fatalf("publish service binding on consumer: %v", err)
	}

	// Phase 3: Deploy an agent on the consumer with RequiredServices.
	// The daemon's reconcile path for this agent:
	//   resolveServiceMounts("test-svc")
	//     → resolveServiceBinding("test-svc") → svc.Entity
	//     → d.services[entity.UserID()] → service on provider machine
	//     → resolveRemoteServiceMount → startTunnel → tunnel socket
	//     → launcherServiceMount{Role: "test-svc", SocketPath: tunnel}
	//   create-sandbox IPC with the tunnel socket as a service mount
	//
	// The proxy socket appearing proves the entire chain worked:
	// binding resolution, service directory lookup, peer address
	// discovery, WebRTC tunnel creation, and sandbox creation with
	// the remote service mount.
	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")
	deployAgent(t, admin, consumer, agentOptions{
		Binary:           testAgentBinary,
		Localpart:        "agent/xm-svc-test",
		RequiredServices: []string{"test-svc"},
		SkipWaitForReady: true,
	})
}
