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
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/service"
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
		homeserverURL string
		machineName   string
		principalName string
		serverName    string
		runDir        string
		stateDir      string
		storeDir      string
		showVersion   bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&principalName, "principal-name", "", "service principal localpart (e.g., service/artifact/main) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&stateDir, "state-dir", "", "directory containing session.json (required)")
	flag.StringVar(&storeDir, "store-dir", "", "artifact store root directory (required)")
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

	// Resolve and join the service directory room.
	serviceRoomID, err := service.ResolveServiceRoom(ctx, session, serverName)
	if err != nil {
		return fmt.Errorf("resolving service room: %w", err)
	}
	logger.Info("service room ready", "room_id", serviceRoomID)

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

	clk := clock.Real()

	artifactService := &ArtifactService{
		store:         store,
		metadataStore: metadataStore,
		refIndex:      refIndex,
		session:       session,
		clock:         clk,
		principalName: principalName,
		machineName:   machineName,
		serverName:    serverName,
		runDir:        runDir,
		serviceRoomID: serviceRoomID,
		startedAt:     clk.Now(),
		rooms:         make(map[string]*artifactRoomState),
		logger:        logger,
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
	session       *messaging.Session
	clock         clock.Clock

	principalName string
	machineName   string
	serverName    string
	runDir        string
	serviceRoomID string
	startedAt     time.Time

	// writeMu serializes all write operations (Store.Write +
	// MetadataStore.Write + RefIndex.Add as an atomic unit). The
	// Store is safe for concurrent reads but not concurrent writes.
	writeMu sync.Mutex

	// rooms maps room IDs to per-room artifact scope configuration.
	// Only rooms with m.bureau.artifact_scope are tracked.
	rooms map[string]*artifactRoomState

	logger *slog.Logger
}
