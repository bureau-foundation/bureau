// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

// artifactAccess defines the artifact store operations used by the
// agent service. The production implementation is *artifactstore.Client;
// tests provide a mock that stores artifacts in memory.
type artifactAccess interface {
	Store(ctx context.Context, header *artifactstore.StoreHeader, content io.Reader) (*artifactstore.StoreResponse, error)
	Fetch(ctx context.Context, ref string) (*artifactstore.FetchResult, error)
	Resolve(ctx context.Context, ref string) (*artifactstore.ResolveResponse, error)
	Tags(ctx context.Context, prefix string) (*artifactstore.TagsResponse, error)
}

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
		version.Print("bureau-agent-service")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "agent",
		Description:  "Agent lifecycle, session, context, and metrics service",
		Capabilities: []string{"session", "context", "metrics"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Initialize the artifact client using standard service paths.
	// The agent service template declares RequiredServices: ["artifact"],
	// which causes the daemon to bind-mount the artifact service socket
	// and token at these paths. The agent service requires artifact
	// access for two purposes: storing inline checkpoint data on behalf
	// of agents, and fetching/concatenating deltas during materialization.
	const (
		artifactServiceSocketPath = "/run/bureau/service/artifact.sock"
		artifactServiceTokenPath  = "/run/bureau/service/token/artifact.token"
	)
	artifactClient, err := artifactstore.NewClient(artifactServiceSocketPath, artifactServiceTokenPath)
	if err != nil {
		return fmt.Errorf("creating artifact client: %w", err)
	}
	boot.Logger.Info("artifact client initialized", "socket", artifactServiceSocketPath)

	// Resolve the machine config room. Agent session, context pointer,
	// and metrics state events live here, keyed by principal localpart.
	// Context commit metadata lives in the artifact service. Room membership is
	// handled by principal.Create (which invites to the config room)
	// and the proxy's acceptPendingInvites (which joins at startup).
	configRoomAlias := boot.Machine.RoomAlias()
	configRoomID, err := boot.Session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("resolving config room alias %q: %w", configRoomAlias, err)
	}

	boot.Logger.Info("rooms ready",
		"service_room", boot.ServiceRoomID,
		"system_room", boot.SystemRoomID,
		"config_room", configRoomID,
	)

	agentService := &AgentService{
		session:            boot.Session,
		artifactClient:     artifactClient,
		clock:              boot.Clock,
		principalName:      boot.PrincipalName,
		machineName:        boot.MachineName,
		serverName:         boot.ServerName,
		configRoomID:       configRoomID,
		startedAt:          boot.Clock.Now(),
		commitIndex:        make(map[string]agent.ContextCommitContent),
		principalTimelines: make(map[string][]timelineEntry),
		logger:             boot.Logger,
	}

	// Perform initial /sync to populate agent state from config room.
	sinceToken, err := agentService.initialSync(ctx)
	if err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// Start the socket server.
	socketServer := boot.NewSocketServer()
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
	artifactClient artifactAccess
	clock          clock.Clock

	principalName string
	machineName   string
	serverName    ref.ServerName
	configRoomID  ref.RoomID
	startedAt     time.Time

	// commitIndex maps ctx-* commit IDs to their deserialized content.
	// Populated write-through by checkpoint-context and
	// update-context-metadata handlers. Commits not in the index
	// are fetched on demand from the artifact store (CAS).
	commitIndex map[string]agent.ContextCommitContent

	// principalTimelines maps principal localparts to their
	// checkpoint timelines, sorted by CreatedAt ascending. Used by
	// resolve-context to find the nearest checkpoint at or before a
	// given timestamp. Populated write-through by checkpoint-context
	// and lazily rebuilt from CAS on resolve-context cache miss.
	principalTimelines map[string][]timelineEntry

	// timelinesLoaded tracks whether principalTimelines has been
	// populated from the artifact store. False on startup (timelines
	// empty); set to true after the first resolve-context triggers a
	// full scan of ctx/ tags. Distinguishes "no commits exist" from
	// "haven't loaded yet".
	timelinesLoaded bool

	logger *slog.Logger
}

// timelineEntry maps a timestamp to a context commit ID in a
// principal's checkpoint timeline. Entries are sorted by CreatedAt
// ascending within each principal's timeline slice.
type timelineEntry struct {
	CreatedAt string // ISO 8601 timestamp
	CommitID  string
}
