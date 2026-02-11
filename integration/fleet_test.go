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
	"time"

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
	machineKeyJSON, err := admin.GetStateEvent(ctx, machine.MachinesRoomID,
		"m.bureau.machine_key", machine.Name)
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
	statusJSON, err := admin.GetStateEvent(ctx, machine.MachinesRoomID,
		"m.bureau.machine_status", machine.Name)
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
		"m.room.power_levels", "")
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

	// Verify MachineStatus reflects the running sandbox. Poll because the
	// daemon publishes a new heartbeat on each status tick after reconciliation.
	ctx := t.Context()
	deadline := time.Now().Add(15 * time.Second)
	for {
		statusJSON, err := admin.GetStateEvent(ctx, machine.MachinesRoomID,
			"m.bureau.machine_status", machine.Name)
		if err == nil {
			var status struct {
				Sandboxes struct {
					Running int `json:"running"`
				} `json:"sandboxes"`
			}
			if err := json.Unmarshal(statusJSON, &status); err == nil && status.Sandboxes.Running > 0 {
				t.Logf("machine status shows %d running sandbox(es)", status.Sandboxes.Running)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for MachineStatus to show running sandboxes")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestOperatorFlow verifies the operator-facing observation pipeline:
// list targets, observe a principal's terminal, and confirm authorization
// enforcement. This builds a full stack (launcher + daemon + principal with
// ObservePolicy), then exercises the daemon's observe socket as both a Go
// library client and through the bureau CLI binary.
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
		DefaultObservePolicy: map[string]any{
			"allowed_observers":   []string{"**"},
			"readwrite_observers": []string{"bureau-admin"},
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
		// Poll until the principal appears in the list. The proxy socket
		// exists on disk before the launcher responds to the daemon's IPC
		// call, so there's a brief window where the socket is ready but
		// the daemon's d.running map hasn't been updated yet.
		var response *observe.ListResponse
		deadline := time.Now().Add(5 * time.Second)
		for {
			var err error
			response, err = observe.ListTargets(machine.ObserveSocket, observe.ListRequest{
				Observer: adminUserID,
				Token:    adminToken,
			})
			if err != nil {
				t.Fatalf("ListTargets: %v", err)
			}
			if len(response.Principals) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for principal to appear in list")
			}
			time.Sleep(100 * time.Millisecond)
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
