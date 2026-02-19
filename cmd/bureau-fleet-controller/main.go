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
	"github.com/bureau-foundation/bureau/lib/schema"
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
		fmt.Printf("bureau-fleet-controller %s\n", version.Info())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.Bootstrap(ctx, service.BootstrapConfig{
		Flags:        flags,
		Audience:     "fleet",
		Description:  "Fleet controller for service placement and machine lifecycle",
		Capabilities: []string{"placement", "scaling", "failover"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Resolve fleet-specific rooms beyond the standard service and system rooms.
	fleetRoomID, err := service.ResolveRoom(ctx, boot.Session, boot.Fleet.RoomAlias())
	if err != nil {
		return fmt.Errorf("resolving fleet room: %w", err)
	}

	machineRoomID, err := service.ResolveRoom(ctx, boot.Session, boot.Fleet.MachineRoomAlias())
	if err != nil {
		return fmt.Errorf("resolving fleet machine room: %w", err)
	}

	boot.Logger.Info("fleet rooms ready",
		"fleet", flags.FleetPrefix,
		"fleet_room", fleetRoomID,
		"system_room", boot.SystemRoomID,
		"machine_room", machineRoomID,
		"service_room", boot.ServiceRoomID,
	)

	fleetController := &FleetController{
		session:       boot.Session,
		configStore:   boot.Session,
		clock:         boot.Clock,
		principalName: boot.PrincipalName,
		machineName:   boot.MachineName,
		serverName:    boot.ServerName,
		runDir:        boot.RunDir,
		serviceRoomID: boot.ServiceRoomID,
		startedAt:     boot.Clock.Now(),
		machines:      make(map[string]*machineState),
		services:      make(map[string]*fleetServiceState),
		definitions:   make(map[string]*schema.MachineDefinitionContent),
		config:        make(map[string]*schema.FleetConfigContent),
		leases:        make(map[string]*schema.HALeaseContent),
		configRooms:   make(map[string]string),
		fleetRoomID:   fleetRoomID,
		machineRoomID: machineRoomID,
		logger:        boot.Logger,
	}

	// Perform initial /sync to build the fleet model.
	sinceToken, err := fleetController.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server in a goroutine.
	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()
	fleetController.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop in a goroutine.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, fleetController.handleSync, boot.Clock, boot.Logger)

	boot.Logger.Info("fleet controller running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"machines", len(fleetController.machines),
		"services", len(fleetController.services),
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
	serviceRoomID ref.RoomID
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
	fleetRoomID ref.RoomID

	// machineRoomID is the fleet-scoped machine room, resolved from the fleet prefix.
	machineRoomID ref.RoomID

	logger *slog.Logger
}
