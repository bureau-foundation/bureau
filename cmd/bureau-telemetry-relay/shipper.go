// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// BatchShipper sends CBOR-encoded telemetry batches to the telemetry
// service. The relay uses this interface so that tests can substitute
// a fake implementation without needing a real service socket.
type BatchShipper interface {
	Ship(ctx context.Context, data []byte) error

	// Close shuts down the shipper, closing any persistent connection.
	// Called after the shipper goroutine exits during graceful shutdown.
	Close()
}

// streamShipper ships CBOR-encoded telemetry batches to the telemetry
// service over a persistent streaming connection. The connection is
// established on the first Ship call and reused for subsequent calls.
// If the connection breaks, the next Ship call establishes a new one.
type streamShipper struct {
	socketPath string
	tokenBytes []byte

	conn    net.Conn
	encoder *codec.Encoder
	decoder *codec.Decoder
}

// streamDialTimeout is the maximum time to wait for a connection to
// the telemetry service socket.
const streamDialTimeout = 5 * time.Second

// newStreamShipper creates a BatchShipper that maintains a persistent
// streaming connection to the telemetry service. The service token is
// read from tokenPath once at creation time.
func newStreamShipper(socketPath, tokenPath string) (BatchShipper, error) {
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading service token from %s: %w", tokenPath, err)
	}
	if len(tokenBytes) == 0 {
		return nil, fmt.Errorf("service token file %s is empty", tokenPath)
	}
	return &streamShipper{
		socketPath: socketPath,
		tokenBytes: tokenBytes,
	}, nil
}

// Ship sends a pre-encoded CBOR batch over the streaming connection.
// If no connection exists, one is established first (including the
// handshake and readiness ack). On any error, the connection is closed
// so the next Ship call reconnects with a fresh handshake.
func (s *streamShipper) Ship(ctx context.Context, data []byte) error {
	if s.conn == nil {
		if err := s.connect(ctx); err != nil {
			return err
		}
	}

	// Send the pre-encoded batch as a raw CBOR value. The service
	// decodes this as a telemetry.TelemetryBatch.
	if err := s.encoder.Encode(codec.RawMessage(data)); err != nil {
		s.closeConnection()
		return fmt.Errorf("writing batch: %w", err)
	}

	// Read the per-batch acknowledgment.
	var ack telemetry.StreamAck
	if err := s.decoder.Decode(&ack); err != nil {
		s.closeConnection()
		return fmt.Errorf("reading ack: %w", err)
	}
	if !ack.OK {
		s.closeConnection()
		return fmt.Errorf("batch rejected: %s", ack.Error)
	}

	return nil
}

// Close shuts down the persistent connection if one is open.
func (s *streamShipper) Close() {
	s.closeConnection()
}

// connect establishes a streaming connection to the telemetry service.
// It sends the handshake (action + token) and waits for the service's
// readiness ack before returning.
func (s *streamShipper) connect(ctx context.Context) error {
	dialer := net.Dialer{Timeout: streamDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", s.socketPath, err)
	}

	s.conn = conn
	s.encoder = codec.NewEncoder(conn)
	s.decoder = codec.NewDecoder(conn)

	// Send the handshake: action + token. The socket server verifies
	// the token and routes to the "ingest" stream handler.
	handshake := map[string]any{
		"action": "ingest",
		"token":  s.tokenBytes,
	}
	if err := s.encoder.Encode(handshake); err != nil {
		s.closeConnection()
		return fmt.Errorf("writing handshake: %w", err)
	}

	// Wait for the readiness signal. If auth fails, the socket server
	// writes {ok: false, error: "..."} directly. If auth succeeds,
	// the ingest handler writes {ok: true}.
	var ack telemetry.StreamAck
	if err := s.decoder.Decode(&ack); err != nil {
		s.closeConnection()
		return fmt.Errorf("reading handshake response: %w", err)
	}
	if !ack.OK {
		s.closeConnection()
		return fmt.Errorf("handshake rejected: %s", ack.Error)
	}

	return nil
}

// closeConnection tears down the streaming connection and clears all
// connection state. The next Ship call will establish a new connection.
func (s *streamShipper) closeConnection() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
		s.encoder = nil
		s.decoder = nil
	}
}

// Backoff constants for the shipper retry loop. Starts at
// initialBackoff and doubles on each consecutive failure, capped at
// maxBackoff. Resets to initialBackoff on success.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// runShipper drains the buffer by shipping batches to the telemetry
// service. It runs in its own goroutine for the relay's lifetime.
//
// The loop peeks at the oldest entry, attempts to ship it, and pops
// it on success. On failure it backs off exponentially (1s → 2s → 4s
// → ... → 30s cap). When the context is cancelled, it makes one
// final drain attempt with a short timeout before returning.
//
// The shipped counter is incremented atomically on each successful
// ship (it is read concurrently by the status handler).
func runShipper(ctx context.Context, buffer *Buffer, shipper BatchShipper, clk clock.Clock, shipped *atomic.Uint64, logger *slog.Logger) {
	backoff := initialBackoff

	for {
		// Wait for data or shutdown.
		select {
		case <-buffer.Notify():
		case <-ctx.Done():
			drainBuffer(buffer, shipper, shipped, logger)
			return
		}

		// Drain all available entries.
		for {
			data := buffer.Peek()
			if data == nil {
				break
			}

			if err := shipper.Ship(ctx, data); err != nil {
				if ctx.Err() != nil {
					drainBuffer(buffer, shipper, shipped, logger)
					return
				}
				logger.Warn("batch ship failed, will retry",
					"error", err,
					"backoff", backoff,
					"buffer_entries", buffer.Len(),
				)
				select {
				case <-clk.After(backoff):
				case <-ctx.Done():
					drainBuffer(buffer, shipper, shipped, logger)
					return
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			buffer.Pop()
			shipped.Add(1)
			backoff = initialBackoff
		}
	}
}

// drainBuffer makes one best-effort pass through the buffer after
// shutdown, using a short timeout per batch. This ensures that any
// data accumulated during graceful shutdown has a chance to ship.
func drainBuffer(buffer *Buffer, shipper BatchShipper, shipped *atomic.Uint64, logger *slog.Logger) {
	const drainTimeout = 5 * time.Second
	drainContext, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	for {
		data := buffer.Peek()
		if data == nil {
			return
		}
		if err := shipper.Ship(drainContext, data); err != nil {
			logger.Warn("drain: batch ship failed, abandoning remaining",
				"error", err,
				"remaining", buffer.Len(),
			)
			return
		}
		buffer.Pop()
		shipped.Add(1)
	}
}
