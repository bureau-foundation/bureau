// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifact"
	artifactfuse "github.com/bureau-foundation/bureau/lib/artifact/fuse"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		homeserverURL  string
		machineName    string
		principalName  string
		serverName     string
		runDir         string
		stateDir       string
		storeDir       string
		cacheDir       string
		cacheSize      int64
		mountpoint     string
		upstreamSocket string
		showVersion    bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&principalName, "principal-name", "", "service principal localpart (e.g., service/artifact/main) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&stateDir, "state-dir", "", "directory containing session.json (required)")
	flag.StringVar(&storeDir, "store-dir", "", "artifact store root directory (required)")
	flag.StringVar(&cacheDir, "cache-dir", "", "local cache directory (optional, enables ring cache)")
	flag.Int64Var(&cacheSize, "cache-size", 0, "cache device size in bytes (required if --cache-dir is set)")
	flag.StringVar(&mountpoint, "mountpoint", "", "FUSE mount directory for read-only artifact access (optional)")
	flag.StringVar(&upstreamSocket, "upstream-socket", "", "Unix socket path for upstream shared cache (optional)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-artifact-service %s\n", version.Info())
		return nil
	}

	if machineName == "" {
		return fmt.Errorf("--machine-name is required")
	}
	if err := principal.ValidateLocalpart(machineName); err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}

	if principalName == "" {
		return fmt.Errorf("--principal-name is required")
	}
	if err := principal.ValidateLocalpart(principalName); err != nil {
		return fmt.Errorf("invalid principal name: %w", err)
	}

	if stateDir == "" {
		return fmt.Errorf("--state-dir is required")
	}

	if storeDir == "" {
		return fmt.Errorf("--store-dir is required")
	}

	if err := principal.ValidateRunDir(runDir); err != nil {
		return fmt.Errorf("run directory validation: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load and validate the Matrix session.
	client, session, err := service.LoadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	_ = client
	defer session.Close()

	userID, err := service.ValidateSession(ctx, session)
	if err != nil {
		return err
	}
	logger.Info("matrix session valid", "user_id", userID)

	// Resolve and join global rooms the service needs.
	serviceRoomID, err := service.ResolveServiceRoom(ctx, session, serverName)
	if err != nil {
		return fmt.Errorf("resolving service room: %w", err)
	}

	systemRoomID, err := service.ResolveSystemRoom(ctx, session, serverName)
	if err != nil {
		return fmt.Errorf("resolving system room: %w", err)
	}
	logger.Info("global rooms ready",
		"service_room", serviceRoomID,
		"system_room", systemRoomID,
	)

	// Load the daemon's token signing public key for authenticating
	// incoming service requests. The daemon publishes this key as a
	// state event in #bureau/system at startup.
	signingKey, err := service.LoadTokenSigningKey(ctx, session, systemRoomID, machineName)
	if err != nil {
		return fmt.Errorf("loading token signing key: %w", err)
	}
	logger.Info("token signing key loaded", "machine", machineName)

	authConfig := &service.AuthConfig{
		PublicKey: signingKey,
		Audience:  "artifact",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     clock.Real(),
	}

	// Initialize the artifact store.
	store, err := artifact.NewStore(storeDir)
	if err != nil {
		return fmt.Errorf("creating artifact store: %w", err)
	}

	// Initialize per-artifact metadata persistence.
	metadataStore, err := artifact.NewMetadataStore(filepath.Join(storeDir, "metadata"))
	if err != nil {
		return fmt.Errorf("creating metadata store: %w", err)
	}

	// Build the ref-to-hash index from existing metadata files.
	refIndex := artifact.NewRefIndex()
	refMap, err := metadataStore.ScanRefs()
	if err != nil {
		return fmt.Errorf("scanning metadata for ref index: %w", err)
	}
	refIndex.Build(refMap)
	logger.Info("ref index built", "artifacts", refIndex.Len())

	// Initialize persistent tag storage.
	tagStore, err := artifact.NewTagStore(filepath.Join(storeDir, "tags"))
	if err != nil {
		return fmt.Errorf("creating tag store: %w", err)
	}
	logger.Info("tag store loaded", "tags", tagStore.Len())

	// Build the in-memory artifact index for filtered queries.
	artifactIndex := artifact.NewArtifactIndex()
	allMetadata, err := metadataStore.ScanAll()
	if err != nil {
		return fmt.Errorf("scanning metadata for artifact index: %w", err)
	}
	artifactIndex.Build(allMetadata)
	logger.Info("artifact index built", "artifacts", artifactIndex.Len())

	// Optionally initialize the ring cache for container-level caching.
	var cache *artifact.Cache
	if cacheDir != "" {
		if cacheSize <= 0 {
			return fmt.Errorf("--cache-size is required when --cache-dir is set")
		}
		var err error
		cache, err = artifact.NewCache(artifact.CacheConfig{
			Path:       cacheDir,
			DeviceSize: cacheSize,
		})
		if err != nil {
			return fmt.Errorf("creating cache: %w", err)
		}
		defer cache.Close()
		logger.Info("cache initialized",
			"path", cacheDir,
			"device_size", cacheSize,
		)
	}

	clk := clock.Real()

	artifactService := &ArtifactService{
		store:          store,
		metadataStore:  metadataStore,
		refIndex:       refIndex,
		tagStore:       tagStore,
		artifactIndex:  artifactIndex,
		cache:          cache,
		authConfig:     authConfig,
		session:        session,
		clock:          clk,
		principalName:  principalName,
		machineName:    machineName,
		serverName:     serverName,
		runDir:         runDir,
		serviceRoomID:  serviceRoomID,
		upstreamSocket: upstreamSocket,
		startedAt:      clk.Now(),
		pushTargets:    make(map[string]servicetoken.PushTarget),
		rooms:          make(map[string]*artifactRoomState),
		logger:         logger,
	}

	// Optionally mount the FUSE filesystem for artifact access. Mounted
	// after ArtifactService creation so the FUSE write path can share
	// the service's write mutex (the Store is not safe for concurrent
	// writes). LIFO defer order ensures FUSE is unmounted before the
	// service deregisters, preventing writes to a half-torn-down store.
	if mountpoint != "" {
		fuseServer, err := artifactfuse.Mount(artifactfuse.Options{
			Mountpoint: mountpoint,
			Store:      store,
			Cache:      cache,
			TagStore:   tagStore,
			Clock:      clk,
			WriteMu:    &artifactService.writeMu,
			AllowOther: true,
			Logger:     logger,
		})
		if err != nil {
			return fmt.Errorf("mounting FUSE filesystem: %w", err)
		}
		defer func() {
			if err := fuseServer.Unmount(); err != nil {
				logger.Error("failed to unmount FUSE filesystem", "error", err)
			} else {
				logger.Info("FUSE filesystem unmounted", "mountpoint", mountpoint)
			}
		}()
	}

	// Register in #bureau/service so daemons can discover us.
	machineUserID := principal.MatrixUserID(machineName, serverName)
	if err := service.Register(ctx, session, serviceRoomID, principalName, serverName, service.Registration{
		Machine:      machineUserID,
		Protocol:     "cbor",
		Description:  "Content-addressable artifact storage service",
		Capabilities: []string{"content-addressed-store"},
	}); err != nil {
		return fmt.Errorf("registering service: %w", err)
	}
	logger.Info("service registered",
		"principal", principalName,
		"machine", machineUserID,
	)

	// Deregister on shutdown. Use a background context since the main
	// context may already be cancelled.
	defer func() {
		deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregCancel()
		if err := service.Deregister(deregCtx, session, serviceRoomID, principalName); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
	}()

	// Perform initial /sync to discover rooms with artifact scope.
	sinceToken, err := artifactService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket listener in a goroutine.
	socketPath := principal.RunDirSocketPath(runDir, principalName)
	socketDone := make(chan error, 1)
	go func() {
		socketDone <- artifactService.serve(ctx, socketPath)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, artifactService.handleSync, clk, logger)

	logger.Info("artifact service running",
		"principal", principalName,
		"socket", socketPath,
		"artifacts", refIndex.Len(),
		"rooms", len(artifactService.rooms),
		"upstream", upstreamSocket,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutting down")

	// Wait for the socket listener to drain active connections.
	if err := <-socketDone; err != nil {
		logger.Error("socket listener error", "error", err)
	}

	return nil
}

// ArtifactService is the core service state.
type ArtifactService struct {
	store         *artifact.Store
	metadataStore *artifact.MetadataStore
	refIndex      *artifact.RefIndex
	tagStore      *artifact.TagStore
	artifactIndex *artifact.ArtifactIndex
	cache         *artifact.Cache     // nil if no cache directory configured
	authConfig    *service.AuthConfig // nil in tests that don't exercise auth
	session       *messaging.Session
	clock         clock.Clock

	principalName string
	machineName   string
	serverName    string
	runDir        string
	serviceRoomID string
	startedAt     time.Time

	// upstreamSocket is the Unix socket path for the shared cache
	// service. Empty means no upstream — all fetches are local only.
	// Set at startup via --upstream-socket or at runtime via the
	// set-upstream action (daemon-signed reconfiguration).
	upstreamSocket string

	// upstreamToken is a daemon-minted service token included in
	// requests to the upstream shared cache. For local upstream
	// connections (same machine), the daemon mints this token with
	// its signing key. For remote upstream connections (via tunnel),
	// this is nil — the tunnel handler on the remote machine injects
	// a token signed by that machine's daemon.
	upstreamToken []byte

	// upstreamMu protects reads and writes to upstreamSocket and
	// upstreamToken. Handlers read-lock when checking the upstream;
	// the set-upstream action write-locks when changing both fields.
	upstreamMu sync.RWMutex

	// pushTargets maps machine localparts to their push target
	// configuration (socket path + optional token). Updated by the
	// daemon via the set-push-targets action.
	pushTargets map[string]servicetoken.PushTarget

	// pushTargetsMu protects reads and writes to pushTargets.
	// Handlers read-lock when resolving push targets during store;
	// the set-push-targets action write-locks when replacing them.
	pushTargetsMu sync.RWMutex

	// writeMu serializes all write operations (Store.Write +
	// MetadataStore.Write + RefIndex.Add as an atomic unit). The
	// Store is safe for concurrent reads but not concurrent writes.
	writeMu sync.Mutex

	// rooms maps room IDs to per-room artifact scope configuration.
	// Only rooms with m.bureau.artifact_scope are tracked.
	rooms map[string]*artifactRoomState

	logger *slog.Logger
}
