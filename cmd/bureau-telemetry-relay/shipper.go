// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/service"
)

// BatchShipper sends a CBOR-encoded telemetry batch to the telemetry
// service. The relay uses this interface so that tests can substitute
// a fake implementation without needing a real service socket.
type BatchShipper interface {
	Ship(ctx context.Context, data []byte) error
}

// socketShipper ships CBOR-encoded telemetry batches to the telemetry
// service by calling its "ingest" action via a ServiceClient.
type socketShipper struct {
	client *service.ServiceClient
}

// newSocketShipper creates a BatchShipper that sends batches to the
// telemetry service at the given socket path, authenticating with the
// service token at tokenPath.
func newSocketShipper(socketPath, tokenPath string) (BatchShipper, error) {
	client, err := service.NewServiceClient(socketPath, tokenPath)
	if err != nil {
		return nil, err
	}
	return &socketShipper{client: client}, nil
}

// Ship sends a pre-encoded CBOR batch to the telemetry service's
// "ingest" action. The batch is sent as a codec.RawMessage so the
// service receives the pre-encoded bytes without double-encoding.
func (s *socketShipper) Ship(ctx context.Context, data []byte) error {
	return s.client.Call(ctx, "ingest", map[string]any{
		"batch": codec.RawMessage(data),
	}, nil)
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
