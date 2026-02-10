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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

// uptimeSeconds returns the system uptime in seconds, or 0 if unavailable.
func uptimeSeconds() int64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	return info.Uptime
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
