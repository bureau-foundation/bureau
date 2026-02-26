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

	// Relay-specific tuning flags, passed via the template Command field.
	bufferMaxBytes := flag.Int("buffer-max-bytes", 64*1024*1024,
		"maximum byte size of the outgoing batch buffer")
	flushInterval := flag.Duration("flush-interval", 5*time.Second,
		"how often the accumulator is flushed to the buffer")
	flushThresholdBytes := flag.Int("flush-threshold-bytes", 256*1024,
		"accumulator size in bytes that triggers an immediate flush")

	flag.Parse()

	if showVersion {
		version.Print("bureau-telemetry-relay")
		return nil
	}

	// The telemetry service socket and token paths are set by the
	// daemon via RequiredServices resolution and template env vars.
	telemetryServiceSocket := os.Getenv("BUREAU_TELEMETRY_SERVICE_SOCKET")
	if telemetryServiceSocket == "" {
		return fmt.Errorf("BUREAU_TELEMETRY_SERVICE_SOCKET is required")
	}
	telemetryTokenPath := os.Getenv("BUREAU_TELEMETRY_TOKEN_PATH")
	if telemetryTokenPath == "" {
		return fmt.Errorf("BUREAU_TELEMETRY_TOKEN_PATH is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "telemetry",
		Description:  "Per-machine telemetry relay",
		Capabilities: []string{"ingest"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Create the outgoing shipper that maintains a persistent
	// streaming connection to the telemetry service's "ingest"
	// action.
	shipper, err := newStreamShipper(telemetryServiceSocket, telemetryTokenPath)
	if err != nil {
		return fmt.Errorf("creating shipper: %w", err)
	}

	relay := &Relay{
		accumulator: NewAccumulator(boot.Fleet, boot.Machine, *flushThresholdBytes),
		buffer:      NewBuffer(*bufferMaxBytes),
		shipper:     shipper,
		clock:       boot.Clock,
		startedAt:   boot.Clock.Now(),
		logger:      boot.Logger,
	}

	// Start the socket server.
	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()
	relay.registerActions(socketServer)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	// Start the periodic flush loop.
	go relay.runFlushLoop(ctx, *flushInterval)

	// Start the shipper goroutine. The done channel signals when the
	// shipper has finished its drain pass so we can close the
	// streaming connection cleanly.
	shipperDone := make(chan struct{})
	go func() {
		runShipper(ctx, relay.buffer, relay.shipper, relay.clock, &relay.shipped, relay.logger)
		close(shipperDone)
	}()

	boot.Logger.Info("telemetry relay running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
		"telemetry_service", telemetryServiceSocket,
		"flush_interval", *flushInterval,
		"flush_threshold", *flushThresholdBytes,
		"buffer_max", *bufferMaxBytes,
	)

	// Wait for shutdown signal.
	<-ctx.Done()
	boot.Logger.Info("shutting down")

	// Wait for the socket server to drain active connections.
	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	// Final flush: push any remaining accumulator contents to the
	// buffer. The shipper's drain pass (triggered by ctx
	// cancellation) will attempt to ship these.
	relay.flushToBuffer()

	// Wait for the shipper to finish its drain pass, then close
	// the streaming connection.
	<-shipperDone
	relay.shipper.Close()

	return nil
}

// Relay holds the relay's runtime state. Created in run() and shared
// between the socket handlers, flush loop, and shipper goroutine.
type Relay struct {
	accumulator *Accumulator
	buffer      *Buffer
	shipper     BatchShipper
	clock       clock.Clock
	startedAt   time.Time
	// shipped is read by the status handler and written by the
	// shipper goroutine, so it uses atomic operations.
	shipped atomic.Uint64
	logger  *slog.Logger
}
