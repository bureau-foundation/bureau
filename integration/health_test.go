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

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

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
// rotation and socket health monitoring. When credentials are rotated and
// the new service sandbox is unhealthy, the daemon should roll back to the
// previous credentials and sandbox.
//
// Sequence:
//  1. Deploy telemetry mock with socket health checks. Wait for healthy.
//  2. Rotate credentials (push new m.bureau.credentials state event).
//  3. Wait for CredRotationCompleted — new sandbox is running.
//  4. Make the new mock unhealthy via set-health.
//  5. Wait for HealthCheckRolledBack — daemon reverts to previous credentials.
//  6. Verify the rolled-back mock starts healthy (previous credentials restored).
func TestSocketHealthCredRotation(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

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
	systemRoomID := resolveSystemRoom(t, admin)
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

	templateRef, err := schema.ParseTemplateRef("bureau/template:service-telemetry-credrot")
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
	// sync cycle and destroys+recreates the sandbox with new credentials.
	rotated := loginFleetPrincipal(t, fleet, account.Localpart, password)
	if rotated.Token == account.Token {
		t.Fatal("login returned the same token (expected a new device token)")
	}

	credWatch := watchRoom(t, admin, machine.ConfigRoomID)
	pushCredentials(t, admin, machine, rotated)

	// Wait for credential rotation to complete (new sandbox is running).
	waitForNotification[schema.CredentialsRotatedMessage](
		t, &credWatch, schema.MsgTypeCredentialsRotated, machine.UserID,
		func(m schema.CredentialsRotatedMessage) bool {
			return m.Principal == account.Localpart && m.Status == schema.CredRotationCompleted
		}, "credentials rotation completed for "+account.Localpart)

	// The new sandbox has a fresh mock process (healthy by default).
	// Wait for the new service socket to appear. The symlink is recreated
	// by the launcher during sandbox setup, but the target doesn't exist
	// until the service binary creates its CBOR socket.
	waitForFile(t, socketPath)
	rotatedSocketTarget, err := os.Readlink(socketPath)
	if err != nil {
		t.Fatalf("readlink rotated service socket %s: %v", socketPath, err)
	}
	waitForFile(t, rotatedSocketTarget)
	newClient := service.NewServiceClientFromToken(socketPath, nil)

	type setHealthRequest struct {
		Healthy bool `cbor:"healthy"`
	}
	if err := newClient.Call(t.Context(), "set-health", setHealthRequest{Healthy: false}, nil); err != nil {
		t.Fatalf("set-health on rotated service failed: %v", err)
	}

	// Wait for the daemon to detect the unhealthy service and roll back
	// to the previous credentials.
	healthWatch := watchRoom(t, admin, machine.ConfigRoomID)
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
