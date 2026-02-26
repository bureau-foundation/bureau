// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"log/slog"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/service"
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

	telemetryService := &TelemetryService{
		authConfig: boot.AuthConfig,
		clock:      boot.Clock,
		logger:     boot.Logger,
		startedAt:  boot.Clock.Now(),
	}

	// Start the CBOR socket server with ingestion and query actions.
	socketServer := boot.NewSocketServer()
	socketServer.RegisterRevocationHandler()
	telemetryService.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	boot.Logger.Info("telemetry service running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for the socket server to drain active connections
	// (including any streaming ingest connections).
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	return nil
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

	// Ingestion counters, updated atomically by ingest stream handlers.
	batchesReceived atomic.Uint64
	spansReceived   atomic.Uint64
	metricsReceived atomic.Uint64
	logsReceived    atomic.Uint64

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

// statusResponse is the CBOR response for the unauthenticated "status"
// action. Contains only aggregate operational metrics — no fleet, machine,
// or source identifiers that could disclose topology.
type statusResponse struct {
	BatchesReceived uint64  `cbor:"batches_received"`
	SpansReceived   uint64  `cbor:"spans_received"`
	MetricsReceived uint64  `cbor:"metrics_received"`
	LogsReceived    uint64  `cbor:"logs_received"`
	ConnectedRelays int     `cbor:"connected_relays"`
	UptimeSeconds   float64 `cbor:"uptime_seconds"`
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
}

// handleStatus returns aggregate ingestion stats. This is the only
// unauthenticated action — it exposes operational metrics but no
// telemetry content or topology information.
func (s *TelemetryService) handleStatus(_ context.Context, _ []byte) (any, error) {
	s.relayMu.Lock()
	relays := s.connectedRelays
	s.relayMu.Unlock()

	return statusResponse{
		BatchesReceived: s.batchesReceived.Load(),
		SpansReceived:   s.spansReceived.Load(),
		MetricsReceived: s.metricsReceived.Load(),
		LogsReceived:    s.logsReceived.Load(),
		ConnectedRelays: relays,
		UptimeSeconds:   s.clock.Now().Sub(s.startedAt).Seconds(),
	}, nil
}
