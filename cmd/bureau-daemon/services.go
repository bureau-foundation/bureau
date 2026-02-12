// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// syncServiceDirectory fetches all m.bureau.service state events from the
// service room and updates the local cache. Returns the set of service
// localparts that were added, removed, or changed since the last sync.
//
// This is the foundation for service routing: the daemon needs to know what
// services exist and where they run before it can configure proxies to route
// to them.
func (d *Daemon) syncServiceDirectory(ctx context.Context) (added, removed, updated []string, err error) {
	events, err := d.session.GetRoomState(ctx, d.serviceRoomID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetching service room state: %w", err)
	}

	// Build the new directory from state events.
	current := make(map[string]*schema.Service, len(events))
	for _, event := range events {
		if event.Type != schema.EventTypeService {
			continue
		}
		if event.StateKey == nil {
			continue
		}

		// Re-marshal the map[string]any content to JSON, then unmarshal
		// into the typed struct. This round-trip is the cleanest way to
		// convert between the messaging library's generic Event and our
		// typed schema without coupling the two packages.
		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			d.logger.Warn("failed to marshal service event content",
				"state_key", *event.StateKey,
				"error", err,
			)
			continue
		}

		var service schema.Service
		if err := json.Unmarshal(contentJSON, &service); err != nil {
			d.logger.Warn("failed to parse service event",
				"state_key", *event.StateKey,
				"error", err,
			)
			continue
		}

		// Skip entries with empty principal — this means the service has
		// been deregistered (the state event was redacted or cleared).
		if service.Principal == "" {
			continue
		}

		current[*event.StateKey] = &service
	}

	// Diff against the cached directory.
	for localpart, service := range current {
		previous, existed := d.services[localpart]
		if !existed {
			added = append(added, localpart)
			d.logger.Info("service registered",
				"localpart", localpart,
				"principal", service.Principal,
				"machine", service.Machine,
				"protocol", service.Protocol,
			)
		} else if serviceChanged(previous, service) {
			updated = append(updated, localpart)
			d.logger.Info("service updated",
				"localpart", localpart,
				"principal", service.Principal,
				"machine", service.Machine,
				"protocol", service.Protocol,
			)
		}
	}

	for localpart, service := range d.services {
		if _, exists := current[localpart]; !exists {
			removed = append(removed, localpart)
			d.logger.Info("service deregistered",
				"localpart", localpart,
				"principal", service.Principal,
			)
		}
	}

	// Replace the cache.
	d.services = current
	return added, removed, updated, nil
}

// serviceChanged returns true if any fields of the service registration differ.
func serviceChanged(previous, current *schema.Service) bool {
	return previous.Principal != current.Principal ||
		previous.Machine != current.Machine ||
		previous.Protocol != current.Protocol ||
		previous.Description != current.Description ||
		!stringSlicesEqual(previous.Capabilities, current.Capabilities)
}

// stringSlicesEqual returns true if two string slices have the same elements
// in the same order.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// localServices returns the subset of the service directory running on this
// machine. These are services that the daemon can route to directly (via the
// provider's local proxy socket) without needing inter-daemon transport.
func (d *Daemon) localServices() map[string]*schema.Service {
	local := make(map[string]*schema.Service)
	for localpart, service := range d.services {
		if service.Machine == d.machineUserID {
			local[localpart] = service
		}
	}
	return local
}

// remoteServices returns the subset of the service directory running on other
// machines. These require inter-daemon transport for routing.
func (d *Daemon) remoteServices() map[string]*schema.Service {
	remote := make(map[string]*schema.Service)
	for localpart, service := range d.services {
		if service.Machine != d.machineUserID {
			remote[localpart] = service
		}
	}
	return remote
}

// reconcileServices is called after syncServiceDirectory detects changes. For
// each running principal, it determines which services should be reachable and
// updates the principal's proxy configuration via the admin API.
//
// For local services (provider on the same machine): the daemon derives the
// provider's proxy socket path from its localpart and registers a route on
// each consumer's proxy via PUT /v1/admin/services/{name}. The consumer
// then reaches the service via /http/<name>/... on its own proxy socket.
//
// For remote services (provider on another machine): routes through the
// relay socket when transport is configured. Consumer proxies send requests
// to the relay, which forwards them to the correct peer daemon.
func (d *Daemon) reconcileServices(ctx context.Context, added, removed, updated []string) {
	if len(added) == 0 && len(removed) == 0 && len(updated) == 0 {
		return
	}

	d.lastActivityAt = time.Now()

	for _, localpart := range added {
		service := d.services[localpart]
		serviceName := principal.ProxyServiceName(localpart)

		if service.Machine == d.machineUserID {
			providerLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
			if err != nil {
				d.logger.Error("cannot derive provider socket from service principal",
					"service", localpart,
					"principal", service.Principal,
					"error", err,
				)
				continue
			}
			providerSocket := principal.RunDirSocketPath(d.runDir, providerLocalpart)

			d.logger.Info("local service registered, configuring proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"provider_socket", providerSocket,
				"consumers", len(d.running),
			)
			d.configureProxyRoute(ctx, serviceName, providerSocket)
			d.proxyRoutes[serviceName] = providerSocket
		} else if d.relaySocketPath != "" {
			// Remote service with transport enabled: route through relay.
			d.logger.Info("remote service registered, configuring relay routes",
				"service", localpart,
				"proxy_name", serviceName,
				"machine", service.Machine,
				"consumers", len(d.running),
			)
			d.configureProxyRoute(ctx, serviceName, d.relaySocketPath)
			d.proxyRoutes[serviceName] = d.relaySocketPath
		} else {
			d.logger.Warn("remote service registered but transport not configured",
				"service", localpart,
				"machine", service.Machine,
				"protocol", service.Protocol,
			)
		}
	}

	for _, localpart := range removed {
		serviceName := principal.ProxyServiceName(localpart)
		if _, wasRouted := d.proxyRoutes[serviceName]; wasRouted {
			d.logger.Info("service removed, cleaning up proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"consumers", len(d.running),
			)
			d.removeProxyRoute(ctx, serviceName)
			delete(d.proxyRoutes, serviceName)
		}
	}

	for _, localpart := range updated {
		service := d.services[localpart]
		serviceName := principal.ProxyServiceName(localpart)

		if service.Machine == d.machineUserID {
			providerLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
			if err != nil {
				d.logger.Error("cannot derive provider socket from service principal",
					"service", localpart,
					"principal", service.Principal,
					"error", err,
				)
				continue
			}
			providerSocket := principal.RunDirSocketPath(d.runDir, providerLocalpart)

			d.logger.Info("local service updated, reconfiguring proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"provider_socket", providerSocket,
			)
			d.configureProxyRoute(ctx, serviceName, providerSocket)
			d.proxyRoutes[serviceName] = providerSocket
		} else if d.relaySocketPath != "" {
			// Remote service with transport: route through relay.
			d.logger.Info("remote service updated, reconfiguring relay routes",
				"service", localpart,
				"proxy_name", serviceName,
				"machine", service.Machine,
			)
			d.configureProxyRoute(ctx, serviceName, d.relaySocketPath)
			d.proxyRoutes[serviceName] = d.relaySocketPath
		} else if _, wasRouted := d.proxyRoutes[serviceName]; wasRouted {
			// Service moved to remote machine, no transport: remove route.
			d.logger.Info("service migrated to remote machine, removing route",
				"service", localpart,
				"proxy_name", serviceName,
				"machine", service.Machine,
			)
			d.removeProxyRoute(ctx, serviceName)
			delete(d.proxyRoutes, serviceName)
		}
	}
}

// configureProxyRoute registers a service route on all running consumers'
// proxies. Each consumer's proxy gets a PUT /v1/admin/services/{name} call
// pointing at the provider's Unix socket.
func (d *Daemon) configureProxyRoute(ctx context.Context, serviceName, providerSocket string) {
	for consumerLocalpart := range d.running {
		if err := d.registerProxyRoute(ctx, consumerLocalpart, serviceName, providerSocket); err != nil {
			d.logger.Error("failed to register service on consumer proxy",
				"service", serviceName,
				"consumer", consumerLocalpart,
				"error", err,
			)
		}
	}
}

// removeProxyRoute unregisters a service from all running consumers' proxies.
func (d *Daemon) removeProxyRoute(ctx context.Context, serviceName string) {
	for consumerLocalpart := range d.running {
		if err := d.unregisterProxyRoute(ctx, consumerLocalpart, serviceName); err != nil {
			d.logger.Error("failed to unregister service from consumer proxy",
				"service", serviceName,
				"consumer", consumerLocalpart,
				"error", err,
			)
		}
	}
}

// configureConsumerProxy registers all known service routes on a single
// consumer's proxy. Called after a new sandbox starts to ensure it can reach
// all services that were discovered before it started.
func (d *Daemon) configureConsumerProxy(ctx context.Context, consumerLocalpart string) {
	for serviceName, providerSocket := range d.proxyRoutes {
		if err := d.registerProxyRoute(ctx, consumerLocalpart, serviceName, providerSocket); err != nil {
			d.logger.Error("failed to register service on new consumer proxy",
				"service", serviceName,
				"consumer", consumerLocalpart,
				"error", err,
			)
		}
	}
}

// proxyAdminClient creates an HTTP client that communicates via a Unix socket.
// The daemon uses this to reach proxy admin sockets for service routing.
func proxyAdminClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// adminServiceRegistration is the JSON body for PUT /v1/admin/services/{name}.
// Matches the wire format of proxy.AdminServiceRequest but defined locally to
// keep the daemon binary decoupled from the proxy library.
type adminServiceRegistration struct {
	// UpstreamURL provides the URL for path construction. For local service
	// routing, this includes a path prefix that routes through the provider's
	// proxy: "http://localhost/http/<service-name>". The actual connection
	// goes through UpstreamUnix.
	UpstreamURL string `json:"upstream_url,omitempty"`

	// UpstreamUnix is the provider's proxy admin socket path.
	UpstreamUnix string `json:"upstream_unix,omitempty"`
}

// registerProxyRoute registers a single service on a single consumer's proxy
// via the admin API (PUT /v1/admin/services/{name}).
func (d *Daemon) registerProxyRoute(ctx context.Context, consumerLocalpart, serviceName, providerSocket string) error {
	adminSocket := d.adminSocketPathFunc(consumerLocalpart)
	client := proxyAdminClient(adminSocket)

	// The upstream URL includes the path prefix so requests are forwarded
	// to /http/<service-name>/... on the provider's proxy. The provider's
	// proxy uses the same flat service name (by convention) and routes to
	// the actual backend. The Unix socket provides the physical transport.
	body, err := json.Marshal(adminServiceRegistration{
		UpstreamURL:  "http://localhost/http/" + serviceName,
		UpstreamUnix: providerSocket,
	})
	if err != nil {
		return fmt.Errorf("marshaling registration request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost/v1/admin/services/"+serviceName,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, string(responseBody))
	}

	return nil
}

// pushServiceDirectory pushes the current service directory to all running
// consumer proxies via the admin API (PUT /v1/admin/directory). Called after
// syncServiceDirectory detects changes and after new consumers start.
func (d *Daemon) pushServiceDirectory(ctx context.Context) {
	if len(d.running) == 0 {
		return
	}

	directory := d.buildServiceDirectory()

	for consumerLocalpart := range d.running {
		if err := d.pushDirectoryToProxy(ctx, consumerLocalpart, directory); err != nil {
			d.logger.Error("failed to push service directory to consumer proxy",
				"consumer", consumerLocalpart,
				"error", err,
			)
		}
	}
}

// pushDirectoryToProxy pushes the service directory to a single consumer's
// proxy via the admin API (PUT /v1/admin/directory). Called when a new
// sandbox starts and after the directory changes.
func (d *Daemon) pushDirectoryToProxy(ctx context.Context, consumerLocalpart string, directory []adminDirectoryEntry) error {
	adminSocket := d.adminSocketPathFunc(consumerLocalpart)
	client := proxyAdminClient(adminSocket)

	body, err := json.Marshal(directory)
	if err != nil {
		return fmt.Errorf("marshaling directory: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost/v1/admin/directory",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, string(responseBody))
	}

	return nil
}

// buildServiceDirectory converts the daemon's service map into a wire-format
// directory suitable for pushing to consumer proxies.
func (d *Daemon) buildServiceDirectory() []adminDirectoryEntry {
	entries := make([]adminDirectoryEntry, 0, len(d.services))
	for localpart, service := range d.services {
		entries = append(entries, adminDirectoryEntry{
			Localpart:    localpart,
			Principal:    service.Principal,
			Machine:      service.Machine,
			Protocol:     service.Protocol,
			Description:  service.Description,
			Capabilities: service.Capabilities,
			Metadata:     service.Metadata,
		})
	}
	return entries
}

// adminDirectoryEntry is the JSON wire format for a single entry in the
// service directory pushed to consumer proxies. Matches the proxy's
// ServiceDirectoryEntry but defined locally to keep the daemon binary
// decoupled from the proxy library.
type adminDirectoryEntry struct {
	Localpart    string         `json:"localpart"`
	Principal    string         `json:"principal"`
	Machine      string         `json:"machine"`
	Protocol     string         `json:"protocol"`
	Description  string         `json:"description,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// pushVisibilityToProxy pushes the service visibility patterns for a specific
// consumer to its proxy via the admin API (PUT /v1/admin/visibility). Called
// when a new sandbox starts so the proxy knows which services the agent is
// allowed to discover.
func (d *Daemon) pushVisibilityToProxy(ctx context.Context, consumerLocalpart string, patterns []string) error {
	adminSocket := d.adminSocketPathFunc(consumerLocalpart)
	client := proxyAdminClient(adminSocket)

	body, err := json.Marshal(patterns)
	if err != nil {
		return fmt.Errorf("marshaling visibility patterns: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost/v1/admin/visibility",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, string(responseBody))
	}

	return nil
}

// pushMatrixPolicyToProxy pushes the Matrix access policy to a single
// principal's proxy via the admin API (PUT /v1/admin/policy). Called when
// a running principal's MatrixPolicy changes in the MachineConfig.
func (d *Daemon) pushMatrixPolicyToProxy(ctx context.Context, localpart string, policy *schema.MatrixPolicy) error {
	adminSocket := d.adminSocketPathFunc(localpart)
	client := proxyAdminClient(adminSocket)

	body, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("marshaling matrix policy: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost/v1/admin/policy",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, string(responseBody))
	}

	return nil
}

// unregisterProxyRoute removes a single service from a single consumer's proxy
// via the admin API (DELETE /v1/admin/services/{name}).
func (d *Daemon) unregisterProxyRoute(ctx context.Context, consumerLocalpart, serviceName string) error {
	adminSocket := d.adminSocketPathFunc(consumerLocalpart)
	client := proxyAdminClient(adminSocket)

	request, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://localhost/v1/admin/services/"+serviceName,
		nil,
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	// 404 is acceptable — the service may have been removed already (e.g.,
	// the proxy restarted and lost its in-memory state).
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, string(responseBody))
	}

	return nil
}
