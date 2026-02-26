// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

const (
	// tailHeartbeatInterval is how often the tail handler sends a
	// heartbeat frame to connected clients. Keeps the connection alive
	// and lets clients detect a dead server.
	tailHeartbeatInterval = 10 * time.Second

	// tailEventBufferSize is the channel capacity for each tail
	// subscriber. At the relay's default 5-second flush interval,
	// 64 slots is ~5 minutes of backlog before events are dropped.
	// Subscribers detect gaps via batch SequenceNumber discontinuity.
	tailEventBufferSize = 64

	// tailControlBufferSize is the channel capacity for inbound
	// control messages from a single tail client.
	tailControlBufferSize = 8
)

// tailEvent is the internal representation of an ingested batch
// prepared for fan-out to tail subscribers. It pre-computes the
// Machine localpart and unique Source localparts so that each
// subscriber's filter check doesn't redundantly extract them.
type tailEvent struct {
	// machineLocalpart is the batch-level Machine ref's localpart,
	// used for O(1) pattern matching against the entire batch.
	machineLocalpart string

	// sourceLocalparts contains the unique Source localparts from
	// all records in the batch (spans, metrics, logs). Used for
	// per-record filtering when the machine localpart doesn't match.
	sourceLocalparts []string

	// rawBatch is the CBOR-encoded TelemetryBatch bytes, ready to
	// send to subscribers without re-encoding.
	rawBatch codec.RawMessage
}

// tailSubscriber represents a connected tail client. The ingest
// handler sends tailEvents on the events channel; the handleTail
// goroutine reads from it, applies the filter, and writes matching
// batches to the client connection.
type tailSubscriber struct {
	events chan tailEvent
	done   chan struct{}
}

// tailFrame is the CBOR frame sent to a tail client. The Type field
// distinguishes batch data from heartbeat keepalives.
//
// Wire protocol after handshake:
//
//	Server → Client: tailFrame{Type: "batch", Batch: <raw CBOR>}
//	Server → Client: tailFrame{Type: "heartbeat"}              (periodic)
//	Client → Server: tailControl{Subscribe: [...]}             (dynamic)
//	Client → Server: tailControl{Unsubscribe: [...]}           (dynamic)
type tailFrame struct {
	Type  string           `cbor:"type"`
	Batch codec.RawMessage `cbor:"batch,omitempty"`
}

// tailControl is the CBOR message received from a tail client to
// dynamically adjust the source filter. Subscribe adds glob patterns;
// Unsubscribe removes them. Patterns use the same syntax as
// principal.MatchPattern: *, **, and ? on hierarchical localpart paths.
//
// Examples of patterns:
//
//   - "my_bureau/fleet/prod/**"             — everything in a fleet
//   - "**/machine/gpu-*"                    — all machines named gpu-*
//   - "my_bureau/fleet/prod/service/relay"  — exact service match
//   - "**"                                  — all sources
type tailControl struct {
	Subscribe   []string `cbor:"subscribe,omitempty"`
	Unsubscribe []string `cbor:"unsubscribe,omitempty"`
}

// tailFilter manages the set of active glob patterns for a single tail
// subscriber. All access is serialized by the handleTail goroutine's
// select loop, so no mutex is needed.
type tailFilter struct {
	patterns []string
}

// matches reports whether the given tailEvent matches any active
// pattern. Checks the batch-level Machine localpart first (cheap, one
// check per batch), then falls back to per-record Source localparts.
// Returns false when no patterns are active (default-deny).
func (f *tailFilter) matches(event tailEvent) bool {
	if len(f.patterns) == 0 {
		return false
	}
	if principal.MatchAnyPattern(f.patterns, event.machineLocalpart) {
		return true
	}
	for _, sourceLocalpart := range event.sourceLocalparts {
		if principal.MatchAnyPattern(f.patterns, sourceLocalpart) {
			return true
		}
	}
	return false
}

// addPatterns adds glob patterns to the filter, ignoring duplicates.
func (f *tailFilter) addPatterns(patterns []string) {
	for _, pattern := range patterns {
		duplicate := false
		for _, existing := range f.patterns {
			if existing == pattern {
				duplicate = true
				break
			}
		}
		if !duplicate {
			f.patterns = append(f.patterns, pattern)
		}
	}
}

// removePatterns removes glob patterns from the filter.
func (f *tailFilter) removePatterns(patterns []string) {
	for _, pattern := range patterns {
		for i, existing := range f.patterns {
			if existing == pattern {
				f.patterns = append(f.patterns[:i], f.patterns[i+1:]...)
				break
			}
		}
	}
}

// addSubscriber registers a new tail subscriber for fan-out from the
// ingest handler. Called by handleTail when a client connects.
func (s *TelemetryService) addSubscriber(subscriber *tailSubscriber) {
	s.subscriberMu.Lock()
	s.tailSubscribers = append(s.tailSubscribers, subscriber)
	s.subscriberMu.Unlock()
}

// removeSubscriber deregisters a tail subscriber. Called by handleTail
// when a client disconnects.
func (s *TelemetryService) removeSubscriber(subscriber *tailSubscriber) {
	s.subscriberMu.Lock()
	for i, existing := range s.tailSubscribers {
		if existing == subscriber {
			s.tailSubscribers = append(s.tailSubscribers[:i], s.tailSubscribers[i+1:]...)
			break
		}
	}
	s.subscriberMu.Unlock()
}

// fanOutToSubscribers sends a tailEvent to all registered subscribers.
// Uses non-blocking sends: if a subscriber's channel is full, the
// event is dropped for that subscriber. Subscribers detect gaps via
// the batch's SequenceNumber field.
func (s *TelemetryService) fanOutToSubscribers(event tailEvent) {
	s.subscriberMu.RLock()
	defer s.subscriberMu.RUnlock()

	for _, subscriber := range s.tailSubscribers {
		select {
		case subscriber.events <- event:
		default:
			// Subscriber is slow — drop this event. The subscriber
			// detects the gap via SequenceNumber discontinuity.
		}
	}
}

// hasSubscribers reports whether any tail subscribers are registered.
// Used by the ingest handler to skip tailEvent construction when
// nobody is listening.
func (s *TelemetryService) hasSubscribers() bool {
	s.subscriberMu.RLock()
	hasAny := len(s.tailSubscribers) > 0
	s.subscriberMu.RUnlock()
	return hasAny
}

// handleTail is the streaming handler for the "tail" action. After
// authentication and grant verification, the client receives a
// readiness ack and then a bidirectional CBOR stream:
//
//   - Server → Client: tailFrame (type "batch" with raw CBOR, or
//     type "heartbeat" for keepalive)
//   - Client → Server: tailControl (subscribe/unsubscribe patterns)
//
// The client starts with an empty filter (no patterns match). It
// sends tailControl messages to subscribe to sources by glob pattern,
// matching against entity localparts. The filter supports Bureau's
// standard pathing and globbing via principal.MatchPattern.
//
// The stream stays open until the client disconnects, the context is
// cancelled, or the client sends an invalid message.
func (s *TelemetryService) handleTail(ctx context.Context, token *servicetoken.Token, _ []byte, conn net.Conn) {
	subject := token.Subject

	if !servicetoken.GrantsAllow(token.Grants, "telemetry/tail", "") {
		s.logger.Warn("tail: access denied",
			"subject", subject,
		)
		codec.NewEncoder(conn).Encode(streamAck{Error: "access denied: missing grant for telemetry/tail"})
		return
	}

	encoder := codec.NewEncoder(conn)

	// Register this subscriber BEFORE sending the readiness ack.
	// This guarantees no events are missed between the client
	// receiving the ack and the subscriber being active: by the
	// time the client sees the ack, the subscriber channel is
	// already receiving events from any concurrent ingest handlers.
	subscriber := &tailSubscriber{
		events: make(chan tailEvent, tailEventBufferSize),
		done:   make(chan struct{}),
	}
	s.addSubscriber(subscriber)

	defer func() {
		s.removeSubscriber(subscriber)
		s.logger.Info("tail stream ended", "subject", subject)
	}()

	// Send readiness signal so the client knows the stream is
	// established and can begin sending control messages.
	if err := encoder.Encode(streamAck{OK: true}); err != nil {
		s.logger.Debug("tail: failed to write ready signal",
			"subject", subject,
			"error", err,
		)
		return
	}

	s.logger.Info("tail stream started", "subject", subject)

	// Close the connection on context cancellation to unblock the
	// reader goroutine's blocking decode.
	handlerDone := make(chan struct{})
	defer close(handlerDone)

	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	// Start the reader goroutine that decodes tailControl messages
	// from the client connection.
	controlChannel := make(chan tailControl, tailControlBufferSize)
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- readTailControls(conn, controlChannel)
	}()

	filter := &tailFilter{}
	heartbeatTicker := s.clock.NewTicker(tailHeartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case event := <-subscriber.events:
			if !filter.matches(event) {
				continue
			}
			if err := encoder.Encode(tailFrame{Type: "batch", Batch: event.rawBatch}); err != nil {
				s.logger.Debug("tail: failed to write batch",
					"subject", subject,
					"error", err,
				)
				return
			}

		case control := <-controlChannel:
			filter.addPatterns(control.Subscribe)
			filter.removePatterns(control.Unsubscribe)
			s.logger.Debug("tail: filter updated",
				"subject", subject,
				"patterns", filter.patterns,
			)

		case <-heartbeatTicker.C:
			if err := encoder.Encode(tailFrame{Type: "heartbeat"}); err != nil {
				s.logger.Debug("tail: failed to write heartbeat",
					"subject", subject,
					"error", err,
				)
				return
			}

		case err := <-readerDone:
			if err != nil && ctx.Err() == nil {
				s.logger.Debug("tail: client read error",
					"subject", subject,
					"error", err,
				)
			}
			return

		case <-ctx.Done():
			return
		}
	}
}

// readTailControls reads tailControl messages from the client
// connection and sends them on the control channel. Returns nil when
// the connection is closed cleanly (EOF or closed socket); returns the
// error for any other decode failure.
func readTailControls(conn net.Conn, controls chan<- tailControl) error {
	decoder := codec.NewDecoder(conn)
	for {
		var control tailControl
		if err := decoder.Decode(&control); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if opError := (*net.OpError)(nil); errors.As(err, &opError) && opError.Err.Error() == "use of closed network connection" {
				return nil
			}
			return err
		}
		controls <- control
	}
}

// extractSourceLocalparts collects the unique Source localparts from
// all records in a telemetry batch (spans, metrics, log records, and
// output deltas). Returns nil if the batch contains no records with
// non-empty Source localparts.
func extractSourceLocalparts(batch *telemetry.TelemetryBatch) []string {
	// Pre-size for the common case: most batches have records from
	// 1-3 distinct sources.
	seen := make(map[string]struct{}, 4)

	for i := range batch.Spans {
		localpart := batch.Spans[i].Source.Localpart()
		if localpart != "" {
			seen[localpart] = struct{}{}
		}
	}
	for i := range batch.Metrics {
		localpart := batch.Metrics[i].Source.Localpart()
		if localpart != "" {
			seen[localpart] = struct{}{}
		}
	}
	for i := range batch.Logs {
		localpart := batch.Logs[i].Source.Localpart()
		if localpart != "" {
			seen[localpart] = struct{}{}
		}
	}
	for i := range batch.OutputDeltas {
		localpart := batch.OutputDeltas[i].Source.Localpart()
		if localpart != "" {
			seen[localpart] = struct{}{}
		}
	}

	if len(seen) == 0 {
		return nil
	}

	localparts := make([]string, 0, len(seen))
	for localpart := range seen {
		localparts = append(localparts, localpart)
	}
	return localparts
}
