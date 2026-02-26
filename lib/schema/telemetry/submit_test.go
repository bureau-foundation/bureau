// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func TestSubmitRequestStampIdentity(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	request := SubmitRequest{
		Fleet:   fleet,
		Machine: machine,
		Source:  source,
		Spans: []Span{
			{TraceID: TraceID{0xaa}, SpanID: SpanID{0xbb}, Operation: "test.op"},
			{TraceID: TraceID{0xcc}, SpanID: SpanID{0xdd}, Operation: "test.op2"},
		},
		Metrics: []MetricPoint{
			{Name: "test_gauge", Kind: MetricKindGauge, Value: 42},
		},
		Logs: []LogRecord{
			{Severity: SeverityInfo, Body: "test log"},
		},
	}

	request.StampIdentity()

	// Verify all spans got identity stamped.
	for i, span := range request.Spans {
		if span.Fleet.IsZero() {
			t.Errorf("Spans[%d].Fleet is zero after StampIdentity", i)
		}
		if span.Machine.IsZero() {
			t.Errorf("Spans[%d].Machine is zero after StampIdentity", i)
		}
		if span.Source.IsZero() {
			t.Errorf("Spans[%d].Source is zero after StampIdentity", i)
		}
	}

	// Verify metrics got identity stamped.
	if request.Metrics[0].Fleet.IsZero() {
		t.Error("Metrics[0].Fleet is zero after StampIdentity")
	}
	if request.Metrics[0].Machine.IsZero() {
		t.Error("Metrics[0].Machine is zero after StampIdentity")
	}
	if request.Metrics[0].Source.IsZero() {
		t.Error("Metrics[0].Source is zero after StampIdentity")
	}

	// Verify logs got identity stamped.
	if request.Logs[0].Fleet.IsZero() {
		t.Error("Logs[0].Fleet is zero after StampIdentity")
	}
	if request.Logs[0].Machine.IsZero() {
		t.Error("Logs[0].Machine is zero after StampIdentity")
	}
	if request.Logs[0].Source.IsZero() {
		t.Error("Logs[0].Source is zero after StampIdentity")
	}
}

func TestSubmitRequestCBORRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := SubmitRequest{
		Fleet:   fleet,
		Machine: machine,
		Source:  source,
		Spans: []Span{
			{
				TraceID:   TraceID{0xaa},
				SpanID:    SpanID{0xbb},
				Operation: "socket.handle",
				StartTime: 1000000000,
				Duration:  500000,
				Status:    SpanStatusOK,
			},
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded SubmitRequest
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	// Envelope identity should round-trip.
	if decoded.Fleet.IsZero() {
		t.Error("Fleet is zero after CBOR round-trip")
	}
	if decoded.Machine.IsZero() {
		t.Error("Machine is zero after CBOR round-trip")
	}
	if decoded.Source.IsZero() {
		t.Error("Source is zero after CBOR round-trip")
	}

	// Per-span identity should be zero (omitted in CBOR, decoded as
	// zero values).
	if len(decoded.Spans) != 1 {
		t.Fatalf("Spans length: got %d, want 1", len(decoded.Spans))
	}
	if !decoded.Spans[0].Fleet.IsZero() {
		t.Error("Span Fleet should be zero (identity is at envelope level)")
	}
	if !decoded.Spans[0].Machine.IsZero() {
		t.Error("Span Machine should be zero (identity is at envelope level)")
	}
	if !decoded.Spans[0].Source.IsZero() {
		t.Error("Span Source should be zero (identity is at envelope level)")
	}

	// Non-identity span fields should survive.
	if decoded.Spans[0].Operation != "socket.handle" {
		t.Errorf("Span Operation: got %q, want %q",
			decoded.Spans[0].Operation, "socket.handle")
	}
	if decoded.Spans[0].TraceID != original.Spans[0].TraceID {
		t.Errorf("Span TraceID mismatch")
	}

	// StampIdentity restores per-span identity.
	decoded.StampIdentity()
	if decoded.Spans[0].Fleet.IsZero() {
		t.Error("Span Fleet still zero after StampIdentity")
	}
	if decoded.Spans[0].Machine.IsZero() {
		t.Error("Span Machine still zero after StampIdentity")
	}
	if decoded.Spans[0].Source.IsZero() {
		t.Error("Span Source still zero after StampIdentity")
	}
}

func TestSubmitRequestEmptySlicesOmitted(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	// Only spans, no metrics or logs.
	request := SubmitRequest{
		Fleet:   fleet,
		Machine: machine,
		Source:  source,
		Spans: []Span{
			{TraceID: TraceID{0xaa}, SpanID: SpanID{0xbb}, Operation: "test"},
		},
	}

	data, err := codec.Marshal(request)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var raw map[string]any
	if err := codec.Unmarshal(data, &raw); err != nil {
		t.Fatalf("CBOR Unmarshal to map: %v", err)
	}

	if _, present := raw["spans"]; !present {
		t.Error("spans should be present")
	}
	if _, present := raw["metrics"]; present {
		t.Error("metrics should be omitted when nil")
	}
	if _, present := raw["logs"]; present {
		t.Error("logs should be omitted when nil")
	}
}
