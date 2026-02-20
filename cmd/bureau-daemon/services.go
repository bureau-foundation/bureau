// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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
		if service.Principal.IsZero() {
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
		if service.Machine == d.machine {
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
		if service.Machine != d.machine {
			remote[localpart] = service
		}
	}
	return remote
}

// runningConsumers returns a snapshot of the currently running principal
// localparts. The snapshot is taken under reconcileMu.RLock so callers
// can iterate it without holding the lock during network I/O. Used by
// service directory operations that push to proxy admin sockets — taking
// a snapshot avoids concurrent map iteration with background goroutines
// (watchSandboxExit, rollbackPrincipal) that modify d.running under
// reconcileMu.Lock.
func (d *Daemon) runningConsumers() []ref.Entity {
	d.reconcileMu.RLock()
	defer d.reconcileMu.RUnlock()
	consumers := make([]ref.Entity, 0, len(d.running))
	for principal := range d.running {
		consumers = append(consumers, principal)
	}
	return consumers
}

// reconcileServices is called after syncServiceDirectory detects changes. For
// each consumer in the provided snapshot, it determines which services should
// be reachable and updates the consumer's proxy configuration via the admin
// API.
//
// The consumers parameter is a snapshot of d.running taken under
// reconcileMu.RLock by the caller. This avoids holding the lock during
// proxy admin HTTP calls while preventing concurrent map iteration with
// background goroutines that modify d.running.
//
// For local services (provider on the same machine): the daemon derives the
// provider's proxy socket path from its localpart and registers a route on
// each consumer's proxy via PUT /v1/admin/services/{name}. The consumer
// then reaches the service via /http/<name>/... on its own proxy socket.
//
// For remote services (provider on another machine): routes through the
// relay socket when transport is configured. Consumer proxies send requests
// to the relay, which forwards them to the correct peer daemon.
func (d *Daemon) reconcileServices(ctx context.Context, consumers []ref.Entity, added, removed, updated []string) {
	if len(added) == 0 && len(removed) == 0 && len(updated) == 0 {
		return
	}

	d.lastActivityAt = d.clock.Now()

	for _, localpart := range added {
		service := d.services[localpart]
		serviceName := principal.ProxyServiceName(localpart)

		if service.Machine == d.machine {
			providerSocket := service.Principal.SocketPath(d.fleetRunDir)

			d.logger.Info("local service registered, configuring proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"provider_socket", providerSocket,
				"consumers", len(consumers),
			)
			d.configureProxyRoute(ctx, consumers, serviceName, providerSocket)
			d.reconcileMu.Lock()
			d.proxyRoutes[serviceName] = providerSocket
			d.reconcileMu.Unlock()
		} else if d.relaySocketPath != "" {
			// Remote service with transport enabled: route through relay.
			d.logger.Info("remote service registered, configuring relay routes",
				"service", localpart,
				"proxy_name", serviceName,
				"machine", service.Machine,
				"consumers", len(consumers),
			)
			d.configureProxyRoute(ctx, consumers, serviceName, d.relaySocketPath)
			d.reconcileMu.Lock()
			d.proxyRoutes[serviceName] = d.relaySocketPath
			d.reconcileMu.Unlock()
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
		d.reconcileMu.RLock()
		_, wasRouted := d.proxyRoutes[serviceName]
		d.reconcileMu.RUnlock()
		if wasRouted {
			d.logger.Info("service removed, cleaning up proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"consumers", len(consumers),
			)
			d.removeProxyRoute(ctx, consumers, serviceName)
			d.reconcileMu.Lock()
			delete(d.proxyRoutes, serviceName)
			d.reconcileMu.Unlock()
		}
	}

	for _, localpart := range updated {
		service := d.services[localpart]
		serviceName := principal.ProxyServiceName(localpart)

		if service.Machine == d.machine {
			providerSocket := service.Principal.SocketPath(d.fleetRunDir)

			d.logger.Info("local service updated, reconfiguring proxy routes",
				"service", localpart,
				"proxy_name", serviceName,
				"provider_socket", providerSocket,
			)
			d.configureProxyRoute(ctx, consumers, serviceName, providerSocket)
			d.reconcileMu.Lock()
			d.proxyRoutes[serviceName] = providerSocket
			d.reconcileMu.Unlock()
		} else if d.relaySocketPath != "" {
			// Remote service with transport: route through relay.
			d.logger.Info("remote service updated, reconfiguring relay routes",
				"service", localpart,
				"proxy_name", serviceName,
				"machine", service.Machine,
			)
			d.configureProxyRoute(ctx, consumers, serviceName, d.relaySocketPath)
			d.reconcileMu.Lock()
			d.proxyRoutes[serviceName] = d.relaySocketPath
			d.reconcileMu.Unlock()
		} else {
			d.reconcileMu.RLock()
			_, wasRouted := d.proxyRoutes[serviceName]
			d.reconcileMu.RUnlock()
			if wasRouted {
				// Service moved to remote machine, no transport: remove route.
				d.logger.Info("service migrated to remote machine, removing route",
					"service", localpart,
					"proxy_name", serviceName,
					"machine", service.Machine,
				)
				d.removeProxyRoute(ctx, consumers, serviceName)
				d.reconcileMu.Lock()
				delete(d.proxyRoutes, serviceName)
				d.reconcileMu.Unlock()
			}
		}
	}
}

// configureProxyRoute registers a service route on each consumer proxy in
// the snapshot. Each consumer's proxy gets a PUT /v1/admin/services/{name}
// call pointing at the provider's Unix socket. The consumers slice is a
// snapshot of d.running taken under reconcileMu.RLock, so this function
// does not need to hold the lock during the HTTP calls.
func (d *Daemon) configureProxyRoute(ctx context.Context, consumers []ref.Entity, serviceName, providerSocket string) {
	for _, consumer := range consumers {
		if err := d.registerProxyRoute(ctx, consumer, serviceName, providerSocket); err != nil {
			d.logger.Error("failed to register service on consumer proxy",
				"service", serviceName,
				"consumer", consumer,
				"error", err,
			)
		}
	}
}

// removeProxyRoute unregisters a service from each consumer proxy in the
// snapshot. The consumers slice is a snapshot of d.running taken under
// reconcileMu.RLock.
func (d *Daemon) removeProxyRoute(ctx context.Context, consumers []ref.Entity, serviceName string) {
	for _, consumer := range consumers {
		if err := d.unregisterProxyRoute(ctx, consumer, serviceName); err != nil {
			d.logger.Error("failed to unregister service from consumer proxy",
				"service", serviceName,
				"consumer", consumer,
				"error", err,
			)
		}
	}
}

// configureConsumerProxy registers all known service routes on a single
// consumer's proxy. Called after a new sandbox starts to ensure it can reach
// all services that were discovered before it started.
func (d *Daemon) configureConsumerProxy(ctx context.Context, consumer ref.Entity) {
	for serviceName, providerSocket := range d.proxyRoutes {
		if err := d.registerProxyRoute(ctx, consumer, serviceName, providerSocket); err != nil {
			d.logger.Error("failed to register service on new consumer proxy",
				"service", serviceName,
				"consumer", consumer,
				"error", err,
			)
		}
	}
}

// configureExternalProxyServices registers external HTTP API upstreams on a
// consumer's proxy. These are services declared in the template's ProxyServices
// field — external APIs (like Anthropic, OpenAI) that the proxy should forward
// to with credential injection. Called after a new sandbox starts or when an
// adopted principal's proxy is configured.
func (d *Daemon) configureExternalProxyServices(ctx context.Context, consumer ref.Entity, proxyServices map[string]schema.TemplateProxyService) {
	for name, service := range proxyServices {
		if err := d.registerExternalProxyService(ctx, consumer, name, service); err != nil {
			d.logger.Error("failed to register external proxy service",
				"service", name,
				"consumer", consumer,
				"upstream", service.Upstream,
				"error", err,
			)
		}
	}
}

// registerExternalProxyService registers a single external HTTP API upstream
// on a consumer's proxy via the admin API (PUT /v1/admin/services/{name}).
// Unlike registerProxyRoute (which routes to another principal's proxy socket),
// this registers an internet-facing upstream with credential injection headers.
func (d *Daemon) registerExternalProxyService(ctx context.Context, consumer ref.Entity, serviceName string, service schema.TemplateProxyService) error {
	adminSocket := d.adminSocketPathFunc(consumer)
	client := proxyAdminClient(adminSocket)

	body, err := json.Marshal(adminServiceRegistration{
		UpstreamURL:   service.Upstream,
		InjectHeaders: service.InjectHeaders,
		StripHeaders:  service.StripHeaders,
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
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	d.logger.Info("registered external proxy service",
		"service", serviceName,
		"consumer", consumer,
		"upstream", service.Upstream,
	)
	return nil
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
	// proxy: "http://localhost/http/<service-name>". For external services,
	// this is the actual upstream URL (e.g., "https://api.anthropic.com").
	// The actual connection goes through UpstreamUnix when set.
	UpstreamURL string `json:"upstream_url,omitempty"`

	// UpstreamUnix is the provider's proxy socket path. Used for local
	// service routing where the upstream is another principal's proxy.
	UpstreamUnix string `json:"upstream_unix,omitempty"`

	// InjectHeaders maps HTTP header names to credential names. The proxy
	// reads each credential from the principal's credential bundle and sets
	// it as the specified header on upstream requests.
	InjectHeaders map[string]string `json:"inject_headers,omitempty"`

	// StripHeaders lists HTTP headers to remove from incoming requests
	// before forwarding to the upstream.
	StripHeaders []string `json:"strip_headers,omitempty"`
}

// registerProxyRoute registers a single service on a single consumer's proxy
// via the admin API (PUT /v1/admin/services/{name}).
func (d *Daemon) registerProxyRoute(ctx context.Context, consumer ref.Entity, serviceName, providerSocket string) error {
	adminSocket := d.adminSocketPathFunc(consumer)
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
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	return nil
}

// pushServiceDirectory pushes the current service directory to each consumer
// proxy in the snapshot via the admin API (PUT /v1/admin/directory). The
// consumers slice is a snapshot of d.running taken under reconcileMu.RLock,
// so this function does not need to hold the lock during the HTTP calls.
// Called after syncServiceDirectory detects changes and after new consumers
// start.
func (d *Daemon) pushServiceDirectory(ctx context.Context, consumers []ref.Entity) {
	if len(consumers) == 0 {
		return
	}

	directory := d.buildServiceDirectory()

	for _, consumer := range consumers {
		if err := d.pushDirectoryToProxy(ctx, consumer, directory); err != nil {
			d.logger.Error("failed to push service directory to consumer proxy",
				"consumer", consumer,
				"error", err,
			)
		}
	}
}

// putProxyAdmin sends a JSON PUT request to a principal's proxy admin API.
// Used by push-to-proxy functions (directory, authorization) which differ
// only in the endpoint path and payload type.
func (d *Daemon) putProxyAdmin(ctx context.Context, principal ref.Entity, path string, payload any) error {
	adminSocket := d.adminSocketPathFunc(principal)
	client := proxyAdminClient(adminSocket)

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s payload: %w", path, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost"+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("creating %s request: %w", path, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connecting to admin socket %s: %w", adminSocket, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d: %s", path, response.StatusCode, netutil.ErrorBody(response.Body))
	}

	return nil
}

// pushDirectoryToProxy pushes the service directory to a single consumer's
// proxy via the admin API (PUT /v1/admin/directory). Called when a new
// sandbox starts and after the directory changes.
func (d *Daemon) pushDirectoryToProxy(ctx context.Context, consumer ref.Entity, directory []adminDirectoryEntry) error {
	return d.putProxyAdmin(ctx, consumer, "/v1/admin/directory", directory)
}

// buildServiceDirectory converts the daemon's service map into a wire-format
// directory suitable for pushing to consumer proxies.
func (d *Daemon) buildServiceDirectory() []adminDirectoryEntry {
	entries := make([]adminDirectoryEntry, 0, len(d.services))
	for localpart, service := range d.services {
		entries = append(entries, adminDirectoryEntry{
			Localpart:    localpart,
			Principal:    service.Principal.UserID().String(),
			Machine:      service.Machine.UserID().String(),
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

// pushAuthorizationToProxy pushes the pre-resolved authorization grants to a
// single principal's proxy via the admin API (PUT /v1/admin/authorization).
// Called when a new sandbox starts and when the principal's grants change at
// runtime (config change, temporal grant arrival/expiry).
func (d *Daemon) pushAuthorizationToProxy(ctx context.Context, principal ref.Entity, grants []schema.Grant) error {
	return d.putProxyAdmin(ctx, principal, "/v1/admin/authorization", grants)
}

// synthesizeGrants converts the shorthand MatrixPolicy and ServiceVisibility
// fields on PrincipalAssignment into authorization grants. These fields are
// the simple configuration surface — operators set booleans and glob patterns
// instead of spelling out full Grant objects. When a principal has the full
// Authorization field set (or the machine has DefaultPolicy), the daemon
// uses the authorization index instead of calling this function.
func synthesizeGrants(policy *schema.MatrixPolicy, visibility []string) []schema.Grant {
	var grants []schema.Grant
	if policy != nil {
		if policy.AllowJoin {
			grants = append(grants, schema.Grant{Actions: []string{"matrix/join"}})
		}
		if policy.AllowInvite {
			grants = append(grants, schema.Grant{Actions: []string{"matrix/invite"}})
		}
		if policy.AllowRoomCreate {
			grants = append(grants, schema.Grant{Actions: []string{"matrix/create-room"}})
		}
	}
	if len(visibility) > 0 {
		grants = append(grants, schema.Grant{
			Actions: []string{"service/discover"},
			Targets: visibility,
		})
	}
	return grants
}

// resolveGrantsForProxy returns the authorization grants that should be pushed
// to a principal's proxy. If the machine config or per-principal assignment
// uses the authorization framework (DefaultPolicy or per-principal
// Authorization), the grants come from the pre-merged authorization index.
// Otherwise, grants are synthesized from the shorthand MatrixPolicy and
// ServiceVisibility fields.
func (d *Daemon) resolveGrantsForProxy(localpart string, assignment schema.PrincipalAssignment, config *schema.MachineConfig) []schema.Grant {
	if config.DefaultPolicy != nil || assignment.Authorization != nil {
		return d.authorizationIndex.Grants(localpart)
	}
	return synthesizeGrants(assignment.MatrixPolicy, assignment.ServiceVisibility)
}

// unregisterProxyRoute removes a single service from a single consumer's proxy
// via the admin API (DELETE /v1/admin/services/{name}).
func (d *Daemon) unregisterProxyRoute(ctx context.Context, consumer ref.Entity, serviceName string) error {
	adminSocket := d.adminSocketPathFunc(consumer)
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
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
	}

	return nil
}

// discoverSharedCache looks for an "artifact-cache" room service
// binding in #bureau/service. If found, resolves the shared cache
// service principal to a socket path and pushes the upstream
// configuration to the local artifact service.
//
// For local shared caches (same machine): derives the socket path
// from the principal's localpart. For remote shared caches (different
// machine): creates a transport tunnel socket that bridges connections
// to the remote service.
func (d *Daemon) discoverSharedCache(ctx context.Context) {
	content, err := d.session.GetStateEvent(ctx, d.serviceRoomID, schema.EventTypeRoomService, "artifact-cache")
	if err != nil {
		// No artifact-cache binding is normal — not all fleets have
		// a shared cache. Only log at debug level.
		d.logger.Debug("no artifact-cache binding in service room",
			"error", err,
		)
		return
	}

	var binding schema.RoomServiceContent
	if err := json.Unmarshal(content, &binding); err != nil {
		d.logger.Error("failed to parse artifact-cache binding",
			"error", err,
		)
		return
	}

	if binding.Principal.IsZero() {
		d.logger.Debug("artifact-cache binding has empty principal, clearing upstream")
		d.stopTunnel("upstream")
		d.pushUpstreamConfig(ctx, "")
		return
	}

	// Look up the service in the directory to find which machine runs it.
	cacheService, exists := d.services[binding.Principal.Localpart()]
	if !exists {
		d.logger.Warn("artifact-cache binding references unknown service",
			"principal", binding.Principal,
			"localpart", binding.Principal.Localpart(),
		)
		return
	}

	if cacheService.Machine == d.machine {
		// Local shared cache: derive socket path directly. Stop any
		// existing tunnel from a previous remote cache.
		d.stopTunnel("upstream")
		socketPath := binding.Principal.SocketPath(d.fleetRunDir)
		d.logger.Info("discovered local shared cache",
			"principal", binding.Principal,
			"socket", socketPath,
		)
		d.pushUpstreamConfig(ctx, socketPath)
	} else {
		// Remote shared cache: create a transport tunnel socket that
		// bridges connections to the remote service via the transport.
		if d.transportDialer == nil {
			d.logger.Warn("discovered remote shared cache but transport is not configured",
				"principal", binding.Principal,
				"machine", cacheService.Machine,
			)
			return
		}

		peerAddress, ok := d.peerAddresses[cacheService.Machine.UserID().String()]
		if !ok || peerAddress == "" {
			d.logger.Warn("discovered remote shared cache but no transport address for its machine",
				"principal", binding.Principal,
				"machine", cacheService.Machine,
			)
			return
		}

		tunnelSocketPath := filepath.Join(d.runDir, "tunnel", "artifact-cache.sock")
		if err := d.startTunnel("upstream", binding.Principal.Localpart(), peerAddress, tunnelSocketPath); err != nil {
			d.logger.Error("failed to start tunnel socket for shared cache",
				"principal", binding.Principal,
				"peer_address", peerAddress,
				"error", err,
			)
			return
		}

		d.logger.Info("discovered remote shared cache, tunnel started",
			"principal", binding.Principal,
			"machine", cacheService.Machine,
			"peer_address", peerAddress,
			"tunnel_socket", tunnelSocketPath,
		)
		d.pushUpstreamConfig(ctx, tunnelSocketPath)
	}
}

// pushUpstreamConfig signs an upstream configuration message and
// pushes it to the local artifact service. The artifact service
// verifies the signature against the daemon's public key.
//
// An empty socketPath means "clear the upstream" — the artifact
// service will stop forwarding cache misses.
func (d *Daemon) pushUpstreamConfig(ctx context.Context, socketPath string) {
	// Find the local artifact service socket. Search the service
	// directory for a local service with the "artifact" capability.
	artifactSocketPath := d.findLocalArtifactSocket()
	if artifactSocketPath == "" {
		d.logger.Debug("no local artifact service found to push upstream config to")
		return
	}

	config := &servicetoken.UpstreamConfig{
		UpstreamSocket: socketPath,
		IssuedAt:       d.clock.Now().Unix(),
	}

	// For local upstream connections, mint a service token so the
	// artifact service can authenticate against the upstream. For
	// remote connections (via tunnel), leave nil — the tunnel
	// handler on the remote machine injects a token signed by that
	// machine's daemon.
	if socketPath != "" && !d.isTunnelSocket(socketPath) {
		tokenID, err := generateTokenID()
		if err != nil {
			d.logger.Error("failed to generate token ID for upstream token",
				"error", err,
			)
			return
		}

		now := d.clock.Now()
		token := &servicetoken.Token{
			Subject:   d.machine.Localpart(),
			Machine:   d.machine.Localpart(),
			Audience:  "artifact",
			Grants:    []servicetoken.Grant{{Actions: []string{"artifact/*"}}},
			ID:        tokenID,
			IssuedAt:  now.Unix(),
			ExpiresAt: now.Add(5 * time.Minute).Unix(),
		}

		tokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, token)
		if err != nil {
			d.logger.Error("failed to mint upstream service token",
				"error", err,
			)
			return
		}
		config.ServiceToken = tokenBytes
	}

	signed, err := servicetoken.SignUpstreamConfig(d.tokenSigningPrivateKey, config)
	if err != nil {
		d.logger.Error("failed to sign upstream config",
			"upstream_socket", socketPath,
			"error", err,
		)
		return
	}

	if err := artifactServiceCall(artifactSocketPath, map[string]any{
		"action":        "set-upstream",
		"signed_config": signed,
	}, nil); err != nil {
		d.logger.Warn("failed to push upstream config to artifact service",
			"artifact_socket", artifactSocketPath,
			"upstream_socket", socketPath,
			"error", err,
		)
		return
	}

	d.logger.Info("pushed upstream config to artifact service",
		"artifact_socket", artifactSocketPath,
		"upstream_socket", socketPath,
	)
}

// discoverPushTargets builds the push target directory for the local
// artifact service. Each remote artifact service (content-addressed-store
// capability, different machine) gets a tunnel socket for push-through
// replication. Local artifact services on other instances get direct
// socket paths with minted tokens.
//
// Tunnels are named "push/<machineLocalpart>" and stopped when the
// remote service disappears from the directory.
func (d *Daemon) discoverPushTargets(ctx context.Context) {
	targets := make(map[string]servicetoken.PushTarget)

	// Track which push tunnels should exist after this pass so we
	// can stop tunnels for machines no longer in the directory.
	activePushTunnels := make(map[string]bool)

	for localpart, service := range d.services {
		// Only consider artifact services.
		hasCapability := false
		for _, cap := range service.Capabilities {
			if cap == "content-addressed-store" {
				hasCapability = true
				break
			}
		}
		if !hasCapability {
			continue
		}

		// Skip our own local artifact service — no point pushing
		// to ourselves.
		if service.Machine == d.machine {
			continue
		}

		machineLocalpart := service.Machine.Localpart()

		peerAddress, ok := d.peerAddresses[service.Machine.UserID().String()]
		if !ok || peerAddress == "" {
			d.logger.Debug("skipping push target: no transport address",
				"service", localpart,
				"machine", service.Machine,
			)
			continue
		}

		if d.transportDialer == nil {
			d.logger.Debug("skipping push target: transport not configured",
				"service", localpart,
				"machine", service.Machine,
			)
			continue
		}

		// Create a tunnel for this remote artifact service.
		tunnelName := "push/" + machineLocalpart
		sanitized := strings.ReplaceAll(machineLocalpart, "/", "-")
		tunnelSocketPath := filepath.Join(d.runDir, "tunnel", "push-"+sanitized+".sock")

		if err := d.startTunnel(tunnelName, localpart, peerAddress, tunnelSocketPath); err != nil {
			d.logger.Error("failed to start push tunnel",
				"machine", machineLocalpart,
				"peer_address", peerAddress,
				"error", err,
			)
			continue
		}

		activePushTunnels[tunnelName] = true

		// Remote targets: token is nil (tunnel handler injects it).
		targets[machineLocalpart] = servicetoken.PushTarget{
			SocketPath: tunnelSocketPath,
		}

		d.logger.Info("push target configured",
			"machine", machineLocalpart,
			"tunnel_socket", tunnelSocketPath,
			"peer_address", peerAddress,
		)
	}

	// Stop push tunnels for machines no longer in the directory.
	for name := range d.tunnels {
		if !strings.HasPrefix(name, "push/") {
			continue
		}
		if !activePushTunnels[name] {
			d.logger.Info("stopping stale push tunnel", "name", name)
			d.stopTunnel(name)
		}
	}

	// Push the configuration to the local artifact service.
	d.pushPushTargetsConfig(ctx, targets)
}

// pushPushTargetsConfig signs a push targets configuration and pushes
// it to the local artifact service via the set-push-targets action.
func (d *Daemon) pushPushTargetsConfig(ctx context.Context, targets map[string]servicetoken.PushTarget) {
	artifactSocketPath := d.findLocalArtifactSocket()
	if artifactSocketPath == "" {
		d.logger.Debug("no local artifact service found to push push-targets config to")
		return
	}

	config := &servicetoken.PushTargetsConfig{
		Targets:  targets,
		IssuedAt: d.clock.Now().Unix(),
	}

	signed, err := servicetoken.SignPushTargetsConfig(d.tokenSigningPrivateKey, config)
	if err != nil {
		d.logger.Error("failed to sign push targets config", "error", err)
		return
	}

	if err := artifactServiceCall(artifactSocketPath, map[string]any{
		"action":        "set-push-targets",
		"signed_config": signed,
	}, nil); err != nil {
		d.logger.Warn("failed to push push-targets config to artifact service",
			"artifact_socket", artifactSocketPath,
			"targets", len(targets),
			"error", err,
		)
		return
	}

	d.logger.Info("pushed push-targets config to artifact service",
		"artifact_socket", artifactSocketPath,
		"targets", len(targets),
	)
}

// isTunnelSocket returns true if the given socket path belongs to a
// daemon-managed tunnel. Tunnel sockets live in the "tunnel"
// subdirectory of the daemon's run directory.
func (d *Daemon) isTunnelSocket(socketPath string) bool {
	for _, tunnel := range d.tunnels {
		if tunnel.socketPath == socketPath {
			return true
		}
	}
	return false
}

// artifactServiceCall sends a CBOR message to the artifact service
// over its Unix socket using the artifact wire protocol (4-byte
// uint32 big-endian length prefix + CBOR bytes). This is separate
// from the standard lib/service.ServiceClient which uses bare CBOR
// with a Response{OK, Error, Data} envelope — the artifact service
// uses a different protocol because artifact transfers interleave
// CBOR messages with raw binary streams.
//
// The request is a map of fields that will be CBOR-encoded. The
// response is decoded into result (if non-nil). Returns an error if
// the response contains an "error" field (artifact ErrorResponse).
func artifactServiceCall(socketPath string, request map[string]any, result any) error {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to artifact service at %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Encode and write the length-prefixed CBOR request.
	requestBytes, err := codec.Marshal(request)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	var lengthPrefix [4]byte
	binary.BigEndian.PutUint32(lengthPrefix[:], uint32(len(requestBytes)))
	if _, err := conn.Write(lengthPrefix[:]); err != nil {
		return fmt.Errorf("writing request length: %w", err)
	}
	if _, err := conn.Write(requestBytes); err != nil {
		return fmt.Errorf("writing request body: %w", err)
	}

	// Read the length-prefixed CBOR response.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var responseLength [4]byte
	if _, err := io.ReadFull(conn, responseLength[:]); err != nil {
		return fmt.Errorf("reading response length: %w", err)
	}
	length := binary.BigEndian.Uint32(responseLength[:])
	if length > 64*1024 {
		return fmt.Errorf("response size %d exceeds maximum", length)
	}
	responseBytes := make([]byte, length)
	if _, err := io.ReadFull(conn, responseBytes); err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	// Check for error response.
	var errorResponse struct {
		Error string `json:"error"`
	}
	if err := codec.Unmarshal(responseBytes, &errorResponse); err == nil && errorResponse.Error != "" {
		return fmt.Errorf("artifact service error: %s", errorResponse.Error)
	}

	// Decode into result if provided.
	if result != nil {
		if err := codec.Unmarshal(responseBytes, result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}
