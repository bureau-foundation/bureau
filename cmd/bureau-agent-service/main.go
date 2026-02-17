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
	"sync"
	"syscall"
	"time"

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
		homeserverURL string
		machineName   string
		principalName string
		serverName    string
		runDir        string
		stateDir      string
		showVersion   bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&principalName, "principal-name", "", "service principal localpart (e.g., service/agent/iree) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&stateDir, "state-dir", "", "directory containing session.json (required)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-agent-service %s\n", version.Info())
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
	_, session, err := service.LoadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
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

	// Resolve and join the machine config room. Agent state events
	// live here as state events keyed by principal localpart.
	configRoomAlias := principal.RoomAlias("bureau/config/"+machineName, serverName)
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving config room alias %q: %w", configRoomAlias, err)
	}
	if _, err := session.JoinRoom(ctx, configRoomID); err != nil {
		return fmt.Errorf("joining config room %s: %w", configRoomID, err)
	}

	logger.Info("rooms ready",
		"service_room", serviceRoomID,
		"system_room", systemRoomID,
		"config_room", configRoomID,
	)

	// Load the daemon's token signing public key for authenticating
	// incoming service requests.
	signingKey, err := service.LoadTokenSigningKey(ctx, session, systemRoomID, machineName)
	if err != nil {
		return fmt.Errorf("loading token signing key: %w", err)
	}
	logger.Info("token signing key loaded", "machine", machineName)

	clk := clock.Real()

	authConfig := &service.AuthConfig{
		PublicKey: signingKey,
		Audience:  "agent",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     clk,
	}

	agentService := &AgentService{
		session:       session,
		clock:         clk,
		principalName: principalName,
		machineName:   machineName,
		serverName:    serverName,
		configRoomID:  configRoomID,
		startedAt:     clk.Now(),
		logger:        logger,
	}

	// Register in #bureau/service so daemons can discover us.
	machineUserID := principal.MatrixUserID(machineName, serverName)
	if err := service.Register(ctx, session, serviceRoomID, principalName, serverName, service.Registration{
		Machine:      machineUserID,
		Protocol:     "cbor",
		Description:  "Agent lifecycle, session, context, and metrics service",
		Capabilities: []string{"session", "context", "metrics"},
	}); err != nil {
		return fmt.Errorf("registering service: %w", err)
	}
	logger.Info("service registered",
		"principal", principalName,
		"machine", machineUserID,
	)

	// Deregister on shutdown.
	defer func() {
		deregisterContext, deregisterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregisterCancel()
		if err := service.Deregister(deregisterContext, session, serviceRoomID, principalName); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
	}()

	// Perform initial /sync to populate agent state from config room.
	sinceToken, err := agentService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server.
	socketPath := principal.RunDirSocketPath(runDir, principalName)
	socketServer := service.NewSocketServer(socketPath, logger, authConfig)
	socketServer.RegisterRevocationHandler()
	agentService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop.
	go service.RunSyncLoop(ctx, session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, agentService.handleSync, clk, logger)

	logger.Info("agent service running",
		"principal", principalName,
		"socket", socketPath,
		"config_room", configRoomID,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutting down")

	// Wait for the socket server to drain active connections.
	if err := <-socketDone; err != nil {
		logger.Error("socket server error", "error", err)
	}

	return nil
}

// AgentService is the core service state.
//
// Concurrent access from socket handlers and the sync loop is
// serialized by mutex. Read-only handlers hold a read lock; mutation
// handlers and the sync loop hold a write lock.
type AgentService struct {
	mutex sync.RWMutex

	session *messaging.Session
	clock   clock.Clock

	principalName string
	machineName   string
	serverName    string
	configRoomID  string
	startedAt     time.Time

	logger *slog.Logger
}
