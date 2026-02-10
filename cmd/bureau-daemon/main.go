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
	"bufio"
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
	"os/exec"
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
	"github.com/bureau-foundation/bureau/observe"
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
		homeserverURL      string
		machineName        string
		serverName         string
		stateDir           string
		launcherSocket     string
		adminUser          string
		relaySocket        string
		observeSocket      string
		tmuxSocket         string
		observeRelayBinary string
		pollInterval       time.Duration
		statusInterval     time.Duration
		showVersion        bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/bureau", "directory containing session.json from the launcher")
	flag.StringVar(&launcherSocket, "launcher-socket", "/run/bureau/launcher.sock", "path to the launcher IPC socket")
	flag.StringVar(&adminUser, "admin-user", "bureau-admin", "admin account username (for config room invites)")
	flag.StringVar(&relaySocket, "relay-socket", "/run/bureau/relay.sock", "Unix socket path for the transport relay (consumer proxies connect here for remote services)")
	flag.StringVar(&observeSocket, "observe-socket", "/run/bureau/observe.sock", "Unix socket for observation requests from clients")
	flag.StringVar(&tmuxSocket, "tmux-socket", "/run/bureau/tmux.sock", "tmux server socket for Bureau-managed sessions")
	flag.StringVar(&observeRelayBinary, "observe-relay-binary", "bureau-observe-relay", "path to the observation relay binary")
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
		observeSocketPath:   observeSocket,
		tmuxServerSocket:    tmuxSocket,
		observeRelayBinary:  observeRelayBinary,
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              logger,
	}

	// Start WebRTC transport and relay socket.
	if err := daemon.startTransport(ctx, relaySocket); err != nil {
		return fmt.Errorf("starting transport: %w", err)
	}
	defer daemon.stopTransport()

	// Start observation socket listener.
	if daemon.observeSocketPath != "" {
		if err := daemon.startObserveListener(ctx); err != nil {
			return fmt.Errorf("starting observe listener: %w", err)
		}
		defer daemon.stopObserveListener()
	}

	// Layout watchers are started during reconciliation for each running
	// principal. Ensure they all stop on shutdown.
	defer daemon.stopAllLayoutWatchers()

	// Initial reconciliation.
	if err := daemon.reconcile(ctx); err != nil {
		logger.Error("initial reconciliation failed", "error", err)
		// Continue running — the next poll will retry.
	}

	// Sync peer transport addresses before the first service directory
	// sync so we know how to reach remote machines.
	if err := daemon.syncPeerAddresses(ctx); err != nil {
		logger.Error("initial peer address sync failed", "error", err)
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

	// Observation: the daemon listens for observation requests from clients
	// and routes them to local relays or remote daemons via the transport.

	// observeSocketPath is the Unix socket where the daemon accepts
	// observation requests from clients (bureau observe CLI). Defaults to
	// /run/bureau/observe.sock, separate from the launcher IPC socket to
	// keep observation traffic off the privileged IPC channel.
	observeSocketPath string

	// observeListener is the net.Listener for the observation socket.
	observeListener net.Listener

	// tmuxServerSocket is the path to Bureau's dedicated tmux server socket.
	// Passed to relay processes as BUREAU_TMUX_SOCKET so they attach to the
	// correct tmux server. Defaults to /run/bureau/tmux.sock.
	tmuxServerSocket string

	// observeRelayBinary is the path to the bureau-observe-relay binary.
	// The daemon forks this for each observation session. Defaults to
	// "bureau-observe-relay" (found via PATH).
	observeRelayBinary string

	// Layout sync: each running principal has a layout watcher goroutine
	// that monitors its tmux session for changes and publishes them to
	// Matrix. On sandbox creation, the watcher also restores a previously
	// published layout (if one exists in Matrix).

	// layoutWatchers tracks running layout sync goroutines, keyed by
	// principal localpart. Protected by layoutWatchersMu.
	layoutWatchers   map[string]*layoutWatcher
	layoutWatchersMu sync.Mutex

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
	desired := make(map[string]schema.PrincipalAssignment, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.AutoStart {
			desired[assignment.Localpart] = assignment
		}
	}

	// Create sandboxes for principals that should be running but aren't.
	for localpart, assignment := range desired {
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
			MatrixPolicy:         assignment.MatrixPolicy,
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

		// Start watching the tmux session for layout changes. This also
		// restores any previously saved layout from Matrix.
		d.startLayoutWatcher(ctx, localpart)

		// Register all known local service routes on the new consumer's
		// proxy so it can reach services that were discovered before it
		// started. The proxy socket is created synchronously by Start(),
		// so it should be accepting connections by the time the launcher
		// responds to create-sandbox.
		d.configureConsumerProxy(ctx, localpart)
	}

	// Destroy sandboxes for principals that should not be running.
	for localpart := range d.running {
		if _, shouldRun := desired[localpart]; shouldRun {
			continue
		}

		d.logger.Info("stopping principal", "principal", localpart)

		// Stop the layout watcher before destroying the sandbox. This
		// ensures a clean shutdown rather than having the watcher see
		// the tmux session disappear underneath it.
		d.stopLayoutWatcher(localpart)

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

// startTransport initializes the WebRTC transport (for peer daemon
// communication) and the relay Unix socket (for consumer proxy outbound
// routing). The WebRTC transport handles both inbound and outbound
// connections via the same pool of PeerConnections; signaling is done
// through Matrix state events in the machines room.
func (d *Daemon) startTransport(ctx context.Context, relaySocketPath string) error {
	// Fetch initial TURN credentials from the homeserver.
	turn, err := d.session.TURNCredentials(ctx)
	if err != nil {
		d.logger.Warn("failed to fetch TURN credentials, using host candidates only", "error", err)
		// Proceed without TURN — host/srflx candidates may suffice on LAN.
	}
	iceConfig := transport.ICEConfigFromTURN(turn)

	// Create the Matrix signaler for SDP exchange.
	signaler := transport.NewMatrixSignaler(d.session, d.machinesRoomID, d.logger)

	// Create the WebRTC transport (implements both Listener and Dialer).
	webrtcTransport := transport.NewWebRTCTransport(signaler, d.machineName, iceConfig, d.logger)
	d.transportListener = webrtcTransport
	d.transportDialer = webrtcTransport
	d.relaySocketPath = relaySocketPath

	d.logger.Info("WebRTC transport started",
		"machine", d.machineName,
		"relay_socket", relaySocketPath,
		"turn_configured", turn != nil,
	)

	// Start the inbound handler on the transport listener. This serves
	// requests from peer daemons and routes them to local provider proxies.
	// Also handles observation requests from peer daemons.
	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", d.handleTransportInbound)
	inboundMux.HandleFunc("/observe/", d.handleTransportObserve)

	go func() {
		if err := webrtcTransport.Serve(ctx, inboundMux); err != nil {
			d.logger.Error("transport serve error", "error", err)
		}
	}()

	// Start TURN credential refresh goroutine.
	if turn != nil {
		go d.refreshTURNCredentials(ctx, webrtcTransport, turn.TTL)
	}

	// Create the relay Unix socket. Consumer proxies register remote
	// services pointing here; the relay handler forwards requests to
	// the correct peer daemon via the transport.
	if err := os.MkdirAll(filepath.Dir(relaySocketPath), 0755); err != nil {
		webrtcTransport.Close()
		return fmt.Errorf("creating relay socket directory: %w", err)
	}
	if err := os.Remove(relaySocketPath); err != nil && !os.IsNotExist(err) {
		webrtcTransport.Close()
		return fmt.Errorf("removing existing relay socket: %w", err)
	}

	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		webrtcTransport.Close()
		return fmt.Errorf("creating relay socket at %s: %w", relaySocketPath, err)
	}
	d.relayListener = relayListener

	if err := os.Chmod(relaySocketPath, 0660); err != nil {
		relayListener.Close()
		webrtcTransport.Close()
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

// refreshTURNCredentials periodically fetches fresh TURN credentials from
// the Matrix homeserver and updates the WebRTC transport's ICE configuration.
// TURN credentials are time-limited HMAC tokens; the TTL comes from the
// homeserver's /voip/turnServer response.
func (d *Daemon) refreshTURNCredentials(ctx context.Context, webrtcTransport *transport.WebRTCTransport, ttlSeconds int) {
	// Refresh at half the TTL to avoid using expired credentials, with a
	// floor of 5 minutes to prevent hammering the homeserver.
	interval := time.Duration(ttlSeconds/2) * time.Second
	if interval < 5*time.Minute {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			turn, err := d.session.TURNCredentials(ctx)
			if err != nil {
				d.logger.Warn("TURN credential refresh failed", "error", err)
				continue
			}
			webrtcTransport.UpdateICEConfig(transport.ICEConfigFromTURN(turn))
			d.logger.Debug("TURN credentials refreshed")
		}
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
			if err := d.syncPeerAddresses(ctx); err != nil {
				d.logger.Error("peer address sync failed", "error", err)
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

	transportAddress := d.transportListener.Address()

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
	Action               string              `json:"action"`
	Principal            string              `json:"principal,omitempty"`
	EncryptedCredentials string              `json:"encrypted_credentials,omitempty"`
	MatrixPolicy         *schema.MatrixPolicy `json:"matrix_policy,omitempty"`
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

// --- Observation routing ---
//
// The daemon routes observation requests from clients (bureau observe CLI) to
// relay processes that attach to tmux sessions. For local principals, the
// daemon forks a relay directly. For remote principals, it forwards the
// request through the transport to the remote daemon.
//
// The observation protocol has two phases:
//   - Handshake: client sends observeRequest JSON, daemon sends observeResponse JSON
//   - Streaming: after success, the socket carries the binary observation protocol
//     (framed messages: data, resize, history, metadata). The daemon bridges bytes
//     zero-copy between the client and the relay — no parsing of protocol messages.

// observeRequest mirrors observe.ObserveRequest. Defined locally to avoid
// importing the observe package (which has in-progress compilation issues from
// another agent) and to parallel the existing pattern with launcherIPCRequest.
// The JSON wire format is the contract between client and daemon.
type observeRequest struct {
	// Action selects the request type. Empty or "observe" starts a
	// streaming observation session. "query_layout" fetches and expands
	// a channel's layout (pure request/response, no streaming).
	Action string `json:"action,omitempty"`

	// Principal is the localpart of the target (e.g., "iree/amdgpu/pm").
	// Used when Action is "observe" (or empty).
	Principal string `json:"principal,omitempty"`

	// Mode is "readwrite" or "readonly".
	// Used when Action is "observe" (or empty).
	Mode string `json:"mode,omitempty"`

	// Channel is the Matrix room alias (e.g., "#iree/amdgpu/general:bureau.local").
	// Used when Action is "query_layout".
	Channel string `json:"channel,omitempty"`
}

// observeResponse mirrors observe.ObserveResponse. Defined locally for the
// same reasons as observeRequest.
type observeResponse struct {
	// OK is true if the observation session was established.
	OK bool `json:"ok"`

	// Session is the tmux session name (e.g., "bureau/iree/amdgpu/pm").
	Session string `json:"session,omitempty"`

	// Machine is the machine localpart hosting the principal.
	Machine string `json:"machine,omitempty"`

	// Error describes why the request failed.
	Error string `json:"error,omitempty"`
}

// startObserveListener creates the observation Unix socket and starts
// accepting client connections in a goroutine.
func (d *Daemon) startObserveListener(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.observeSocketPath), 0755); err != nil {
		return fmt.Errorf("creating observe socket directory: %w", err)
	}

	// Remove stale socket from a previous run.
	if err := os.Remove(d.observeSocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing observe socket: %w", err)
	}

	listener, err := net.Listen("unix", d.observeSocketPath)
	if err != nil {
		return fmt.Errorf("creating observe socket at %s: %w", d.observeSocketPath, err)
	}
	d.observeListener = listener

	if err := os.Chmod(d.observeSocketPath, 0660); err != nil {
		listener.Close()
		return fmt.Errorf("setting observe socket permissions: %w", err)
	}

	d.logger.Info("observe listener started", "socket", d.observeSocketPath)

	go d.acceptObserveConnections(ctx)
	return nil
}

// stopObserveListener closes the observation socket and removes the file.
func (d *Daemon) stopObserveListener() {
	if d.observeListener != nil {
		d.observeListener.Close()
		os.Remove(d.observeSocketPath)
	}
}

// acceptObserveConnections runs the accept loop for the observation socket.
// Each connection is handled in its own goroutine.
func (d *Daemon) acceptObserveConnections(ctx context.Context) {
	for {
		connection, err := d.observeListener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				if !strings.Contains(err.Error(), "use of closed network connection") {
					d.logger.Error("accept observe connection", "error", err)
				}
				return
			}
		}
		go d.handleObserveClient(connection)
	}
}

// handleObserveClient processes a single request on the observe socket.
// It reads the initial JSON line, dispatches on the action field, and
// either establishes a streaming observation session or handles a
// request/response query.
func (d *Daemon) handleObserveClient(clientConnection net.Conn) {
	defer clientConnection.Close()

	// Set a deadline for the JSON handshake. Cleared before entering
	// the streaming bridge (for observe) or left in place for queries.
	clientConnection.SetDeadline(time.Now().Add(10 * time.Second))

	var request observeRequest
	if err := json.NewDecoder(clientConnection).Decode(&request); err != nil {
		d.sendObserveError(clientConnection, fmt.Sprintf("invalid request: %v", err))
		return
	}

	switch request.Action {
	case "", "observe":
		d.handleObserveSession(clientConnection, request)
	case "query_layout":
		d.handleQueryLayout(clientConnection, request)
	default:
		d.sendObserveError(clientConnection,
			fmt.Sprintf("unknown action %q", request.Action))
	}
}

// handleObserveSession handles a streaming observation request. It validates
// the principal, determines whether it is local or remote, and either forks
// a relay or forwards through the transport.
func (d *Daemon) handleObserveSession(clientConnection net.Conn, request observeRequest) {
	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		d.sendObserveError(clientConnection, fmt.Sprintf("invalid principal: %v", err))
		return
	}

	if request.Mode != "readwrite" && request.Mode != "readonly" {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid mode %q: must be readwrite or readonly", request.Mode))
		return
	}

	d.logger.Info("observation requested",
		"principal", request.Principal,
		"mode", request.Mode,
	)

	// Check if the principal is running locally.
	if d.running[request.Principal] {
		d.handleLocalObserve(clientConnection, request)
		return
	}

	// Check if the principal is a known service on a remote machine
	// reachable via the transport.
	if peerAddress, ok := d.findPrincipalPeer(request.Principal); ok {
		d.handleRemoteObserve(clientConnection, request, peerAddress)
		return
	}

	d.sendObserveError(clientConnection,
		fmt.Sprintf("principal %q not found", request.Principal))
}

// handleQueryLayout handles a query_layout request: resolves a channel alias
// to a room ID, fetches the m.bureau.layout state event and room membership,
// expands ObserveMembers panes, and returns the concrete layout as JSON.
//
// This is pure request/response — the connection is closed after the response.
func (d *Daemon) handleQueryLayout(clientConnection net.Conn, request observeRequest) {
	if request.Channel == "" {
		d.sendObserveError(clientConnection, "channel is required for query_layout")
		return
	}

	// Use a generous timeout for the Matrix queries (alias resolution +
	// state event fetch + membership fetch).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d.logger.Info("query_layout requested", "channel", request.Channel)

	// Resolve the channel alias to a room ID.
	roomID, err := d.session.ResolveAlias(ctx, request.Channel)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("resolve channel %q: %v", request.Channel, err))
		return
	}

	// Fetch the m.bureau.layout state event from the channel room.
	// Channel-level layouts use an empty state key (as opposed to
	// per-principal layouts which use the principal's localpart).
	rawLayout, err := d.session.GetStateEvent(ctx, roomID, schema.EventTypeLayout, "")
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("fetch layout for %q: %v", request.Channel, err))
		return
	}

	var layoutContent schema.LayoutContent
	if err := json.Unmarshal(rawLayout, &layoutContent); err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("parse layout for %q: %v", request.Channel, err))
		return
	}

	layout := observe.SchemaToLayout(layoutContent)

	// Fetch room members for ObserveMembers pane expansion.
	matrixMembers, err := d.session.GetRoomMembers(ctx, roomID)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("fetch members for %q: %v", request.Channel, err))
		return
	}

	// Convert messaging.RoomMember to observe.RoomMember. Only include
	// joined members — invited, left, and banned members are excluded.
	var observeMembers []observe.RoomMember
	for _, member := range matrixMembers {
		if member.Membership != "join" {
			continue
		}
		localpart, localpartErr := principal.LocalpartFromMatrixID(member.UserID)
		if localpartErr != nil {
			// Skip members whose user IDs don't match the Bureau
			// @localpart:server convention (e.g., the admin account
			// might be @admin:bureau.local without a hierarchical
			// localpart).
			continue
		}
		observeMembers = append(observeMembers, observe.RoomMember{
			Localpart: localpart,
			// Role is not yet populated from Matrix — Bureau role
			// metadata will come from a principal identity state
			// event. For now, role-based ObserveMembers filtering
			// matches no roles, so all-members filters work but
			// role-specific filters return empty.
		})
	}

	expanded := observe.ExpandMembers(layout, observeMembers)

	d.logger.Info("query_layout completed",
		"channel", request.Channel,
		"room_id", roomID,
		"members", len(observeMembers),
		"windows", len(expanded.Windows),
	)

	response := observe.QueryLayoutResponse{
		OK:     true,
		Layout: expanded,
	}
	json.NewEncoder(clientConnection).Encode(response)
}

// handleLocalObserve handles observation of a principal running on this machine.
// It forks a relay process, sends the success response, and bridges bytes
// between the client and the relay.
func (d *Daemon) handleLocalObserve(clientConnection net.Conn, request observeRequest) {
	sessionName := "bureau/" + request.Principal
	readOnly := request.Mode == "readonly"

	relayConnection, relayCommand, err := d.forkObserveRelay(sessionName, readOnly)
	if err != nil {
		d.logger.Error("fork observe relay failed",
			"principal", request.Principal,
			"error", err,
		)
		d.sendObserveError(clientConnection,
			fmt.Sprintf("failed to start relay: %v", err))
		return
	}

	// Send success response and clear the handshake deadline before
	// entering the streaming bridge.
	response := observeResponse{
		OK:      true,
		Session: sessionName,
		Machine: d.machineName,
	}
	clientConnection.SetDeadline(time.Time{})
	if err := json.NewEncoder(clientConnection).Encode(response); err != nil {
		d.logger.Error("send observe response failed",
			"principal", request.Principal,
			"error", err,
		)
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		return
	}

	d.logger.Info("observation started",
		"principal", request.Principal,
		"session", sessionName,
	)

	// Bridge bytes zero-copy between client and relay. This blocks until
	// one side disconnects or errors.
	bridgeConnections(clientConnection, relayConnection)

	// Clean up the relay process. Send SIGTERM first for graceful shutdown,
	// escalate to SIGKILL after a timeout.
	cleanupRelayProcess(relayCommand)

	d.logger.Info("observation ended", "principal", request.Principal)
}

// handleRemoteObserve forwards an observation request to a remote daemon via
// the transport layer. It connects to the peer, performs an HTTP handshake
// with the remote daemon's /observe/ handler, then bridges the resulting
// raw connection to the client.
func (d *Daemon) handleRemoteObserve(clientConnection net.Conn, request observeRequest, peerAddress string) {
	d.logger.Info("forwarding observation to remote daemon",
		"principal", request.Principal,
		"peer", peerAddress,
	)

	// Dial the remote daemon via the transport layer (WebRTC data channel).
	dialContext, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()
	rawConnection, err := d.transportDialer.DialContext(dialContext, peerAddress)
	if err != nil {
		d.sendObserveError(clientConnection,
			fmt.Sprintf("cannot reach peer at %s: %v", peerAddress, err))
		return
	}

	// Build and send an HTTP POST to the remote daemon's observation handler.
	// The principal localpart contains '/' which is fine in the URL path —
	// the remote handler strips the /observe/ prefix and takes the rest.
	// The Host header uses "transport" as a placeholder — the transport
	// layer ignores it since the connection is already routed to the peer.
	requestBody, _ := json.Marshal(request)
	httpRequest, err := http.NewRequest("POST",
		"http://transport/observe/"+request.Principal,
		bytes.NewReader(requestBody))
	if err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection, fmt.Sprintf("build request: %v", err))
		return
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if err := httpRequest.Write(rawConnection); err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("send request to peer: %v", err))
		return
	}

	// Read the HTTP response. The bufio.Reader may read ahead into the
	// post-HTTP observation stream — we preserve those bytes via
	// bufferedConn for the bridge.
	bufferedReader := bufio.NewReader(rawConnection)
	httpResponse, err := http.ReadResponse(bufferedReader, httpRequest)
	if err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("read peer response: %v", err))
		return
	}

	responseBody, _ := io.ReadAll(httpResponse.Body)
	httpResponse.Body.Close()

	var peerResponse observeResponse
	if err := json.Unmarshal(responseBody, &peerResponse); err != nil {
		rawConnection.Close()
		d.sendObserveError(clientConnection,
			fmt.Sprintf("invalid peer response: %v", err))
		return
	}

	if !peerResponse.OK {
		rawConnection.Close()
		d.sendObserveError(clientConnection, peerResponse.Error)
		return
	}

	// Forward the success response to the client.
	clientConnection.SetDeadline(time.Time{})
	if err := json.NewEncoder(clientConnection).Encode(peerResponse); err != nil {
		rawConnection.Close()
		return
	}

	d.logger.Info("remote observation established",
		"principal", request.Principal,
		"session", peerResponse.Session,
		"peer", peerAddress,
	)

	// Bridge client ↔ remote daemon. Use bufferedConn to include any bytes
	// the bufio.Reader read ahead beyond the HTTP response.
	peerConn := &bufferedConn{reader: bufferedReader, Conn: rawConnection}
	bridgeConnections(clientConnection, peerConn)

	d.logger.Info("remote observation ended", "principal", request.Principal)
}

// handleTransportObserve handles observation requests arriving from peer
// daemons over the transport listener. The peer sends an HTTP POST with an
// observeRequest body. On success, the handler hijacks the HTTP connection,
// forks a relay, writes the observeResponse, and bridges bytes between the
// hijacked connection and the relay.
func (d *Daemon) handleTransportObserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract principal from path: /observe/<localpart>
	principalLocalpart := strings.TrimPrefix(r.URL.Path, "/observe/")
	if principalLocalpart == "" {
		http.Error(w, "empty principal in path", http.StatusBadRequest)
		return
	}

	var request observeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("invalid request: %v", err),
		})
		return
	}

	// The principal in the path and body must match.
	if request.Principal != principalLocalpart {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("principal mismatch: path=%q body=%q",
				principalLocalpart, request.Principal),
		})
		return
	}

	if !d.running[request.Principal] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("principal %q not running on this machine", request.Principal),
		})
		return
	}

	sessionName := "bureau/" + request.Principal
	readOnly := request.Mode == "readonly"

	relayConnection, relayCommand, err := d.forkObserveRelay(sessionName, readOnly)
	if err != nil {
		d.logger.Error("fork observe relay for transport failed",
			"principal", request.Principal,
			"error", err,
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(observeResponse{
			Error: fmt.Sprintf("failed to start relay: %v", err),
		})
		return
	}

	// Hijack the HTTP connection to switch to the binary observation protocol.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		d.logger.Error("response writer does not support hijacking")
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		http.Error(w, "server does not support connection hijacking", http.StatusInternalServerError)
		return
	}

	transportConnection, transportBuffer, err := hijacker.Hijack()
	if err != nil {
		d.logger.Error("hijack transport connection failed", "error", err)
		relayConnection.Close()
		relayCommand.Process.Kill()
		relayCommand.Wait()
		return
	}

	// Write the HTTP response manually on the hijacked connection.
	responseJSON, _ := json.Marshal(observeResponse{
		OK:      true,
		Session: sessionName,
		Machine: d.machineName,
	})
	fmt.Fprintf(transportBuffer, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(transportBuffer, "Content-Type: application/json\r\n")
	fmt.Fprintf(transportBuffer, "Content-Length: %d\r\n", len(responseJSON))
	fmt.Fprintf(transportBuffer, "\r\n")
	transportBuffer.Write(responseJSON)
	transportBuffer.Flush()

	d.logger.Info("transport observation started",
		"principal", request.Principal,
		"session", sessionName,
	)

	// Bridge the hijacked connection to the relay.
	bridgeConnections(transportConnection, relayConnection)

	cleanupRelayProcess(relayCommand)

	d.logger.Info("transport observation ended", "principal", request.Principal)
}

// forkObserveRelay creates a socketpair and forks the observation relay binary.
// Returns the daemon's end of the socketpair (as a net.Conn) and the relay's
// exec.Cmd. The relay receives the other end of the socketpair as fd 3.
//
// The relay binary is invoked as: bureau-observe-relay <session-name>
// Environment:
//   - BUREAU_TMUX_SOCKET: tmux server socket path
//   - BUREAU_OBSERVE_READONLY=1: if readOnly is true
func (d *Daemon) forkObserveRelay(sessionName string, readOnly bool) (net.Conn, *exec.Cmd, error) {
	// Create a socketpair. fds[0] goes to the relay as fd 3; fds[1] stays
	// with the daemon and is converted to a net.Conn.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("creating socketpair: %w", err)
	}

	relayFile := os.NewFile(uintptr(fds[0]), "relay-socket")
	daemonFile := os.NewFile(uintptr(fds[1]), "daemon-socket")

	// Convert the daemon's end to a net.Conn for proper socket semantics
	// (deadline support, etc.). FileConn dups the fd internally, so we
	// close the original.
	daemonConnection, err := net.FileConn(daemonFile)
	daemonFile.Close()
	if err != nil {
		relayFile.Close()
		return nil, nil, fmt.Errorf("converting daemon socket to net.Conn: %w", err)
	}

	// Build environment for the relay process.
	environment := os.Environ()
	if d.tmuxServerSocket != "" {
		environment = append(environment, "BUREAU_TMUX_SOCKET="+d.tmuxServerSocket)
	}
	if readOnly {
		environment = append(environment, "BUREAU_OBSERVE_READONLY=1")
	}

	command := exec.Command(d.observeRelayBinary, sessionName)
	command.Env = environment
	command.ExtraFiles = []*os.File{relayFile} // becomes fd 3 in child
	command.Stderr = os.Stderr

	if err := command.Start(); err != nil {
		daemonConnection.Close()
		relayFile.Close()
		return nil, nil, fmt.Errorf("starting observe relay %q: %w", d.observeRelayBinary, err)
	}

	// Close the relay's end in the parent — the child has its own copy.
	relayFile.Close()

	return daemonConnection, command, nil
}

// findPrincipalPeer looks up which remote machine hosts a principal by
// checking the service directory. Returns the peer's transport address if
// the principal is a known remote service with a reachable peer.
//
// This provides best-effort remote observation for service principals. For
// non-service principals (regular agents), a future principal directory
// would be needed.
func (d *Daemon) findPrincipalPeer(localpart string) (peerAddress string, ok bool) {
	if d.transportDialer == nil {
		return "", false
	}

	for _, service := range d.services {
		serviceLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
		if err != nil {
			continue
		}
		if serviceLocalpart != localpart {
			continue
		}
		if service.Machine == d.machineUserID {
			continue // Local, not remote.
		}
		address, exists := d.peerAddresses[service.Machine]
		if !exists || address == "" {
			continue
		}
		return address, true
	}
	return "", false
}

// bridgeConnections copies bytes bidirectionally between two connections.
// Returns when either direction finishes (EOF, error, or closed connection).
// Both connections are closed before returning.
func bridgeConnections(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(a, b)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(b, a)
		done <- struct{}{}
	}()

	// Wait for one direction to finish, then close both to unblock the other.
	<-done
	a.Close()
	b.Close()
	<-done
}

// cleanupRelayProcess sends SIGTERM and waits for the relay to exit.
// If the relay doesn't exit within 5 seconds, it is killed with SIGKILL.
func cleanupRelayProcess(command *exec.Cmd) {
	command.Process.Signal(syscall.SIGTERM)
	exitChannel := make(chan error, 1)
	go func() { exitChannel <- command.Wait() }()
	select {
	case <-exitChannel:
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		<-exitChannel
	}
}

// sendObserveError writes an error observeResponse to the connection.
func (d *Daemon) sendObserveError(connection net.Conn, message string) {
	d.logger.Warn("observe request failed", "error", message)
	json.NewEncoder(connection).Encode(observeResponse{Error: message})
}

// bufferedConn wraps a net.Conn with a buffered reader for reads while
// writing directly to the underlying connection. Used after HTTP response
// parsing where the bufio.Reader may have read ahead into the post-HTTP
// streaming data.
type bufferedConn struct {
	reader *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// uptimeSeconds returns the system uptime in seconds, or 0 if unavailable.
func uptimeSeconds() int64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	return info.Uptime
}

// --- Layout sync ---
//
// Each running principal has a layout watcher goroutine that bidirectionally
// syncs the tmux session layout with a Matrix state event:
//
//   - tmux → Matrix: the watcher monitors the tmux session via control mode
//     (tmux -C) for layout-relevant notifications (window add/close/rename,
//     pane split/resize). On change, it reads the full layout, diffs against
//     the last-published version, and publishes a state event update if
//     anything changed.
//
//   - Matrix → tmux: on sandbox creation, if a layout state event already
//     exists for the principal, the watcher applies it to the newly created
//     tmux session. This restores the agent's last-known workspace.
//
// The layout state event lives in the per-machine config room with the
// principal's localpart as the state key. The SourceMachine field stamps
// which machine published it, enabling loop prevention in multi-machine
// scenarios.

// layoutWatcher tracks a running layout sync goroutine for a single principal.
type layoutWatcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startLayoutWatcher begins monitoring the tmux session for a principal.
// Safe to call multiple times for the same principal (subsequent calls are
// no-ops). The watcher runs until stopped or the parent context is cancelled.
func (d *Daemon) startLayoutWatcher(ctx context.Context, localpart string) {
	d.layoutWatchersMu.Lock()
	defer d.layoutWatchersMu.Unlock()

	if _, exists := d.layoutWatchers[localpart]; exists {
		return
	}

	watchContext, cancel := context.WithCancel(ctx)
	watcher := &layoutWatcher{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	d.layoutWatchers[localpart] = watcher

	go d.runLayoutWatcher(watchContext, localpart, watcher.done)
}

// stopLayoutWatcher cancels the layout watcher for a principal and waits
// for the goroutine to exit. No-op if no watcher is running.
func (d *Daemon) stopLayoutWatcher(localpart string) {
	d.layoutWatchersMu.Lock()
	watcher, exists := d.layoutWatchers[localpart]
	if !exists {
		d.layoutWatchersMu.Unlock()
		return
	}
	delete(d.layoutWatchers, localpart)
	d.layoutWatchersMu.Unlock()

	watcher.cancel()
	<-watcher.done
}

// stopAllLayoutWatchers cancels all running layout watchers and waits for
// all goroutines to exit. Called during daemon shutdown.
func (d *Daemon) stopAllLayoutWatchers() {
	d.layoutWatchersMu.Lock()
	watchers := make(map[string]*layoutWatcher, len(d.layoutWatchers))
	for localpart, watcher := range d.layoutWatchers {
		watchers[localpart] = watcher
	}
	d.layoutWatchers = make(map[string]*layoutWatcher)
	d.layoutWatchersMu.Unlock()

	// Cancel all watchers first, then wait. This ensures parallel
	// shutdown rather than sequential.
	for _, watcher := range watchers {
		watcher.cancel()
	}
	for _, watcher := range watchers {
		<-watcher.done
	}
}

// runLayoutWatcher is the main goroutine for a single principal's layout
// sync. It restores the layout from Matrix (if available), then watches
// for changes and publishes updates.
func (d *Daemon) runLayoutWatcher(ctx context.Context, localpart string, done chan struct{}) {
	defer close(done)

	sessionName := "bureau/" + localpart

	// Start the control mode client to watch for layout changes. Must be
	// started before restoring from Matrix so we don't miss layout events
	// triggered by the restore itself (the debounce + LayoutEqual check
	// prevents spurious re-publishes).
	controlClient, err := observe.NewControlClient(ctx, d.tmuxServerSocket, sessionName)
	if err != nil {
		d.logger.Error("start layout control client failed",
			"principal", localpart,
			"error", err,
		)
		return
	}
	defer controlClient.Stop()

	// Try to restore the layout from Matrix. If a layout event exists for
	// this principal, apply it to the (newly created) tmux session and use
	// it as the baseline for change detection.
	var lastPublished *observe.Layout
	if stored, err := d.readLayoutEvent(ctx, localpart); err == nil {
		if err := observe.ApplyLayout(d.tmuxServerSocket, sessionName, stored); err != nil {
			d.logger.Error("restore layout from matrix failed",
				"principal", localpart,
				"error", err,
			)
		} else {
			lastPublished = stored
			d.logger.Info("layout restored from matrix",
				"principal", localpart,
				"windows", len(stored.Windows),
			)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		d.logger.Error("read layout event failed",
			"principal", localpart,
			"error", err,
		)
	}

	// Watch for layout changes and publish to Matrix.
	for range controlClient.Events() {
		current, err := observe.ReadTmuxLayout(d.tmuxServerSocket, sessionName)
		if err != nil {
			d.logger.Error("read tmux layout failed",
				"principal", localpart,
				"error", err,
			)
			continue
		}

		if observe.LayoutEqual(current, lastPublished) {
			continue
		}

		content := observe.LayoutToSchema(current)
		content.SourceMachine = d.machineUserID

		if _, err := d.session.SendStateEvent(ctx, d.configRoomID,
			schema.EventTypeLayout, localpart, content); err != nil {
			d.logger.Error("publish layout failed",
				"principal", localpart,
				"error", err,
			)
			continue
		}

		lastPublished = current
		d.logger.Info("layout published",
			"principal", localpart,
			"windows", len(current.Windows),
		)
	}
}

// readLayoutEvent reads the layout state event for a principal from the
// config room and returns it as a runtime Layout.
func (d *Daemon) readLayoutEvent(ctx context.Context, localpart string) (*observe.Layout, error) {
	raw, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeLayout, localpart)
	if err != nil {
		return nil, err
	}

	var content schema.LayoutContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("parsing layout event for %q: %w", localpart, err)
	}

	return observe.SchemaToLayout(content), nil
}
