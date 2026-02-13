// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"
)

// TestServiceDiscovery exercises the full cross-machine service discovery
// pipeline through real processes:
//
//   - Admin publishes m.bureau.service in #bureau/service
//   - Consumer machine's daemon detects it via /sync
//   - Daemon pushes directory to all consumer proxies
//   - Consumer principals query GET /v1/services with visibility filtering
//
// This proves the daemon /sync → syncServiceDirectory → reconcileServices →
// pushServiceDirectory → proxy HandleServiceDirectory flow works end-to-end,
// including ServiceVisibility glob filtering and query parameter filtering.
func TestServiceDiscovery(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	// Boot two machines. The provider hosts the service principal. The
	// consumer has principals that query for services through their proxies.
	provider := newTestMachine(t, "machine/svc-prov")
	consumer := newTestMachine(t, "machine/svc-cons")

	startMachine(t, admin, provider, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})
	startMachine(t, admin, consumer, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// --- Phase 1: Register a service on the provider machine ---

	// Deploy a principal that represents the service. In production,
	// services are principals running on a machine — deploying it creates
	// the proxy socket and makes the daemon aware of the principal.
	serviceAgent := registerPrincipal(t, "service/stt/test", "svc-discovery-password")
	deployPrincipals(t, admin, provider, deploymentConfig{
		Principals: []principalSpec{{Account: serviceAgent}},
	})

	// Watch the consumer daemon's config room for service directory updates.
	// Set up the watch BEFORE publishing the service event so the roomWatch
	// captures the sync position before any directory change messages arrive.
	serviceWatch := watchRoom(t, admin, consumer.ConfigRoomID)

	// Publish a service event in #bureau/service advertising the service.
	// The consumer daemon detects this via /sync and pushes the directory
	// to its principals' proxies.
	serviceRoomID, err := admin.ResolveAlias(ctx, "#bureau/service:"+testServerName)
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}

	_, err = admin.SendStateEvent(ctx, serviceRoomID, "m.bureau.service",
		"service/stt/test", map[string]any{
			"principal":    serviceAgent.UserID,
			"machine":      provider.UserID,
			"protocol":     "http",
			"capabilities": []string{"streaming"},
			"description":  "Test STT service",
		})
	if err != nil {
		t.Fatalf("publish service event: %v", err)
	}

	// --- Phase 2: Deploy consumer principals with different visibility ---

	consumerWide := registerPrincipal(t, "test/svc-wide", "svc-wide-password")
	consumerNarrow := registerPrincipal(t, "test/svc-narrow", "svc-narrow-password")

	consumerSockets := deployPrincipals(t, admin, consumer, deploymentConfig{
		Principals: []principalSpec{
			{
				Account:           consumerWide,
				ServiceVisibility: []string{"service/stt/*", "service/embedding/*"},
			},
			{
				Account:           consumerNarrow,
				ServiceVisibility: []string{"service/embedding/*"},
			},
		},
	})

	wideClient := proxyHTTPClient(consumerSockets[consumerWide.Localpart])
	narrowClient := proxyHTTPClient(consumerSockets[consumerNarrow.Localpart])

	// Wait for the consumer daemon to process the service event. The
	// daemon posts this message AFTER pushServiceDirectory completes, so
	// all running consumer proxies are guaranteed to have the directory
	// by the time this returns. Combined with deployPrincipals having
	// returned (consumer sockets exist, directory pushed during reconcile),
	// the proxy is ready to query.
	serviceWatch.WaitForMessage(t, "added service/stt/test", consumer.UserID)

	// --- Phase 3: Verify service propagation and visibility isolation ---

	t.Run("ServicePropagation", func(t *testing.T) {
		entries := proxyServiceDiscovery(t, wideClient, "")

		if len(entries) != 1 {
			t.Fatalf("expected 1 service entry, got %d", len(entries))
		}

		entry := entries[0]
		if entry.Localpart != "service/stt/test" {
			t.Errorf("localpart = %q, want %q", entry.Localpart, "service/stt/test")
		}
		if entry.Principal != serviceAgent.UserID {
			t.Errorf("principal = %q, want %q", entry.Principal, serviceAgent.UserID)
		}
		if entry.Machine != provider.UserID {
			t.Errorf("machine = %q, want %q", entry.Machine, provider.UserID)
		}
		if entry.Protocol != "http" {
			t.Errorf("protocol = %q, want %q", entry.Protocol, "http")
		}
		if entry.Description != "Test STT service" {
			t.Errorf("description = %q, want %q", entry.Description, "Test STT service")
		}
		if len(entry.Capabilities) != 1 || entry.Capabilities[0] != "streaming" {
			t.Errorf("capabilities = %v, want [streaming]", entry.Capabilities)
		}
	})

	t.Run("VisibilityIsolation", func(t *testing.T) {
		// The narrow consumer has visibility ["service/embedding/*"], which
		// does not match "service/stt/test". If the wide consumer already
		// sees the service, the daemon has already pushed the directory to
		// ALL proxies on the consumer machine — the narrow consumer simply
		// filters it out via its visibility patterns.
		entries := proxyServiceDiscovery(t, narrowClient, "")
		if len(entries) != 0 {
			t.Errorf("narrow consumer should see 0 services (visibility blocks stt), got %d", len(entries))
			for _, entry := range entries {
				t.Logf("  visible: %s (protocol=%s)", entry.Localpart, entry.Protocol)
			}
		}
	})

	// --- Phase 4: Query parameter filtering ---

	t.Run("QueryParameterFiltering", func(t *testing.T) {
		// Protocol filter: http matches the STT service.
		httpEntries := proxyServiceDiscovery(t, wideClient, "protocol=http")
		if len(httpEntries) != 1 {
			t.Errorf("protocol=http: expected 1 service, got %d", len(httpEntries))
		}

		// Protocol filter: grpc does not match.
		grpcEntries := proxyServiceDiscovery(t, wideClient, "protocol=grpc")
		if len(grpcEntries) != 0 {
			t.Errorf("protocol=grpc: expected 0 services, got %d", len(grpcEntries))
		}

		// Capability filter: streaming matches.
		streamingEntries := proxyServiceDiscovery(t, wideClient, "capability=streaming")
		if len(streamingEntries) != 1 {
			t.Errorf("capability=streaming: expected 1 service, got %d", len(streamingEntries))
		}

		// Capability filter: translation does not match.
		translationEntries := proxyServiceDiscovery(t, wideClient, "capability=translation")
		if len(translationEntries) != 0 {
			t.Errorf("capability=translation: expected 0 services, got %d", len(translationEntries))
		}
	})

	// --- Phase 5: Service deregistration ---

	t.Run("ServiceDeregistration", func(t *testing.T) {
		deregWatch := watchRoom(t, admin, consumer.ConfigRoomID)

		// Deregister the service by publishing an empty-content state event.
		// The daemon's syncServiceDirectory skips entries with empty
		// Principal (services.go:67), treating this as a deregistration.
		_, err = admin.SendStateEvent(ctx, serviceRoomID, "m.bureau.service",
			"service/stt/test", map[string]any{})
		if err != nil {
			t.Fatalf("deregister service: %v", err)
		}

		// Wait for the daemon to process the deregistration and push the
		// empty directory to all consumer proxies.
		deregWatch.WaitForMessage(t, "removed service/stt/test", consumer.UserID)

		entries := proxyServiceDiscovery(t, wideClient, "")
		if len(entries) != 0 {
			t.Errorf("wide consumer should see 0 services after deregistration, got %d", len(entries))
		}
	})

	// --- Phase 6: Partial visibility with second service ---

	t.Run("PartialVisibility", func(t *testing.T) {
		partialWatch := watchRoom(t, admin, consumer.ConfigRoomID)

		// Register a second service that the narrow consumer CAN see.
		embeddingAgent := registerPrincipal(t, "service/embedding/test", "embedding-password")
		deployPrincipals(t, admin, provider, deploymentConfig{
			Principals: []principalSpec{
				{Account: serviceAgent},
				{Account: embeddingAgent},
			},
		})

		_, err = admin.SendStateEvent(ctx, serviceRoomID, "m.bureau.service",
			"service/embedding/test", map[string]any{
				"principal":    embeddingAgent.UserID,
				"machine":      provider.UserID,
				"protocol":     "http",
				"capabilities": []string{"batch"},
				"description":  "Test embedding service",
			})
		if err != nil {
			t.Fatalf("publish embedding service event: %v", err)
		}

		// Wait for the consumer daemon to process the new service event
		// and push the updated directory to all consumer proxies.
		partialWatch.WaitForMessage(t, "added service/embedding/test", consumer.UserID)

		// The narrow consumer's visibility ["service/embedding/*"] matches
		// "service/embedding/test" but not the deregistered "service/stt/test".
		narrowEntries := proxyServiceDiscovery(t, narrowClient, "")
		if len(narrowEntries) != 1 {
			t.Fatalf("narrow consumer: expected 1 service, got %d", len(narrowEntries))
		}
		if narrowEntries[0].Localpart != "service/embedding/test" {
			t.Errorf("narrow consumer sees %q, want %q",
				narrowEntries[0].Localpart, "service/embedding/test")
		}

		// The wide consumer should also see exactly 1 service (the STT
		// service was deregistered in Phase 5).
		wideEntries := proxyServiceDiscovery(t, wideClient, "")
		if len(wideEntries) != 1 {
			t.Fatalf("wide consumer: expected 1 service, got %d", len(wideEntries))
		}
		if wideEntries[0].Localpart != "service/embedding/test" {
			t.Errorf("wide consumer sees %q, want %q",
				wideEntries[0].Localpart, "service/embedding/test")
		}
	})

	t.Log("service discovery lifecycle verified: register → propagate → filter → deregister → partial visibility")
}
