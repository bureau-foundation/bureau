// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// handleIngest is the streaming handler for the "ingest" action.
// After authentication (handled by the socket server), the relay
// streams CBOR [telemetry.TelemetryBatch] values on the connection.
// The handler decodes each batch, updates stats, logs a summary, and
// sends an ack. The stream stays open until the relay disconnects,
// the context is cancelled, or a decode error occurs.
//
// Wire protocol after handshake:
//
//	Service → Relay: StreamAck{OK: true}          (readiness signal)
//	Relay   → Service: TelemetryBatch             (CBOR, self-delimiting)
//	Service → Relay: StreamAck{OK: true}          (per-batch ack)
//	Relay   → Service: TelemetryBatch             (next batch)
//	Service → Relay: StreamAck{OK: true}          (ack)
//	...
func (s *TelemetryService) handleIngest(ctx context.Context, token *servicetoken.Token, _ []byte, conn net.Conn) {
	relaySubject := token.Subject
	relayMachine := token.Machine

	// Check authorization grant.
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/ingest", "") {
		s.logger.Warn("ingest: access denied",
			"subject", relaySubject,
			"machine", relayMachine,
		)
		codec.NewEncoder(conn).Encode(telemetry.StreamAck{Error: "access denied: missing grant for telemetry/ingest"})
		return
	}

	encoder := codec.NewEncoder(conn)

	// Send readiness signal so the relay knows the stream is
	// established and can begin sending batches.
	if err := encoder.Encode(telemetry.StreamAck{OK: true}); err != nil {
		s.logger.Debug("ingest: failed to write ready signal",
			"subject", relaySubject,
			"error", err,
		)
		return
	}

	s.logger.Info("ingest stream started",
		"subject", relaySubject,
		"machine", relayMachine,
	)

	// Track this relay as connected.
	s.relayMu.Lock()
	s.connectedRelays++
	s.relayMu.Unlock()

	defer func() {
		s.relayMu.Lock()
		s.connectedRelays--
		s.relayMu.Unlock()
		s.logger.Info("ingest stream ended",
			"subject", relaySubject,
			"machine", relayMachine,
		)
	}()

	// Close the connection when the context is cancelled to unblock
	// any blocking read in the batch loop below. The socket server's
	// deferred conn.Close() handles the normal-return case.
	handlerDone := make(chan struct{})
	defer close(handlerDone)

	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	decoder := codec.NewDecoder(conn)

	for {
		// Decode as RawMessage first so the raw CBOR bytes are
		// available for fan-out to tail subscribers without
		// re-encoding. The typed unmarshal below validates the
		// batch structure and extracts fields for counters/logging.
		var rawBatch codec.RawMessage
		if err := decoder.Decode(&rawBatch); err != nil {
			if ctx.Err() != nil || netutil.IsExpectedCloseError(err) {
				return
			}
			s.logger.Warn("ingest: decode failed, closing stream",
				"subject", relaySubject,
				"error", err,
			)
			encoder.Encode(telemetry.StreamAck{Error: "decode error"})
			return
		}

		// Unmarshal the raw bytes into the typed struct for counter
		// updates and logging.
		var batch telemetry.TelemetryBatch
		if err := codec.Unmarshal(rawBatch, &batch); err != nil {
			s.logger.Warn("ingest: unmarshal failed, closing stream",
				"subject", relaySubject,
				"error", err,
			)
			encoder.Encode(telemetry.StreamAck{Error: "unmarshal error"})
			return
		}

		// Update ingestion counters.
		spanCount := uint64(len(batch.Spans))
		metricCount := uint64(len(batch.Metrics))
		logCount := uint64(len(batch.Logs))
		outputDeltaCount := uint64(len(batch.OutputDeltas))

		s.batchesReceived.Add(1)
		s.spansReceived.Add(spanCount)
		s.metricsReceived.Add(metricCount)
		s.logsReceived.Add(logCount)
		s.outputDeltasReceived.Add(outputDeltaCount)

		s.logger.Info("batch received",
			"machine", batch.Machine,
			"sequence", batch.SequenceNumber,
			"spans", spanCount,
			"metrics", metricCount,
			"logs", logCount,
			"output_deltas", outputDeltaCount,
		)

		// Acknowledge the batch so the relay can pop it from its
		// outbound buffer.
		if err := encoder.Encode(telemetry.StreamAck{OK: true}); err != nil {
			s.logger.Debug("ingest: failed to write ack",
				"subject", relaySubject,
				"error", err,
			)
			return
		}

		// Fan out to tail subscribers. Only build the tailEvent
		// when there are active subscribers to avoid unnecessary
		// source localpart extraction.
		if s.hasSubscribers() {
			s.fanOutToSubscribers(tailEvent{
				machineLocalpart: batch.Machine.Localpart(),
				sourceLocalparts: extractSourceLocalparts(&batch),
				rawBatch:         rawBatch,
			})
		}
	}
}
