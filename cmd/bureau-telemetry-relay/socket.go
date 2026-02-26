// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// relayStatusResponse is the wire format for the "status" action.
// Returns relay health information for liveness checks and
// operational monitoring.
type relayStatusResponse struct {
	UptimeSeconds        float64 `cbor:"uptime_seconds"`
	AccumulatorSizeBytes int     `cbor:"accumulator_size_bytes"`
	BufferEntries        int     `cbor:"buffer_entries"`
	BufferSizeBytes      int     `cbor:"buffer_size_bytes"`
	BatchesShipped       uint64  `cbor:"batches_shipped"`
	BatchesDropped       uint64  `cbor:"batches_dropped"`
	SequenceNumber       uint64  `cbor:"sequence_number"`
}

// registerActions registers the relay's socket API actions.
//
// "submit" is authenticated — only services with valid tokens can
// push telemetry to the relay. "status" is unauthenticated for
// liveness probing.
func (r *Relay) registerActions(server *service.SocketServer) {
	server.HandleAuth("submit", r.handleSubmit)
	server.Handle("status", r.handleStatus)
}

// handleSubmit receives telemetry records from local services,
// accumulates them, and triggers a flush-to-buffer if the
// accumulator's size threshold is crossed. The flush-to-buffer is
// inline (the caller holds no lock that would conflict) to minimize
// latency between threshold detection and buffer entry.
//
// The [telemetry.SubmitRequest] carries identity at the envelope level.
// StampIdentity re-hydrates per-record identity before accumulating so
// that the downstream [telemetry.TelemetryBatch] carries full identity.
func (r *Relay) handleSubmit(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var request telemetry.SubmitRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid submit request")
	}

	if request.Fleet.IsZero() || request.Machine.IsZero() || request.Source.IsZero() {
		return nil, errors.New("submit request must include fleet, machine, and source identity")
	}

	if len(request.Spans) == 0 && len(request.Metrics) == 0 && len(request.Logs) == 0 && len(request.OutputDeltas) == 0 {
		return nil, errors.New("submit request must contain at least one span, metric, log, or output delta record")
	}

	request.StampIdentity()

	shouldFlush := false

	if len(request.Spans) > 0 {
		crossed, err := r.accumulator.AddSpans(request.Spans)
		if err != nil {
			return nil, err
		}
		if crossed {
			shouldFlush = true
		}
	}

	if len(request.Metrics) > 0 {
		crossed, err := r.accumulator.AddMetrics(request.Metrics)
		if err != nil {
			return nil, err
		}
		if crossed {
			shouldFlush = true
		}
	}

	if len(request.Logs) > 0 {
		crossed, err := r.accumulator.AddLogs(request.Logs)
		if err != nil {
			return nil, err
		}
		if crossed {
			shouldFlush = true
		}
	}

	if len(request.OutputDeltas) > 0 {
		crossed, err := r.accumulator.AddOutputDeltas(request.OutputDeltas)
		if err != nil {
			return nil, err
		}
		if crossed {
			shouldFlush = true
		}
	}

	if shouldFlush {
		r.flushToBuffer()
	}

	return nil, nil
}

// handleStatus returns a minimal liveness and health response. This
// is the only unauthenticated action — it reveals operational
// metrics but no telemetry content.
func (r *Relay) handleStatus(_ context.Context, _ []byte) (any, error) {
	return &relayStatusResponse{
		UptimeSeconds:        r.clock.Now().Sub(r.startedAt).Seconds(),
		AccumulatorSizeBytes: r.accumulator.SizeBytes(),
		BufferEntries:        r.buffer.Len(),
		BufferSizeBytes:      r.buffer.SizeBytes(),
		BatchesShipped:       r.shipped.Load(),
		BatchesDropped:       r.buffer.Dropped(),
		SequenceNumber:       r.accumulator.SequenceNumber(),
	}, nil
}

// flushToBuffer drains the accumulator into a TelemetryBatch,
// CBOR-encodes it, and pushes it into the buffer for the shipper to
// send. Called both by the submit handler (when the size threshold is
// crossed) and by the periodic flush loop.
func (r *Relay) flushToBuffer() {
	batch := r.accumulator.Flush()
	if batch == nil {
		return
	}

	data, err := codec.Marshal(batch)
	if err != nil {
		r.logger.Error("failed to marshal telemetry batch",
			"error", err,
			"sequence_number", batch.SequenceNumber,
		)
		return
	}

	if err := r.buffer.Push(data); err != nil {
		r.logger.Error("failed to push batch to buffer",
			"error", err,
			"size", len(data),
			"sequence_number", batch.SequenceNumber,
		)
	}
}

// runFlushLoop periodically flushes the accumulator into the buffer.
// This ensures data ships even when the accumulator never reaches the
// size threshold (low-traffic machines). The loop runs until the
// context is cancelled.
func (r *Relay) runFlushLoop(ctx context.Context, interval time.Duration) {
	ticker := r.clock.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.flushToBuffer()
		case <-ctx.Done():
			return
		}
	}
}
