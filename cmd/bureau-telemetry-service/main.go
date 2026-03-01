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

	// Log manager tuning flags, passed via the template Command field.
	chunkSizeThreshold := flag.Int("chunk-size-threshold", defaultChunkSizeThreshold,
		"buffer size in bytes that triggers an immediate flush to artifact storage")
	maxBytesPerSession := flag.Int64("max-bytes-per-session", defaultMaxBytesPerSession,
		"maximum stored output bytes per session before old chunks are evicted")

	// Storage flags.
	storagePath := flag.String("storage-path", "/var/bureau/telemetry/telemetry.db",
		"filesystem path for the SQLite telemetry database")
	storagePoolSize := flag.Int("storage-pool-size", 4,
		"number of SQLite connections in the pool")
	retentionSpanDays := flag.Int("retention-span-days", 7,
		"days to retain span data before partition drop")
	retentionMetricDays := flag.Int("retention-metric-days", 14,
		"days to retain metric data before partition drop")
	retentionLogDays := flag.Int("retention-log-days", 7,
		"days to retain log data before partition drop")

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

	logMgr := newLogManager(artifactClient, boot.Clock, boot.Logger)
	logMgr.chunkSizeThreshold = *chunkSizeThreshold
	logMgr.maxBytesPerSession = *maxBytesPerSession

	// Ensure the storage directory exists. In production sandboxes,
	// the template's CreateDirs or a persistent volume mount provides
	// the parent directory. Auto-creating it here prevents confusing
	// "no such file or directory" errors if the template omits it.
	if err := os.MkdirAll(filepath.Dir(*storagePath), 0o755); err != nil {
		return fmt.Errorf("creating storage directory: %w", err)
	}

	// Open the SQLite store for span, metric, and log persistence.
	store, err := OpenStore(StoreConfig{
		Path:       *storagePath,
		PoolSize:   *storagePoolSize,
		ServerName: boot.ServerName,
		Clock:      boot.Clock,
		Logger:     boot.Logger,
	})
	if err != nil {
		return fmt.Errorf("opening telemetry store: %w", err)
	}
	defer store.Close()

	retention := RetentionConfig{
		Spans:   time.Duration(*retentionSpanDays) * 24 * time.Hour,
		Metrics: time.Duration(*retentionMetricDays) * 24 * time.Hour,
		Logs:    time.Duration(*retentionLogDays) * 24 * time.Hour,
	}

	telemetryService := &TelemetryService{
		authConfig:          boot.AuthConfig,
		clock:               boot.Clock,
		logger:              boot.Logger,
		startedAt:           boot.Clock.Now(),
		artifactPersistence: artifactClient != nil,
		logManager:          logMgr,
		store:               store,
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

	// Start the retention ticker. Runs hourly, dropping partition
	// tables older than the configured retention period.
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		retentionTicker := boot.Clock.NewTicker(1 * time.Hour)
		defer retentionTicker.Stop()

		// Run retention once at startup to clean up any partitions
		// that expired while the service was stopped.
		if err := store.RunRetention(ctx, retention); err != nil {
			boot.Logger.Error("initial retention failed", "error", err)
		}

		for {
			select {
			case <-retentionTicker.C:
				if err := store.RunRetention(ctx, retention); err != nil {
					boot.Logger.Error("retention failed", "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	boot.Logger.Info("telemetry service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"artifact_persistence", artifactClient != nil,
		"storage_path", *storagePath,
		"retention_spans", retention.Spans,
		"retention_metrics", retention.Metrics,
		"retention_logs", retention.Logs,
		"chunk_size_threshold", *chunkSizeThreshold,
		"max_bytes_per_session", *maxBytesPerSession,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for the socket server to drain active connections
	// (including any streaming ingest connections).
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	// Wait for background goroutines to stop.
	<-logManagerDone
	<-retentionDone

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
	// artifact storage, and log metadata tracking via mutable tags.
	// Always non-nil; when the artifact service is unavailable,
	// the manager's internal artifact client is nil and
	// HandleDeltas returns early without storing anything.
	logManager *logManager

	// store persists spans, metrics, and logs to SQLite with
	// time-partitioned tables. Write path: called from handleIngest
	// after fan-out to tail subscribers. Query path: used by query
	// actions (traces, metrics, logs, top).
	store *Store

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

	// Authenticated operational actions for deterministic testing.
	// These trigger the same logic as the background tickers but
	// on demand, so callers don't need to wait for timer intervals.
	server.HandleAuth("flush", s.handleFlush)
	server.HandleAuth("reap", s.handleReap)
}

// handleStatus returns aggregate ingestion stats. This is the only
// unauthenticated action — it exposes operational metrics but no
// telemetry content or topology information.
func (s *TelemetryService) handleStatus(ctx context.Context, _ []byte) (any, error) {
	s.relayMu.Lock()
	relays := s.connectedRelays
	s.relayMu.Unlock()

	storageStats, err := s.store.Stats(ctx)
	if err != nil {
		s.logger.Error("status: failed to read storage stats", "error", err)
		// Return the response with zero-valued storage stats rather
		// than failing the entire status request.
	}

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
		Storage:              storageStats,
	}, nil
}

// handleCompleteLog flushes remaining output for a session and
// transitions its log metadata to "complete". Called by the daemon
// when a sandbox exits. Idempotent — returns success if the session
// was already completed or never existed.
func (s *TelemetryService) handleCompleteLog(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/ingest", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/ingest")
	}

	var request telemetry.CompleteLogRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding complete-log request: %w", err)
	}
	if request.Source.IsZero() {
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

// handleFlush triggers an immediate flush of all session buffers that
// have pending data. Equivalent to the background flush ticker firing.
// Used by integration tests for deterministic metadata verification
// without waiting on timer intervals.
func (s *TelemetryService) handleFlush(ctx context.Context, token *servicetoken.Token, _ []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/ingest", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/ingest")
	}

	s.logManager.tickFlush(ctx)
	return nil, nil
}

// handleReap triggers an immediate reaper scan: stale session completion
// and oversized session eviction. Equivalent to the background reaper
// ticker firing. Used by integration tests for deterministic eviction
// verification without waiting on the 1-minute reaper interval.
func (s *TelemetryService) handleReap(ctx context.Context, token *servicetoken.Token, _ []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/ingest", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/ingest")
	}

	s.logManager.tickReaper(ctx)
	return nil, nil
}
