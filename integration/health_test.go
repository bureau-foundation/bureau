// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestSocketHealthDestroy verifies that the daemon detects a Bureau-native
// service's socket health failure and destroys the sandbox. On a first
// deployment, there is no previous spec to roll back to, so the daemon
// destroys the sandbox and posts a HealthCheckDestroyed notification.
//
// The full rollback-and-recreate path (where a previous spec exists) is
// exercised by TestSocketHealthCredRotation.
//
// Timing budget: ~7s (5s grace + 2×1s probes + destroy latency).
func TestSocketHealthDestroy(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "sockhealth")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Deploy telemetry mock with socket health checks. The mock starts
	// healthy (atomic bool = true) and the daemon will probe the CBOR
	// "health" action at 1-second intervals after a 5-second grace
	// period. The grace period covers service bootstrap (BootstrapViaProxy
	// involves Matrix operations: proxy login, service registration,
	// room resolution).
	mockService := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    resolvedBinary(t, "TELEMETRY_MOCK_BINARY"),
		Name:      "telemetry-mock-sockhealth",
		Localpart: "service/telemetry/sockhealth",
		HealthCheck: &schema.HealthCheck{
			Type:               schema.HealthCheckTypeSocket,
			IntervalSeconds:    1,
			FailureThreshold:   2,
			GracePeriodSeconds: 5,
		},
	})

	// Verify the mock is reachable and healthy.
	unauthClient := service.NewServiceClientFromToken(mockService.SocketPath, nil)
	var healthResponse service.HealthResponse
	if err := unauthClient.Call(t.Context(), "health", nil, &healthResponse); err != nil {
		t.Fatalf("initial health check failed: %v", err)
	}
	if !healthResponse.Healthy {
		t.Fatal("mock should start healthy")
	}

	// Watch the config room for health check notifications BEFORE making
	// the mock unhealthy.
	watch := watchRoom(t, admin, machine.ConfigRoomID)

	// Make the mock unhealthy. The daemon's health monitor will detect
	// this after the grace period and, after reaching the failure
	// threshold, destroy the sandbox.
	type setHealthRequest struct {
		Healthy bool `cbor:"healthy"`
	}
	if err := unauthClient.Call(t.Context(), "set-health", setHealthRequest{Healthy: false}, nil); err != nil {
		t.Fatalf("set-health failed: %v", err)
	}

	// Wait for the daemon to post a HealthCheckDestroyed notification.
	// On first deployment there is no previous spec, so the daemon
	// destroys the sandbox rather than rolling back. The daemon
	// intentionally does NOT auto-recreate: if the first-ever
	// deployment fails health checks, the template or credentials
	// likely need operator intervention.
	waitForNotification[schema.HealthCheckMessage](
		t, &watch, schema.MsgTypeHealthCheck, machine.UserID,
		func(m schema.HealthCheckMessage) bool {
			return m.Principal == "service/telemetry/sockhealth" &&
				m.Outcome == schema.HealthCheckDestroyed
		}, "health check destroyed for service/telemetry/sockhealth")
}

// TestSocketHealthCredRotation exercises the interaction between credential
// rotation and socket health monitoring. Credentials are rotated in-place
// (proxy update, no sandbox restart). The service is then made unhealthy,
// triggering the health monitor to roll back to previous credentials via
// full sandbox destroy+recreate.
//
// Sequence:
//  1. Deploy telemetry mock with socket health checks. Wait for healthy.
//  2. Rotate credentials (push new m.bureau.credentials state event).
//  3. Wait for CredRotationLiveUpdated — proxy updated in-place, sandbox unchanged.
//  4. Make the mock unhealthy via set-health (same process, same client).
//  5. Wait for HealthCheckRolledBack — daemon reverts to previous credentials.
//  6. Verify the rolled-back mock starts healthy (previous credentials restored).
func TestSocketHealthCredRotation(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "credrot")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register the service principal with a password so we can rotate
	// credentials later by logging in again.
	password := "credrot-test-password"
	account := registerFleetPrincipal(t, fleet, "service/telemetry/credrot", password)

	// The service directory uses fleet-scoped localparts as state keys
	// (e.g., "bureau/fleet/.../service/telemetry/credrot"), while daemon
	// notifications (health check, credential rotation) use bare account
	// localparts. Construct the fleet-scoped entity for the service
	// directory predicate.
	principalEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, account.Localpart)
	if err != nil {
		t.Fatalf("construct fleet-scoped entity: %v", err)
	}
	fleetScopedLocalpart := principalEntity.Localpart()

	// Invite the service principal to the system room (token signing
	// key access) and fleet service room (service registration).
	// These rooms have PL 100 for invites, so only the admin can do
	// this. principal.Create handles it automatically; manual
	// deployment paths must do it explicitly.
	systemRoomID := resolveSystemRoom(t, admin, ns.Namespace)
	if err := admin.InviteUser(t.Context(), systemRoomID, account.UserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			t.Fatalf("invite service to system room: %v", err)
		}
	}
	if err := admin.InviteUser(t.Context(), fleet.ServiceRoomID, account.UserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			t.Fatalf("invite service to fleet service room: %v", err)
		}
	}

	// Push initial credentials and deploy with socket health checks.
	pushCredentials(t, admin, machine, account)
	grantTemplateAccess(t, admin, machine)

	templateRef, err := schema.ParseTemplateRef(ns.Namespace.TemplateRoomAliasLocalpart() + ":service-telemetry-credrot")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	binary := resolvedBinary(t, "TELEMETRY_MOCK_BINARY")
	content := serviceTemplateContent(binary, nil, nil, nil)
	content.HealthCheck = &schema.HealthCheck{
		Type:               schema.HealthCheckTypeSocket,
		IntervalSeconds:    1,
		FailureThreshold:   2,
		GracePeriodSeconds: 5,
	}
	if _, err := templatedef.Push(t.Context(), admin, templateRef, content, testServer); err != nil {
		t.Fatalf("push template: %v", err)
	}

	// Set up watch before publishing config.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: account.Localpart, Template: templateRef.String()}},
	})

	// Wait for the service to register in the fleet service directory.
	// Service directory state keys are fleet-scoped localparts.
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(m schema.ServiceDirectoryUpdatedMessage) bool {
			for _, added := range m.Added {
				if added == fleetScopedLocalpart {
					return true
				}
			}
			return false
		}, "service directory update for "+fleetScopedLocalpart)

	// Wait for the service socket to be ready. The symlink is created
	// by the launcher during sandbox setup, but the target (the actual
	// CBOR socket) is created by the service binary after BootstrapViaProxy
	// completes. The service directory notification can arrive before the
	// socket target exists.
	socketPath := machine.PrincipalServiceSocketPath(t, account.Localpart)
	socketTarget, err := os.Readlink(socketPath)
	if err != nil {
		t.Fatalf("readlink service socket %s: %v", socketPath, err)
	}
	waitForFile(t, socketTarget)

	// Verify initial health.
	unauthClient := service.NewServiceClientFromToken(socketPath, nil)
	var healthResponse service.HealthResponse
	if err := unauthClient.Call(t.Context(), "health", nil, &healthResponse); err != nil {
		t.Fatalf("initial health check failed: %v", err)
	}
	if !healthResponse.Healthy {
		t.Fatal("mock should start healthy")
	}

	// Rotate credentials: log in again to get a new token, push updated
	// credentials. The daemon detects the ciphertext change on its next
	// sync cycle and updates the proxy in-place (no sandbox restart).
	rotated := loginFleetPrincipal(t, fleet, account.Localpart, password)
	if rotated.Token == account.Token {
		t.Fatal("login returned the same token (expected a new device token)")
	}

	credWatch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// Wait for in-place credential update. The daemon pushes decrypted
	// credentials to the proxy admin socket without restarting the sandbox.
	waitForNotification[schema.CredentialsRotatedMessage](
		t, &credWatch, schema.MsgTypeCredentialsRotated, machine.UserID,
		func(m schema.CredentialsRotatedMessage) bool {
			return m.Principal == account.Localpart && m.Status == schema.CredRotationLiveUpdated
		}, "credentials live-updated for "+account.Localpart)

	// The sandbox is still running with the same mock process. Set health
	// to false so the health monitor detects unhealthy and triggers a
	// rollback (destroy sandbox + recreate with previous credentials).
	// Start the health watch before setting unhealthy to avoid missing
	// the rollback notification.
	healthWatch := watchRoom(t, admin, machine.ConfigRoomID)

	type setHealthRequest struct {
		Healthy bool `cbor:"healthy"`
	}
	if err := unauthClient.Call(t.Context(), "set-health", setHealthRequest{Healthy: false}, nil); err != nil {
		t.Fatalf("set-health on rotated service failed: %v", err)
	}

	// Wait for the daemon to detect the unhealthy service and roll back
	// to the previous credentials.
	waitForNotification[schema.HealthCheckMessage](
		t, &healthWatch, schema.MsgTypeHealthCheck, machine.UserID,
		func(m schema.HealthCheckMessage) bool {
			return m.Principal == account.Localpart &&
				m.Outcome == schema.HealthCheckRolledBack
		}, "health check rolled back for "+account.Localpart)

	// After rollback, a new sandbox starts with the previous (working)
	// credentials. The fresh mock process is healthy by default.
	waitForFile(t, socketPath)
	rolledBackSocketTarget, err := os.Readlink(socketPath)
	if err != nil {
		t.Fatalf("readlink rolled-back service socket %s: %v", socketPath, err)
	}
	waitForFile(t, rolledBackSocketTarget)
	rolledBackClient := service.NewServiceClientFromToken(socketPath, nil)
	var rolledBackHealth service.HealthResponse
	if err := rolledBackClient.Call(t.Context(), "health", nil, &rolledBackHealth); err != nil {
		t.Fatalf("health check on rolled-back service failed: %v", err)
	}
	if !rolledBackHealth.Healthy {
		t.Error("rolled-back service should be healthy (previous credentials restored)")
	}
}

// TestLiveCredentialRotation verifies that credential rotation updates the
// proxy's credentials in-place without restarting the sandbox. The test
// deploys a telemetry mock service, sets its health to false to create
// persistent in-process state, rotates credentials, then verifies:
//
//   - CredRotationLiveUpdated notification is posted (not Restarting+Completed).
//   - The mock service's unhealthy state persists — proves the sandbox was
//     NOT restarted (a restart would create a fresh mock defaulting healthy).
func TestLiveCredentialRotation(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "livecred")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register the service principal with a password so we can rotate
	// credentials later by logging in again.
	password := "livecred-test-password"
	account := registerFleetPrincipal(t, fleet, "service/telemetry/livecred", password)

	// Construct the fleet-scoped entity for the service directory predicate.
	principalEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, account.Localpart)
	if err != nil {
		t.Fatalf("construct fleet-scoped entity: %v", err)
	}
	fleetScopedLocalpart := principalEntity.Localpart()

	// Invite the service principal to system and fleet service rooms.
	systemRoomID := resolveSystemRoom(t, admin, ns.Namespace)
	if err := admin.InviteUser(t.Context(), systemRoomID, account.UserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			t.Fatalf("invite service to system room: %v", err)
		}
	}
	if err := admin.InviteUser(t.Context(), fleet.ServiceRoomID, account.UserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			t.Fatalf("invite service to fleet service room: %v", err)
		}
	}

	// Deploy telemetry mock WITHOUT health checks so the daemon's health
	// monitor doesn't interfere with our state manipulation.
	pushCredentials(t, admin, machine, account)
	grantTemplateAccess(t, admin, machine)

	templateRef, err := schema.ParseTemplateRef(ns.Namespace.TemplateRoomAliasLocalpart() + ":service-telemetry-livecred")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	binary := resolvedBinary(t, "TELEMETRY_MOCK_BINARY")
	content := serviceTemplateContent(binary, nil, nil, nil)
	// No HealthCheck — the daemon will not auto-rollback.
	if _, err := templatedef.Push(t.Context(), admin, templateRef, content, testServer); err != nil {
		t.Fatalf("push template: %v", err)
	}

	// Set up watch before publishing config.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: account.Localpart, Template: templateRef.String()}},
	})

	// Wait for the service to register in the fleet service directory.
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(m schema.ServiceDirectoryUpdatedMessage) bool {
			for _, added := range m.Added {
				if added == fleetScopedLocalpart {
					return true
				}
			}
			return false
		}, "service directory update for "+fleetScopedLocalpart)

	// Wait for the service socket to be ready.
	socketPath := machine.PrincipalServiceSocketPath(t, account.Localpart)
	socketTarget, err := os.Readlink(socketPath)
	if err != nil {
		t.Fatalf("readlink service socket %s: %v", socketPath, err)
	}
	waitForFile(t, socketTarget)

	// Set health to false — this creates persistent in-process state.
	// If the sandbox restarts, the fresh mock defaults to healthy.
	client := service.NewServiceClientFromToken(socketPath, nil)
	type setHealthRequest struct {
		Healthy bool `cbor:"healthy"`
	}
	if err := client.Call(t.Context(), "set-health", setHealthRequest{Healthy: false}, nil); err != nil {
		t.Fatalf("set-health on service failed: %v", err)
	}

	// Verify baseline: the mock is unhealthy.
	var baselineHealth service.HealthResponse
	if err := client.Call(t.Context(), "health", nil, &baselineHealth); err != nil {
		t.Fatalf("baseline health check failed: %v", err)
	}
	if baselineHealth.Healthy {
		t.Fatal("mock should be unhealthy after set-health")
	}

	// Rotate credentials: log in again for a new token, push updated
	// credentials. The daemon should detect the ciphertext change and
	// update the proxy in-place (no sandbox restart).
	rotated := loginFleetPrincipal(t, fleet, account.Localpart, password)
	if rotated.Token == account.Token {
		t.Fatal("login returned the same token (expected a new device token)")
	}

	credWatch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// Wait for CredRotationLiveUpdated (NOT Restarting+Completed).
	waitForNotification[schema.CredentialsRotatedMessage](
		t, &credWatch, schema.MsgTypeCredentialsRotated, machine.UserID,
		func(m schema.CredentialsRotatedMessage) bool {
			return m.Principal == account.Localpart && m.Status == schema.CredRotationLiveUpdated
		}, "credentials live-updated for "+account.Localpart)

	// Verify the sandbox was NOT restarted: the mock should still be
	// unhealthy (the set-health state persists in the same process).
	var afterRotationHealth service.HealthResponse
	if err := client.Call(t.Context(), "health", nil, &afterRotationHealth); err != nil {
		t.Fatalf("health check after rotation failed: %v", err)
	}
	if afterRotationHealth.Healthy {
		t.Fatal("mock health should still be false after live rotation (sandbox should not have restarted)")
	}
}

// TestCredentialRotationFallback verifies that when the in-place credential
// push fails (proxy admin socket unreachable), the daemon falls back to
// destroying and recreating the sandbox. The test:
//
//  1. Deploys a principal with a working sandbox.
//  2. Renames the proxy admin socket to make it unreachable.
//  3. Pushes new credentials.
//  4. Daemon detects the change, tries in-place update, push fails.
//  5. Falls back to destroy+recreate (CredRotationRestarting → CredRotationCompleted).
//  6. The recreated sandbox gets credentials through the normal pipe mechanism.
//  7. Verifies the proxy serves the correct identity with the new token.
func TestCredentialRotationFallback(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin
	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "fallback")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		Fleet:          fleet,
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register and deploy the principal.
	password := "fallback-test-password"
	agent := registerFleetPrincipal(t, fleet, "test/fallback", password)
	pushCredentials(t, admin, machine, agent)
	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Localpart: agent.Localpart}},
	})

	// Wait for the proxy socket to appear (sandbox is running).
	proxySocket := machine.PrincipalProxySocketPath(t, agent.Localpart)
	waitForFile(t, proxySocket)
	proxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, proxyClient); whoami != agent.UserID.String() {
		t.Fatalf("initial whoami = %q, want %q", whoami, agent.UserID)
	}

	// Rename the admin socket so the daemon's in-place push fails.
	// The daemon will detect the credential change, successfully decrypt
	// via the launcher, but fail to connect to the admin socket. This
	// triggers the fallback path: destroy sandbox + recreate.
	adminSocket := machine.PrincipalProxyAdminSocketPath(t, agent.Localpart)
	disabledSocket := adminSocket + ".disabled"
	if err := os.Rename(adminSocket, disabledSocket); err != nil {
		t.Fatalf("rename admin socket: %v", err)
	}
	// Restore after the test completes (cleanup, not functionally required
	// since the fallback destroys the old proxy and creates a new one).
	t.Cleanup(func() { os.Rename(disabledSocket, adminSocket) })

	// Rotate credentials.
	rotated := loginFleetPrincipal(t, fleet, agent.Localpart, password)
	if rotated.Token == agent.Token {
		t.Fatal("login returned the same token (expected a new device token)")
	}

	watch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// The daemon's in-place push fails because the admin socket is renamed.
	// It falls back to destroy+recreate. Wait for CredRotationCompleted
	// (the "create missing" pass recreates the sandbox with the new
	// credentials via the standard pipe mechanism).
	waitForNotification[schema.CredentialsRotatedMessage](
		t, &watch, schema.MsgTypeCredentialsRotated, machine.UserID,
		func(m schema.CredentialsRotatedMessage) bool {
			return m.Principal == agent.Localpart && m.Status == schema.CredRotationCompleted
		}, "credentials rotation completed (fallback) for "+agent.Localpart)

	// The recreated proxy should have the new token. Wait for the proxy
	// socket to reappear (new sandbox creates a new proxy process).
	waitForFile(t, proxySocket)
	newProxyClient := proxyHTTPClient(proxySocket)
	if whoami := proxyWhoami(t, newProxyClient); whoami != agent.UserID.String() {
		t.Fatalf("post-fallback whoami = %q, want %q", whoami, agent.UserID)
	}
}
