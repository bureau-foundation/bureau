// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-daemon is the unprivileged, network-facing Bureau process. It connects
// to the Matrix homeserver, reads machine configuration, and orchestrates the
// launcher (via unix socket IPC) to create and destroy sandboxes.
//
// The daemon has no access to credentials or private keys — it forwards
// encrypted credential bundles to the launcher, which decrypts them. This
// privilege separation means the network-facing process never holds plaintext
// secrets.
//
// On startup:
//  1. Loads the Matrix session written by the launcher during first-boot.
//  2. Ensures the per-machine config room exists.
//  3. Reads MachineConfig state to determine which principals to run.
//  4. Reconciles: creates missing sandboxes, destroys extra ones.
//  5. Enters a polling loop to watch for config changes.
//  6. Periodically publishes MachineStatus heartbeats.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		homeserverURL    string
		machineName      string
		serverName       string
		stateDir         string
		launcherSocket   string
		adminUser        string
		transportListen  string
		relaySocket      string
		pollInterval     time.Duration
		statusInterval   time.Duration
		showVersion      bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/bureau", "directory containing session.json from the launcher")
	flag.StringVar(&launcherSocket, "launcher-socket", "/run/bureau/launcher.sock", "path to the launcher IPC socket")
	flag.StringVar(&adminUser, "admin-user", "bureau-admin", "admin account username (for config room invites)")
	flag.StringVar(&transportListen, "transport-listen", "", "TCP address for inbound transport connections from peer daemons (e.g., :7891)")
	flag.StringVar(&relaySocket, "relay-socket", "/run/bureau/relay.sock", "Unix socket path for the transport relay (consumer proxies connect here for remote services)")
	flag.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "how often to poll for config changes")
	flag.DurationVar(&statusInterval, "status-interval", 60*time.Second, "how often to publish machine status")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-daemon %s\n", version.Info())
		return nil
	}

	if machineName == "" {
		return fmt.Errorf("--machine-name is required")
	}
	if err := principal.ValidateLocalpart(machineName); err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load the Matrix session saved by the launcher.
	session, err := loadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	// Validate the session is still valid.
	userID, err := session.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("validating matrix session: %w", err)
	}
	logger.Info("matrix session valid", "user_id", userID)

	// Ensure the per-machine config room exists.
	configRoomAlias := principal.RoomAlias("bureau/config/"+machineName, serverName)
	configRoomID, err := ensureConfigRoom(ctx, session, configRoomAlias, machineName, serverName, adminUser, logger)
	if err != nil {
		return fmt.Errorf("ensuring config room: %w", err)
	}
	logger.Info("config room ready", "room_id", configRoomID, "alias", configRoomAlias)

	// Resolve the machines room for status reporting.
	machinesAlias := principal.RoomAlias("bureau/machines", serverName)
	machinesRoomID, err := session.ResolveAlias(ctx, machinesAlias)
	if err != nil {
		return fmt.Errorf("resolving machines room alias %q: %w", machinesAlias, err)
	}

	// Resolve the services room for the service directory.
	servicesAlias := principal.RoomAlias("bureau/services", serverName)
	servicesRoomID, err := session.ResolveAlias(ctx, servicesAlias)
	if err != nil {
		return fmt.Errorf("resolving services room alias %q: %w", servicesAlias, err)
	}
	logger.Info("services room ready", "room_id", servicesRoomID, "alias", servicesAlias)

	machineUserID := principal.MatrixUserID(machineName, serverName)

	daemon := &Daemon{
		session:             session,
		machineName:         machineName,
		machineUserID:       machineUserID,
		serverName:          serverName,
		configRoomID:        configRoomID,
		machinesRoomID:      machinesRoomID,
		servicesRoomID:      servicesRoomID,
		launcherSocket:      launcherSocket,
		pollInterval:        pollInterval,
		statusInterval:      statusInterval,
		running:             make(map[string]bool),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		peerAddresses:       make(map[string]string),
		peerTransports:      make(map[string]http.RoundTripper),
		adminSocketPathFunc: principal.AdminSocketPath,
		logger:              logger,
	}

	// Start transport listener and relay if configured.
	if transportListen != "" {
		if err := daemon.startTransport(ctx, transportListen, relaySocket); err != nil {
			return fmt.Errorf("starting transport: %w", err)
		}
		defer daemon.stopTransport()
	}

	// Initial reconciliation.
	if err := daemon.reconcile(ctx); err != nil {
		logger.Error("initial reconciliation failed", "error", err)
		// Continue running — the next poll will retry.
	}

	// Sync peer transport addresses before the first service directory
	// sync so we know how to reach remote machines.
	if daemon.transportListener != nil {
		if err := daemon.syncPeerAddresses(ctx); err != nil {
			logger.Error("initial peer address sync failed", "error", err)
		}
	}

	// Initial service directory sync.
	if added, removed, updated, err := daemon.syncServiceDirectory(ctx); err != nil {
		logger.Error("initial service directory sync failed", "error", err)
	} else {
		logger.Info("service directory synced",
			"services", len(daemon.services),
			"local", len(daemon.localServices()),
			"remote", len(daemon.remoteServices()),
		)
		daemon.reconcileServices(ctx, added, removed, updated)
	}

	// Start the polling and status loops.
	go daemon.pollLoop(ctx)
	go daemon.statusLoop(ctx)

	// Wait for shutdown.
	<-ctx.Done()
	logger.Info("shutting down")
	return nil
}

// Daemon is the core daemon state.
type Daemon struct {
	session        *messaging.Session
	machineName    string
	machineUserID  string
	serverName     string
	configRoomID   string
	machinesRoomID string
	servicesRoomID string
	launcherSocket string
	pollInterval   time.Duration
	statusInterval time.Duration

	// running tracks which principals we've asked the launcher to create.
	// Keys are principal localparts.
	running map[string]bool

	// services is the cached service directory, built from m.bureau.service
	// state events in #bureau/services. Keyed by service localpart (the
	// state_key of the Matrix event, e.g., "service/stt/whisper").
	services map[string]*schema.Service

	// proxyRoutes tracks services currently registered on consumer proxies
	// via the admin API. Keyed by proxy service name (flat, e.g.,
	// "service-stt-whisper"), value is the upstream Unix socket path used
	// in the registration. For local services, this is the provider's
	// proxy socket. For remote services routed via transport, this is
	// the relay socket. The value is used for logging; removal uses only
	// the key (DELETE /v1/admin/services/{name}).
	proxyRoutes map[string]string

	// adminSocketPathFunc returns the admin socket path for a consumer
	// principal's proxy. Defaults to principal.AdminSocketPath. Tests
	// override this to use temp directories.
	adminSocketPathFunc func(localpart string) string

	// Transport: daemon-to-daemon communication for cross-machine routing.
	// These fields are nil/empty when --transport-listen is not set
	// (local-only mode).

	// transportListener accepts inbound connections from peer daemons.
	// Runs on the TCP address specified by --transport-listen.
	transportListener transport.Listener

	// transportDialer opens connections to peer daemons for outbound
	// request forwarding via the relay.
	transportDialer transport.Dialer

	// relaySocketPath is the Unix socket where the relay handler listens.
	// Consumer proxies register remote services with this socket as
	// their upstream. The relay handler receives requests and forwards
	// them to the correct peer daemon via the transport.
	relaySocketPath string

	// relayListener is the net.Listener for the relay Unix socket.
	relayListener net.Listener

	// relayServer serves HTTP on the relay socket.
	relayServer *http.Server

	// peerAddresses maps machine user IDs to their transport addresses,
	// populated from MachineStatus state events in #bureau/machines.
	// Used by the relay handler to find where to forward requests.
	peerAddresses map[string]string

	// peerTransports caches http.RoundTripper instances per transport
	// address. Each transport pools TCP connections to a specific peer.
	peerTransports   map[string]http.RoundTripper
	peerTransportsMu sync.RWMutex

	// lastActivityAt is the timestamp of the last meaningful daemon action
	// (sandbox creation, destruction, config reconciliation with changes).
	// Published in MachineStatus so consumers can determine idle duration.
	lastActivityAt time.Time

	logger *slog.Logger
}

// reconcile reads the current MachineConfig from Matrix and ensures the
// running sandboxes match the desired state.
func (d *Daemon) reconcile(ctx context.Context) error {
	config, err := d.readMachineConfig(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			// No config yet — nothing to do.
			d.logger.Info("no machine config found, waiting for assignment")
			return nil
		}
		return fmt.Errorf("reading machine config: %w", err)
	}

	// Determine the desired set of principals.
	desired := make(map[string]bool, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.AutoStart {
			desired[assignment.Localpart] = true
		}
	}

	// Create sandboxes for principals that should be running but aren't.
	for localpart := range desired {
		if d.running[localpart] {
			continue
		}

		d.logger.Info("starting principal", "principal", localpart)

		// Read the credentials for this principal.
		credentials, err := d.readCredentials(ctx, localpart)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Warn("no credentials found for principal, skipping", "principal", localpart)
				continue
			}
			d.logger.Error("reading credentials", "principal", localpart, "error", err)
			continue
		}

		// Send create-sandbox to the launcher.
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:               "create-sandbox",
			Principal:            localpart,
			EncryptedCredentials: credentials.Ciphertext,
		})
		if err != nil {
			d.logger.Error("create-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("create-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		d.running[localpart] = true
		d.lastActivityAt = time.Now()
		d.logger.Info("principal started", "principal", localpart)

		// Register all known local service routes on the new consumer's
		// proxy so it can reach services that were discovered before it
		// started. The proxy socket is created synchronously by Start(),
		// so it should be accepting connections by the time the launcher
		// responds to create-sandbox.
		d.configureConsumerProxy(ctx, localpart)
	}

	// Destroy sandboxes for principals that should not be running.
	for localpart := range d.running {
		if desired[localpart] {
			continue
		}

		d.logger.Info("stopping principal", "principal", localpart)

		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "destroy-sandbox",
			Principal: localpart,
		})
		if err != nil {
			d.logger.Error("destroy-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("destroy-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		delete(d.running, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("principal stopped", "principal", localpart)
	}

	return nil
}

// readMachineConfig reads the MachineConfig state event from the config room.
func (d *Daemon) readMachineConfig(ctx context.Context) (*schema.MachineConfig, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeMachineConfig, d.machineName)
	if err != nil {
		return nil, err
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("parsing machine config: %w", err)
	}
	return &config, nil
}

// readCredentials reads the Credentials state event for a specific principal.
func (d *Daemon) readCredentials(ctx context.Context, principalLocalpart string) (*schema.Credentials, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeCredentials, principalLocalpart)
	if err != nil {
		return nil, err
	}

	var credentials schema.Credentials
	if err := json.Unmarshal(content, &credentials); err != nil {
		return nil, fmt.Errorf("parsing credentials for %q: %w", principalLocalpart, err)
	}
	return &credentials, nil
}

// syncServiceDirectory fetches all m.bureau.service state events from the
// services room and updates the local cache. Returns the set of service
// localparts that were added, removed, or changed since the last sync.
//
// This is the foundation for service routing: the daemon needs to know what
// services exist and where they run before it can configure proxies to route
// to them.
func (d *Daemon) syncServiceDirectory(ctx context.Context) (added, removed, updated []string, err error) {
	events, err := d.session.GetRoomState(ctx, d.servicesRoomID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetching services room state: %w", err)
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
// For remote services (provider on another machine): requires inter-daemon
// transport which is not yet built. Logged as a warning so the operator knows
// the service exists but isn't routable.
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
			providerSocket := principal.SocketPath(providerLocalpart)

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
			providerSocket := principal.SocketPath(providerLocalpart)

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

// startTransport initializes and starts the transport listener (TCP inbound)
// and relay socket (Unix socket for consumer proxy outbound routing). Both
// run HTTP servers: the transport listener accepts requests from peer daemons
// and routes them to local provider proxies; the relay accepts requests from
// consumer proxies and forwards them to peer daemons.
func (d *Daemon) startTransport(ctx context.Context, listenAddress, relaySocketPath string) error {
	// Create the TCP transport listener.
	tcpListener, err := transport.NewTCPListener(listenAddress)
	if err != nil {
		return fmt.Errorf("creating transport listener on %s: %w", listenAddress, err)
	}
	d.transportListener = tcpListener
	d.transportDialer = &transport.TCPDialer{Timeout: 10 * time.Second}
	d.relaySocketPath = relaySocketPath

	d.logger.Info("transport listener started",
		"listen_address", tcpListener.Address(),
		"relay_socket", relaySocketPath,
	)

	// Start the inbound handler on the transport listener. This serves
	// requests from peer daemons and routes them to local provider proxies.
	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", d.handleTransportInbound)

	go func() {
		if err := tcpListener.Serve(ctx, inboundMux); err != nil {
			d.logger.Error("transport listener error", "error", err)
		}
	}()

	// Create the relay Unix socket. Consumer proxies register remote
	// services pointing here; the relay handler forwards requests to
	// the correct peer daemon via the transport.
	if err := os.MkdirAll(filepath.Dir(relaySocketPath), 0755); err != nil {
		tcpListener.Close()
		return fmt.Errorf("creating relay socket directory: %w", err)
	}
	if err := os.Remove(relaySocketPath); err != nil && !os.IsNotExist(err) {
		tcpListener.Close()
		return fmt.Errorf("removing existing relay socket: %w", err)
	}

	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		tcpListener.Close()
		return fmt.Errorf("creating relay socket at %s: %w", relaySocketPath, err)
	}
	d.relayListener = relayListener

	if err := os.Chmod(relaySocketPath, 0660); err != nil {
		relayListener.Close()
		tcpListener.Close()
		return fmt.Errorf("setting relay socket permissions: %w", err)
	}

	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/http/", d.handleRelay)
	d.relayServer = &http.Server{
		Handler:      relayMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming.
	}

	go func() {
		if err := d.relayServer.Serve(relayListener); err != nil && err != http.ErrServerClosed {
			d.logger.Error("relay server error", "error", err)
		}
	}()

	d.logger.Info("relay socket started", "socket", relaySocketPath)
	return nil
}

// stopTransport shuts down the transport listener and relay socket.
func (d *Daemon) stopTransport() {
	if d.transportListener != nil {
		d.transportListener.Close()
	}
	if d.relayServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.relayServer.Shutdown(ctx)
	}
	if d.relayListener != nil {
		os.Remove(d.relaySocketPath)
	}
}

// handleRelay is the HTTP handler for the relay Unix socket. Consumer
// proxies send requests here for remote services. The handler parses the
// service name from the path, looks up which peer machine hosts it, and
// reverse-proxies the request to that peer's transport listener.
//
// Request path format: /http/<service-name>/...
// The entire path is forwarded unchanged to the peer daemon's inbound handler.
func (d *Daemon) handleRelay(w http.ResponseWriter, r *http.Request) {
	serviceName, err := parseServiceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Find which machine provides this service.
	_, service, ok := d.serviceByProxyName(serviceName)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown service: %s", serviceName), http.StatusNotFound)
		return
	}

	// Look up the machine's transport address.
	peerAddress, ok := d.peerAddresses[service.Machine]
	if !ok || peerAddress == "" {
		d.logger.Warn("no transport address for remote service's machine",
			"service", serviceName,
			"machine", service.Machine,
		)
		http.Error(w, fmt.Sprintf("no transport address for machine %s", service.Machine), http.StatusBadGateway)
		return
	}

	// Get or create a cached HTTP transport for this peer.
	roundTripper := d.peerHTTPTransport(peerAddress)

	// Reverse proxy the request to the peer daemon. The path stays the
	// same: /http/<service>/... → peer daemon's inbound handler picks
	// it up and routes to the local provider proxy.
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = peerAddress
			// Path, query, and headers are preserved from the original request.
		},
		Transport: roundTripper,
	}

	d.logger.Info("relay forwarding to peer",
		"service", serviceName,
		"peer_address", peerAddress,
		"method", r.Method,
		"path", r.URL.Path,
	)

	proxy.ServeHTTP(w, r)
}

// handleTransportInbound is the HTTP handler for the transport listener.
// Peer daemons send requests here for services running on this machine.
// The handler parses the service name from the path, finds the local
// provider proxy, and reverse-proxies the request to it.
//
// Request path format: /http/<service-name>/...
// The entire path is forwarded unchanged to the provider proxy.
func (d *Daemon) handleTransportInbound(w http.ResponseWriter, r *http.Request) {
	serviceName, err := parseServiceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	providerSocket, ok := d.localProviderSocket(serviceName)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown local service: %s", serviceName), http.StatusNotFound)
		return
	}

	// Reverse proxy to the provider proxy via Unix socket. The path
	// is forwarded unchanged — the provider proxy's HandleHTTPProxy
	// will strip /http/<service>/ and route to the backend.
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = "localhost"
			// Path, query, and headers are preserved from the original request.
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", providerSocket)
			},
		},
	}

	d.logger.Info("inbound routing to local provider",
		"service", serviceName,
		"provider_socket", providerSocket,
		"method", r.Method,
		"path", r.URL.Path,
	)

	proxy.ServeHTTP(w, r)
}

// syncPeerAddresses reads MachineStatus state events from the machines room
// to discover transport addresses of peer daemons. Called on each poll cycle
// so the relay handler has up-to-date routing information.
func (d *Daemon) syncPeerAddresses(ctx context.Context) error {
	events, err := d.session.GetRoomState(ctx, d.machinesRoomID)
	if err != nil {
		return fmt.Errorf("fetching machines room state: %w", err)
	}

	updated := 0
	for _, event := range events {
		if event.Type != schema.EventTypeMachineStatus {
			continue
		}

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}

		var status schema.MachineStatus
		if err := json.Unmarshal(contentJSON, &status); err != nil {
			continue
		}

		// Skip our own machine and entries without transport addresses.
		if status.Principal == d.machineUserID || status.TransportAddress == "" {
			continue
		}

		if d.peerAddresses[status.Principal] != status.TransportAddress {
			d.logger.Info("peer transport address discovered",
				"machine", status.Principal,
				"address", status.TransportAddress,
			)
			d.peerAddresses[status.Principal] = status.TransportAddress
			updated++
		}
	}

	if updated > 0 {
		d.logger.Info("peer addresses synced", "peers", len(d.peerAddresses), "updated", updated)
	}
	return nil
}

// serviceByProxyName looks up a service entry by its flat proxy name
// (e.g., "service-stt-whisper"). Returns the service localpart, the
// service entry, and whether it was found.
func (d *Daemon) serviceByProxyName(proxyName string) (string, *schema.Service, bool) {
	for localpart, service := range d.services {
		if principal.ProxyServiceName(localpart) == proxyName {
			return localpart, service, true
		}
	}
	return "", nil, false
}

// localProviderSocket returns the proxy Unix socket path for a local
// service identified by its flat proxy name (e.g., "service-stt-whisper").
// Returns the socket path and whether the service was found as a local
// provider on this machine.
func (d *Daemon) localProviderSocket(proxyName string) (string, bool) {
	for localpart, service := range d.services {
		if principal.ProxyServiceName(localpart) != proxyName {
			continue
		}
		if service.Machine != d.machineUserID {
			continue
		}
		providerLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
		if err != nil {
			d.logger.Error("cannot derive socket from service principal",
				"service", localpart,
				"principal", service.Principal,
				"error", err,
			)
			return "", false
		}
		return principal.SocketPath(providerLocalpart), true
	}
	return "", false
}

// peerHTTPTransport returns a cached http.RoundTripper for the given peer
// transport address. Each peer gets its own transport with connection
// pooling. Thread-safe.
func (d *Daemon) peerHTTPTransport(address string) http.RoundTripper {
	d.peerTransportsMu.RLock()
	roundTripper, ok := d.peerTransports[address]
	d.peerTransportsMu.RUnlock()
	if ok {
		return roundTripper
	}

	d.peerTransportsMu.Lock()
	defer d.peerTransportsMu.Unlock()

	// Double-check after acquiring write lock.
	if roundTripper, ok := d.peerTransports[address]; ok {
		return roundTripper
	}

	roundTripper = transport.HTTPTransport(d.transportDialer, address)
	d.peerTransports[address] = roundTripper
	return roundTripper
}

// parseServiceFromPath extracts the flat service name from an HTTP path
// like /http/<service-name>/... Used by both the relay and inbound handlers.
func parseServiceFromPath(path string) (string, error) {
	if !strings.HasPrefix(path, "/http/") {
		return "", fmt.Errorf("path must start with /http/")
	}
	remaining := strings.TrimPrefix(path, "/http/")
	parts := strings.SplitN(remaining, "/", 2)
	if parts[0] == "" {
		return "", fmt.Errorf("empty service name in path")
	}
	return parts[0], nil
}

// pollLoop periodically polls for config changes and syncs the service directory.
func (d *Daemon) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.reconcile(ctx); err != nil {
				d.logger.Error("reconciliation failed", "error", err)
			}

			// Sync peer transport addresses before the service
			// directory so we have up-to-date routing information.
			if d.transportListener != nil {
				if err := d.syncPeerAddresses(ctx); err != nil {
					d.logger.Error("peer address sync failed", "error", err)
				}
			}

			added, removed, updated, err := d.syncServiceDirectory(ctx)
			if err != nil {
				d.logger.Error("service directory sync failed", "error", err)
			} else {
				d.reconcileServices(ctx, added, removed, updated)
			}
		}
	}
}

// statusLoop periodically publishes MachineStatus heartbeats.
func (d *Daemon) statusLoop(ctx context.Context) {
	// Publish initial status immediately.
	d.publishStatus(ctx)

	ticker := time.NewTicker(d.statusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.publishStatus(ctx)
		}
	}
}

// publishStatus sends a MachineStatus state event to the machines room.
func (d *Daemon) publishStatus(ctx context.Context) {
	runningCount := 0
	for range d.running {
		runningCount++
	}

	var lastActivity string
	if !d.lastActivityAt.IsZero() {
		lastActivity = d.lastActivityAt.UTC().Format(time.RFC3339)
	}

	var transportAddress string
	if d.transportListener != nil {
		transportAddress = d.transportListener.Address()
	}

	status := schema.MachineStatus{
		Principal: d.machineUserID,
		Sandboxes: schema.SandboxCounts{
			Running: runningCount,
		},
		UptimeSeconds:    uptimeSeconds(),
		LastActivityAt:   lastActivity,
		TransportAddress: transportAddress,
	}

	_, err := d.session.SendStateEvent(ctx, d.machinesRoomID, schema.EventTypeMachineStatus, d.machineName, status)
	if err != nil {
		d.logger.Error("publishing machine status", "error", err)
		return
	}
	d.logger.Debug("published machine status", "running_sandboxes", runningCount)
}

// launcherIPCRequest mirrors the launcher's IPCRequest type. Defined here to
// avoid importing cmd/bureau-launcher (which is a main package and cannot be
// imported). The JSON wire format is the contract between daemon and launcher.
type launcherIPCRequest struct {
	Action               string `json:"action"`
	Principal            string `json:"principal,omitempty"`
	EncryptedCredentials string `json:"encrypted_credentials,omitempty"`
}

// launcherIPCResponse mirrors the launcher's IPCResponse type.
type launcherIPCResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	ProxyPID int    `json:"proxy_pid,omitempty"`
}

// launcherRequest sends a request to the launcher and reads the response.
func (d *Daemon) launcherRequest(ctx context.Context, request launcherIPCRequest) (*launcherIPCResponse, error) {
	// Connect to the launcher's unix socket.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return nil, fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
	}
	defer conn.Close()

	// Use the context's deadline if set, otherwise fall back to 30 seconds
	// (matching the launcher's handler timeout).
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	conn.SetDeadline(deadline)

	// Send the request.
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("sending request to launcher: %w", err)
	}

	// Read the response.
	var response launcherIPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("reading response from launcher: %w", err)
	}

	return &response, nil
}

// sessionData is the JSON structure stored by the launcher for the Matrix session.
type sessionData struct {
	HomeserverURL string `json:"homeserver_url"`
	UserID        string `json:"user_id"`
	AccessToken   string `json:"access_token"`
}

// loadSession reads the Matrix session from the state directory.
// This is the same session.json written by bureau-launcher during first-boot.
func loadSession(stateDir string, homeserverURL string, logger *slog.Logger) (*messaging.Session, error) {
	sessionPath := filepath.Join(stateDir, "session.json")

	jsonData, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("reading session from %s: %w", sessionPath, err)
	}

	var data sessionData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return nil, fmt.Errorf("parsing session from %s: %w", sessionPath, err)
	}

	if data.AccessToken == "" {
		return nil, fmt.Errorf("session file %s has empty access token", sessionPath)
	}

	// Use the homeserver URL from the flag if provided, falling back to
	// the saved URL.
	serverURL := homeserverURL
	if serverURL == "" {
		serverURL = data.HomeserverURL
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: serverURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	return client.SessionFromToken(data.UserID, data.AccessToken), nil
}

// ensureConfigRoom ensures the per-machine config room exists. If it doesn't,
// creates it with the admin user invited.
func ensureConfigRoom(ctx context.Context, session *messaging.Session, alias, machineName, serverName, adminUser string, logger *slog.Logger) (string, error) {
	// Try to resolve the alias first.
	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		// Room exists — join it (idempotent).
		if _, err := session.JoinRoom(ctx, roomID); err != nil {
			logger.Warn("join config room returned error (may already be joined)", "error", err)
		}
		return roomID, nil
	}

	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("resolving config room alias %q: %w", alias, err)
	}

	// Room doesn't exist — create it.
	logger.Info("creating per-machine config room", "alias", alias)

	// The config room alias is e.g., "#bureau/config/machine/workstation:bureau.local".
	// The room_alias_name is the localpart without # or :server.
	aliasLocalpart := principal.RoomAliasLocalpart(alias)

	adminUserID := principal.MatrixUserID(adminUser, serverName)

	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Config: " + machineName,
		Topic:      "Machine configuration and credentials for " + machineName,
		Alias:      aliasLocalpart,
		Preset:     "private_chat",
		Invite:     []string{adminUserID},
		Visibility: "private",
		PowerLevelContentOverride: schema.ConfigRoomPowerLevels(adminUserID),
	})
	if err != nil {
		// If the room was created between our alias check and now, try to
		// resolve again.
		if messaging.IsMatrixError(err, messaging.ErrCodeRoomInUse) {
			roomID, err = session.ResolveAlias(ctx, alias)
			if err != nil {
				return "", fmt.Errorf("room exists but cannot resolve alias %q: %w", alias, err)
			}
			if _, err := session.JoinRoom(ctx, roomID); err != nil {
				logger.Warn("join config room returned error (may already be joined)", "error", err)
			}
			return roomID, nil
		}
		return "", fmt.Errorf("creating config room: %w", err)
	}

	logger.Info("created config room",
		"room_id", response.RoomID,
		"alias", alias,
		"admin", adminUserID,
	)
	return response.RoomID, nil
}

// uptimeSeconds returns the system uptime in seconds, or 0 if unavailable.
func uptimeSeconds() int64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	return info.Uptime
}
