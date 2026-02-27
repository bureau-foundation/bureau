// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// invalidateMachineTokens invalidates all access tokens for a machine so
// the daemon's next /sync receives M_UNKNOWN_TOKEN and triggers emergency
// shutdown. Uses the HomeserverAdmin interface to handle server differences
// (Synapse HTTP admin API vs Continuwuity admin room commands).
//
// Tries DeactivateUser first (permanent, strongest response), then falls
// back to ForceLogout (invalidates tokens without deactivation).
func invalidateMachineTokens(t *testing.T, hsAdmin messaging.HomeserverAdmin, machine *testMachine) {
	t.Helper()
	ctx := t.Context()

	if err := hsAdmin.DeactivateUser(ctx, machine.UserID, false); err == nil {
		t.Log("invalidated machine tokens via DeactivateUser")
		return
	}
	t.Log("DeactivateUser failed, trying ForceLogout")

	if err := hsAdmin.ForceLogout(ctx, machine.UserID); err != nil {
		t.Fatalf("could not invalidate machine tokens: %v", err)
	}
	t.Log("invalidated machine tokens via ForceLogout")
}

// TestMachineRevocation_DaemonSelfDestruct proves that invalidating a
// machine's Matrix access tokens causes the daemon to detect the auth
// failure, destroy all running sandboxes via emergency shutdown, and exit.
// This is Layer 1 of the revocation defense — no cooperation from the
// compromised machine is needed.
//
// Sequence:
//   - Provision machine, start launcher + daemon
//   - Deploy one principal, verify proxy socket exists and is functional
//   - Invalidate the machine's access tokens (admin API or LogoutAll)
//   - Verify proxy socket disappears (sandbox destroyed by emergency shutdown)
//   - Test cleanup verifies daemon process has exited
func TestMachineRevocation_DaemonSelfDestruct(t *testing.T) {
	t.Parallel()

	const principalLocalpart = "agent/revoke-destruct"

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Set up and start the machine.
	machine := newTestMachine(t, fleet, "revoke-destruct")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: launcherBinary,
		DaemonBinary:   daemonBinary,
		ProxyBinary:    proxyBinary,
		Fleet:          fleet,
	})

	// Deploy a principal and verify the proxy is functional.
	deployment := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Localpart:    principalLocalpart,
			MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
		}},
	})

	proxySocket := deployment.ProxySockets[principalLocalpart]
	proxyClient := proxyHTTPClient(proxySocket)
	whoami := proxyWhoami(t, proxyClient)
	if whoami != deployment.Accounts[principalLocalpart].UserID.String() {
		t.Fatalf("proxy whoami = %q, want %q", whoami, deployment.Accounts[principalLocalpart].UserID)
	}

	// Invalidate the machine's Matrix access tokens. The daemon's next
	// /sync attempt will receive M_UNKNOWN_TOKEN, triggering
	// emergencyShutdown which destroys all sandboxes and exits.
	t.Log("invalidating machine account tokens to trigger daemon self-destruct")
	hsAdmin := homeserverAdmin(t)
	invalidateMachineTokens(t, hsAdmin, machine)

	// Wait for the proxy socket to disappear. This proves the daemon
	// detected the auth failure, called emergencyShutdown, and
	// destroyed the sandbox via launcher IPC.
	waitForFileGone(t, proxySocket)
	t.Log("proxy socket removed — daemon emergency shutdown destroyed the sandbox")

	// The daemon should have exited. The test cleanup (registered by
	// startMachine → startProcess) sends SIGTERM and waits 5 seconds.
	// If the daemon already exited, cleanup reaps the zombie immediately.
	// If the daemon is stuck, the cleanup's timeout fails the test.
}

// TestMachineRevocation_CLIRevoke exercises the full "bureau machine revoke"
// command end-to-end. This verifies the revocation defense layers:
//   - Layer 1: Machine account deactivated (on homeservers that support
//     the Synapse admin API — falls back gracefully otherwise)
//   - Layer 2: State events cleared, machine kicked from rooms
//   - Layer 3: Revocation event published for fleet-wide notification
//
// On homeservers that don't support the Synapse admin API (e.g.
// Continuwuity), Layer 1 fails gracefully. The daemon still removes
// all principals because Layer 2 tombstones credential state events,
// which the daemon's reconciliation detects via /sync.
//
// Sequence:
//   - Provision machine, start launcher + daemon
//   - Deploy two principals, verify both proxies are functional
//   - Run "bureau machine revoke" CLI command
//   - Verify both proxy sockets disappear (reconciliation or self-destruct)
//   - Verify credential revocation event published to machine room
//   - Verify machine_key and machine_status cleared
//   - Verify credential state events tombstoned in config room
//   - Verify machine kicked from rooms
func TestMachineRevocation_CLIRevoke(t *testing.T) {
	t.Parallel()

	const principalALocalpart = "agent/revoke-cli-a"
	const principalBLocalpart = "agent/revoke-cli-b"

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Set up and start the machine.
	machine := newTestMachine(t, fleet, "revoke-cli")
	machineName := machine.Name
	machineUserID := machine.UserID
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: launcherBinary,
		DaemonBinary:   daemonBinary,
		ProxyBinary:    proxyBinary,
		Fleet:          fleet,
	})

	// Deploy two principals.
	deployment := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{
			{
				Localpart:    principalALocalpart,
				MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
			},
			{
				Localpart:    principalBLocalpart,
				MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
			},
		},
	})

	// Verify both proxies are functional.
	for _, localpart := range []string{principalALocalpart, principalBLocalpart} {
		client := proxyHTTPClient(deployment.ProxySockets[localpart])
		proxyWhoami(t, client)
	}
	t.Log("both principals deployed and proxies functional")

	// Set up a watch on the machine room BEFORE running revoke, so we
	// can detect the revocation event published by the CLI.
	machineRoomWatch := watchRoom(t, admin, machine.MachineRoomID)

	// Run the revoke command. The HomeserverAdmin interface handles
	// server differences — on Synapse it uses the HTTP admin API, on
	// Continuwuity it uses admin room commands. Layer 1 (account
	// deactivation) invalidates the machine's tokens. Layer 2 (state
	// cleanup) tombstones credentials, causing the daemon to reconcile
	// and remove principals.
	const revokeReason = "integration test: emergency revocation"
	hsAdmin := homeserverAdmin(t)
	revokeMachine(t, admin, hsAdmin, machine.Ref, revokeReason)
	t.Log("bureau machine revoke completed")

	// Wait for both proxy sockets to disappear. On homeservers with
	// Synapse admin API support, this happens via emergency shutdown
	// (auth failure → self-destruct). On Continuwuity, this happens
	// via normal reconciliation (credential tombstones → remove pass).
	waitForFileGone(t, deployment.ProxySockets[principalALocalpart])
	waitForFileGone(t, deployment.ProxySockets[principalBLocalpart])
	t.Log("both proxy sockets removed — principals destroyed")

	// Verify the credential revocation event was published.
	revocationJSON := machineRoomWatch.WaitForStateEvent(t,
		schema.EventTypeCredentialRevocation, machineName)
	var revocation schema.CredentialRevocationContent
	if err := json.Unmarshal(revocationJSON, &revocation); err != nil {
		t.Fatalf("unmarshal revocation event: %v", err)
	}

	if revocation.Machine != machineName {
		t.Errorf("revocation machine = %q, want %q", revocation.Machine, machineName)
	}
	if revocation.MachineUserID != machineUserID {
		t.Errorf("revocation machine_user_id = %q, want %q", revocation.MachineUserID, machineUserID)
	}
	t.Logf("revocation account_deactivated = %v (depends on homeserver admin API support)", revocation.AccountDeactivated)
	if revocation.Reason != revokeReason {
		t.Errorf("revocation reason = %q, want %q", revocation.Reason, revokeReason)
	}
	if revocation.InitiatedBy.IsZero() {
		t.Error("revocation initiated_by should not be empty")
	}
	if revocation.InitiatedAt == "" {
		t.Error("revocation initiated_at should not be empty")
	}

	// Verify both principals are listed in the revocation event. The
	// Principals field contains fleet-scoped localparts because credential
	// state keys are fleet-scoped (from credential.Provision).
	entityA, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, principalALocalpart)
	if err != nil {
		t.Fatalf("construct entity for principal A: %v", err)
	}
	entityB, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, principalBLocalpart)
	if err != nil {
		t.Fatalf("construct entity for principal B: %v", err)
	}
	principalSet := make(map[string]bool)
	for _, principal := range revocation.Principals {
		principalSet[principal] = true
	}
	for _, expected := range []string{entityA.Localpart(), entityB.Localpart()} {
		if !principalSet[expected] {
			t.Errorf("revocation principals should contain %q, got %v", expected, revocation.Principals)
		}
	}

	// Verify machine_key was cleared (tombstoned with empty content).
	clearedKeyJSON, err := admin.GetStateEvent(ctx, machine.MachineRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		t.Fatalf("get machine key after revocation: %v", err)
	}
	var clearedKey struct {
		PublicKey string `json:"public_key"`
	}
	if json.Unmarshal(clearedKeyJSON, &clearedKey) == nil && clearedKey.PublicKey != "" {
		t.Errorf("machine key public_key should be empty after revocation, got %q", clearedKey.PublicKey)
	}

	// Verify machine_status was cleared.
	clearedStatusJSON, err := admin.GetStateEvent(ctx, machine.MachineRoomID,
		schema.EventTypeMachineStatus, machineName)
	if err != nil {
		t.Fatalf("get machine status after revocation: %v", err)
	}
	var clearedStatus struct {
		Principal string `json:"principal"`
	}
	if json.Unmarshal(clearedStatusJSON, &clearedStatus) == nil && clearedStatus.Principal != "" {
		t.Errorf("machine status should be empty after revocation, got principal=%q", clearedStatus.Principal)
	}

	// Verify credential state events were tombstoned in the config room.
	events, err := admin.GetRoomState(ctx, machine.ConfigRoomID)
	if err != nil {
		t.Fatalf("get config room state: %v", err)
	}
	for _, event := range events {
		if event.Type != schema.EventTypeCredentials {
			continue
		}
		if event.StateKey == nil {
			continue
		}
		// Non-empty credential content means the tombstone failed.
		contentBytes, _ := json.Marshal(event.Content)
		if string(contentBytes) != "{}" && string(contentBytes) != "null" {
			t.Errorf("credential for %q should be tombstoned, got %s", *event.StateKey, contentBytes)
		}
	}

	// Verify machine was kicked from the machine room. The admin reads
	// the room membership — the machine user should no longer be joined.
	members, err := admin.GetRoomMembers(ctx, machine.MachineRoomID)
	if err != nil {
		t.Fatalf("get machine room members: %v", err)
	}
	for _, member := range members {
		if member.UserID == machineUserID && member.Membership == "join" {
			t.Errorf("machine %s should have been kicked from machine room but is still joined", machineUserID)
		}
	}

	t.Log("full CLI revocation lifecycle verified: state cleared, revocation event published")
}
