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
	"github.com/bureau-foundation/bureau/lib/schema"
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
		fleetPrefix   string
		runDir        string
		stateDir      string
		showVersion   bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&principalName, "principal-name", "", "service principal localpart (e.g., service/fleet/prod) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&fleetPrefix, "fleet", "", "fleet prefix (e.g., bureau/fleet/prod) (required)")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&stateDir, "state-dir", "", "directory containing session.json (required)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-fleet-controller %s\n", version.Info())
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

	if fleetPrefix == "" {
		return fmt.Errorf("--fleet is required")
	}
	fleetNamespace, fleetName, err := principal.ParseFleetPrefix(fleetPrefix)
	if err != nil {
		return fmt.Errorf("invalid fleet prefix: %w", err)
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

	// Resolve and join the system room (global).
	systemRoomID, err := service.ResolveSystemRoom(ctx, session, serverName)
	if err != nil {
		return fmt.Errorf("resolving system room: %w", err)
	}

	// Resolve and join fleet-scoped rooms. Each fleet has its own config,
	// machine, and service rooms derived from the fleet prefix.
	fleetAlias := principal.RoomAlias(schema.FleetRoomAlias(fleetNamespace, fleetName), serverName)
	fleetRoomID, err := session.ResolveAlias(ctx, fleetAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet room alias %q: %w", fleetAlias, err)
	}
	if _, err := session.JoinRoom(ctx, fleetRoomID); err != nil {
		return fmt.Errorf("joining fleet room %s: %w", fleetRoomID, err)
	}

	machineAlias := principal.RoomAlias(schema.FleetMachineRoomAlias(fleetNamespace, fleetName), serverName)
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet machine room alias %q: %w", machineAlias, err)
	}
	if _, err := session.JoinRoom(ctx, machineRoomID); err != nil {
		return fmt.Errorf("joining fleet machine room %s: %w", machineRoomID, err)
	}

	serviceAlias := principal.RoomAlias(schema.FleetServiceRoomAlias(fleetNamespace, fleetName), serverName)
	serviceRoomID, err := session.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet service room alias %q: %w", serviceAlias, err)
	}
	if _, err := session.JoinRoom(ctx, serviceRoomID); err != nil {
		return fmt.Errorf("joining fleet service room %s: %w", serviceRoomID, err)
	}

	logger.Info("fleet rooms ready",
		"fleet", fleetPrefix,
		"fleet_room", fleetRoomID,
		"system_room", systemRoomID,
		"machine_room", machineRoomID,
		"service_room", serviceRoomID,
	)

	// Load the daemon's token signing public key for authenticating
	// incoming service requests. The daemon publishes this key as a
	// state event in #bureau/system at startup.
	signingKey, err := service.LoadTokenSigningKey(ctx, session, systemRoomID, machineName)
	if err != nil {
		return fmt.Errorf("loading token signing key: %w", err)
	}
	logger.Info("token signing key loaded", "machine", machineName)

	clk := clock.Real()

	authConfig := &service.AuthConfig{
		PublicKey: signingKey,
		Audience:  "fleet",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     clk,
	}

	fleet := &FleetController{
		session:       session,
		configStore:   session,
		clock:         clk,
		principalName: principalName,
		machineName:   machineName,
		serverName:    serverName,
		runDir:        runDir,
		serviceRoomID: serviceRoomID,
		startedAt:     clk.Now(),
		machines:      make(map[string]*machineState),
		services:      make(map[string]*fleetServiceState),
		definitions:   make(map[string]*schema.MachineDefinitionContent),
		config:        make(map[string]*schema.FleetConfigContent),
		leases:        make(map[string]*schema.HALeaseContent),
		configRooms:   make(map[string]string),
		fleetRoomID:   fleetRoomID,
		machineRoomID: machineRoomID,
		logger:        logger,
	}

	// Register in the fleet's service room so daemons can discover us.
	machineUserID := principal.MatrixUserID(machineName, serverName)
	if err := service.Register(ctx, session, serviceRoomID, principalName, serverName, service.Registration{
		Machine:      machineUserID,
		Protocol:     "cbor",
		Description:  "Fleet controller for service placement and machine lifecycle",
		Capabilities: []string{"placement", "scaling", "failover"},
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
		deregisterContext, deregisterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregisterCancel()
		if err := service.Deregister(deregisterContext, session, serviceRoomID, principalName); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
	}()

	// Perform initial /sync to build the fleet model.
	sinceToken, err := fleet.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server in a goroutine.
	socketPath := principal.RunDirSocketPath(runDir, principalName)
	socketServer := service.NewSocketServer(socketPath, logger, authConfig)
	socketServer.RegisterRevocationHandler()
	fleet.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, fleet.handleSync, clk, logger)

	logger.Info("fleet controller running",
		"principal", principalName,
		"socket", socketPath,
		"machines", len(fleet.machines),
		"services", len(fleet.services),
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

// FleetController is the core service state for fleet management.
type FleetController struct {
	mu sync.Mutex

	session     *messaging.DirectSession
	configStore configStore
	clock       clock.Clock

	principalName string
	machineName   string
	serverName    string
	runDir        string
	serviceRoomID string
	startedAt     time.Time

	// In-memory fleet model, rebuilt from /sync.
	machines    map[string]*machineState
	services    map[string]*fleetServiceState
	definitions map[string]*schema.MachineDefinitionContent
	config      map[string]*schema.FleetConfigContent
	leases      map[string]*schema.HALeaseContent

	// configRooms maps machine localparts to their config room IDs.
	// Populated from room aliases during initial sync.
	configRooms map[string]string

	// fleetRoomID is the fleet config room, resolved from the fleet prefix.
	fleetRoomID string

	// machineRoomID is the fleet-scoped machine room, resolved from the fleet prefix.
	machineRoomID string

	logger *slog.Logger
}
