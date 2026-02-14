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
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// TestMachineJoinsFleet verifies the complete machine bootstrap path:
// launcher registers with the homeserver and publishes its key, daemon
// starts, performs initial sync, creates the config room, and publishes
// a MachineStatus heartbeat. This proves the full launcher→daemon→Matrix
// lifecycle works end-to-end.
func TestMachineJoinsFleet(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
	})

	// startMachine already proved: key published, status heartbeat received,
	// config room created. The assertions below verify the content details
	// that startMachine doesn't check.
	ctx := t.Context()

	// Verify machine key algorithm and value.
	machineKeyJSON, err := admin.GetStateEvent(ctx, machine.MachineRoomID,
		schema.EventTypeMachineKey, machine.Name)
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
		schema.EventTypeMachineStatus, machine.Name)
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
	if status.Principal != machine.UserID {
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
	adminUserID := "@bureau-admin:" + testServerName
	if level, ok := powerLevels.Users[adminUserID]; !ok || level != 100 {
		t.Errorf("admin power level = %d (present=%v), want 100", level, ok)
	}
	if level, ok := powerLevels.Users[machine.UserID]; !ok || level != 50 {
		t.Errorf("machine power level = %d (present=%v), want 50", level, ok)
	}
}

// TestPrincipalAssignment verifies the full sandbox lifecycle: admin writes
// a MachineConfig assigning a principal, the daemon reconciles, the launcher
// creates a proxy, and the proxy correctly serves the principal's Matrix
// identity. This proves credential encryption, IPC, proxy spawning, and
// reconciliation work end-to-end.
func TestPrincipalAssignment(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/sandbox")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	agent := registerPrincipal(t, "test/agent", "test-principal-password")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Account: agent}},
	})

	// Verify the proxy serves the principal's identity.
	proxyClient := proxyHTTPClient(proxySockets[agent.Localpart])
	whoamiUserID := proxyWhoami(t, proxyClient)
	if whoamiUserID != agent.UserID {
		t.Errorf("whoami user_id = %q, want %q", whoamiUserID, agent.UserID)
	}

	// Verify MachineStatus reflects the running sandbox. The daemon publishes
	// status on its interval; watch from the current sync position.
	sandboxWatch := watchRoom(t, admin, machine.MachineRoomID)
	sandboxWatch.WaitForMachineStatus(t, machine.Name, func(status schema.MachineStatus) bool {
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

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/observe")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:     resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:       resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:        resolvedBinary(t, "PROXY_BINARY"),
		ObserveRelayBinary: resolvedBinary(t, "OBSERVE_RELAY_BINARY"),
	})

	observed := registerPrincipal(t, "test/observed", "test-observe-password")
	deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Account: observed}},
		DefaultPolicy: &schema.AuthorizationPolicy{
			Allowances: []schema.Allowance{
				{Actions: []string{"observe"}, Actors: []string{"**"}},
				{Actions: []string{"observe/read-write"}, Actors: []string{"bureau-admin"}},
			},
		},
	})

	// Wait for the observe socket to be ready (separate from proxy sockets).
	waitForFile(t, machine.ObserveSocket, 5*time.Second)

	// Get the admin's access token for observe socket authentication.
	credentials := loadCredentials(t)
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminToken == "" {
		t.Fatal("MATRIX_ADMIN_TOKEN missing from credential file")
	}
	adminUserID := "@bureau-admin:" + testServerName

	// --- Sub-test: list targets via Go library ---
	t.Run("ListTargets", func(t *testing.T) {
		// Wait for the daemon's heartbeat to report running sandboxes. This
		// proves d.running has been updated (the daemon counts sandboxes from
		// its running map when publishing the heartbeat). ListTargets reads
		// from the same running map, so a single call suffices after this.
		listWatch := watchRoom(t, admin, machine.MachineRoomID)
		listWatch.WaitForMachineStatus(t, machine.Name, func(status schema.MachineStatus) bool {
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
			if entry.Localpart == observed.Localpart {
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
				observed.Localpart, len(response.Principals))
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
			if entry.Localpart == observed.Localpart {
				foundPrincipal = true
				break
			}
		}
		if !foundPrincipal {
			t.Errorf("CLI: principal %q not found in list output", observed.Localpart)
		}
	})

	// --- Sub-test: observe handshake via Go library ---
	t.Run("ObserveHandshake", func(t *testing.T) {
		session, err := observe.Connect(machine.ObserveSocket, observe.ObserveRequest{
			Principal: observed.Localpart,
			Mode:      "readwrite",
			Observer:  adminUserID,
			Token:     adminToken,
		})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer session.Close()

		if session.Metadata.Principal != observed.Localpart {
			t.Errorf("metadata principal = %q, want %q",
				session.Metadata.Principal, observed.Localpart)
		}
		if session.Metadata.Machine != machine.Name {
			t.Errorf("metadata machine = %q, want %q",
				session.Metadata.Machine, machine.Name)
		}
	})

	// --- Sub-test: observe denied without authentication ---
	t.Run("ObserveDeniedUnauthorized", func(t *testing.T) {
		_, err := observe.Connect(machine.ObserveSocket, observe.ObserveRequest{
			Principal: observed.Localpart,
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

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/rotate")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register and deploy the principal with its initial token.
	password := "rotate-test-password"
	agent := registerPrincipal(t, "test/rotate", password)
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Account: agent}},
	})

	// Verify the proxy serves the correct identity with the original token.
	proxySocket := proxySockets[agent.Localpart]
	proxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, proxyClient); whoami != agent.UserID {
		t.Fatalf("initial whoami = %q, want %q", whoami, agent.UserID)
	}

	// Log in again to get a fresh access token. This creates a new device
	// on the homeserver — the original token remains valid but the new one
	// is what we'll push as updated credentials.
	rotated := loginPrincipal(t, agent.Localpart, password)
	if rotated.Token == agent.Token {
		t.Fatal("login returned the same token as registration (expected a new device token)")
	}

	// Push new credentials. The ciphertext will differ because:
	//   (a) the token payload is different, and
	//   (b) age encryption uses a fresh ephemeral key per Encrypt call.
	// The daemon compares ciphertext on each reconcile cycle and triggers
	// a destroy+create when it changes.
	watch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// Wait for the daemon to complete the rotation. The daemon posts
	// "Restarted X with new credentials" after destroying the old sandbox
	// and creating the new one. This is a deterministic completion signal
	// (no inode polling needed).
	watch.WaitForMessage(t, "Restarted "+agent.Localpart+" with new credentials",
		machine.UserID)

	// Verify the restarted proxy serves the correct identity. The new proxy
	// process holds the rotated token, so whoami still returns the same
	// user_id (the account didn't change, only its access token).
	newProxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, newProxyClient); whoami != agent.UserID {
		t.Fatalf("post-rotation whoami = %q, want %q", whoami, agent.UserID)
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

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	// Boot two machines. The provider hosts the principal and needs the
	// proxy binary (to create a sandbox) and the observe relay binary
	// (to fork a relay for the observation session). The consumer just
	// needs a daemon — it routes observation requests but doesn't host
	// any principals.
	provider := newTestMachine(t, "machine/xm-prov")
	consumer := newTestMachine(t, "machine/xm-cons")

	startMachine(t, admin, provider, machineOptions{
		LauncherBinary:     resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:       resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:        resolvedBinary(t, "PROXY_BINARY"),
		ObserveRelayBinary: resolvedBinary(t, "OBSERVE_RELAY_BINARY"),
	})
	startMachine(t, admin, consumer, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
	})

	// Deploy a principal on the provider with observation allowances
	// that let the admin observe in readwrite mode.
	observed := registerPrincipal(t, "test/xm-obs", "xm-observe-password")
	deployPrincipals(t, admin, provider, deploymentConfig{
		Principals: []principalSpec{{Account: observed}},
		DefaultPolicy: &schema.AuthorizationPolicy{
			Allowances: []schema.Allowance{
				{Actions: []string{"observe"}, Actors: []string{"**"}},
				{Actions: []string{"observe/read-write"}, Actors: []string{"bureau-admin"}},
			},
		},
	})

	// Push a MachineConfig on the consumer. The consumer doesn't host
	// any principals — it routes observation requests to the provider,
	// which performs its own authorization. No observation allowances
	// are needed on the consumer side.
	pushMachineConfig(t, admin, consumer, deploymentConfig{})

	// Publish a service entry in #bureau/service so the consumer daemon
	// can discover the principal on the provider machine. In production,
	// services are registered by the daemon or admin; here we simulate
	// that by pushing the state event directly.
	serviceRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasService, testServerName))
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}
	_, err = admin.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService,
		observed.Localpart, map[string]any{
			"principal":   observed.UserID,
			"machine":     provider.UserID,
			"protocol":    "bureau-observe",
			"description": "test principal for cross-machine observation",
		})
	if err != nil {
		t.Fatalf("publish service event: %v", err)
	}

	// Wait for the observe socket on the consumer machine.
	waitForFile(t, consumer.ObserveSocket, 5*time.Second)

	// Get admin credentials for observe socket authentication.
	credentials := loadCredentials(t)
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	if adminToken == "" {
		t.Fatal("MATRIX_ADMIN_TOKEN missing from credential file")
	}
	adminUserID := "@bureau-admin:" + testServerName

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
				if entry.Localpart == observed.Localpart && entry.Observable {
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
			if entry.Localpart == observed.Localpart {
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
			Principal: observed.Localpart,
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
		if session.Metadata.Principal != observed.Localpart {
			t.Errorf("metadata principal = %q, want %q",
				session.Metadata.Principal, observed.Localpart)
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

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/reconcile")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register both principals upfront (Matrix account creation only —
	// no sandboxes until credentials and config are pushed).
	alpha := registerPrincipal(t, "test/alpha", "alpha-password")
	beta := registerPrincipal(t, "test/beta", "beta-password")

	// Push encrypted credentials for both principals. Credentials are
	// inert until a MachineConfig references the principal — the daemon
	// only acts on principals listed in the config with auto_start=true.
	pushCredentials(t, admin, machine, alpha)
	pushCredentials(t, admin, machine, beta)

	alphaSocket := machine.PrincipalSocketPath(alpha.Localpart)
	betaSocket := machine.PrincipalSocketPath(beta.Localpart)

	// --- Phase 1: deploy alpha ---
	t.Run("AddFirstPrincipal", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{{Account: alpha}},
		})
		waitForFile(t, alphaSocket, 15*time.Second)

		alphaClient := proxyHTTPClient(alphaSocket)
		if whoami := proxyWhoami(t, alphaClient); whoami != alpha.UserID {
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
				{Account: alpha},
				{Account: beta},
			},
		})
		waitForFile(t, betaSocket, 15*time.Second)

		// Beta should now be reachable with the correct identity.
		betaClient := proxyHTTPClient(betaSocket)
		if whoami := proxyWhoami(t, betaClient); whoami != beta.UserID {
			t.Errorf("beta whoami = %q, want %q", whoami, beta.UserID)
		}

		// Alpha should still be running and unaffected by the config update.
		alphaClient := proxyHTTPClient(alphaSocket)
		if whoami := proxyWhoami(t, alphaClient); whoami != alpha.UserID {
			t.Errorf("alpha whoami = %q, want %q (should survive config update)", whoami, alpha.UserID)
		}
	})

	// --- Phase 3: remove alpha, keep beta ---
	t.Run("RemoveFirstPrincipal", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{
			Principals: []principalSpec{{Account: beta}},
		})

		// Alpha's proxy should be torn down: launcher sends SIGTERM, proxy
		// cleans up and removes its socket.
		waitForFileGone(t, alphaSocket, 15*time.Second)

		// Beta should still be running and unaffected.
		betaClient := proxyHTTPClient(betaSocket)
		if whoami := proxyWhoami(t, betaClient); whoami != beta.UserID {
			t.Errorf("beta whoami = %q, want %q (should survive alpha removal)", whoami, beta.UserID)
		}
	})

	// --- Phase 4: remove all principals ---
	t.Run("RemoveAllPrincipals", func(t *testing.T) {
		pushMachineConfig(t, admin, machine, deploymentConfig{})

		waitForFileGone(t, betaSocket, 15*time.Second)

		// Verify MachineStatus reflects zero running sandboxes.
		zeroWatch := watchRoom(t, admin, machine.MachineRoomID)
		zeroWatch.WaitForMachineStatus(t, machine.Name, func(status schema.MachineStatus) bool {
			return status.Sandboxes.Running == 0
		}, "MachineStatus with 0 running sandboxes")
	})
}
