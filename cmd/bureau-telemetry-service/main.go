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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/version"
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
		version.Print("bureau-telemetry-service")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:    "telemetry",
		Description: "Telemetry aggregation and query service",
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Set up the artifact client for output delta persistence.
	// If the artifact service socket doesn't exist (service not
	// deployed), log a warning and run without persistence.
	var artifactClient artifactStorer
	artifactClient, err = createArtifactClient(boot.Logger)
	if err != nil {
		boot.Logger.Warn("artifact service not available, output persistence disabled",
			"error", err,
		)
		artifactClient = nil
	}

	logMgr := newLogManager(artifactClient, boot.Session, boot.Clock, boot.Logger)

	telemetryService := &TelemetryService{
		authConfig:          boot.AuthConfig,
		clock:               boot.Clock,
		logger:              boot.Logger,
		startedAt:           boot.Clock.Now(),
		artifactPersistence: artifactClient != nil,
		logManager:          logMgr,
	}

	// Start the CBOR socket server with ingestion and query actions.
	socketServer := boot.NewSocketServer()
	socketServer.RegisterRevocationHandler()
	telemetryService.registerActions(socketServer)

	// Start the log manager's background flush and reaper tickers.
	logManagerDone := make(chan struct{})
	go func() {
		logMgr.Run(ctx)
		close(logManagerDone)
	}()

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	boot.Logger.Info("telemetry service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"artifact_persistence", artifactClient != nil,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for the socket server to drain active connections
	// (including any streaming ingest connections).
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	// Wait for the log manager's background goroutines to stop.
	<-logManagerDone

	// Drain remaining output buffers.
	logMgr.Close(context.Background())

	return nil
}

// Artifact service socket paths, set by the daemon when the telemetry
// service template includes RequiredServices: ["artifact"].
const (
	artifactSocketPath = "/run/bureau/service/artifact.sock"
	artifactTokenPath  = "/run/bureau/service/token/artifact.token"
)

// createArtifactClient creates an authenticated artifact store client.
// Returns an error if the socket or token file doesn't exist.
func createArtifactClient(logger *slog.Logger) (*artifactstore.Client, error) {
	if _, err := os.Stat(artifactSocketPath); err != nil {
		return nil, fmt.Errorf("artifact socket not found: %w", err)
	}
	if _, err := os.Stat(artifactTokenPath); err != nil {
		return nil, fmt.Errorf("artifact token not found: %w", err)
	}

	client, err := artifactstore.NewClient(artifactSocketPath, artifactTokenPath)
	if err != nil {
		return nil, fmt.Errorf("creating artifact client: %w", err)
	}

	logger.Info("artifact client ready",
		"socket", artifactSocketPath,
	)
	return client, nil
}

// TelemetryService is the core state for the telemetry aggregation
// service. It tracks ingestion statistics and connected relay count.
//
// Ingestion stats use atomics for lock-free reads from the status
// handler while streaming ingest goroutines write concurrently. The
// connected relay count is protected by relayMu because it requires
// increment/decrement (not a simple atomic store).
type TelemetryService struct {
	authConfig *service.AuthConfig
	clock      clock.Clock
	logger     *slog.Logger
	startedAt  time.Time

	// artifactPersistence records whether the artifact client was
	// successfully created at startup. Exposed via the status
	// endpoint so operators and tests can verify that output delta
	// persistence is active.
	artifactPersistence bool

	// logManager handles output delta persistence: buffering,
	// artifact storage, and m.bureau.log state event tracking.
	// Always non-nil; when the artifact service is unavailable,
	// the manager's internal artifact client is nil and
	// HandleDeltas returns early without storing anything.
	logManager *logManager

	// Ingestion counters, updated atomically by ingest stream handlers.
	batchesReceived      atomic.Uint64
	spansReceived        atomic.Uint64
	metricsReceived      atomic.Uint64
	logsReceived         atomic.Uint64
	outputDeltasReceived atomic.Uint64

	// relayMu protects connectedRelays. Read by the status handler,
	// written by ingest stream handlers on connect/disconnect.
	relayMu         sync.Mutex
	connectedRelays int

	// subscriberMu protects tailSubscribers. The ingest handler reads
	// under RLock to fan out batches; the tail handler writes under
	// Lock to add/remove subscribers.
	subscriberMu    sync.RWMutex
	tailSubscribers []*tailSubscriber
}

// registerActions registers the service's socket actions on the server.
func (s *TelemetryService) registerActions(server *service.SocketServer) {
	// Unauthenticated liveness and stats endpoint.
	server.Handle("status", s.handleStatus)

	// Authenticated streaming ingestion from relays.
	server.HandleAuthStream("ingest", s.handleIngest)

	// Authenticated streaming tail for live telemetry consumption.
	// Clients subscribe to source patterns and receive matching
	// batches as they are ingested.
	server.HandleAuthStream("tail", s.handleTail)

	// Authenticated session completion. Called by the daemon when
	// a sandbox exits to flush remaining output and mark the log
	// entity as complete.
	server.HandleAuth("complete-log", s.handleCompleteLog)
}

// handleStatus returns aggregate ingestion stats. This is the only
// unauthenticated action — it exposes operational metrics but no
// telemetry content or topology information.
func (s *TelemetryService) handleStatus(_ context.Context, _ []byte) (any, error) {
	s.relayMu.Lock()
	relays := s.connectedRelays
	s.relayMu.Unlock()

	return telemetry.ServiceStatus{
		BatchesReceived:      s.batchesReceived.Load(),
		SpansReceived:        s.spansReceived.Load(),
		MetricsReceived:      s.metricsReceived.Load(),
		LogsReceived:         s.logsReceived.Load(),
		OutputDeltasReceived: s.outputDeltasReceived.Load(),
		ConnectedRelays:      relays,
		UptimeSeconds:        s.clock.Now().Sub(s.startedAt).Seconds(),
		ArtifactPersistence:  s.artifactPersistence,
		LogManager:           s.logManager.Stats(),
	}, nil
}

// handleCompleteLog flushes remaining output for a session and
// transitions its m.bureau.log entity to "complete". Called by the
// daemon when a sandbox exits. Idempotent — returns success if the
// session was already completed or never existed.
func (s *TelemetryService) handleCompleteLog(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/ingest", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/ingest")
	}

	var request telemetry.CompleteLogRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding complete-log request: %w", err)
	}
	if request.Source == "" {
		return nil, fmt.Errorf("source is required")
	}

	if err := s.logManager.CompleteLog(ctx, request.Source, request.SessionID); err != nil {
		return nil, fmt.Errorf("completing log: %w", err)
	}

	s.logger.Info("log session completed",
		"source", request.Source,
		"session_id", request.SessionID,
		"subject", token.Subject,
	)

	return telemetry.CompleteLogResponse{Completed: true}, nil
}
