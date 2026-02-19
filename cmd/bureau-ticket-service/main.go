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
	"github.com/bureau-foundation/bureau/lib/ref"
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
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., bureau/fleet/prod/machine/workstation) (required)")
	flag.StringVar(&principalName, "principal-name", "", "service principal localpart (e.g., service/ticket/iree) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&fleetPrefix, "fleet", "", "fleet prefix (e.g., bureau/fleet/prod) (required)")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets")
	flag.StringVar(&stateDir, "state-dir", "", "directory containing session.json (required)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-ticket-service %s\n", version.Info())
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

	// Construct typed identity refs from the string flags.
	fleet, err := ref.ParseFleet(fleetPrefix, serverName)
	if err != nil {
		return fmt.Errorf("invalid fleet: %w", err)
	}
	_, bareMachineName, err := ref.ExtractEntityName(machineName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}
	machineRef, err := ref.NewMachine(fleet, bareMachineName)
	if err != nil {
		return fmt.Errorf("invalid machine ref: %w", err)
	}
	_, bareServiceName, err := ref.ExtractEntityName(principalName)
	if err != nil {
		return fmt.Errorf("invalid principal name: %w", err)
	}
	serviceRef, err := ref.NewService(fleet, bareServiceName)
	if err != nil {
		return fmt.Errorf("invalid service ref: %w", err)
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

	// Resolve and join fleet-scoped rooms the service needs.
	serviceRoomID, err := service.ResolveFleetServiceRoom(ctx, session, fleet)
	if err != nil {
		return fmt.Errorf("resolving fleet service room: %w", err)
	}

	namespace := fleet.Namespace()
	systemRoomID, err := service.ResolveSystemRoom(ctx, session, namespace)
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
	signingKey, err := service.LoadTokenSigningKey(ctx, session, systemRoomID, machineRef)
	if err != nil {
		return fmt.Errorf("loading token signing key: %w", err)
	}
	logger.Info("token signing key loaded", "machine", machineName)

	clk := clock.Real()

	authConfig := &service.AuthConfig{
		PublicKey: signingKey,
		Audience:  "ticket",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     clk,
	}

	ticketService := &TicketService{
		session:       session,
		writer:        session,
		resolver:      session,
		clock:         clk,
		principalName: principalName,
		machineName:   machineName,
		serverName:    serverName,
		runDir:        runDir,
		serviceRoomID: serviceRoomID,
		startedAt:     clk.Now(),
		rooms:         make(map[string]*roomState),
		aliasCache:    make(map[string]string),
		timerNotify:   make(chan struct{}, 1),
		logger:        logger,
	}

	// Register in the fleet service room so daemons can discover us.
	if err := service.Register(ctx, session, serviceRoomID, serviceRef, service.Registration{
		Machine:      machineRef.UserID(),
		Protocol:     "cbor",
		Description:  "Ticket tracking and coordination service",
		Capabilities: []string{"dependency-graph", "gate-evaluation"},
	}); err != nil {
		return fmt.Errorf("registering service: %w", err)
	}
	logger.Info("service registered",
		"principal", serviceRef.Localpart(),
		"machine", machineRef.UserID(),
	)

	// Deregister on shutdown. Use a background context since the main
	// context may already be cancelled.
	defer func() {
		deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deregCancel()
		if err := service.Deregister(deregCtx, session, serviceRoomID, serviceRef); err != nil {
			logger.Error("failed to deregister service", "error", err)
		} else {
			logger.Info("service deregistered")
		}
	}()

	// Perform initial /sync to build the ticket index.
	sinceToken, err := ticketService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Seed the timer heap from pre-existing timer gates before
	// starting the timer loop. No lock needed — no concurrent
	// access yet (socket and sync goroutines haven't started).
	ticketService.rebuildTimerHeap()

	// Start the socket server in a goroutine.
	socketPath := principal.RunDirSocketPath(runDir, principalName)
	socketServer := service.NewSocketServer(socketPath, logger, authConfig)
	socketServer.RegisterRevocationHandler()
	ticketService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, ticketService.handleSync, clk, logger)

	// Start the timer loop. Timer gates fire at precise target
	// times via a min-heap and AfterFunc, rather than polling.
	go ticketService.startTimerLoop(ctx)

	logger.Info("ticket service running",
		"principal", principalName,
		"socket", socketPath,
		"rooms", len(ticketService.rooms),
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

// TicketService is the core service state.
//
// Concurrent access from socket handlers, the sync loop, and the
// timer loop is serialized by mu. Read-only handlers hold a read
// lock; mutation handlers, the sync loop, and the timer loop hold
// a write lock. The wrappers withReadLock and withWriteLock apply
// the appropriate lock at handler registration time so individual
// handlers do not need to manage locking themselves.
type TicketService struct {
	// mu serializes access to rooms, aliasCache, timers, and every
	// Index reachable through rooms. Socket handlers run in
	// per-connection goroutines, the sync loop and timer loop each
	// run in their own goroutines, and all three paths read or
	// mutate the same shared state.
	mu sync.RWMutex

	session  *messaging.DirectSession
	writer   matrixWriter
	resolver aliasResolver
	clock    clock.Clock

	principalName string
	machineName   string
	serverName    string
	runDir        string
	serviceRoomID ref.RoomID
	startedAt     time.Time

	// rooms maps room IDs to per-room state. Only rooms with
	// m.bureau.ticket_config are tracked here. Protected by mu.
	rooms map[string]*roomState

	// aliasCache maps room aliases to resolved room IDs. Used by
	// cross-room gate evaluation to avoid re-resolving the same
	// alias on every sync batch. Entries persist for the service's
	// lifetime; stale entries are harmless (alias changes cause a
	// new room ID that won't match the old one, and the gate won't
	// fire — operator action is needed anyway when aliases change).
	// Protected by mu.
	aliasCache map[string]string

	// timers is a min-heap of pending timer gate deadlines, ordered
	// by target time (earliest first). Entries use lazy deletion:
	// on pop, the gate is verified against the current index state
	// before firing. Protected by mu.
	timers timerHeap

	// timerNotify signals the timer loop that it should wake up
	// and process expired entries or reschedule. Buffered with
	// capacity 1 so that signals from AfterFunc callbacks and
	// mutation handlers never block.
	timerNotify chan struct{}

	// timerFunc is the currently scheduled AfterFunc, set to fire
	// at the heap minimum. Nil when the heap is empty or no timer
	// is scheduled. Protected by mu.
	timerFunc *clock.Timer

	logger *slog.Logger
}
