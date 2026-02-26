// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/hwinfo"
	"github.com/bureau-foundation/bureau/lib/hwinfo/amdgpu"
	"github.com/bureau-foundation/bureau/lib/hwinfo/nvidia"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/transport"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	var (
		homeserverURL          string
		machineName            string
		serverName             string
		fleetPrefix            string
		runDir                 string
		stateDir               string
		adminUser              string
		observeRelayBinary     string
		workspaceRoot          string
		pipelineExecutorBinary string
		pipelineEnvironment    string
		operatorsGroup         string
		statusInterval         time.Duration
		maxIdleInterval        time.Duration
		haBaseDelay            time.Duration
		drainGracePeriod       time.Duration
		showVersion            bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., bureau/fleet/prod/machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&fleetPrefix, "fleet", "", "fleet prefix (e.g., bureau/fleet/prod) (required)")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets and ephemeral state (must match the launcher's --run-dir)")
	flag.StringVar(&stateDir, "state-dir", principal.DefaultStateDir, "directory containing session.json from the launcher")
	flag.StringVar(&adminUser, "admin-user", "bureau-admin", "admin account username (for config room invites)")
	flag.StringVar(&observeRelayBinary, "observe-relay-binary", "bureau-observe-relay", "path to the observation relay binary")
	flag.StringVar(&workspaceRoot, "workspace-root", principal.DefaultWorkspaceRoot, "root directory for project workspaces")
	flag.StringVar(&pipelineExecutorBinary, "pipeline-executor-binary", "", "path to bureau-pipeline-executor binary (enables pipeline.execute command)")
	flag.StringVar(&pipelineEnvironment, "pipeline-environment", "", "Nix store path providing pipeline executor's toolchain (e.g., /nix/store/...-runner-env)")
	flag.DurationVar(&statusInterval, "status-interval", 60*time.Second, "how often to collect machine status metrics")
	flag.DurationVar(&maxIdleInterval, "max-idle-interval", defaultMaxIdleInterval, "maximum time between heartbeat publishes when metrics are unchanged (liveness interval)")
	flag.DurationVar(&haBaseDelay, "ha-base-delay", 1*time.Second, "base unit for HA acquisition timing (all backoff ranges and verification scale from this; 0 for instant acquisition in tests)")
	flag.DurationVar(&drainGracePeriod, "drain-grace-period", 10*time.Second, "time to wait after SIGTERM before force-killing a drained sandbox (agents should finish cleanup within this window)")
	flag.StringVar(&operatorsGroup, "operators-group", principal.OperatorsGroupName, "Unix group for operator socket access (empty to disable)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		version.Print("bureau-daemon")
		return nil
	}

	if machineName == "" {
		return fmt.Errorf("--machine-name is required")
	}
	parsedServerName, err := ref.ParseServerName(serverName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}
	machine, err := ref.ParseMachine(machineName, parsedServerName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}
	fleet := machine.Fleet()

	// Validate the --fleet flag matches the fleet embedded in the machine
	// localpart. The launcher passes both flags, and a mismatch would
	// produce wrong room aliases for fleet-scoped rooms.
	if fleetPrefix != "" && fleetPrefix != fleet.Localpart() {
		return fmt.Errorf("--fleet %q does not match fleet in --machine-name %q (expected %q)", fleetPrefix, machineName, fleet.Localpart())
	}

	if err := principal.ValidateRunDir(runDir); err != nil {
		return fmt.Errorf("run directory validation: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("machine", machine.Localpart())
	slog.SetDefault(logger)

	if maxLocalpart := principal.MaxLocalpartAvailable(runDir); maxLocalpart < principal.MaxLocalpartLength {
		logger.Warn("--run-dir limits maximum localpart length",
			"run_dir", runDir,
			"max_localpart", maxLocalpart,
			"default_max", principal.MaxLocalpartLength,
		)
	}

	// Look up the operators group GID for setting socket group ownership
	// on operator-facing sockets. In production, the group is created by
	// "bureau machine doctor --fix". Pass an empty --operators-group to
	// disable group ownership (integration tests, development environments
	// without the system group).
	operatorsGID := principal.LookupOperatorsGID(operatorsGroup)
	if operatorsGroup == "" {
		logger.Info("operators group disabled (--operators-group is empty)")
	} else if operatorsGID < 0 {
		logger.Warn("operators group not found (operator socket group ownership will not be set)",
			"group", operatorsGroup)
	} else {
		logger.Info("operators group found", "group", operatorsGroup, "gid", operatorsGID)
	}

	// Compute the daemon's binary hash and resolve its filesystem path.
	// Done early since it's pure filesystem I/O with no dependencies.
	// Both values are stable for the lifetime of this process — the
	// binary doesn't change while it's running (exec() creates a new
	// process with its own hash and path).
	var daemonBinaryHash, daemonBinaryPath string
	if selfHash, selfPath, hashErr := version.ComputeSelfHash(); hashErr == nil {
		daemonBinaryHash = selfHash
		daemonBinaryPath = selfPath
		logger.Info("daemon binary hash computed", "hash", daemonBinaryHash, "path", daemonBinaryPath)
	} else {
		logger.Warn("failed to compute daemon binary hash", "error", hashErr)
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Wrap the signal context so the daemon can self-cancel on
	// unrecoverable errors (e.g., Matrix account deactivated).
	ctx, daemonCancel := context.WithCancel(signalCtx)
	defer daemonCancel()

	// Load the Matrix session saved by the launcher. The client is also
	// needed for the token verifier (creates temporary sessions for whoami).
	client, session, err := service.LoadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	// Validate the session is still valid.
	userID, err := service.ValidateSession(ctx, session)
	if err != nil {
		return err
	}
	logger.Info("matrix session valid", "user_id", userID)

	// Load or generate the Ed25519 keypair used to sign service identity
	// tokens. The keypair is persisted in stateDir so it survives daemon
	// restarts. On first boot, a new keypair is generated and saved.
	tokenSigningPublicKey, tokenSigningPrivateKey, keyWasGenerated, err := servicetoken.LoadOrGenerateKeypair(stateDir)
	if err != nil {
		return fmt.Errorf("loading token signing keypair: %w", err)
	}
	logger.Info("token signing keypair ready",
		"public_key", hex.EncodeToString(tokenSigningPublicKey),
		"generated", keyWasGenerated,
	)

	// Load the machine's age public key. Written by the launcher at first
	// boot, shared via the state directory. The daemon uses this to encrypt
	// credential bundles that the launcher can decrypt. If the key is not
	// yet available (launcher hasn't run first-boot yet), credential
	// provisioning will return clear errors when attempted.
	var machinePublicKey string
	machinePublicKeyPath := filepath.Join(stateDir, "machine-key.pub")
	machinePublicKeyData, err := os.ReadFile(machinePublicKeyPath)
	if err != nil {
		logger.Warn("machine age public key not available (credential provisioning disabled)",
			"path", machinePublicKeyPath,
			"error", err,
		)
	} else {
		machinePublicKey = strings.TrimSpace(string(machinePublicKeyData))
		if err := sealed.ParsePublicKey(machinePublicKey); err != nil {
			return fmt.Errorf("invalid machine age public key in %s: %w", machinePublicKeyPath, err)
		}
		logger.Info("machine age public key loaded")
	}

	// Resolve and join the per-machine config room. The room must already
	// exist (created by "bureau machine provision"). The alias follows the
	// @→# convention: the machine's fleet-scoped localpart IS the room
	// alias localpart.
	configRoomAlias := machine.RoomAlias()
	configRoomID, err := resolveConfigRoom(ctx, session, configRoomAlias, logger)
	if err != nil {
		return fmt.Errorf("resolving config room: %w", err)
	}
	logger.Info("config room ready", "room_id", configRoomID, "alias", configRoomAlias)

	// Resolve and join the system room. The room is invite-only (created
	// with the private_chat preset by bureau matrix setup), so the machine
	// account must have been invited by the admin before the join can
	// succeed. The daemon defensively re-joins on every startup to handle
	// membership recovery (e.g., homeserver reset, account re-creation).
	systemAlias := fleet.Namespace().SystemRoomAlias()
	systemRoomID, err := session.ResolveAlias(ctx, systemAlias)
	if err != nil {
		return fmt.Errorf("resolving system room alias %q: %w", systemAlias, err)
	}
	if _, err := session.JoinRoom(ctx, systemRoomID); err != nil {
		logger.Warn("failed to join system room (requires admin invitation)",
			"room_id", systemRoomID, "alias", systemAlias, "error", err)
	}

	// Resolve and join the fleet-scoped rooms. Each fleet has its own
	// machine, service, and config rooms derived from the fleet prefix.
	fleetAlias := fleet.RoomAlias()
	fleetRoomID, err := session.ResolveAlias(ctx, fleetAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet room alias %q: %w", fleetAlias, err)
	}
	if _, err := session.JoinRoom(ctx, fleetRoomID); err != nil {
		logger.Warn("failed to join fleet room (requires admin invitation)",
			"room_id", fleetRoomID, "alias", fleetAlias, "error", err)
	}

	machineAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet machine room alias %q: %w", machineAlias, err)
	}
	if _, err := session.JoinRoom(ctx, machineRoomID); err != nil {
		logger.Warn("failed to join fleet machine room (requires admin invitation)",
			"room_id", machineRoomID, "alias", machineAlias, "error", err)
	}

	serviceAlias := fleet.ServiceRoomAlias()
	serviceRoomID, err := session.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet service room alias %q: %w", serviceAlias, err)
	}
	if _, err := session.JoinRoom(ctx, serviceRoomID); err != nil {
		logger.Warn("failed to join fleet service room (requires admin invitation)",
			"room_id", serviceRoomID, "alias", serviceAlias, "error", err)
	}

	// Resolve the namespace-scoped template and pipeline rooms. The daemon
	// is invited to these during machine provisioning and auto-accepts. It
	// needs membership to read templates on-demand, but state changes in
	// these rooms must NOT trigger reconciliation — they are shared across
	// all machines in the namespace, and member events from other machines
	// joining would cause spurious reconcile cycles.
	namespace := fleet.Namespace()
	templateAlias := namespace.TemplateRoomAlias()
	templateRoomID, err := session.ResolveAlias(ctx, templateAlias)
	if err != nil {
		logger.Warn("failed to resolve template room (will still work via on-demand resolution)",
			"alias", templateAlias, "error", err)
	}
	pipelineAlias := namespace.PipelineRoomAlias()
	pipelineRoomID, err := session.ResolveAlias(ctx, pipelineAlias)
	if err != nil {
		logger.Warn("failed to resolve pipeline room",
			"alias", pipelineAlias, "error", err)
	}

	logger.Info("fleet rooms ready",
		"fleet", fleet.Localpart(),
		"fleet_room", fleetRoomID,
		"system_room", systemRoomID,
		"machine_room", machineRoomID,
		"service_room", serviceRoomID,
		"template_room", templateRoomID,
		"pipeline_room", pipelineRoomID,
	)

	// Build the sync filter after room resolution. The system, template,
	// and pipeline rooms are excluded via "not_rooms" because the daemon
	// only reads their state on-demand (token signing keys, template
	// definitions, pipeline definitions) and never needs reactive /sync
	// delivery from them.
	syncFilter := buildSyncFilter([]ref.RoomID{systemRoomID, templateRoomID, pipelineRoomID})

	// Check the watchdog from a previous exec() attempt. This detects
	// whether a prior daemon self-update succeeded (we're the new binary)
	// or failed (we're the old binary restarted after a crash).
	var failedExecPath string
	if daemonBinaryPath != "" {
		failedExecPath = checkDaemonWatchdog(
			filepath.Join(stateDir, "daemon-watchdog.cbor"),
			daemonBinaryPath,
			session,
			configRoomID,
			logger,
		)
	}

	daemon := &Daemon{
		session:                session,
		clock:                  clock.Real(),
		client:                 client,
		tokenVerifier:          newTokenVerifier(client, 5*time.Minute, clock.Real(), logger),
		tokenSigningPublicKey:  tokenSigningPublicKey,
		tokenSigningPrivateKey: tokenSigningPrivateKey,
		authorizationIndex:     authorization.NewIndex(),
		machinePublicKey:       machinePublicKey,
		machine:                machine,
		fleet:                  fleet,
		adminUser:              adminUser,
		systemRoomID:           systemRoomID,
		configRoomID:           configRoomID,
		machineRoomID:          machineRoomID,
		serviceRoomID:          serviceRoomID,
		fleetRoomID:            fleetRoomID,
		syncFilter:             syncFilter,
		runDir:                 runDir,
		fleetRunDir:            fleet.RunDir(runDir),
		launcherSocket:         principal.LauncherSocketPath(runDir),
		statusInterval:         statusInterval,
		maxIdleInterval:        maxIdleInterval,
		drainGracePeriod:       drainGracePeriod,
		operatorsGID:           operatorsGID,
		daemonBinaryHash:       daemonBinaryHash,
		daemonBinaryPath:       daemonBinaryPath,
		stateDir:               stateDir,
		previousCgroupCPU:      make(map[ref.Entity]*hwinfo.CgroupCPUReading),
		cgroupPathFunc: func(principal ref.Entity) string {
			return hwinfo.CgroupDefaultPath(principal.AccountLocalpart())
		},
		workspaceAliases:      make(map[ref.RoomID]ref.RoomAlias),
		failedExecPaths:       make(map[string]bool),
		startFailures:         make(map[ref.Entity]*startFailure),
		running:               make(map[ref.Entity]bool),
		statusNotify:          make(chan struct{}, 1),
		dynamicPrincipals:     make(map[ref.Entity]bool),
		completed:             make(map[ref.Entity]bool),
		draining:              make(map[ref.Entity]context.CancelFunc),
		pipelineTickets:       make(map[string]ref.Entity),
		pipelineEnabledRooms:  make(map[ref.RoomID]bool),
		exitWatchers:          make(map[ref.Entity]context.CancelFunc),
		proxyExitWatchers:     make(map[ref.Entity]context.CancelFunc),
		lastCredentials:       make(map[ref.Entity]string),
		lastGrants:            make(map[ref.Entity][]schema.Grant),
		lastTokenMint:         make(map[ref.Entity]time.Time),
		activeTokens:          make(map[ref.Entity][]activeToken),
		lastServiceMounts:     make(map[ref.Entity][]launcherServiceMount),
		lastObserveAllowances: make(map[ref.Entity][]schema.Allowance),
		lastSpecs:             make(map[ref.Entity]*schema.SandboxSpec),
		previousSpecs:         make(map[ref.Entity]*schema.SandboxSpec),
		lastTemplates:         make(map[ref.Entity]*schema.TemplateContent),
		healthMonitors:        make(map[ref.Entity]*healthMonitor),
		services:              make(map[string]*schema.Service),
		proxyRoutes:           make(map[string]string),
		peerAddresses:         make(map[string]string),
		peerTransports:        make(map[string]http.RoundTripper),
		tunnels:               make(map[string]*tunnelInstance),
		adminSocketPathFunc: func(principal ref.Entity) string {
			return principal.ProxyAdminSocketPath(fleet.RunDir(runDir))
		},
		observeSocketPath:      principal.ObserveSocketPath(runDir),
		tmuxServer:             tmux.NewServer(principal.TmuxSocketPath(runDir), ""),
		observeRelayBinary:     observeRelayBinary,
		layoutWatchers:         make(map[ref.Entity]*layoutWatcher),
		validateCommandFunc:    validateCommandBinary,
		workspaceRoot:          workspaceRoot,
		pipelineExecutorBinary: pipelineExecutorBinary,
		pipelineEnvironment:    pipelineEnvironment,
		shutdownCtx:            ctx,
		shutdownCancel:         daemonCancel,
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

	// Start the credential provisioning service socket. Connector
	// principals with credential/* grants get this socket mounted into
	// their sandbox and use it to inject per-principal credentials
	// (API tokens, service keys, etc.) into age-encrypted bundles.
	if _, err := daemon.startCredentialService(ctx); err != nil {
		return fmt.Errorf("starting credential service: %w", err)
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

	// Publish the token signing public key to #bureau/system so services
	// can discover it for token verification. Conditional: only publishes
	// if the key was just generated or the existing state event differs.
	daemon.publishTokenSigningKey(ctx, keyWasGenerated)

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

	// Initialize the HA watchdog for daemon-level failover of
	// ha_class:critical services.
	daemon.haWatchdog = newHAWatchdog(daemon, haBaseDelay, logger)

	// Perform the initial Matrix /sync to establish a since token and
	// baseline state (reconcile, peer addresses, service directory).
	sinceToken, err := daemon.initialSync(ctx)
	if err != nil {
		if isAuthError(err) {
			return fmt.Errorf("machine account authentication failed: %w", err)
		}
		logger.Error("initial sync failed", "error", err)
		// Continue — the sync loop will start from scratch with an empty
		// token, which triggers a fresh initial sync.
	}

	// Start the incremental sync loop, status heartbeat loop, and
	// token refresh loop.
	go service.RunSyncLoop(ctx, daemon.session, service.SyncConfig{
		Filter:      daemon.syncFilter,
		OnSyncError: daemon.syncErrorHandler,
	}, sinceToken, daemon.processSyncResponse, daemon.clock, daemon.logger)
	go daemon.statusLoop(ctx)
	go daemon.tokenRefreshLoop(ctx)

	// Signal that this daemon is online and ready to manage sandboxes.
	// Presence is the canonical liveness primitive: the fleet controller
	// uses it for fast crash detection (seconds vs heartbeat staleness).
	if err := daemon.session.SetPresence(ctx, "online", ""); err != nil {
		logger.Error("setting initial presence to online", "error", err)
	}

	// Release any HA leases on shutdown so other daemons can take
	// over quickly without waiting for the lease to expire.
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		daemon.haWatchdog.releaseAllLeases(releaseCtx)
	}()

	// Set presence to offline on shutdown. Uses context.Background()
	// because ctx is already cancelled when the defer runs. On crash
	// or token revocation this fails gracefully — the homeserver
	// detects the TCP drop and marks the daemon offline via its
	// built-in inactivity timeout.
	defer func() {
		presenceCtx, presenceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer presenceCancel()
		if err := daemon.session.SetPresence(presenceCtx, "offline", "shutting down"); err != nil {
			logger.Error("setting presence to offline on shutdown", "error", err)
		}
	}()

	// Wait for shutdown.
	<-ctx.Done()
	logger.Info("shutting down")
	return nil
}

// Daemon is the core daemon state.
type Daemon struct {
	session *messaging.DirectSession

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
	// reconciliation. Used by observation and service routing to access
	// principal assignments. Nil before the first successful reconcile.
	lastConfig *schema.MachineConfig

	// authorizationIndex holds per-principal resolved authorization
	// policies. Built from MachineConfig.DefaultPolicy merged with
	// per-principal PrincipalAssignment.Authorization and room-level
	// m.bureau.authorization policies. Updated on every reconcile cycle
	// and incrementally via temporal grants from /sync. Read by
	// observation authorization and token minting.
	authorizationIndex *authorization.Index

	// tokenSigningPublicKey is the Ed25519 public key used to verify
	// service identity tokens. Published to #bureau/system at startup
	// so services can discover it.
	tokenSigningPublicKey ed25519.PublicKey

	// tokenSigningPrivateKey is the Ed25519 private key used to sign
	// service identity tokens issued to sandboxed principals.
	tokenSigningPrivateKey ed25519.PrivateKey

	// machinePublicKey is the age x25519 public key for this machine's
	// keypair. The launcher holds the private key; the daemon uses the
	// public key to encrypt credential bundles via the provisioning
	// service. Loaded from <stateDir>/machine-key.pub at startup. Empty
	// if the key file is not yet available (launcher hasn't completed
	// first-boot), which disables credential provisioning with a clear
	// error message.
	machinePublicKey string

	machine       ref.Machine
	fleet         ref.Fleet
	adminUser     string // admin account localpart (for fleet controller PL grants)
	systemRoomID  ref.RoomID
	configRoomID  ref.RoomID
	machineRoomID ref.RoomID
	serviceRoomID ref.RoomID
	fleetRoomID   ref.RoomID // fleet room for HA leases, service definitions, and alerts
	syncFilter    string     // inline Matrix /sync filter JSON (room- and type-scoped)

	// workspaceAliases maps room IDs to their canonical aliases for
	// rooms discovered via /sync invites (workspace rooms). Populated
	// when the daemon accepts a room invite and reads the room's
	// m.room.canonical_alias state event. Core rooms (config, system,
	// fleet, machine, service) are not stored here — their aliases are
	// derived deterministically from d.machine and d.fleet.
	workspaceAliases   map[ref.RoomID]ref.RoomAlias
	workspaceAliasesMu sync.RWMutex
	runDir             string // base runtime directory (e.g., /run/bureau)
	fleetRunDir        string // fleet-scoped runtime directory (e.g., /run/bureau/fleet/prod)
	launcherSocket     string
	statusInterval     time.Duration
	maxIdleInterval    time.Duration
	drainGracePeriod   time.Duration

	// operatorsGID is the numeric GID of the bureau-operators system
	// group. Operator-facing sockets (observe.sock, service endpoints)
	// are chown'd to this group so members of bureau-operators can
	// connect. Set to -1 if the group does not exist (development
	// environments without "bureau machine doctor" setup), in which
	// case group ownership changes are skipped.
	operatorsGID int

	// daemonBinaryHash is the SHA256 hex digest of the currently running
	// daemon binary, computed once at startup via os.Executable(). Used
	// by BureauVersion comparison to determine whether a config update
	// requires a daemon restart (via exec()). Empty if the hash could
	// not be computed (treated as always-changed by version.Compare).
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

	// startFailures tracks principals whose sandbox creation failed on a
	// previous reconcile cycle. Each entry records the failure category,
	// error message, attempt count, and a computed next-retry time using
	// exponential backoff (1s, 2s, 4s, 8s, 16s, capped at 30s). The
	// "create missing" pass in reconcile() skips principals whose
	// nextRetryAt is still in the future, preventing a tight retry loop
	// when a failure is persistent (e.g., service not yet registered,
	// credentials not yet provisioned).
	//
	// Entries are cleared by event-driven signals: config room state
	// changes clear all entries (the config may have changed); service
	// directory syncs clear entries with category "service_resolution"
	// (the missing service may have appeared). watchSandboxExit and
	// watchProxyExit also clear entries so crash-restart isn't delayed.
	//
	// Protected by reconcileMu (same as running, lastSpecs, etc.).
	startFailures map[ref.Entity]*startFailure

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
	// Keys are ref.Entity values from PrincipalAssignment.Principal.
	running map[ref.Entity]bool

	// statusNotify is signaled (non-blocking) whenever the running set
	// changes. The statusLoop selects on this channel alongside the
	// periodic ticker, so MachineStatus is published immediately when
	// sandbox state changes rather than waiting for the next heartbeat.
	statusNotify chan struct{}

	// ephemeralCounter is an atomic counter used to generate short,
	// unique localparts for ephemeral principals (pipeline executors,
	// worktree operations). Monotonically increasing within the daemon
	// process lifetime. Cross-restart uniqueness is not needed since
	// ephemeral sandboxes don't survive daemon restarts.
	ephemeralCounter atomic.Uint64

	// dynamicPrincipals tracks running principals that were created
	// dynamically by the daemon (not from MachineConfig principal
	// assignments). Pipeline executors are the current dynamic
	// sandbox type: they run a pipeline, publish results, and exit.
	// The reconcile loop must NOT kill them when their start condition
	// becomes unsatisfied, because the pipeline itself may have
	// published the state change that invalidated the condition (e.g.,
	// a teardown pipeline sets workspace status to "archived", which
	// makes the teardown's "destroying" start condition false). Killing
	// a pipeline executor mid-execution leaves the workspace in a
	// stuck state with no pipeline_result event. Dynamic principals
	// are removed from this set when their sandbox exits (in
	// watchSandboxExit) and default to RestartPolicy "on-failure".
	dynamicPrincipals map[ref.Entity]bool

	// completed tracks principals that exited cleanly and should not
	// be restarted (based on their RestartPolicy). Principals with
	// policy "on-failure" that exit 0, or "never" that exit at all,
	// are added here. Entries are removed when the principal is removed
	// from the config or when the daemon restarts (in-memory only).
	completed map[ref.Entity]bool

	// draining tracks principals that have been sent SIGTERM and are
	// in their grace period before force-kill. The reconcile loop
	// skips draining principals in the "destroy unneeded" pass. Each
	// entry holds a cancel function that aborts the grace period
	// timer goroutine. Entries are cleaned up by destroyPrincipal
	// (force-kill after grace period) or watchSandboxExit (graceful
	// exit during drain).
	draining map[ref.Entity]context.CancelFunc

	// pipelineTickets maps pip- ticket state keys (e.g., "pip-a3f9")
	// to the principal entity running the executor for that ticket.
	// Prevents double-execution: both the /sync pipeline ticket
	// watcher and reconcile's applyPipelineExecutorOverlay path
	// register their tickets here. Cleaned up when the executor
	// sandbox exits (in watchSandboxExit and destroyPrincipal).
	pipelineTickets map[string]ref.Entity

	// pipelineEnabledRooms tracks which rooms have m.bureau.pipeline_config
	// state events. Populated from /sync state events (both initial and
	// incremental). processPipelineTickets skips pip- tickets in rooms
	// not in this set — pipeline execution requires explicit opt-in via
	// "bureau pipeline enable". Protected by reconcileMu.
	pipelineEnabledRooms map[ref.RoomID]bool

	// lastCredentials stores the ciphertext from the most recently
	// deployed m.bureau.credentials state event for each running
	// principal. On each reconcile cycle, the daemon compares the
	// current ciphertext against this stored value — a mismatch
	// triggers a destroy+create cycle to rotate the proxy's credentials.
	lastCredentials map[ref.Entity]string

	// lastGrants stores the pre-resolved authorization grants most
	// recently pushed to each running principal's proxy. On each
	// reconcile cycle, the daemon compares the current grants against
	// this stored value — a mismatch triggers a PUT /v1/admin/authorization
	// call to hot-reload the proxy's grant-based enforcement without
	// restarting the sandbox.
	lastGrants map[ref.Entity][]schema.Grant

	// lastTokenMint stores when service tokens were last minted for
	// each running principal. The token refresh goroutine checks this
	// to determine which principals need fresh tokens (those past 80%
	// of the token TTL). Set during sandbox creation and on each
	// successful refresh. Reset to zero time when grants change to
	// trigger an early re-mint. Protected by reconcileMu.
	lastTokenMint map[ref.Entity]time.Time

	// activeTokens tracks unexpired token IDs for each running
	// principal. Updated on mint and refresh; read on destroy for
	// emergency revocation push to services. Each entry includes the
	// token ID, its service role (for routing the revocation to the
	// correct service), and its natural expiry time (for blacklist
	// auto-cleanup on the service side). Expired entries are pruned
	// on each mint. Protected by reconcileMu.
	activeTokens map[ref.Entity][]activeToken

	// lastServiceMounts stores the resolved service socket paths for
	// each running principal. Populated at sandbox creation from
	// resolveServiceMounts; read at destroy time to push token
	// revocations to the relevant services. Protected by reconcileMu.
	lastServiceMounts map[ref.Entity][]launcherServiceMount

	// lastObserveAllowances stores the per-principal resolved allowances
	// from the authorization index at the end of the most recent reconcile
	// cycle. Used purely for change detection: when a principal's
	// allowances in the index differ from the stored value, the daemon
	// re-evaluates active observation sessions and terminates any that
	// no longer pass authorization. Compared by reflect.DeepEqual.
	lastObserveAllowances map[ref.Entity][]schema.Allowance

	// lastSpecs stores the SandboxSpec sent to the launcher for each
	// running principal. Used to detect payload-only changes that can
	// be hot-reloaded without restarting the sandbox. Nil entries mean
	// the principal was created without a SandboxSpec (no template).
	lastSpecs map[ref.Entity]*schema.SandboxSpec

	// previousSpecs stores the last-known-good SandboxSpec before a
	// structural restart. On health check rollback, the daemon recreates
	// the sandbox with this spec. Cleared after rollback or when the
	// principal is removed from config. Keyed by principal localpart.
	previousSpecs map[ref.Entity]*schema.SandboxSpec

	// lastTemplates stores the resolved TemplateContent for each running
	// principal. Health monitors read HealthCheck config from here.
	// Keyed by principal localpart.
	lastTemplates map[ref.Entity]*schema.TemplateContent

	// exitWatchers tracks per-principal cancellation functions for
	// watchSandboxExit goroutines. When the daemon intentionally
	// destroys a sandbox (credential rotation, structural restart,
	// condition change), it cancels the watcher first so the old
	// goroutine does not see the destroy as an unexpected exit and
	// corrupt the daemon's running state. Protected by reconcileMu.
	exitWatchers map[ref.Entity]context.CancelFunc

	// proxyExitWatchers tracks per-principal cancellation functions
	// for watchProxyExit goroutines. Mirrors exitWatchers but watches
	// the proxy process instead of the tmux session. When the proxy
	// dies unexpectedly, the watcher destroys the sandbox and triggers
	// re-reconciliation. Protected by reconcileMu.
	proxyExitWatchers map[ref.Entity]context.CancelFunc

	// healthMonitors tracks running health check goroutines, keyed by
	// principal localpart. Protected by healthMonitorsMu. Started after
	// sandbox creation when the resolved template has a HealthCheck.
	healthMonitors   map[ref.Entity]*healthMonitor
	healthMonitorsMu sync.Mutex

	// lastPublishedStatus stores the most recently published MachineStatus
	// for adaptive heartbeat comparison. On each collection tick, the
	// current status is compared against this value using deadband
	// thresholds. A publish is suppressed when all metrics are within
	// their deadbands AND the liveness window (maxIdleInterval) has not
	// elapsed. Accessed only by statusLoop (single goroutine).
	lastPublishedStatus *schema.MachineStatus

	// lastPublishTime records when the last heartbeat was published to
	// Matrix. Used to enforce the liveness window: even when metrics
	// are unchanged, a heartbeat is published after maxIdleInterval
	// to confirm the machine is still alive.
	lastPublishTime time.Time

	// previousCPU stores the last /proc/stat reading for CPU utilization
	// delta computation. First heartbeat after startup reports 0%.
	previousCPU *hwinfo.CPUReading

	// previousCgroupCPU stores the last cgroup cpu.stat reading for each
	// running principal. Keyed by localpart. Used for delta computation
	// across heartbeat intervals. Accessed only by publishStatus in the
	// statusLoop goroutine, so no mutex is needed (same pattern as
	// previousCPU).
	previousCgroupCPU map[ref.Entity]*hwinfo.CgroupCPUReading

	// cgroupPathFunc returns the cgroup v2 directory path for a
	// principal's sandbox. Defaults to hwinfo.CgroupDefaultPath. Tests
	// override this to use temp directories with synthetic cgroup files.
	cgroupPathFunc func(principal ref.Entity) string

	// gpuCollectors holds per-vendor GPU metric collectors. Each collector
	// keeps render node file descriptors open for the daemon's lifetime
	// to avoid per-heartbeat open/close overhead. Closed on shutdown.
	gpuCollectors []hwinfo.GPUCollector

	// serviceResyncCountdown tracks remaining /sync cycles where the
	// daemon should unconditionally re-read the service room state,
	// regardless of whether /sync delivered service room events. Set
	// to a positive value after reconcile starts new sandboxes (which
	// may register services via BootstrapViaProxy). Decremented each
	// cycle; cleared to zero when syncServiceDirectory finds new
	// services. This is defense-in-depth against /sync event delivery
	// gaps under concurrent write load: GetRoomState reads authoritative
	// state regardless of event stream position ordering.
	serviceResyncCountdown int

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
	// the key (DELETE /v1/admin/services/{name}). Protected by
	// reconcileMu — written by reconcileServices in the sync loop,
	// read by configureConsumerProxy during reconcile and health
	// rollback.
	proxyRoutes map[string]string

	// adminSocketPathFunc returns the admin socket path for a consumer
	// principal's proxy. Defaults to parsing the localpart via
	// ref.ParseEntityLocalpart and deriving the fleet-scoped path.
	// Tests override this to use temp directories.
	adminSocketPathFunc func(principal ref.Entity) string

	// prefetchFunc fetches a Nix store path and its closure from
	// configured substituters. Defaults to prefetchNixStore. Tests
	// override this to avoid requiring a real Nix installation.
	prefetchFunc func(ctx context.Context, storePath string) error

	// validateCommandFunc checks that a command binary is resolvable
	// before sending a create-sandbox request to the launcher. Defaults
	// to validateCommandBinary. Tests override this when using fictional
	// binary paths that don't need to exist on the host.
	validateCommandFunc func(command string, environmentPath string) error

	// collectStatusFunc assembles a MachineStatus for the heartbeat
	// loop. Defaults to nil, which means statusLoop calls the real
	// collectStatus (reads /proc, cgroups, GPU). Tests override this
	// to return deterministic metrics, avoiding flaky deadband
	// comparisons caused by real system metric fluctuation.
	collectStatusFunc func(ctx context.Context) schema.MachineStatus

	// statusLoopIterationDone is signaled (non-blocking) after each
	// statusLoop iteration completes — both publishes and skips. Nil
	// in production (zero overhead). Tests set this to synchronize
	// with the loop goroutine without time.Sleep.
	statusLoopIterationDone chan struct{}

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

	// tunnels tracks active outbound tunnel sockets, keyed by a
	// logical name. Each tunnel creates a local Unix socket that
	// bridges accepted connections to a remote service via the
	// transport. Names follow a convention:
	//   - "upstream": shared cache tunnel for artifact fallthrough
	//   - "push/<machine-localpart>": push target tunnel
	tunnels map[string]*tunnelInstance

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
	// maintains this registry so that enforceObserveAllowanceChange can
	// terminate sessions whose observers no longer pass authorization
	// after an allowance tightening.
	observeSessions   []*activeObserveSession
	observeSessionsMu sync.Mutex

	// Layout sync: each running principal has a layout watcher goroutine
	// that monitors its tmux session for changes and publishes them to
	// Matrix. On sandbox creation, the watcher also restores a previously
	// published layout (if one exists in Matrix).

	// layoutWatchers tracks running layout sync goroutines, keyed by
	// principal localpart. Protected by layoutWatchersMu.
	layoutWatchers   map[ref.Entity]*layoutWatcher
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
	// SIGINT/SIGTERM or when the daemon detects an unrecoverable
	// condition (e.g., Matrix account deactivated). Used by async
	// operations (pipeline execution) that outlive a single sync
	// cycle and need the daemon's lifecycle for cancellation.
	shutdownCtx context.Context

	// shutdownCancel cancels shutdownCtx. Called by emergencyShutdown
	// to trigger a daemon exit when the Matrix account is deactivated.
	// Nil in unit tests that don't need self-cancellation.
	shutdownCancel context.CancelFunc

	// haWatchdog manages HA lease acquisition and renewal for
	// ha_class:critical services. Nil when the fleet room is not
	// available (fleet features disabled).
	haWatchdog *haWatchdog

	// reconcileNotify receives a signal after each asynchronous
	// reconcile completes (sandbox exit, proxy crash recovery, health
	// rollback). Tests use this to wait for state changes without
	// polling. Nil in production — notifications are skipped.
	reconcileNotify chan struct{}

	logger *slog.Logger
}

// roomAlias returns the human-readable room alias for a known room ID.
// For the daemon's 5 core rooms, the alias is derived deterministically
// from the machine and fleet refs. For workspace rooms, the alias is
// looked up from the cache populated on invite acceptance. Returns the
// zero RoomAlias if the room is not tracked.
func (d *Daemon) roomAlias(roomID ref.RoomID) ref.RoomAlias {
	switch roomID {
	case d.configRoomID:
		return d.machine.RoomAlias()
	case d.systemRoomID:
		return d.fleet.Namespace().SystemRoomAlias()
	case d.fleetRoomID:
		return d.fleet.RoomAlias()
	case d.machineRoomID:
		return d.fleet.MachineRoomAlias()
	case d.serviceRoomID:
		return d.fleet.ServiceRoomAlias()
	default:
		d.workspaceAliasesMu.RLock()
		alias := d.workspaceAliases[roomID]
		d.workspaceAliasesMu.RUnlock()
		return alias
	}
}

// displayRoom returns a human-readable string identifying a room. When
// an alias is known, returns the alias (e.g., "#bureau/fleet/prod:server").
// Otherwise returns the opaque room ID. Used in fmt.Errorf messages
// where a single string representation is needed.
func (d *Daemon) displayRoom(roomID ref.RoomID) string {
	if alias := d.roomAlias(roomID); !alias.IsZero() {
		return alias.String()
	}
	return roomID.String()
}

// cacheRoomAlias fetches the m.room.canonical_alias state event for a
// room and stores it in the workspace aliases map. Best-effort: if the
// fetch fails or the room has no alias, the map is not updated and
// subsequent displayRoom calls fall back to the opaque room ID.
func (d *Daemon) cacheRoomAlias(ctx context.Context, roomID ref.RoomID) {
	raw, err := d.session.GetStateEvent(ctx, roomID, schema.MatrixEventTypeCanonicalAlias, "")
	if err != nil {
		return
	}
	var content struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(raw, &content); err != nil || content.Alias == "" {
		return
	}
	alias, err := ref.ParseRoomAlias(content.Alias)
	if err != nil {
		return
	}
	d.workspaceAliasesMu.Lock()
	d.workspaceAliases[roomID] = alias
	d.workspaceAliasesMu.Unlock()
}

// startFailureCategory classifies why a principal failed to start, so
// event-driven clearing can target the right subset of failures. For
// example, a service directory sync only clears "service_resolution"
// failures, not credential or template failures.
type startFailureCategory string

const (
	failureCategoryCredentials       startFailureCategory = "credentials"
	failureCategoryTemplate          startFailureCategory = "template"
	failureCategoryNixPrefetch       startFailureCategory = "nix_prefetch"
	failureCategoryCommandBinary     startFailureCategory = "command_binary"
	failureCategoryServiceResolution startFailureCategory = "service_resolution"
	failureCategoryTokenMinting      startFailureCategory = "token_minting"
	failureCategoryLauncherIPC       startFailureCategory = "launcher_ipc"
	failureCategorySandboxCrash      startFailureCategory = "sandbox_crash"
	failureCategoryProxyCrash        startFailureCategory = "proxy_crash"
)

// startFailure records a failed sandbox creation attempt for exponential
// backoff. The daemon skips principals whose nextRetryAt is in the future
// during the "create missing" reconcile pass.
type startFailure struct {
	category    startFailureCategory
	message     string
	failedAt    time.Time
	attempts    int
	nextRetryAt time.Time
}

// startFailureBackoffBase is the initial backoff duration after the first
// failure. Subsequent failures double the backoff up to startFailureBackoffCap.
const startFailureBackoffBase = 1 * time.Second

// startFailureBackoffCap is the maximum backoff duration. Once reached,
// retries occur at this fixed interval until the failure is cleared by an
// event-driven signal (config change, service directory update, etc.).
const startFailureBackoffCap = 30 * time.Second

// recordStartFailure records or updates a start failure for the given
// principal with exponential backoff. On the first failure for a principal,
// the backoff is startFailureBackoffBase (1s). Each subsequent failure
// doubles the backoff up to startFailureBackoffCap (30s).
//
// Caller must hold reconcileMu.
func (d *Daemon) recordStartFailure(principal ref.Entity, category startFailureCategory, message string) {
	now := d.clock.Now()
	existing := d.startFailures[principal]
	attempts := 1
	if existing != nil {
		attempts = existing.attempts + 1
	}

	backoff := startFailureBackoffBase
	for range attempts - 1 {
		backoff *= 2
		if backoff > startFailureBackoffCap {
			backoff = startFailureBackoffCap
			break
		}
	}

	nextRetryAt := now.Add(backoff)
	d.startFailures[principal] = &startFailure{
		category:    category,
		message:     message,
		failedAt:    now,
		attempts:    attempts,
		nextRetryAt: nextRetryAt,
	}
	d.logger.Info("start failure recorded",
		"principal", principal,
		"category", category,
		"attempts", attempts,
		"backoff", backoff.String(),
		"next_retry_at", nextRetryAt.Format(time.RFC3339Nano),
		"message", message,
	)
}

// clearStartFailures removes all start failure entries, allowing immediate
// retry on the next reconcile cycle. Called when config room state changes
// (the config may have fixed the root cause) or when a sandbox exits
// unexpectedly (crash-restart should not be delayed by a previous failure).
//
// Caller must hold reconcileMu.
func (d *Daemon) clearStartFailures() {
	if count := len(d.startFailures); count > 0 {
		d.logger.Info("clearing all start failures", "count", count)
	}
	clear(d.startFailures)
}

// clearStartFailuresByCategory removes start failure entries matching the
// given category. Returns the number of entries cleared. Used for targeted
// clearing: e.g., a service directory sync clears only service resolution
// failures, not credential or template failures.
//
// Caller must hold reconcileMu.
func (d *Daemon) clearStartFailuresByCategory(category startFailureCategory) int {
	cleared := 0
	for principal, failure := range d.startFailures {
		if failure.category == category {
			delete(d.startFailures, principal)
			cleared++
		}
	}
	return cleared
}

// clearStartFailure removes the start failure entry for a single principal.
// Called when a principal starts successfully or when its sandbox exits
// (the exit watcher should not inherit stale backoff state).
//
// Caller must hold reconcileMu.
func (d *Daemon) clearStartFailure(principal ref.Entity) {
	delete(d.startFailures, principal)
}

// notifyReconcile sends a non-blocking signal on reconcileNotify.
// No-op when the channel is nil (production).
func (d *Daemon) notifyReconcile() {
	if d.reconcileNotify == nil {
		return
	}
	select {
	case d.reconcileNotify <- struct{}{}:
	default:
	}
}

// Deadband thresholds for adaptive heartbeat publishing. A heartbeat
// is suppressed when all metrics fall within these bounds of the last
// published values. Thresholds filter noise from kernel scheduling,
// filesystem cache activity, and GPU thermal fluctuation without
// masking operationally meaningful changes.
const (
	cpuDeadbandPercent            = 5
	memoryDeadbandMB              = 100
	gpuUtilizationDeadbandPercent = 5
	gpuTemperatureDeadbandMilli   = 2000     // 2°C
	gpuVRAMDeadbandBytes          = 52428800 // 50 MB
	principalCPUDeadbandPercent   = 3
	principalMemoryDeadbandMB     = 50
	defaultMaxIdleInterval        = 5 * time.Minute
)

// statusLoop collects MachineStatus metrics at a fixed interval and
// publishes to Matrix only when the data has meaningfully changed or
// the liveness window (maxIdleInterval) has elapsed. Sandbox state
// changes (via notifyStatusChange) always trigger an immediate publish.
//
// The collection interval is fixed to keep cgroup CPU delta computation
// correct (CgroupCPUPercent divides by the interval). The adaptive
// behavior is entirely in the publish decision: identical metrics are
// suppressed, saving the Matrix HTTP call, homeserver DAG insertion,
// and /sync delivery to all watchers.
func (d *Daemon) statusLoop(ctx context.Context) {
	// Always publish on startup — fleet controller needs to see us.
	status := d.gatherStatus(ctx)
	d.publishStatus(ctx, status)

	ticker := d.clock.NewTicker(d.statusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := d.gatherStatus(ctx)
			timeSincePublish := d.clock.Now().Sub(d.lastPublishTime)
			if statusSignificantlyChanged(&status, d.lastPublishedStatus) ||
				timeSincePublish >= d.maxIdleInterval {
				d.publishStatus(ctx, status)
			} else {
				d.logger.Debug("heartbeat unchanged, skipping publish",
					"since_last_publish", timeSincePublish.Round(time.Second))
			}
		case <-d.statusNotify:
			// Sandbox state changed — always publish immediately.
			status := d.gatherStatus(ctx)
			d.publishStatus(ctx, status)
			// Drain any queued notification so the next tick starts clean.
			select {
			case <-d.statusNotify:
			default:
			}
		}

		// Signal iteration complete (publish or skip). Non-blocking:
		// nil channel in production (send on nil never proceeds).
		select {
		case d.statusLoopIterationDone <- struct{}{}:
		default:
		}
	}
}

// gatherStatus returns the current machine status. If collectStatusFunc
// is set (test override), it calls that; otherwise it calls the real
// collectStatus which reads from /proc, cgroups, and GPU devices.
func (d *Daemon) gatherStatus(ctx context.Context) schema.MachineStatus {
	if d.collectStatusFunc != nil {
		return d.collectStatusFunc(ctx)
	}
	return d.collectStatus(ctx)
}

// notifyStatusChange signals that the running set has changed and a
// MachineStatus heartbeat should be published immediately. Non-blocking:
// if a notification is already pending, the signal is coalesced.
// Must be called after any mutation to d.running.
func (d *Daemon) notifyStatusChange() {
	select {
	case d.statusNotify <- struct{}{}:
	default:
	}
}

// collectStatus gathers all machine metrics and assembles a MachineStatus
// struct. Updates CPU delta baselines (previousCPU, previousCgroupCPU)
// on every call so that the next collection has an accurate baseline
// regardless of whether the result is published.
func (d *Daemon) collectStatus(ctx context.Context) schema.MachineStatus {
	d.reconcileMu.RLock()
	runningPrincipals := make([]ref.Entity, 0, len(d.running))
	for principal := range d.running {
		runningPrincipals = append(runningPrincipals, principal)
	}
	var lastActivity string
	if !d.lastActivityAt.IsZero() {
		lastActivity = d.lastActivityAt.UTC().Format(time.RFC3339)
	}
	d.reconcileMu.RUnlock()

	transportAddress := d.transportListener.Address()

	// Compute CPU utilization from the delta since the last collection.
	// The first collection after startup reports 0% (no baseline yet).
	currentCPU := hwinfo.ReadCPUStats()
	cpuUtilization := hwinfo.CPUPercent(d.previousCPU, currentCPU)
	d.previousCPU = currentCPU

	// Per-principal resource collection from cgroup v2.
	principals := d.collectPrincipalResources(runningPrincipals)

	return schema.MachineStatus{
		Principal: d.machine.UserID().String(),
		Sandboxes: schema.SandboxCounts{
			Running: len(runningPrincipals),
		},
		CPUPercent:       int(cpuUtilization),
		MemoryUsedMB:     hwinfo.MemoryUsedMB(),
		GPUStats:         d.collectGPUStats(),
		UptimeSeconds:    uptimeSeconds(),
		LastActivityAt:   lastActivity,
		TransportAddress: transportAddress,
		Principals:       principals,
	}
}

// publishStatus sends a MachineStatus state event to the machine room
// and updates presence. Records the published status and timestamp for
// subsequent change comparison.
func (d *Daemon) publishStatus(ctx context.Context, status schema.MachineStatus) {
	// Update presence status_msg with a compact operational summary.
	// This gives operators and the fleet controller a quick glance at
	// machine state via any Matrix client, without parsing state events.
	statusMsg := fmt.Sprintf("%d sandboxes, %d%% CPU", status.Sandboxes.Running, status.CPUPercent)
	if err := d.session.SetPresence(ctx, "online", statusMsg); err != nil {
		d.logger.Warn("updating presence status_msg", "error", err)
	}

	_, err := d.session.SendStateEvent(ctx, d.machineRoomID, schema.EventTypeMachineStatus, d.machine.Localpart(), status)
	if err != nil {
		d.logger.Error("publishing machine status", "error", err)
		return
	}

	d.lastPublishedStatus = &status
	d.lastPublishTime = d.clock.Now()
	d.logger.Debug("published machine status",
		"running_sandboxes", status.Sandboxes.Running)
}

// statusSignificantlyChanged reports whether current differs meaningfully
// from previous. Fields that always change (UptimeSeconds) are ignored.
// Fields with natural noise (CPU, memory, GPU metrics) use deadband
// thresholds to avoid publishing on minor fluctuations. Returns true
// when previous is nil (first comparison after startup).
func statusSignificantlyChanged(current, previous *schema.MachineStatus) bool {
	if previous == nil {
		return true
	}

	// Sandbox count — always meaningful.
	if current.Sandboxes != previous.Sandboxes {
		return true
	}

	// Transport address — topology change.
	if current.TransportAddress != previous.TransportAddress {
		return true
	}

	// Activity timestamp — something happened.
	if current.LastActivityAt != previous.LastActivityAt {
		return true
	}

	// Machine-wide CPU with deadband.
	if intAbs(current.CPUPercent-previous.CPUPercent) > cpuDeadbandPercent {
		return true
	}

	// Machine-wide memory with deadband.
	if intAbs(current.MemoryUsedMB-previous.MemoryUsedMB) > memoryDeadbandMB {
		return true
	}

	// GPU stats.
	if gpuStatsChanged(current.GPUStats, previous.GPUStats) {
		return true
	}

	// Per-principal resource usage.
	if principalsChanged(current.Principals, previous.Principals) {
		return true
	}

	return false
}

// gpuStatsChanged compares two GPU status slices using deadband
// thresholds. GPUs are matched by PCISlot. Different slice lengths or
// a missing PCISlot match means a GPU was added or removed.
func gpuStatsChanged(current, previous []schema.GPUStatus) bool {
	if len(current) != len(previous) {
		return true
	}

	// Build a lookup from previous by PCISlot for O(n) matching.
	previousBySlot := make(map[string]schema.GPUStatus, len(previous))
	for _, gpu := range previous {
		previousBySlot[gpu.PCISlot] = gpu
	}

	for _, currentGPU := range current {
		previousGPU, exists := previousBySlot[currentGPU.PCISlot]
		if !exists {
			return true
		}
		if intAbs(currentGPU.UtilizationPercent-previousGPU.UtilizationPercent) > gpuUtilizationDeadbandPercent {
			return true
		}
		if int64Abs(currentGPU.VRAMUsedBytes-previousGPU.VRAMUsedBytes) > gpuVRAMDeadbandBytes {
			return true
		}
		if intAbs(currentGPU.TemperatureMillidegrees-previousGPU.TemperatureMillidegrees) > gpuTemperatureDeadbandMilli {
			return true
		}
	}
	return false
}

// principalsChanged compares two principal resource maps using deadband
// thresholds. Different key sets (principal added/removed) or a status
// transition (starting→running→idle) are always meaningful. CPU and
// memory use per-principal deadbands.
func principalsChanged(current, previous map[string]schema.PrincipalResourceUsage) bool {
	if len(current) != len(previous) {
		return true
	}

	for name, currentUsage := range current {
		previousUsage, exists := previous[name]
		if !exists {
			return true
		}
		// Status transitions are always meaningful.
		if currentUsage.Status != previousUsage.Status {
			return true
		}
		if intAbs(currentUsage.CPUPercent-previousUsage.CPUPercent) > principalCPUDeadbandPercent {
			return true
		}
		if intAbs(currentUsage.MemoryMB-previousUsage.MemoryMB) > principalMemoryDeadbandMB {
			return true
		}
	}
	return false
}

// intAbs returns the absolute value of x.
func intAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// int64Abs returns the absolute value of x.
func int64Abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// collectPrincipalResources reads cgroup v2 statistics for each running
// principal and returns a map of resource usage. Called by collectStatus
// in the status loop goroutine. The previousCgroupCPU map is accessed
// without locking because collectStatus is the sole accessor (same
// single-goroutine pattern as previousCPU).
func (d *Daemon) collectPrincipalResources(principals []ref.Entity) map[string]schema.PrincipalResourceUsage {
	if len(principals) == 0 {
		return nil
	}

	intervalMicroseconds := uint64(d.statusInterval / time.Microsecond)

	result := make(map[string]schema.PrincipalResourceUsage, len(principals))
	runningSet := make(map[ref.Entity]bool, len(principals))

	for _, principal := range principals {
		runningSet[principal] = true
		cgroupPath := d.cgroupPathFunc(principal)

		// CPU: read usage_usec and compute delta from previous reading.
		currentCPU := hwinfo.ReadCgroupCPUStats(cgroupPath)
		previousCPU := d.previousCgroupCPU[principal]
		cpuPercent := hwinfo.CgroupCPUPercent(previousCPU, currentCPU, intervalMicroseconds)
		if currentCPU != nil {
			d.previousCgroupCPU[principal] = currentCPU
		}

		// Memory: read current usage.
		memoryBytes := hwinfo.ReadCgroupMemoryBytes(cgroupPath)
		memoryMB := int(memoryBytes / (1024 * 1024))

		status := hwinfo.DerivePrincipalStatus(cpuPercent, previousCPU != nil)

		// The status event uses account localparts as keys (matching the
		// MachineStatus.Principals wire format).
		result[principal.AccountLocalpart()] = schema.PrincipalResourceUsage{
			CPUPercent: cpuPercent,
			MemoryMB:   memoryMB,
			Status:     status,
		}
	}

	// Clean up stale readings for principals that are no longer running.
	for principal := range d.previousCgroupCPU {
		if !runningSet[principal] {
			delete(d.previousCgroupCPU, principal)
		}
	}

	return result
}

// publishMachineInfo probes system hardware and publishes the static
// inventory as an m.bureau.machine_info state event. Called once at
// startup. The state event is idempotent — if the hardware inventory
// hasn't changed since the last daemon run, the homeserver deduplicates
// the event (Matrix state events with identical content are no-ops).
func (d *Daemon) publishMachineInfo(ctx context.Context) {
	amdProber := amdgpu.NewProber()
	nvidiaProber := nvidia.NewProber()
	info := hwinfo.Probe(d.machine.UserID().String(), amdProber, nvidiaProber)
	info.DaemonVersion = version.Info()

	_, err := d.session.SendStateEvent(ctx, d.machineRoomID, schema.EventTypeMachineInfo, d.machine.Localpart(), info)
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

// publishTokenSigningKey publishes the daemon's Ed25519 token signing
// public key to #bureau/system as an m.bureau.token_signing_key state
// event. The state key is the machine localpart (e.g., "bureau/fleet/prod/machine/workstation").
//
// The publish is conditional to avoid unnecessary state events on every
// daemon restart. It always publishes when the key was freshly generated
// (first boot or key file deleted). For existing keys, it fetches the
// current state event and compares the stored public key — publishing
// only if the content differs (e.g., after manual key rotation where
// someone replaced the key files).
func (d *Daemon) publishTokenSigningKey(ctx context.Context, keyWasGenerated bool) {
	publicKeyHex := hex.EncodeToString(d.tokenSigningPublicKey)

	if !keyWasGenerated {
		// Check whether the current state event already has our key.
		existing, err := d.session.GetStateEvent(ctx, d.systemRoomID,
			schema.EventTypeTokenSigningKey, d.machine.UserID().String())
		if err == nil {
			var content schema.TokenSigningKeyContent
			if parseErr := json.Unmarshal(existing, &content); parseErr == nil {
				if content.PublicKey == publicKeyHex {
					d.logger.Info("token signing key already published, skipping")
					return
				}
			}
		} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			d.logger.Error("checking existing token signing key", "error", err)
			// Fall through to publish — better to send a redundant state
			// event than to silently skip on a transient error.
		}
	}

	content := schema.TokenSigningKeyContent{
		PublicKey: publicKeyHex,
		Machine:   d.machine.UserID(),
	}
	_, err := d.session.SendStateEvent(ctx, d.systemRoomID,
		schema.EventTypeTokenSigningKey, d.machine.UserID().String(), content)
	if err != nil {
		d.logger.Error("publishing token signing key", "error", err)
		return
	}
	d.logger.Info("published token signing key",
		"room_id", d.systemRoomID,
		"machine", d.machine.Localpart(),
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

// resolveConfigRoom resolves the per-machine config room alias and joins it.
// The config room must already exist (created by "bureau machine provision").
// The daemon does not create config rooms — that is the admin's responsibility
// during provisioning.
func resolveConfigRoom(ctx context.Context, session *messaging.DirectSession, alias ref.RoomAlias, logger *slog.Logger) (ref.RoomID, error) {
	roomID, err := session.ResolveAlias(ctx, alias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return ref.RoomID{}, fmt.Errorf("config room %q not found — run 'bureau machine provision' first", alias)
		}
		return ref.RoomID{}, fmt.Errorf("resolving config room alias %q: %w", alias, err)
	}

	if _, err := session.JoinRoom(ctx, roomID); err != nil {
		logger.Warn("join config room returned error (may already be joined)", "error", err)
	}
	return roomID, nil
}
