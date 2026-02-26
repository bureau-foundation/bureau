// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		version.Print("bureau-fleet-controller")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "fleet",
		Description:  "Fleet controller for service placement and machine lifecycle",
		Capabilities: []string{"placement", "scaling", "failover"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Resolve fleet-specific rooms beyond the standard service and system
	// rooms. Room membership is handled by the proxy's acceptPendingInvites
	// at startup — the daemon invited this service to the fleet room and
	// machine room before creating the sandbox (via ExtraRooms in the
	// MachineConfig). ResolveAlias is ungated (no proxy grant required).
	fleetRoomAlias := boot.Fleet.RoomAlias()
	fleetRoomID, err := boot.Session.ResolveAlias(ctx, fleetRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet room %q: %w", fleetRoomAlias, err)
	}

	machineRoomAlias := boot.Fleet.MachineRoomAlias()
	machineRoomID, err := boot.Session.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving fleet machine room %q: %w", machineRoomAlias, err)
	}

	// Construct the fleet controller's own entity reference for publishing
	// service bindings to config rooms. Agents with required_services:
	// ["fleet"] discover the fleet controller via this binding.
	serviceEntity, err := ref.NewEntityFromAccountLocalpart(boot.Fleet, boot.PrincipalName)
	if err != nil {
		return fmt.Errorf("constructing fleet controller entity: %w", err)
	}

	boot.Logger.Info("fleet rooms ready",
		"fleet", boot.Fleet,
		"fleet_room", fleetRoomID,
		"system_room", boot.SystemRoomID,
		"machine_room", machineRoomID,
		"service_room", boot.ServiceRoomID,
		"service_entity", serviceEntity,
	)

	fleetController := &FleetController{
		session:       boot.Session,
		configStore:   boot.Session,
		clock:         boot.Clock,
		principalName: boot.PrincipalName,
		machineName:   boot.MachineName,
		serverName:    boot.ServerName,
		fleet:         boot.Fleet,
		serviceEntity: serviceEntity,
		serviceRoomID: boot.ServiceRoomID,
		startedAt:     boot.Clock.Now(),
		machines:      make(map[string]*machineState),
		services:      make(map[string]*fleetServiceState),
		definitions:   make(map[string]*fleet.MachineDefinitionContent),
		config:        make(map[string]*fleet.FleetConfigContent),
		leases:        make(map[string]*fleet.HALeaseContent),
		configRooms:   make(map[string]ref.RoomID),
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
	socketServer := boot.NewSocketServer()
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

	session     messaging.Session
	configStore configStore
	clock       clock.Clock

	principalName string
	machineName   string
	serverName    ref.ServerName
	fleet         ref.Fleet
	serviceEntity ref.Entity
	serviceRoomID ref.RoomID
	startedAt     time.Time

	// In-memory fleet model, rebuilt from /sync.
	machines    map[string]*machineState
	services    map[string]*fleetServiceState
	definitions map[string]*fleet.MachineDefinitionContent
	config      map[string]*fleet.FleetConfigContent
	leases      map[string]*fleet.HALeaseContent

	// configRooms maps machine localparts to their config room IDs.
	// Populated from room aliases during initial sync.
	configRooms map[string]ref.RoomID

	// fleetRoomID is the fleet config room, resolved from the fleet prefix.
	fleetRoomID ref.RoomID

	// machineRoomID is the fleet-scoped machine room, resolved from the fleet prefix.
	machineRoomID ref.RoomID

	logger *slog.Logger
}
