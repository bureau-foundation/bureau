// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"net/http"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestMatrixPolicyHotReload verifies that MatrixPolicy changes take effect
// on running principals without sandbox restart.
//
//   - Phase 1: Deploy with default-deny (no MatrixPolicy). Join is blocked.
//   - Phase 2: Update to AllowJoin=true. Wait for daemon confirmation, join.
//   - Phase 3: Revert to default-deny. Wait for daemon confirmation, join blocked.
//
// The daemon synthesizes authorization grants from MatrixPolicy fields and
// pushes them to the proxy via PUT /v1/admin/authorization. When grants
// change, the daemon posts "Authorization grants updated for <principal>
// (<count> grants)" to the config room. This message serves as a
// synchronization point: once it appears, the proxy is guaranteed to be
// enforcing the new grants.
//
// Phase 3 uses a fresh room (room B) because the agent joined room A in
// phase 2. The Matrix /join endpoint is idempotent for already-joined
// members, so retesting room A would succeed regardless of grants.
//
// This proves the daemon's reconcile → resolveGrantsForProxy →
// pushAuthorizationToProxy → proxy GrantsAllow enforcement flow
// applies runtime policy changes end-to-end.
func TestMatrixPolicyHotReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/policy-hr")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Deploy a proxy-only principal with no MatrixPolicy (default-deny).
	agent := registerPrincipal(t, "agent/policy-hr", "policy-hr-password")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Account: agent}},
	})
	proxyClient := proxyHTTPClient(proxySockets[agent.Localpart])

	// Sanity check: proxy is operational.
	if userID := proxyWhoami(t, proxyClient); userID != agent.UserID {
		t.Fatalf("proxy whoami = %q, want %q", userID, agent.UserID)
	}

	// --- Phase 1: Default-deny blocks join ---

	roomA, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name: "policy-hr-room-a",
	})
	if err != nil {
		t.Fatalf("create room A: %v", err)
	}
	if err := admin.InviteUser(ctx, roomA.RoomID, agent.UserID); err != nil {
		t.Fatalf("invite agent to room A: %v", err)
	}

	statusCode, body := proxyTryJoinRoom(t, proxyClient, roomA.RoomID)
	if statusCode != http.StatusForbidden {
		t.Fatalf("phase 1: expected 403 for join with default-deny, got %d: %s",
			statusCode, body)
	}
	t.Log("phase 1 passed: join blocked with default-deny grants")

	// --- Phase 2: Hot-reload AllowJoin=true ---

	watch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:      agent,
			MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true},
		}},
	})

	// Wait for the daemon's grants hot-reload confirmation. The watch
	// checkpoint was taken before the config push, so only new messages
	// after the push are considered.
	watch.WaitForMessage(t, "Authorization grants updated for agent/policy-hr",
		machine.UserID)
	t.Log("daemon confirmed grants hot-reload for AllowJoin=true")

	// Now the proxy is guaranteed to enforce the new grants.
	statusCode, body = proxyTryJoinRoom(t, proxyClient, roomA.RoomID)
	if statusCode != http.StatusOK {
		t.Fatalf("phase 2: expected 200 for join with AllowJoin=true, got %d: %s",
			statusCode, body)
	}
	t.Log("phase 2 passed: join succeeded after AllowJoin=true hot-reload")

	// --- Phase 3: Revert to default-deny ---

	// Create a fresh room. Room A is already joined, so the homeserver
	// would return 200 on a duplicate join regardless of proxy grants.
	roomB, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name: "policy-hr-room-b",
	})
	if err != nil {
		t.Fatalf("create room B: %v", err)
	}
	if err := admin.InviteUser(ctx, roomB.RoomID, agent.UserID); err != nil {
		t.Fatalf("invite agent to room B: %v", err)
	}

	watch = watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{Account: agent}},
	})

	// Wait for the daemon's grants revert confirmation via a fresh watch.
	watch.WaitForMessage(t, "Authorization grants updated for agent/policy-hr",
		machine.UserID)
	t.Log("daemon confirmed grants revert to default-deny")

	statusCode, body = proxyTryJoinRoom(t, proxyClient, roomB.RoomID)
	if statusCode != http.StatusForbidden {
		t.Fatalf("phase 3: expected 403 for join after grants revert, got %d: %s",
			statusCode, body)
	}
	t.Log("phase 3 passed: join blocked after reverting to default-deny")

	t.Log("grants hot-reload verified: deny → allow → deny")
}

// TestServiceVisibilityHotReload verifies that ServiceVisibility pattern
// changes take effect on running principals without restart.
//
//   - Phase 1: Deploy with visibility matching a service. Service is visible.
//   - Phase 2: Change visibility to non-matching patterns. Service disappears.
//   - Phase 3: Restore matching visibility. Service reappears.
//
// The daemon synthesizes authorization grants from ServiceVisibility patterns
// (as service/discover targets) and pushes them to the proxy via
// PUT /v1/admin/authorization. Visibility filtering happens at query time in
// the proxy's HandleServiceDirectory using GrantsAllow. When grants change,
// the daemon posts "Authorization grants updated for <principal>" to the
// config room — this serves as the synchronization point.
//
// This proves the daemon's reconcile → resolveGrantsForProxy →
// pushAuthorizationToProxy → proxy GrantsAllow filtering flow
// applies runtime visibility changes end-to-end.
func TestServiceVisibilityHotReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/vis-hr")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Deploy a consumer FIRST so the proxy exists when the daemon
	// processes the service event. The daemon's "Service directory
	// updated" message is posted after pushServiceDirectory, which
	// pushes to all running proxies. If the proxy doesn't exist when
	// the service event is processed, the push is a no-op and the
	// message becomes a false synchronization signal.
	agent := registerPrincipal(t, "agent/vis-hr", "vis-hr-password")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:           agent,
			ServiceVisibility: []string{"service/vis-hr/*"},
		}},
	})
	proxyClient := proxyHTTPClient(proxySockets[agent.Localpart])

	// Watch the daemon's config room for service directory updates. Set
	// up the watch BEFORE publishing the service event to capture the
	// sync position before the directory change message arrives.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Publish a test service in #bureau/service. No actual service
	// principal needs to run — the directory entry is constructed from
	// the state event content regardless of whether the principal exists.
	serviceRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasService, testServerName))
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}
	_, err = admin.SendStateEvent(ctx, serviceRoomID, schema.EventTypeService,
		"service/vis-hr/test", map[string]any{
			"principal":   "@service/vis-hr/test:" + testServerName,
			"machine":     machine.UserID,
			"protocol":    "http",
			"description": "Test service for visibility hot-reload",
		})
	if err != nil {
		t.Fatalf("publish service event: %v", err)
	}

	// --- Phase 1: Matching visibility — service is visible ---

	// Wait for the daemon to process the service event. The proxy was
	// deployed above, so pushServiceDirectory includes it — the message
	// is posted after the push completes, guaranteeing the proxy has
	// the updated directory.
	serviceWatch.WaitForMessage(t, "added service/vis-hr/test", machine.UserID)

	entries := proxyServiceDiscovery(t, proxyClient, "")
	if len(entries) != 1 {
		t.Fatalf("phase 1: expected 1 service, got %d", len(entries))
	}
	if entries[0].Localpart != "service/vis-hr/test" {
		t.Errorf("phase 1: localpart = %q, want %q",
			entries[0].Localpart, "service/vis-hr/test")
	}
	t.Log("phase 1 passed: service visible with matching visibility")

	// --- Phase 2: Non-matching visibility — service disappears ---

	watch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:           agent,
			ServiceVisibility: []string{"service/unrelated/*"},
		}},
	})

	// Wait for the daemon's grants hot-reload confirmation, then
	// verify the service is no longer visible.
	watch.WaitForMessage(t, "Authorization grants updated for agent/vis-hr",
		machine.UserID)

	entries = proxyServiceDiscovery(t, proxyClient, "")
	if len(entries) != 0 {
		t.Fatalf("phase 2: expected 0 services with non-matching visibility, got %d", len(entries))
	}
	t.Log("phase 2 passed: service hidden after visibility narrowed")

	// --- Phase 3: Restore matching visibility — service reappears ---

	watch = watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:           agent,
			ServiceVisibility: []string{"service/vis-hr/*"},
		}},
	})

	// Wait for the daemon's grants hot-reload confirmation, then
	// verify the service is visible again.
	watch.WaitForMessage(t, "Authorization grants updated for agent/vis-hr",
		machine.UserID)

	entries = proxyServiceDiscovery(t, proxyClient, "")
	if len(entries) != 1 {
		t.Fatalf("phase 3: expected 1 service, got %d", len(entries))
	}
	if entries[0].Localpart != "service/vis-hr/test" {
		t.Errorf("phase 3: localpart = %q, want %q",
			entries[0].Localpart, "service/vis-hr/test")
	}
	t.Log("phase 3 passed: service visible again after visibility restored")

	t.Log("grants hot-reload verified: match → no-match → match")
}
