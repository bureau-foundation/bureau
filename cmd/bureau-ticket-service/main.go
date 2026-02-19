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
	"github.com/bureau-foundation/bureau/lib/ref"
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
	var flags service.CommonFlags
	service.RegisterCommonFlags(&flags)
	flag.Parse()

	if flags.ShowVersion {
		fmt.Printf("bureau-ticket-service %s\n", version.Info())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.Bootstrap(ctx, service.BootstrapConfig{
		Flags:        flags,
		Audience:     "ticket",
		Description:  "Ticket tracking and coordination service",
		Capabilities: []string{"dependency-graph", "gate-evaluation"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	ticketService := &TicketService{
		session:       boot.Session,
		writer:        boot.Session,
		resolver:      boot.Session,
		clock:         boot.Clock,
		principalName: boot.PrincipalName,
		machineName:   boot.MachineName,
		serverName:    boot.ServerName,
		runDir:        boot.RunDir,
		serviceRoomID: boot.ServiceRoomID,
		startedAt:     boot.Clock.Now(),
		rooms:         make(map[string]*roomState),
		aliasCache:    make(map[string]string),
		timerNotify:   make(chan struct{}, 1),
		logger:        boot.Logger,
	}

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
	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()
	ticketService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, ticketService.handleSync, boot.Clock, boot.Logger)

	// Start the timer loop. Timer gates fire at precise target
	// times via a min-heap and AfterFunc, rather than polling.
	go ticketService.startTimerLoop(ctx)

	boot.Logger.Info("ticket service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"rooms", len(ticketService.rooms),
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for the socket server to drain active connections.
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
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
