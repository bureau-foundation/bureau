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
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/principal"
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

	d.lastActivityAt = d.clock.Now()

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
		return fmt.Errorf("admin API returned %d: %s", response.StatusCode, netutil.ErrorBody(response.Body))
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

// putProxyAdmin sends a JSON PUT request to a principal's proxy admin API.
// Used by push-to-proxy functions (directory, authorization) which differ
// only in the endpoint path and payload type.
func (d *Daemon) putProxyAdmin(ctx context.Context, localpart, path string, payload any) error {
	adminSocket := d.adminSocketPathFunc(localpart)
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
func (d *Daemon) pushDirectoryToProxy(ctx context.Context, consumerLocalpart string, directory []adminDirectoryEntry) error {
	return d.putProxyAdmin(ctx, consumerLocalpart, "/v1/admin/directory", directory)
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

// pushAuthorizationToProxy pushes the pre-resolved authorization grants to a
// single principal's proxy via the admin API (PUT /v1/admin/authorization).
// Called when a new sandbox starts and when the principal's grants change at
// runtime (config change, temporal grant arrival/expiry).
func (d *Daemon) pushAuthorizationToProxy(ctx context.Context, localpart string, grants []schema.Grant) error {
	return d.putProxyAdmin(ctx, localpart, "/v1/admin/authorization", grants)
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

	if binding.Principal == "" {
		d.logger.Debug("artifact-cache binding has empty principal, clearing upstream")
		d.stopTunnelSocket()
		d.pushUpstreamConfig(ctx, "")
		return
	}

	// Look up the service in the directory to find which machine
	// runs it.
	cacheLocalpart, err := principal.LocalpartFromMatrixID(binding.Principal)
	if err != nil {
		d.logger.Error("invalid shared cache principal",
			"principal", binding.Principal,
			"error", err,
		)
		return
	}

	cacheService, exists := d.services[cacheLocalpart]
	if !exists {
		d.logger.Warn("artifact-cache binding references unknown service",
			"principal", binding.Principal,
			"localpart", cacheLocalpart,
		)
		return
	}

	if cacheService.Machine == d.machineUserID {
		// Local shared cache: derive socket path directly. Stop any
		// existing tunnel socket from a previous remote cache.
		d.stopTunnelSocket()
		socketPath := principal.RunDirSocketPath(d.runDir, cacheLocalpart)
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

		peerAddress, ok := d.peerAddresses[cacheService.Machine]
		if !ok || peerAddress == "" {
			d.logger.Warn("discovered remote shared cache but no transport address for its machine",
				"principal", binding.Principal,
				"machine", cacheService.Machine,
			)
			return
		}

		tunnelSocketPath := filepath.Join(d.runDir, "tunnel", "artifact-cache.sock")
		if err := d.startTunnelSocket(cacheLocalpart, peerAddress, tunnelSocketPath); err != nil {
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
	var artifactSocketPath string
	for localpart, svc := range d.services {
		if svc.Machine != d.machineUserID {
			continue
		}
		for _, cap := range svc.Capabilities {
			if cap == "content-addressed-store" {
				artifactSocketPath = principal.RunDirSocketPath(d.runDir, localpart)
				break
			}
		}
		if artifactSocketPath != "" {
			break
		}
	}
	if artifactSocketPath == "" {
		d.logger.Debug("no local artifact service found to push upstream config to")
		return
	}

	config := &servicetoken.UpstreamConfig{
		UpstreamSocket: socketPath,
		IssuedAt:       d.clock.Now().Unix(),
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
