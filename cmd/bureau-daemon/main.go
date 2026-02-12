// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/hwinfo"
	"github.com/bureau-foundation/bureau/lib/hwinfo/amdgpu"
	"github.com/bureau-foundation/bureau/lib/hwinfo/nvidia"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/tmux"
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
		homeserverURL          string
		machineName            string
		serverName             string
		runDir                 string
		stateDir               string
		adminUser              string
		observeRelayBinary     string
		workspaceRoot          string
		pipelineExecutorBinary string
		pipelineEnvironment    string
		statusInterval         time.Duration
		showVersion            bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets and ephemeral state (must match the launcher's --run-dir)")
	flag.StringVar(&stateDir, "state-dir", principal.DefaultStateDir, "directory containing session.json from the launcher")
	flag.StringVar(&adminUser, "admin-user", "bureau-admin", "admin account username (for config room invites)")
	flag.StringVar(&observeRelayBinary, "observe-relay-binary", "bureau-observe-relay", "path to the observation relay binary")
	flag.StringVar(&workspaceRoot, "workspace-root", principal.DefaultWorkspaceRoot, "root directory for project workspaces")
	flag.StringVar(&pipelineExecutorBinary, "pipeline-executor-binary", "", "path to bureau-pipeline-executor binary (enables pipeline.execute command)")
	flag.StringVar(&pipelineEnvironment, "pipeline-environment", "", "Nix store path providing pipeline executor's toolchain (e.g., /nix/store/...-runner-env)")
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

	if err := principal.ValidateRunDir(runDir); err != nil {
		return fmt.Errorf("run directory validation: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if maxLocalpart := principal.MaxLocalpartAvailable(runDir); maxLocalpart < principal.MaxLocalpartLength {
		logger.Warn("--run-dir limits maximum localpart length",
			"run_dir", runDir,
			"max_localpart", maxLocalpart,
			"default_max", principal.MaxLocalpartLength,
		)
	}

	// Compute the daemon's binary hash and resolve its filesystem path.
	// Done early since it's pure filesystem I/O with no dependencies.
	// Both values are stable for the lifetime of this process — the
	// binary doesn't change while it's running (exec() creates a new
	// process with its own hash and path).
	var daemonBinaryHash, daemonBinaryPath string
	if selfHash, selfPath, hashErr := computeSelfHash(); hashErr == nil {
		daemonBinaryHash = selfHash
		daemonBinaryPath = selfPath
		logger.Info("daemon binary hash computed", "hash", daemonBinaryHash, "path", daemonBinaryPath)
	} else {
		logger.Warn("failed to compute daemon binary hash", "error", hashErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load the Matrix session saved by the launcher. The client is also
	// needed for the token verifier (creates temporary sessions for whoami).
	client, session, err := loadSession(stateDir, homeserverURL, logger)
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

	// Resolve and join the global rooms. The rooms are invite-only (created
	// with the private_chat preset by bureau matrix setup), so the machine
	// account must have been invited by the admin before these joins can
	// succeed. The launcher joins machines on first boot; the daemon
	// defensively re-joins on every startup to handle membership recovery
	// (e.g., homeserver reset, account re-creation).

	machineAlias := principal.RoomAlias("bureau/machine", serverName)
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolving machine room alias %q: %w", machineAlias, err)
	}
	if _, err := session.JoinRoom(ctx, machineRoomID); err != nil {
		logger.Warn("failed to join machine room (requires admin invitation)",
			"room_id", machineRoomID, "alias", machineAlias, "error", err)
	}

	serviceAlias := principal.RoomAlias("bureau/service", serverName)
	serviceRoomID, err := session.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		return fmt.Errorf("resolving service room alias %q: %w", serviceAlias, err)
	}
	if _, err := session.JoinRoom(ctx, serviceRoomID); err != nil {
		logger.Warn("failed to join service room (requires admin invitation)",
			"room_id", serviceRoomID, "alias", serviceAlias, "error", err)
	}
	logger.Info("global rooms ready",
		"machine_room", machineRoomID,
		"service_room", serviceRoomID,
	)

	// Check the watchdog from a previous exec() attempt. This detects
	// whether a prior daemon self-update succeeded (we're the new binary)
	// or failed (we're the old binary restarted after a crash).
	var failedExecPath string
	if daemonBinaryPath != "" {
		failedExecPath = checkDaemonWatchdog(
			filepath.Join(stateDir, "daemon-watchdog.json"),
			daemonBinaryPath,
			session,
			configRoomID,
			logger,
		)
	}

	machineUserID := principal.MatrixUserID(machineName, serverName)

	daemon := &Daemon{
		session:           session,
		clock:             clock.Real(),
		client:            client,
		tokenVerifier:     newTokenVerifier(client, 5*time.Minute, logger),
		machineName:       machineName,
		machineUserID:     machineUserID,
		serverName:        serverName,
		configRoomID:      configRoomID,
		machineRoomID:     machineRoomID,
		serviceRoomID:     serviceRoomID,
		runDir:            runDir,
		launcherSocket:    principal.LauncherSocketPath(runDir),
		statusInterval:    statusInterval,
		daemonBinaryHash:  daemonBinaryHash,
		daemonBinaryPath:  daemonBinaryPath,
		stateDir:          stateDir,
		failedExecPaths:   make(map[string]bool),
		running:           make(map[string]bool),
		exitWatchers:      make(map[string]context.CancelFunc),
		proxyExitWatchers: make(map[string]context.CancelFunc),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastMatrixPolicy:  make(map[string]*schema.MatrixPolicy),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		lastSpecs:         make(map[string]*schema.SandboxSpec),
		previousSpecs:     make(map[string]*schema.SandboxSpec),
		lastTemplates:     make(map[string]*schema.TemplateContent),
		healthMonitors:    make(map[string]*healthMonitor),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		adminSocketPathFunc: func(localpart string) string {
			return principal.RunDirAdminSocketPath(runDir, localpart)
		},
		observeSocketPath:      principal.ObserveSocketPath(runDir),
		tmuxServer:             tmux.NewServer(principal.TmuxSocketPath(runDir), ""),
		observeRelayBinary:     observeRelayBinary,
		layoutWatchers:         make(map[string]*layoutWatcher),
		workspaceRoot:          workspaceRoot,
		pipelineExecutorBinary: pipelineExecutorBinary,
		pipelineEnvironment:    pipelineEnvironment,
		shutdownCtx:            ctx,
		logger:                 logger,
	}

	// Seed failed exec paths from watchdog check so the reconcile loop
	// doesn't immediately retry a binary that just crashed.
	if failedExecPath != "" {
		daemon.failedExecPaths[failedExecPath] = true
	}

	// Start WebRTC transport and relay socket.
	if err := daemon.startTransport(ctx, principal.RelaySocketPath(runDir)); err != nil {
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

	// Initialize GPU metric collectors. Each vendor collector opens
	// device nodes (render nodes for AMD, etc.) and holds them open
	// for the daemon's lifetime. Collectors that find no devices for
	// their vendor are no-ops.
	amdCollector := amdgpu.NewCollector(logger)
	nvidiaCollector := nvidia.NewCollector(logger)
	daemon.gpuCollectors = []hwinfo.GPUCollector{amdCollector, nvidiaCollector}
	defer daemon.closeGPUCollectors()

	// Publish static hardware inventory. This is idempotent — Matrix
	// deduplicates state events with identical content, so restarts
	// without hardware changes produce no new events.
	daemon.publishMachineInfo(ctx)

	// Layout watchers and health monitors are started during reconciliation
	// for each running principal. Ensure they all stop on shutdown.
	defer daemon.stopAllLayoutWatchers()
	defer daemon.stopAllHealthMonitors()

	// Query the launcher for sandboxes that survived a daemon restart.
	// Pre-populating d.running before reconcile prevents the daemon from
	// trying to create-sandbox for principals the launcher already has,
	// which would be rejected with "already has a running sandbox" and
	// leave the principal orphaned from daemon management.
	if err := daemon.adoptPreExistingSandboxes(ctx); err != nil {
		logger.Warn("failed to query launcher for pre-existing sandboxes", "error", err)
	}

	// Perform the initial Matrix /sync to establish a since token and
	// baseline state (reconcile, peer addresses, service directory).
	sinceToken, err := daemon.initialSync(ctx)
	if err != nil {
		logger.Error("initial sync failed", "error", err)
		// Continue — the sync loop will start from scratch with an empty
		// token, which triggers a fresh initial sync.
	}

	// Start the incremental sync loop and status heartbeat loop.
	go daemon.syncLoop(ctx, sinceToken)
	go daemon.statusLoop(ctx)

	// Wait for shutdown.
	<-ctx.Done()
	logger.Info("shutting down")
	return nil
}

// Daemon is the core daemon state.
type Daemon struct {
	session *messaging.Session

	// clock provides time operations (Now, After, NewTicker, AfterFunc,
	// Sleep). Production code uses clock.Real(); tests use clock.Fake()
	// for deterministic time control without real sleeps.
	clock clock.Clock

	// client is the unauthenticated Matrix client. Used by the token
	// verifier to create temporary sessions for whoami calls.
	client *messaging.Client

	// tokenVerifier validates Matrix access tokens from observation
	// clients (operators and peer daemons). Successful verifications
	// are cached to avoid per-request homeserver round-trips.
	tokenVerifier *tokenVerifier

	// lastConfig is the MachineConfig from the most recent successful
	// reconciliation. Used by observation authorization to look up
	// per-principal ObservePolicy and the machine-level default. Nil
	// before the first successful reconcile — observation requests are
	// rejected in that state.
	lastConfig *schema.MachineConfig

	machineName    string
	machineUserID  string
	serverName     string
	configRoomID   string
	machineRoomID  string
	serviceRoomID  string
	runDir         string // runtime directory for sockets (e.g., /run/bureau)
	launcherSocket string
	statusInterval time.Duration

	// daemonBinaryHash is the SHA256 hex digest of the currently running
	// daemon binary, computed once at startup via os.Executable(). Used
	// by BureauVersion comparison to determine whether a config update
	// requires a daemon restart (via exec()). Empty if the hash could
	// not be computed (treated as always-changed by CompareBureauVersion).
	daemonBinaryHash string

	// daemonBinaryPath is the absolute filesystem path of the running
	// daemon binary, resolved once at startup via os.Executable(). Used
	// by the exec watchdog to record which binary was running before
	// a transition. On Linux this is the resolved /proc/self/exe target.
	daemonBinaryPath string

	// stateDir is the directory for persistent daemon state (session.json,
	// watchdog files). The exec watchdog is written here so it survives
	// both exec() transitions and process restarts.
	stateDir string

	// failedExecPaths tracks store paths where exec() has already been
	// attempted and failed during this process lifetime. Prevents retry
	// loops where the daemon repeatedly tries to exec a broken binary on
	// every reconcile cycle. Keyed by desired daemon store path.
	failedExecPaths map[string]bool

	// execFunc replaces the current process with a new binary. Defaults
	// to syscall.Exec. Tests override this to capture the exec call
	// without replacing the test process.
	execFunc func(binary string, argv []string, env []string) error

	// reconcileMu serializes access to the running/lastSpecs/previousSpecs/
	// lastTemplates maps and launcher IPC calls that modify sandbox state.
	// The reconcile loop holds this during reconcile(); health monitor
	// goroutines acquire it before performing rollback. Use RLock for
	// read-only access (e.g., publishStatus counting running sandboxes).
	reconcileMu sync.RWMutex

	// running tracks which principals we've asked the launcher to create.
	// Keys are principal localparts.
	running map[string]bool

	// lastCredentials stores the ciphertext from the most recently
	// deployed m.bureau.credentials state event for each running
	// principal. On each reconcile cycle, the daemon compares the
	// current ciphertext against this stored value — a mismatch
	// triggers a destroy+create cycle to rotate the proxy's credentials.
	lastCredentials map[string]string

	// lastVisibility stores the ServiceVisibility patterns most
	// recently pushed to each running principal's proxy. On each
	// reconcile cycle, the daemon compares the current patterns from
	// the PrincipalAssignment against this stored value — a mismatch
	// triggers a PUT /v1/admin/visibility call to hot-reload the
	// proxy's service filtering without restarting the sandbox.
	lastVisibility map[string][]string

	// lastMatrixPolicy stores the per-principal MatrixPolicy from the
	// most recent reconcile cycle. Used purely for change detection:
	// when a principal's MatrixPolicy differs from the stored value,
	// the daemon pushes the new policy to the proxy via the admin API.
	lastMatrixPolicy map[string]*schema.MatrixPolicy

	// lastObservePolicy stores the per-principal ObservePolicy from
	// the most recent reconcile cycle. Used purely for change detection:
	// when a principal's ObservePolicy differs from the stored value,
	// the daemon re-evaluates active observation sessions against the
	// new policy and terminates any that no longer pass authorization.
	lastObservePolicy map[string]*schema.ObservePolicy

	// lastDefaultObservePolicy stores the machine-level DefaultObservePolicy
	// from the most recent reconcile cycle. A change to the default
	// triggers re-evaluation of all active observation sessions, since
	// principals without a per-principal override inherit the default.
	lastDefaultObservePolicy *schema.ObservePolicy

	// lastSpecs stores the SandboxSpec sent to the launcher for each
	// running principal. Used to detect payload-only changes that can
	// be hot-reloaded without restarting the sandbox. Nil entries mean
	// the principal was created without a SandboxSpec (no template).
	lastSpecs map[string]*schema.SandboxSpec

	// previousSpecs stores the last-known-good SandboxSpec before a
	// structural restart. On health check rollback, the daemon recreates
	// the sandbox with this spec. Cleared after rollback or when the
	// principal is removed from config. Keyed by principal localpart.
	previousSpecs map[string]*schema.SandboxSpec

	// lastTemplates stores the resolved TemplateContent for each running
	// principal. Health monitors read HealthCheck config from here.
	// Keyed by principal localpart.
	lastTemplates map[string]*schema.TemplateContent

	// exitWatchers tracks per-principal cancellation functions for
	// watchSandboxExit goroutines. When the daemon intentionally
	// destroys a sandbox (credential rotation, structural restart,
	// condition change), it cancels the watcher first so the old
	// goroutine does not see the destroy as an unexpected exit and
	// corrupt the daemon's running state. Protected by reconcileMu.
	exitWatchers map[string]context.CancelFunc

	// proxyExitWatchers tracks per-principal cancellation functions
	// for watchProxyExit goroutines. Mirrors exitWatchers but watches
	// the proxy process instead of the tmux session. When the proxy
	// dies unexpectedly, the watcher destroys the sandbox and triggers
	// re-reconciliation. Protected by reconcileMu.
	proxyExitWatchers map[string]context.CancelFunc

	// healthMonitors tracks running health check goroutines, keyed by
	// principal localpart. Protected by healthMonitorsMu. Started after
	// sandbox creation when the resolved template has a HealthCheck.
	healthMonitors   map[string]*healthMonitor
	healthMonitorsMu sync.Mutex

	// previousCPU stores the last /proc/stat reading for CPU utilization
	// delta computation. First heartbeat after startup reports 0%.
	previousCPU *cpuReading

	// gpuCollectors holds per-vendor GPU metric collectors. Each collector
	// keeps render node file descriptors open for the daemon's lifetime
	// to avoid per-heartbeat open/close overhead. Closed on shutdown.
	gpuCollectors []hwinfo.GPUCollector

	// services is the cached service directory, built from m.bureau.service
	// state events in #bureau/service. Keyed by service localpart (the
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

	// prefetchFunc fetches a Nix store path and its closure from
	// configured substituters. Defaults to prefetchNixStore. Tests
	// override this to avoid requiring a real Nix installation.
	prefetchFunc func(ctx context.Context, storePath string) error

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
	// populated from MachineStatus state events in #bureau/machine.
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

	// tmuxServer is Bureau's dedicated tmux server. Passed to relay
	// processes via BUREAU_TMUX_SOCKET (using SocketPath()) so they
	// attach to the correct tmux server.
	tmuxServer *tmux.Server

	// observeRelayBinary is the path to the bureau-observe-relay binary.
	// The daemon forks this for each observation session. Defaults to
	// "bureau-observe-relay" (found via PATH).
	observeRelayBinary string

	// observeSessions tracks active observation connections. The daemon
	// maintains this registry so that enforceObservePolicyChange can
	// terminate sessions whose observers no longer pass authorization
	// after a policy tightening.
	observeSessions   []*activeObserveSession
	observeSessionsMu sync.Mutex

	// Layout sync: each running principal has a layout watcher goroutine
	// that monitors its tmux session for changes and publishes them to
	// Matrix. On sandbox creation, the watcher also restores a previously
	// published layout (if one exists in Matrix).

	// layoutWatchers tracks running layout sync goroutines, keyed by
	// principal localpart. Protected by layoutWatchersMu.
	layoutWatchers   map[string]*layoutWatcher
	layoutWatchersMu sync.Mutex

	// workspaceRoot is the root directory for project workspaces
	// (e.g., /var/bureau/workspace). Command handlers use this to
	// locate workspace directories for status, du, and git operations.
	workspaceRoot string

	// pipelineExecutorBinary is the filesystem path to the
	// bureau-pipeline-executor binary. When set, the daemon can handle
	// pipeline.execute commands by spawning an ephemeral sandbox that
	// runs this binary. When empty, pipeline.execute returns an error.
	pipelineExecutorBinary string

	// pipelineEnvironment is the Nix store path providing the
	// pipeline executor's toolchain (e.g., /nix/store/...-runner-env).
	// The launcher bind-mounts /nix/store and prepends this path's bin/
	// to PATH inside the sandbox. When empty, the pipeline executor
	// sandbox has no Nix environment (only what bwrap provides by
	// default), which limits the shell commands pipeline steps can run.
	pipelineEnvironment string

	// shutdownCtx is the daemon's top-level context, cancelled on
	// SIGINT/SIGTERM. Used by async operations (pipeline execution)
	// that outlive a single sync cycle and need the daemon's lifecycle
	// for cancellation.
	shutdownCtx context.Context

	logger *slog.Logger
}

// statusLoop periodically publishes MachineStatus heartbeats.
func (d *Daemon) statusLoop(ctx context.Context) {
	// Publish initial status immediately.
	d.publishStatus(ctx)

	ticker := d.clock.NewTicker(d.statusInterval)
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

// publishStatus sends a MachineStatus state event to the machine room.
func (d *Daemon) publishStatus(ctx context.Context) {
	d.reconcileMu.RLock()
	runningCount := 0
	for range d.running {
		runningCount++
	}
	var lastActivity string
	if !d.lastActivityAt.IsZero() {
		lastActivity = d.lastActivityAt.UTC().Format(time.RFC3339)
	}
	d.reconcileMu.RUnlock()

	transportAddress := d.transportListener.Address()

	// Compute CPU utilization from the delta since the last heartbeat.
	// The first heartbeat after startup reports 0% (no baseline yet).
	currentCPU := readCPUStats()
	cpuUtilization := cpuPercent(d.previousCPU, currentCPU)
	d.previousCPU = currentCPU

	status := schema.MachineStatus{
		Principal: d.machineUserID,
		Sandboxes: schema.SandboxCounts{
			Running: runningCount,
		},
		CPUPercent:       int(cpuUtilization),
		MemoryUsedMB:     memoryUsedMB(),
		GPUStats:         d.collectGPUStats(),
		UptimeSeconds:    uptimeSeconds(),
		LastActivityAt:   lastActivity,
		TransportAddress: transportAddress,
	}

	_, err := d.session.SendStateEvent(ctx, d.machineRoomID, schema.EventTypeMachineStatus, d.machineName, status)
	if err != nil {
		d.logger.Error("publishing machine status", "error", err)
		return
	}
	d.logger.Debug("published machine status", "running_sandboxes", runningCount)
}

// publishMachineInfo probes system hardware and publishes the static
// inventory as an m.bureau.machine_info state event. Called once at
// startup. The state event is idempotent — if the hardware inventory
// hasn't changed since the last daemon run, the homeserver deduplicates
// the event (Matrix state events with identical content are no-ops).
func (d *Daemon) publishMachineInfo(ctx context.Context) {
	amdProber := amdgpu.NewProber()
	nvidiaProber := nvidia.NewProber()
	info := hwinfo.Probe(d.machineUserID, amdProber, nvidiaProber)
	info.DaemonVersion = version.Info()

	_, err := d.session.SendStateEvent(ctx, d.machineRoomID, schema.EventTypeMachineInfo, d.machineName, info)
	if err != nil {
		d.logger.Error("publishing machine info", "error", err)
		return
	}

	gpuCount := len(info.GPUs)
	d.logger.Info("published machine info",
		"hostname", info.Hostname,
		"cpu_model", info.CPU.Model,
		"memory_mb", info.MemoryTotalMB,
		"gpu_count", gpuCount,
	)
}

// collectGPUStats gathers dynamic GPU metrics from all collectors and
// returns them as a single GPUStats slice for the heartbeat.
func (d *Daemon) collectGPUStats() []schema.GPUStatus {
	var stats []schema.GPUStatus
	for _, collector := range d.gpuCollectors {
		if results := collector.Collect(); len(results) > 0 {
			stats = append(stats, results...)
		}
	}
	return stats
}

// closeGPUCollectors releases all GPU collector resources (open render
// node file descriptors, etc.). Called on daemon shutdown.
func (d *Daemon) closeGPUCollectors() {
	for _, collector := range d.gpuCollectors {
		collector.Close()
	}
	d.gpuCollectors = nil
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
// Returns both the client (needed for token verification against the homeserver)
// and the authenticated session.
func loadSession(stateDir string, homeserverURL string, logger *slog.Logger) (*messaging.Client, *messaging.Session, error) {
	sessionPath := filepath.Join(stateDir, "session.json")

	jsonData, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading session from %s: %w", sessionPath, err)
	}

	var data sessionData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		secret.Zero(jsonData)
		return nil, nil, fmt.Errorf("parsing session from %s: %w", sessionPath, err)
	}
	secret.Zero(jsonData)

	if data.AccessToken == "" {
		return nil, nil, fmt.Errorf("session file %s has empty access token", sessionPath)
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
		return nil, nil, fmt.Errorf("creating matrix client: %w", err)
	}

	session, err := client.SessionFromToken(data.UserID, data.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	return client, session, nil
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
	machineUserID := principal.MatrixUserID(machineName, serverName)

	// Create without power_level_content_override. The private_chat preset
	// gives the room creator PL 100, which is needed to send all the preset
	// events (join_rules, history_visibility, etc.) during room creation.
	// If we set the machine to PL 50 via override, the merged power levels
	// take effect before the preset events — and the machine at PL 50 can't
	// send m.room.join_rules (which requires PL 100 per state_default).
	// Instead, we create with preset defaults, then apply restrictive power
	// levels as a separate state event.
	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Config: " + machineName,
		Topic:      "Machine configuration and credentials for " + machineName,
		Alias:      aliasLocalpart,
		Preset:     "private_chat",
		Invite:     []string{adminUserID},
		Visibility: "private",
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

	// Apply restrictive power levels now that the room exists. The machine
	// (creator, PL 100 from preset) lowers itself to PL 50: sufficient to
	// invite and write layouts, but insufficient to modify config or
	// credentials (PL 100). The admin stays at PL 100.
	_, err = session.SendStateEvent(ctx, response.RoomID, "m.room.power_levels", "",
		schema.ConfigRoomPowerLevels(adminUserID, machineUserID))
	if err != nil {
		return "", fmt.Errorf("setting config room power levels: %w", err)
	}

	logger.Info("created config room",
		"room_id", response.RoomID,
		"alias", alias,
		"admin", adminUserID,
	)
	return response.RoomID, nil
}
