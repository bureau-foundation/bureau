// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sync"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// Accumulator collects telemetry records (spans, metrics, logs) from
// local services and produces TelemetryBatch values on demand. It
// tracks the approximate CBOR size of accumulated records so that
// callers can trigger a flush when the accumulator crosses a size
// threshold.
//
// Thread-safe: all methods may be called concurrently. The intended
// usage pattern is that socket handlers call Add* methods while a
// background flush loop and the threshold-check logic call Flush.
type Accumulator struct {
	mu             sync.Mutex
	spans          []telemetry.Span
	metrics        []telemetry.MetricPoint
	logs           []telemetry.LogRecord
	sizeBytes      int
	sequenceNumber uint64
	fleet          ref.Fleet
	machine        ref.Machine
	flushThreshold int
}

// NewAccumulator creates an Accumulator that stamps every flushed
// batch with the given fleet and machine identity. The flushThreshold
// is the approximate CBOR byte size at which Add* methods signal that
// a flush should be triggered. A threshold of 0 disables size-based
// flushing (the caller must flush on a timer alone).
func NewAccumulator(fleet ref.Fleet, machine ref.Machine, flushThreshold int) *Accumulator {
	return &Accumulator{
		fleet:          fleet,
		machine:        machine,
		flushThreshold: flushThreshold,
	}
}

// AddSpans appends spans to the accumulator. Returns true if the
// accumulated size has crossed the flush threshold, signaling that
// the caller should call Flush. Returns an error if the incoming
// spans cannot be CBOR-marshaled (indicating malformed records that
// would fail at ship time anyway).
func (a *Accumulator) AddSpans(spans []telemetry.Span) (bool, error) {
	if len(spans) == 0 {
		return false, nil
	}
	size, err := marshalSize(spans)
	if err != nil {
		return false, fmt.Errorf("measuring span size: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spans = append(a.spans, spans...)
	a.sizeBytes += size
	return a.flushThreshold > 0 && a.sizeBytes >= a.flushThreshold, nil
}

// AddMetrics appends metric points to the accumulator. Returns true
// if the accumulated size has crossed the flush threshold.
func (a *Accumulator) AddMetrics(metrics []telemetry.MetricPoint) (bool, error) {
	if len(metrics) == 0 {
		return false, nil
	}
	size, err := marshalSize(metrics)
	if err != nil {
		return false, fmt.Errorf("measuring metric size: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metrics = append(a.metrics, metrics...)
	a.sizeBytes += size
	return a.flushThreshold > 0 && a.sizeBytes >= a.flushThreshold, nil
}

// AddLogs appends log records to the accumulator. Returns true if
// the accumulated size has crossed the flush threshold.
func (a *Accumulator) AddLogs(logs []telemetry.LogRecord) (bool, error) {
	if len(logs) == 0 {
		return false, nil
	}
	size, err := marshalSize(logs)
	if err != nil {
		return false, fmt.Errorf("measuring log size: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, logs...)
	a.sizeBytes += size
	return a.flushThreshold > 0 && a.sizeBytes >= a.flushThreshold, nil
}

// Flush atomically drains the accumulated records into a
// TelemetryBatch. Returns nil if no records have been accumulated
// since the last flush. Each non-nil flush increments the sequence
// number. The returned batch carries the fleet and machine identity
// configured at construction time.
func (a *Accumulator) Flush() *telemetry.TelemetryBatch {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.spans) == 0 && len(a.metrics) == 0 && len(a.logs) == 0 {
		return nil
	}

	batch := &telemetry.TelemetryBatch{
		Machine:        a.machine,
		Fleet:          a.fleet,
		Spans:          a.spans,
		Metrics:        a.metrics,
		Logs:           a.logs,
		SequenceNumber: a.sequenceNumber,
	}

	a.spans = nil
	a.metrics = nil
	a.logs = nil
	a.sizeBytes = 0
	a.sequenceNumber++

	return batch
}

// SizeBytes returns the approximate CBOR byte size of the currently
// accumulated records.
func (a *Accumulator) SizeBytes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sizeBytes
}

// SequenceNumber returns the sequence number that will be assigned
// to the next flushed batch.
func (a *Accumulator) SequenceNumber() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sequenceNumber
}

// marshalSize returns the CBOR-encoded size of v. This is used to
// estimate the contribution of incoming records to the eventual batch
// size. The estimate is slightly high (each Add call includes its own
// CBOR array header) but accurate enough for threshold detection.
func marshalSize(v any) (int, error) {
	data, err := codec.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
