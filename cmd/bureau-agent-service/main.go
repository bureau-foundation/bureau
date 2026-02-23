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

	"github.com/bureau-foundation/bureau/lib/artifactstore"
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
		fmt.Printf("bureau-agent-service %s\n", version.Info())
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		boot    *service.BootstrapResult
		cleanup func()
		err     error
	)

	if os.Getenv("BUREAU_PROXY_SOCKET") != "" {
		boot, cleanup, err = service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
			Audience:     "agent",
			Description:  "Agent lifecycle, session, context, and metrics service",
			Capabilities: []string{"session", "context", "metrics"},
		})
	} else {
		boot, cleanup, err = service.Bootstrap(ctx, service.BootstrapConfig{
			Flags:        flags,
			Audience:     "agent",
			Description:  "Agent lifecycle, session, context, and metrics service",
			Capabilities: []string{"session", "context", "metrics"},
		})
	}
	if err != nil {
		return err
	}
	defer cleanup()

	// Initialize the artifact client. The agent service requires
	// artifact access for context materialization — there is no useful
	// degraded mode without it.
	artifactSocket := os.Getenv("BUREAU_ARTIFACT_SOCKET")
	if artifactSocket == "" {
		return fmt.Errorf("BUREAU_ARTIFACT_SOCKET environment variable is required")
	}
	artifactToken := os.Getenv("BUREAU_ARTIFACT_TOKEN")
	if artifactToken == "" {
		return fmt.Errorf("BUREAU_ARTIFACT_TOKEN environment variable is required")
	}
	artifactClient, err := artifactstore.NewClient(artifactSocket, artifactToken)
	if err != nil {
		return fmt.Errorf("creating artifact client: %w", err)
	}
	boot.Logger.Info("artifact client initialized", "socket", artifactSocket)

	// Resolve the machine config room. Agent state events live here as
	// state events keyed by principal localpart. The service may not be
	// invited yet — the daemon invites services when it resolves room
	// service bindings during principal deployment. If the join fails,
	// the service continues startup and accepts the invite via the sync
	// loop when it arrives.
	configRoomAlias := boot.Machine.RoomAlias()
	configRoomID, err := boot.Session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving config room alias %q: %w", configRoomAlias, err)
	}
	configRoomJoined := false
	if _, err := boot.Session.JoinRoom(ctx, configRoomID); err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			boot.Logger.Info("config room not yet accessible, will join when invited",
				"config_room", configRoomID,
			)
		} else {
			return fmt.Errorf("joining config room %s: %w", configRoomID, err)
		}
	} else {
		configRoomJoined = true
	}

	boot.Logger.Info("rooms ready",
		"service_room", boot.ServiceRoomID,
		"system_room", boot.SystemRoomID,
		"config_room", configRoomID,
		"config_room_joined", configRoomJoined,
	)

	agentService := &AgentService{
		session:          boot.Session,
		artifactClient:   artifactClient,
		clock:            boot.Clock,
		principalName:    boot.PrincipalName,
		machineName:      boot.MachineName,
		serverName:       boot.ServerName,
		configRoomID:     configRoomID,
		configRoomJoined: configRoomJoined,
		startedAt:        boot.Clock.Now(),
		logger:           boot.Logger,
	}

	// Perform initial /sync to populate agent state from config room.
	sinceToken, err := agentService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server.
	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()
	agentService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the incremental sync loop.
	go service.RunSyncLoop(ctx, boot.Session, service.SyncConfig{
		Filter: syncFilter,
	}, sinceToken, agentService.handleSync, boot.Clock, boot.Logger)

	boot.Logger.Info("agent service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"config_room", configRoomID,
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

// AgentService is the core service state.
//
// Concurrent access from socket handlers and the sync loop is
// serialized by mutex. Read-only handlers hold a read lock; mutation
// handlers and the sync loop hold a write lock.
type AgentService struct {
	mutex sync.RWMutex

	session        messaging.Session
	artifactClient *artifactstore.Client
	clock          clock.Clock

	principalName    string
	machineName      string
	serverName       ref.ServerName
	configRoomID     ref.RoomID
	configRoomJoined bool
	startedAt        time.Time

	logger *slog.Logger
}
