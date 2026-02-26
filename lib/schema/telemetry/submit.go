// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "github.com/bureau-foundation/bureau/lib/ref"

// SubmitRequest is the wire format for the "submit" action on the
// telemetry relay and mock sockets. Local services send telemetry
// records using this structure.
//
// Identity dedup: Fleet, Machine, and Source are specified once at
// the envelope level instead of being repeated on every record. The
// receiver calls [SubmitRequest.StampIdentity] after deserialization
// to populate the per-record identity fields from the envelope.
//
// At least one of the three record slices must be non-empty.
type SubmitRequest struct {
	// Fleet identifies the fleet the submitting service belongs to.
	Fleet ref.Fleet `cbor:"fleet"`

	// Machine identifies the machine the submitting service runs on.
	Machine ref.Machine `cbor:"machine"`

	// Source identifies the process emitting telemetry.
	Source ref.Entity `cbor:"source"`

	// Spans are trace spans to submit.
	Spans []Span `cbor:"spans,omitempty"`

	// Metrics are metric observations to submit.
	Metrics []MetricPoint `cbor:"metrics,omitempty"`

	// Logs are log records to submit.
	Logs []LogRecord `cbor:"logs,omitempty"`

	// OutputDeltas are raw terminal output chunks to submit.
	OutputDeltas []OutputDelta `cbor:"output_deltas,omitempty"`
}

// StampIdentity populates Fleet, Machine, and Source on every record
// from the envelope-level identity. Call this after deserializing a
// SubmitRequest to restore per-record identity before accumulating or
// storing records.
func (r *SubmitRequest) StampIdentity() {
	for i := range r.Spans {
		r.Spans[i].Fleet = r.Fleet
		r.Spans[i].Machine = r.Machine
		r.Spans[i].Source = r.Source
	}
	for i := range r.Metrics {
		r.Metrics[i].Fleet = r.Fleet
		r.Metrics[i].Machine = r.Machine
		r.Metrics[i].Source = r.Source
	}
	for i := range r.Logs {
		r.Logs[i].Fleet = r.Fleet
		r.Logs[i].Machine = r.Machine
		r.Logs[i].Source = r.Source
	}
	for i := range r.OutputDeltas {
		r.OutputDeltas[i].Fleet = r.Fleet
		r.OutputDeltas[i].Machine = r.Machine
		r.OutputDeltas[i].Source = r.Source
	}
}
