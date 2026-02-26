// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// testFleet constructs a ref.Fleet for test use. Panics on failure.
func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	fleet, err := ref.ParseFleetRoomAlias("#test_bureau/fleet/prod:bureau.test")
	if err != nil {
		t.Fatalf("parse fleet: %v", err)
	}
	return fleet
}

// testMachine constructs a ref.Machine for test use. Panics on failure.
func testMachine(t *testing.T) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID("@test_bureau/fleet/prod/machine/gpu-box:bureau.test")
	if err != nil {
		t.Fatalf("parse machine: %v", err)
	}
	return machine
}

// testEntity constructs a ref.Entity for test use. Panics on failure.
func testEntity(t *testing.T) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID("@test_bureau/fleet/prod/service/proxy:bureau.test")
	if err != nil {
		t.Fatalf("parse entity: %v", err)
	}
	return entity
}

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// --- TraceID and SpanID ---

func TestTraceIDRoundTrip(t *testing.T) {
	original := TraceID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}

	text, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	want := "0123456789abcdeffedcba9876543210"
	if string(text) != want {
		t.Errorf("MarshalText = %q, want %q", text, want)
	}

	var decoded TraceID
	if err := decoded.UnmarshalText(text); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %x, want %x", decoded, original)
	}
}

func TestTraceIDZeroValue(t *testing.T) {
	var zero TraceID
	if !zero.IsZero() {
		t.Error("zero-value TraceID.IsZero() = false, want true")
	}
	text, err := zero.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(text) != "00000000000000000000000000000000" {
		t.Errorf("zero MarshalText = %q, want all zeros", text)
	}
}

func TestTraceIDEmptyUnmarshal(t *testing.T) {
	var id TraceID
	if err := id.UnmarshalText([]byte("")); err != nil {
		t.Fatalf("UnmarshalText empty: %v", err)
	}
	if !id.IsZero() {
		t.Error("UnmarshalText empty should produce zero value")
	}
}

func TestTraceIDInvalidHex(t *testing.T) {
	var id TraceID
	if err := id.UnmarshalText([]byte("not-hex")); err == nil {
		t.Error("expected error for invalid hex, got nil")
	}
}

func TestTraceIDWrongLength(t *testing.T) {
	var id TraceID
	if err := id.UnmarshalText([]byte("0123456789abcdef")); err == nil {
		t.Error("expected error for wrong-length hex (8 bytes), got nil")
	}
}

func TestSpanIDRoundTrip(t *testing.T) {
	original := SpanID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}

	text, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	want := "0123456789abcdef"
	if string(text) != want {
		t.Errorf("MarshalText = %q, want %q", text, want)
	}

	var decoded SpanID
	if err := decoded.UnmarshalText(text); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %x, want %x", decoded, original)
	}
}

func TestSpanIDZeroValue(t *testing.T) {
	var zero SpanID
	if !zero.IsZero() {
		t.Error("zero-value SpanID.IsZero() = false, want true")
	}
}

// --- Span ---

func TestSpanJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := Span{
		TraceID:      TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00, 0xee, 0xff},
		SpanID:       SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		ParentSpanID: SpanID{0xf0, 0xe0, 0xd0, 0xc0, 0xb0, 0xa0, 0x90, 0x80},
		Fleet:        fleet,
		Machine:      machine,
		Source:       source,
		Operation:    "proxy.forward",
		StartTime:    1708523456000000000,
		Duration:     42000000,
		Status:       SpanStatusOK,
		Attributes:   map[string]any{"http.method": "POST", "http.status_code": float64(200)},
		Events: []SpanEvent{
			{Name: "cache.miss", Timestamp: 1708523456001000000},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Verify field names match the design doc.
	assertField(t, raw, "trace_id", "aabbccdd11223344556677889900eeff")
	assertField(t, raw, "span_id", "0102030405060708")
	assertField(t, raw, "parent_span_id", "f0e0d0c0b0a09080")
	assertField(t, raw, "operation", "proxy.forward")
	assertField(t, raw, "start_time", float64(1708523456000000000))
	assertField(t, raw, "duration", float64(42000000))
	assertField(t, raw, "status", float64(1))

	// Verify ref types serialize as their text representation.
	if _, ok := raw["fleet"]; !ok {
		t.Error("fleet field missing")
	}
	if _, ok := raw["machine"]; !ok {
		t.Error("machine field missing")
	}
	if _, ok := raw["source"]; !ok {
		t.Error("source field missing")
	}

	// Verify nested structures.
	attrs, ok := raw["attributes"].(map[string]any)
	if !ok {
		t.Fatal("attributes field missing or wrong type")
	}
	assertField(t, attrs, "http.method", "POST")

	events, ok := raw["events"].([]any)
	if !ok {
		t.Fatal("events field missing or wrong type")
	}
	if len(events) != 1 {
		t.Fatalf("events length = %d, want 1", len(events))
	}
	event, ok := events[0].(map[string]any)
	if !ok {
		t.Fatal("event[0] wrong type")
	}
	assertField(t, event, "name", "cache.miss")

	// Verify JSON → struct round-trip.
	var decoded Span
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal to Span: %v", err)
	}
	if decoded.TraceID != original.TraceID {
		t.Errorf("TraceID mismatch: got %s, want %s", decoded.TraceID, original.TraceID)
	}
	if decoded.SpanID != original.SpanID {
		t.Errorf("SpanID mismatch: got %s, want %s", decoded.SpanID, original.SpanID)
	}
	if decoded.Operation != original.Operation {
		t.Errorf("Operation = %q, want %q", decoded.Operation, original.Operation)
	}
	if decoded.Duration != original.Duration {
		t.Errorf("Duration = %d, want %d", decoded.Duration, original.Duration)
	}
	if decoded.Status != original.Status {
		t.Errorf("Status = %d, want %d", decoded.Status, original.Status)
	}
}

func TestSpanStatusMessageOmitted(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	span := Span{
		TraceID:   TraceID{1},
		SpanID:    SpanID{1},
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Operation: "test.op",
		StartTime: 1708523456000000000,
		Status:    SpanStatusOK,
	}

	data, err := json.Marshal(span)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if _, ok := raw["status_message"]; ok {
		t.Error("status_message should be omitted when empty")
	}
	if _, ok := raw["attributes"]; ok {
		t.Error("attributes should be omitted when nil")
	}
	if _, ok := raw["events"]; ok {
		t.Error("events should be omitted when nil")
	}
}

// --- MetricPoint ---

func TestMetricPointGaugeJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := MetricPoint{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Name:      "bureau_sandbox_count",
		Labels:    map[string]string{"state": "running"},
		Kind:      MetricKindGauge,
		Timestamp: 1708523456000000000,
		Value:     7.0,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	assertField(t, raw, "name", "bureau_sandbox_count")
	assertField(t, raw, "kind", float64(0))
	assertField(t, raw, "timestamp", float64(1708523456000000000))
	assertField(t, raw, "value", float64(7))

	labels, ok := raw["labels"].(map[string]any)
	if !ok {
		t.Fatal("labels field missing or wrong type")
	}
	assertField(t, labels, "state", "running")

	if _, ok := raw["histogram"]; ok {
		t.Error("histogram should be omitted for gauge metrics")
	}
}

func TestMetricPointZeroGaugePreserved(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := MetricPoint{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Name:      "bureau_sandbox_count",
		Kind:      MetricKindGauge,
		Timestamp: 1708523456000000000,
		Value:     0.0,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// A gauge value of 0.0 is a valid measurement (e.g., 0 active
	// connections) and must not be omitted.
	assertField(t, raw, "value", float64(0))
}

func TestMetricPointHistogramJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := MetricPoint{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Name:      "bureau_proxy_request_duration_seconds",
		Kind:      MetricKindHistogram,
		Timestamp: 1708523456000000000,
		Histogram: &HistogramValue{
			Boundaries:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
			BucketCounts: []uint64{10, 25, 50, 80, 95, 99, 100, 100, 100},
			Sum:          4.567,
			Count:        100,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded MetricPoint
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Histogram == nil {
		t.Fatal("Histogram is nil after round-trip")
	}
	if decoded.Histogram.Count != 100 {
		t.Errorf("Histogram.Count = %d, want 100", decoded.Histogram.Count)
	}
	if decoded.Histogram.Sum != 4.567 {
		t.Errorf("Histogram.Sum = %f, want 4.567", decoded.Histogram.Sum)
	}
	if len(decoded.Histogram.Boundaries) != 8 {
		t.Errorf("Histogram.Boundaries length = %d, want 8", len(decoded.Histogram.Boundaries))
	}
	if len(decoded.Histogram.BucketCounts) != 9 {
		t.Errorf("Histogram.BucketCounts length = %d, want 9", len(decoded.Histogram.BucketCounts))
	}
}

// --- LogRecord ---

func TestLogRecordJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := LogRecord{
		Fleet:      fleet,
		Machine:    machine,
		Source:     source,
		Severity:   SeverityError,
		Body:       "connection refused to upstream service",
		TraceID:    TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00, 0xee, 0xff},
		SpanID:     SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		Timestamp:  1708523456000000000,
		Attributes: map[string]any{"error": "connection refused", "retry_count": float64(3)},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	assertField(t, raw, "severity", float64(SeverityError))
	assertField(t, raw, "body", "connection refused to upstream service")
	assertField(t, raw, "trace_id", "aabbccdd11223344556677889900eeff")
	assertField(t, raw, "span_id", "0102030405060708")
	assertField(t, raw, "timestamp", float64(1708523456000000000))

	var decoded LogRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal to LogRecord: %v", err)
	}
	if decoded.Severity != SeverityError {
		t.Errorf("Severity = %d, want %d", decoded.Severity, SeverityError)
	}
	if decoded.Body != original.Body {
		t.Errorf("Body = %q, want %q", decoded.Body, original.Body)
	}
}

func TestLogRecordUncorrelated(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	record := LogRecord{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Severity:  SeverityInfo,
		Body:      "service started",
		Timestamp: 1708523456000000000,
	}

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Zero TraceID/SpanID are still present (not omitted) — they're
	// semantically meaningful ("not correlated with a trace").
	assertField(t, raw, "trace_id", "00000000000000000000000000000000")
	assertField(t, raw, "span_id", "0000000000000000")
}

// --- OutputDelta ---

func TestOutputDeltaJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := OutputDelta{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		SessionID: "session-abc-123",
		Sequence:  42,
		Stream:    OutputStreamStdout,
		Timestamp: 1708523456000000000,
		Data:      []byte("hello world\n"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "session_id", "session-abc-123")
	assertField(t, raw, "sequence", float64(42))
	assertField(t, raw, "stream", float64(OutputStreamStdout))
	assertField(t, raw, "timestamp", float64(1708523456000000000))

	// Verify ref types are present.
	if _, ok := raw["fleet"]; !ok {
		t.Error("fleet field missing")
	}
	if _, ok := raw["machine"]; !ok {
		t.Error("machine field missing")
	}
	if _, ok := raw["source"]; !ok {
		t.Error("source field missing")
	}

	// Data is base64-encoded in JSON ([]byte default encoding).
	if _, ok := raw["data"]; !ok {
		t.Error("data field missing")
	}

	// Full struct round-trip.
	var decoded OutputDelta
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal to OutputDelta: %v", err)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %q, want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.Sequence != original.Sequence {
		t.Errorf("Sequence = %d, want %d", decoded.Sequence, original.Sequence)
	}
	if decoded.Stream != original.Stream {
		t.Errorf("Stream = %d, want %d", decoded.Stream, original.Stream)
	}
	if decoded.Timestamp != original.Timestamp {
		t.Errorf("Timestamp = %d, want %d", decoded.Timestamp, original.Timestamp)
	}
	if string(decoded.Data) != string(original.Data) {
		t.Errorf("Data = %q, want %q", decoded.Data, original.Data)
	}
}

func TestOutputDeltaStreamConstants(t *testing.T) {
	// Verify stream constant values are stable (wire format).
	if OutputStreamCombined != 0 {
		t.Errorf("OutputStreamCombined = %d, want 0", OutputStreamCombined)
	}
	if OutputStreamStdout != 1 {
		t.Errorf("OutputStreamStdout = %d, want 1", OutputStreamStdout)
	}
	if OutputStreamStderr != 2 {
		t.Errorf("OutputStreamStderr = %d, want 2", OutputStreamStderr)
	}
}

// --- TelemetryBatch ---

func TestTelemetryBatchJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := TelemetryBatch{
		Machine: machine,
		Fleet:   fleet,
		Spans: []Span{
			{
				TraceID:   TraceID{1},
				SpanID:    SpanID{1},
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				Operation: "test.op",
				StartTime: 1708523456000000000,
				Duration:  1000000,
				Status:    SpanStatusOK,
			},
		},
		Metrics: []MetricPoint{
			{
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				Name:      "bureau_test_gauge",
				Kind:      MetricKindGauge,
				Timestamp: 1708523456000000000,
				Value:     42.0,
			},
		},
		Logs: []LogRecord{
			{
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				Severity:  SeverityInfo,
				Body:      "test log",
				Timestamp: 1708523456000000000,
			},
		},
		SequenceNumber: 17,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	assertField(t, raw, "sequence_number", float64(17))

	spans, ok := raw["spans"].([]any)
	if !ok {
		t.Fatal("spans field missing or wrong type")
	}
	if len(spans) != 1 {
		t.Fatalf("spans length = %d, want 1", len(spans))
	}

	metrics, ok := raw["metrics"].([]any)
	if !ok {
		t.Fatal("metrics field missing or wrong type")
	}
	if len(metrics) != 1 {
		t.Fatalf("metrics length = %d, want 1", len(metrics))
	}

	logs, ok := raw["logs"].([]any)
	if !ok {
		t.Fatal("logs field missing or wrong type")
	}
	if len(logs) != 1 {
		t.Fatalf("logs length = %d, want 1", len(logs))
	}

	// Full struct round-trip.
	var decoded TelemetryBatch
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal to TelemetryBatch: %v", err)
	}
	if decoded.SequenceNumber != 17 {
		t.Errorf("SequenceNumber = %d, want 17", decoded.SequenceNumber)
	}
	if len(decoded.Spans) != 1 {
		t.Fatalf("Spans length = %d, want 1", len(decoded.Spans))
	}
	if decoded.Spans[0].Operation != "test.op" {
		t.Errorf("Spans[0].Operation = %q, want %q", decoded.Spans[0].Operation, "test.op")
	}
}

func TestTelemetryBatchEmptySignals(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)

	batch := TelemetryBatch{
		Machine:        machine,
		Fleet:          fleet,
		SequenceNumber: 1,
	}

	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Empty signal arrays should be omitted.
	if _, ok := raw["spans"]; ok {
		t.Error("spans should be omitted when nil")
	}
	if _, ok := raw["metrics"]; ok {
		t.Error("metrics should be omitted when nil")
	}
	if _, ok := raw["logs"]; ok {
		t.Error("logs should be omitted when nil")
	}
	if _, ok := raw["output_deltas"]; ok {
		t.Error("output_deltas should be omitted when nil")
	}
}

// --- CBOR binary ID encoding ---

func TestTraceIDCBORRoundTrip(t *testing.T) {
	original := TraceID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	// CBOR byte string: 1-byte header (0x50 = major type 2, length 16)
	// + 16 raw bytes = 17 bytes total. The hex text encoding would be
	// 34 bytes (2-byte header + 32 hex chars).
	if len(data) != 17 {
		t.Errorf("CBOR encoding is %d bytes, want 17 (got %x)", len(data), data)
	}

	var decoded TraceID
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip: got %x, want %x", decoded, original)
	}
}

func TestTraceIDCBORZeroValue(t *testing.T) {
	var zero TraceID
	data, err := codec.Marshal(zero)
	if err != nil {
		t.Fatalf("CBOR Marshal zero: %v", err)
	}

	var decoded TraceID
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal zero: %v", err)
	}
	if !decoded.IsZero() {
		t.Errorf("zero-value TraceID should round-trip as zero, got %x", decoded)
	}
}

func TestSpanIDCBORRoundTrip(t *testing.T) {
	original := SpanID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	// CBOR byte string: 1-byte header (0x48 = major type 2, length 8)
	// + 8 raw bytes = 9 bytes total. The hex text encoding would be
	// 18 bytes (2-byte header + 16 hex chars).
	if len(data) != 9 {
		t.Errorf("CBOR encoding is %d bytes, want 9 (got %x)", len(data), data)
	}

	var decoded SpanID
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip: got %x, want %x", decoded, original)
	}
}

func TestSpanIDCBORZeroValue(t *testing.T) {
	var zero SpanID
	data, err := codec.Marshal(zero)
	if err != nil {
		t.Fatalf("CBOR Marshal zero: %v", err)
	}

	var decoded SpanID
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal zero: %v", err)
	}
	if !decoded.IsZero() {
		t.Errorf("zero-value SpanID should round-trip as zero, got %x", decoded)
	}
}

// --- CBOR identity omitempty ---

func TestSpanCBORZeroIdentityOmitted(t *testing.T) {
	// A span with zero-valued identity fields (as sent by the emitter
	// before the relay stamps identity from the submit envelope).
	span := Span{
		TraceID:   TraceID{0xaa},
		SpanID:    SpanID{0xbb},
		Operation: "socket.handle",
		StartTime: 1000000000,
		Duration:  500000,
		Status:    SpanStatusOK,
	}

	data, err := codec.Marshal(span)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	// Unmarshal into a map to inspect which keys are present.
	var raw map[string]any
	if err := codec.Unmarshal(data, &raw); err != nil {
		t.Fatalf("CBOR Unmarshal to map: %v", err)
	}

	// Identity fields should be omitted when zero.
	for _, key := range []string{"fleet", "machine", "source"} {
		if _, present := raw[key]; present {
			t.Errorf("field %q should be omitted for zero identity, but is present", key)
		}
	}

	// Non-identity fields must be present.
	for _, key := range []string{"trace_id", "span_id", "operation", "start_time", "duration", "status"} {
		if _, present := raw[key]; !present {
			t.Errorf("field %q should be present, but is missing", key)
		}
	}

	// ParentSpanID must be present even when zero (intentional — "no
	// parent" is semantically different from "parent unknown").
	if _, present := raw["parent_span_id"]; !present {
		t.Error("parent_span_id should be present even when zero")
	}
}

func TestSpanCBORNonZeroIdentityPresent(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	span := Span{
		TraceID:   TraceID{0xaa},
		SpanID:    SpanID{0xbb},
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Operation: "socket.handle",
		StartTime: 1000000000,
		Duration:  500000,
		Status:    SpanStatusOK,
	}

	data, err := codec.Marshal(span)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var raw map[string]any
	if err := codec.Unmarshal(data, &raw); err != nil {
		t.Fatalf("CBOR Unmarshal to map: %v", err)
	}

	// Identity fields should be present when non-zero.
	for _, key := range []string{"fleet", "machine", "source"} {
		if _, present := raw[key]; !present {
			t.Errorf("field %q should be present for non-zero identity, but is missing", key)
		}
	}
}

func TestSpanCBORZeroIdentityRoundTrip(t *testing.T) {
	// Verify that a span with zero identity marshals, unmarshals, and
	// the identity fields remain zero.
	original := Span{
		TraceID:   TraceID{0xaa},
		SpanID:    SpanID{0xbb},
		Operation: "socket.handle",
		StartTime: 1000000000,
		Duration:  500000,
		Status:    SpanStatusOK,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded Span
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	if !decoded.Fleet.IsZero() {
		t.Error("Fleet should be zero after round-trip of identity-stripped span")
	}
	if !decoded.Machine.IsZero() {
		t.Error("Machine should be zero after round-trip of identity-stripped span")
	}
	if !decoded.Source.IsZero() {
		t.Error("Source should be zero after round-trip of identity-stripped span")
	}
	if decoded.Operation != "socket.handle" {
		t.Errorf("Operation: got %q, want %q", decoded.Operation, "socket.handle")
	}
	if decoded.TraceID != original.TraceID {
		t.Errorf("TraceID: got %s, want %s", decoded.TraceID, original.TraceID)
	}
}

func TestOutputDeltaCBORZeroIdentityOmitted(t *testing.T) {
	delta := OutputDelta{
		SessionID: "session-xyz",
		Sequence:  7,
		Stream:    OutputStreamCombined,
		Timestamp: 1000000000,
		Data:      []byte("output"),
	}

	data, err := codec.Marshal(delta)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var raw map[string]any
	if err := codec.Unmarshal(data, &raw); err != nil {
		t.Fatalf("CBOR Unmarshal to map: %v", err)
	}

	// Identity fields should be omitted when zero.
	for _, key := range []string{"fleet", "machine", "source"} {
		if _, present := raw[key]; present {
			t.Errorf("field %q should be omitted for zero identity, but is present", key)
		}
	}

	// Non-identity fields must be present.
	for _, key := range []string{"session_id", "sequence", "stream", "timestamp", "data"} {
		if _, present := raw[key]; !present {
			t.Errorf("field %q should be present, but is missing", key)
		}
	}
}

func TestOutputDeltaCBORNonZeroIdentityPresent(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	delta := OutputDelta{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		SessionID: "session-xyz",
		Sequence:  7,
		Stream:    OutputStreamStdout,
		Timestamp: 1000000000,
		Data:      []byte("output"),
	}

	data, err := codec.Marshal(delta)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var raw map[string]any
	if err := codec.Unmarshal(data, &raw); err != nil {
		t.Fatalf("CBOR Unmarshal to map: %v", err)
	}

	for _, key := range []string{"fleet", "machine", "source"} {
		if _, present := raw[key]; !present {
			t.Errorf("field %q should be present for non-zero identity, but is missing", key)
		}
	}
}

// --- CBOR round-trips ---

func TestSpanCBORRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := Span{
		TraceID:       TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00, 0xee, 0xff},
		SpanID:        SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		ParentSpanID:  SpanID{0xf0, 0xe0, 0xd0, 0xc0, 0xb0, 0xa0, 0x90, 0x80},
		Fleet:         fleet,
		Machine:       machine,
		Source:        source,
		Operation:     "proxy.forward",
		StartTime:     1708523456000000000,
		Duration:      42000000,
		Status:        SpanStatusError,
		StatusMessage: "upstream timeout",
		Attributes:    map[string]any{"http.method": "POST"},
		Events: []SpanEvent{
			{Name: "retry", Timestamp: 1708523456001000000, Attributes: map[string]any{"attempt": uint64(2)}},
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded Span
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	if decoded.TraceID != original.TraceID {
		t.Errorf("TraceID: got %s, want %s", decoded.TraceID, original.TraceID)
	}
	if decoded.SpanID != original.SpanID {
		t.Errorf("SpanID: got %s, want %s", decoded.SpanID, original.SpanID)
	}
	if decoded.ParentSpanID != original.ParentSpanID {
		t.Errorf("ParentSpanID: got %s, want %s", decoded.ParentSpanID, original.ParentSpanID)
	}
	if decoded.Fleet.IsZero() {
		t.Error("Fleet is zero after CBOR round-trip")
	}
	if decoded.Machine.IsZero() {
		t.Error("Machine is zero after CBOR round-trip")
	}
	if decoded.Source.IsZero() {
		t.Error("Source is zero after CBOR round-trip")
	}
	if decoded.Operation != original.Operation {
		t.Errorf("Operation: got %q, want %q", decoded.Operation, original.Operation)
	}
	if decoded.StartTime != original.StartTime {
		t.Errorf("StartTime: got %d, want %d", decoded.StartTime, original.StartTime)
	}
	if decoded.Duration != original.Duration {
		t.Errorf("Duration: got %d, want %d", decoded.Duration, original.Duration)
	}
	if decoded.Status != SpanStatusError {
		t.Errorf("Status: got %d, want %d", decoded.Status, SpanStatusError)
	}
	if decoded.StatusMessage != "upstream timeout" {
		t.Errorf("StatusMessage: got %q, want %q", decoded.StatusMessage, "upstream timeout")
	}
	if len(decoded.Events) != 1 {
		t.Fatalf("Events length: got %d, want 1", len(decoded.Events))
	}
	if decoded.Events[0].Name != "retry" {
		t.Errorf("Events[0].Name: got %q, want %q", decoded.Events[0].Name, "retry")
	}
}

func TestOutputDeltaCBORRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := OutputDelta{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		SessionID: "session-abc-123",
		Sequence:  99,
		Stream:    OutputStreamStderr,
		Timestamp: 1708523456000000000,
		Data:      []byte("\x1b[31merror: something failed\x1b[0m\n"),
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded OutputDelta
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	if decoded.Fleet.IsZero() {
		t.Error("Fleet is zero after CBOR round-trip")
	}
	if decoded.Machine.IsZero() {
		t.Error("Machine is zero after CBOR round-trip")
	}
	if decoded.Source.IsZero() {
		t.Error("Source is zero after CBOR round-trip")
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.Sequence != original.Sequence {
		t.Errorf("Sequence: got %d, want %d", decoded.Sequence, original.Sequence)
	}
	if decoded.Stream != original.Stream {
		t.Errorf("Stream: got %d, want %d", decoded.Stream, original.Stream)
	}
	if decoded.Timestamp != original.Timestamp {
		t.Errorf("Timestamp: got %d, want %d", decoded.Timestamp, original.Timestamp)
	}
	if string(decoded.Data) != string(original.Data) {
		t.Errorf("Data: got %q, want %q", decoded.Data, original.Data)
	}
}

func TestTelemetryBatchCBORRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := TelemetryBatch{
		Machine: machine,
		Fleet:   fleet,
		Spans: []Span{
			{
				TraceID:   TraceID{0xde, 0xad},
				SpanID:    SpanID{0xbe, 0xef},
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				Operation: "socket.handle",
				StartTime: 1708523456000000000,
				Duration:  500000,
				Status:    SpanStatusOK,
			},
		},
		Logs: []LogRecord{
			{
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				Severity:  SeverityWarn,
				Body:      "high memory pressure",
				Timestamp: 1708523456000000000,
			},
		},
		SequenceNumber: 42,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded TelemetryBatch
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	if decoded.SequenceNumber != 42 {
		t.Errorf("SequenceNumber: got %d, want 42", decoded.SequenceNumber)
	}
	if len(decoded.Spans) != 1 {
		t.Fatalf("Spans length: got %d, want 1", len(decoded.Spans))
	}
	if decoded.Spans[0].Operation != "socket.handle" {
		t.Errorf("Spans[0].Operation: got %q, want %q", decoded.Spans[0].Operation, "socket.handle")
	}
	if len(decoded.Logs) != 1 {
		t.Fatalf("Logs length: got %d, want 1", len(decoded.Logs))
	}
	if decoded.Logs[0].Severity != SeverityWarn {
		t.Errorf("Logs[0].Severity: got %d, want %d", decoded.Logs[0].Severity, SeverityWarn)
	}
	if decoded.Logs[0].Body != "high memory pressure" {
		t.Errorf("Logs[0].Body: got %q, want %q", decoded.Logs[0].Body, "high memory pressure")
	}
	if decoded.Machine.IsZero() {
		t.Error("Batch Machine is zero after CBOR round-trip")
	}
	if decoded.Fleet.IsZero() {
		t.Error("Batch Fleet is zero after CBOR round-trip")
	}
}

func TestTelemetryBatchWithOutputDeltasJSONRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := TelemetryBatch{
		Machine: machine,
		Fleet:   fleet,
		OutputDeltas: []OutputDelta{
			{
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				SessionID: "session-1",
				Sequence:  0,
				Stream:    OutputStreamCombined,
				Timestamp: 1708523456000000000,
				Data:      []byte("hello\n"),
			},
			{
				Fleet:     fleet,
				Machine:   machine,
				Source:    source,
				SessionID: "session-1",
				Sequence:  1,
				Stream:    OutputStreamCombined,
				Timestamp: 1708523456001000000,
				Data:      []byte("world\n"),
			},
		},
		SequenceNumber: 3,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Spans, metrics, logs should be omitted when nil.
	if _, ok := raw["spans"]; ok {
		t.Error("spans should be omitted when nil")
	}
	if _, ok := raw["metrics"]; ok {
		t.Error("metrics should be omitted when nil")
	}
	if _, ok := raw["logs"]; ok {
		t.Error("logs should be omitted when nil")
	}

	deltas, ok := raw["output_deltas"].([]any)
	if !ok {
		t.Fatal("output_deltas field missing or wrong type")
	}
	if len(deltas) != 2 {
		t.Fatalf("output_deltas length = %d, want 2", len(deltas))
	}

	// Full struct round-trip.
	var decoded TelemetryBatch
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal to TelemetryBatch: %v", err)
	}
	if decoded.SequenceNumber != 3 {
		t.Errorf("SequenceNumber = %d, want 3", decoded.SequenceNumber)
	}
	if len(decoded.OutputDeltas) != 2 {
		t.Fatalf("OutputDeltas length = %d, want 2", len(decoded.OutputDeltas))
	}
	if decoded.OutputDeltas[0].SessionID != "session-1" {
		t.Errorf("OutputDeltas[0].SessionID = %q, want %q", decoded.OutputDeltas[0].SessionID, "session-1")
	}
	if decoded.OutputDeltas[1].Sequence != 1 {
		t.Errorf("OutputDeltas[1].Sequence = %d, want 1", decoded.OutputDeltas[1].Sequence)
	}
}

func TestHistogramValueCBORRoundTrip(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	source := testEntity(t)

	original := MetricPoint{
		Fleet:     fleet,
		Machine:   machine,
		Source:    source,
		Name:      "bureau_proxy_request_duration_seconds",
		Kind:      MetricKindHistogram,
		Timestamp: 1708523456000000000,
		Histogram: &HistogramValue{
			Boundaries:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
			BucketCounts: []uint64{10, 25, 50, 80, 95, 99, 100, 100, 100},
			Sum:          4.567,
			Count:        100,
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("CBOR Marshal: %v", err)
	}

	var decoded MetricPoint
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("CBOR Unmarshal: %v", err)
	}

	if decoded.Histogram == nil {
		t.Fatal("Histogram is nil after CBOR round-trip")
	}
	if decoded.Histogram.Count != 100 {
		t.Errorf("Count: got %d, want 100", decoded.Histogram.Count)
	}
	if decoded.Histogram.Sum != 4.567 {
		t.Errorf("Sum: got %f, want 4.567", decoded.Histogram.Sum)
	}
	if len(decoded.Histogram.Boundaries) != 8 {
		t.Errorf("Boundaries length: got %d, want 8", len(decoded.Histogram.Boundaries))
	}
	if len(decoded.Histogram.BucketCounts) != 9 {
		t.Errorf("BucketCounts length: got %d, want 9", len(decoded.Histogram.BucketCounts))
	}
}
