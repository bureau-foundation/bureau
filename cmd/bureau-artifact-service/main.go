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

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	artifactfuse "github.com/bureau-foundation/bureau/lib/artifactstore/fuse"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
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
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")

	// Service-specific flags.
	var (
		storeDir           string
		cacheDir           string
		cacheSize          int64
		mountpoint         string
		upstreamSocket     string
		encryptionKeyStdin bool
	)
	flag.StringVar(&storeDir, "store-dir", "", "artifact store root directory (required)")
	flag.StringVar(&cacheDir, "cache-dir", "", "local cache directory (optional, enables ring cache)")
	flag.Int64Var(&cacheSize, "cache-size", 0, "cache device size in bytes (required if --cache-dir is set)")
	flag.StringVar(&mountpoint, "mountpoint", "", "FUSE mount directory for read-only artifact access (optional)")
	flag.StringVar(&upstreamSocket, "upstream-socket", "", "Unix socket path for upstream shared cache (optional)")
	flag.BoolVar(&encryptionKeyStdin, "encryption-key-stdin", false, "read 32-byte artifact encryption key from stdin")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-artifact-service %s\n", version.Info())
		return nil
	}

	if storeDir == "" {
		return fmt.Errorf("--store-dir is required")
	}

	// Create the logger early — needed for stdin read logging
	// before Bootstrap.
	logger := service.NewLogger()

	// Read the artifact encryption key from stdin if requested.
	// This must happen before the signal context is created, so the
	// key is available immediately. The key is held in guarded
	// memory (mmap-backed, mlock'd, excluded from core dumps,
	// zeroed on close).
	var encryptionKeys *artifactstore.EncryptionKeySet
	if encryptionKeyStdin {
		keyBuffer, err := secret.NewFromReader(os.Stdin, artifactstore.KeySize)
		if err != nil {
			return fmt.Errorf("reading encryption key from stdin: %w", err)
		}
		os.Stdin.Close()

		var keySetErr error
		encryptionKeys, keySetErr = artifactstore.NewEncryptionKeySet(keyBuffer)
		if keySetErr != nil {
			keyBuffer.Close()
			return fmt.Errorf("initializing encryption key set: %w", keySetErr)
		}
		defer encryptionKeys.Close()
		logger.Info("artifact encryption key loaded from stdin")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "artifact",
		Description:  "Content-addressable artifact storage service",
		Capabilities: []string{"content-addressed-store"},
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Initialize the artifact store.
	store, err := artifactstore.NewStore(storeDir)
	if err != nil {
		return fmt.Errorf("creating artifact store: %w", err)
	}

	// Initialize per-artifact metadata persistence.
	metadataStore, err := artifactstore.NewMetadataStore(filepath.Join(storeDir, "metadata"))
	if err != nil {
		return fmt.Errorf("creating metadata store: %w", err)
	}

	// Build the ref-to-hash index from existing metadata files.
	refIndex := artifactstore.NewRefIndex()
	refMap, err := metadataStore.ScanRefs()
	if err != nil {
		return fmt.Errorf("scanning metadata for ref index: %w", err)
	}
	refIndex.Build(refMap)
	logger.Info("ref index built", "artifacts", refIndex.Len())

	// Initialize persistent tag storage.
	tagStore, err := artifactstore.NewTagStore(filepath.Join(storeDir, "tags"))
	if err != nil {
		return fmt.Errorf("creating tag store: %w", err)
	}
	logger.Info("tag store loaded", "tags", tagStore.Len())

	// Build the in-memory artifact index for filtered queries.
	artifactIndex := artifactstore.NewArtifactIndex()
	allMetadata, err := metadataStore.ScanAll()
	if err != nil {
		return fmt.Errorf("scanning metadata for artifact index: %w", err)
	}
	artifactIndex.Build(allMetadata)
	logger.Info("artifact index built", "artifacts", artifactIndex.Len())

	// Optionally initialize the ring cache for container-level caching.
	var cache *artifactstore.Cache
	if cacheDir != "" {
		if cacheSize <= 0 {
			return fmt.Errorf("--cache-size is required when --cache-dir is set")
		}
		var cacheErr error
		cache, cacheErr = artifactstore.NewCache(artifactstore.CacheConfig{
			Path:       cacheDir,
			DeviceSize: cacheSize,
		})
		if cacheErr != nil {
			return fmt.Errorf("creating cache: %w", cacheErr)
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
		authConfig:     boot.AuthConfig,
		encryptionKeys: encryptionKeys,
		session:        boot.Session,
		clock:          clk,
		principalName:  boot.PrincipalName,
		machineName:    boot.MachineName,
		serverName:     boot.ServerName,
		serviceRoomID:  boot.ServiceRoomID,
		upstreamSocket: upstreamSocket,
		startedAt:      clk.Now(),
		pushTargets:    make(map[string]servicetoken.PushTarget),
		rooms:          make(map[ref.RoomID]*artifactRoomState),
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

	// Perform initial /sync to discover rooms with artifact scope.
	sinceToken, err := artifactService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket listener in a goroutine.
	socketDone := make(chan error, 1)
	go func() {
		socketDone <- artifactService.serve(ctx, boot.SocketPath)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, artifactService.handleSync, clk, logger)

	logger.Info("artifact service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
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
	store          *artifactstore.Store
	metadataStore  *artifactstore.MetadataStore
	refIndex       *artifactstore.RefIndex
	tagStore       *artifactstore.TagStore
	artifactIndex  *artifactstore.ArtifactIndex
	cache          *artifactstore.Cache            // nil if no cache directory configured
	authConfig     *service.AuthConfig             // nil in tests that don't exercise auth
	encryptionKeys *artifactstore.EncryptionKeySet // nil if no encryption key provided (local-only mode)
	session        messaging.Session
	clock          clock.Clock

	principalName string
	machineName   string
	serverName    ref.ServerName
	serviceRoomID ref.RoomID
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
	rooms map[ref.RoomID]*artifactRoomState

	logger *slog.Logger
}
