// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"encoding/hex"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// TraceID is a 16-byte globally unique trace identifier. It correlates
// spans across services and machines within a single distributed
// operation. Serialized as a 32-character lowercase hex string in both
// JSON and CBOR (via encoding.TextMarshaler).
type TraceID [16]byte

// MarshalText implements encoding.TextMarshaler. Returns a 32-character
// lowercase hex string. A zero-value TraceID marshals as all zeros.
func (id TraceID) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(id[:])), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Parses a
// 32-character hex string into a TraceID.
func (id *TraceID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*id = TraceID{}
		return nil
	}
	decoded, err := hex.DecodeString(string(data))
	if err != nil {
		return fmt.Errorf("invalid TraceID hex: %w", err)
	}
	if len(decoded) != 16 {
		return fmt.Errorf("invalid TraceID: expected 16 bytes, got %d", len(decoded))
	}
	copy(id[:], decoded)
	return nil
}

// IsZero reports whether this is an uninitialized zero-value TraceID.
func (id TraceID) IsZero() bool { return id == TraceID{} }

// String returns the 32-character lowercase hex representation.
func (id TraceID) String() string { return hex.EncodeToString(id[:]) }

// SpanID is an 8-byte span identifier, unique within a trace.
// Serialized as a 16-character lowercase hex string in both JSON and
// CBOR (via encoding.TextMarshaler).
type SpanID [8]byte

// MarshalText implements encoding.TextMarshaler. Returns a 16-character
// lowercase hex string. A zero-value SpanID marshals as all zeros.
func (id SpanID) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(id[:])), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Parses a
// 16-character hex string into a SpanID.
func (id *SpanID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*id = SpanID{}
		return nil
	}
	decoded, err := hex.DecodeString(string(data))
	if err != nil {
		return fmt.Errorf("invalid SpanID hex: %w", err)
	}
	if len(decoded) != 8 {
		return fmt.Errorf("invalid SpanID: expected 8 bytes, got %d", len(decoded))
	}
	copy(id[:], decoded)
	return nil
}

// IsZero reports whether this is an uninitialized zero-value SpanID.
func (id SpanID) IsZero() bool { return id == SpanID{} }

// String returns the 16-character lowercase hex representation.
func (id SpanID) String() string { return hex.EncodeToString(id[:]) }

// SpanStatus indicates the outcome of a span's operation.
type SpanStatus uint8

const (
	// SpanStatusUnset means the status was not explicitly set by the
	// instrumentation. The operation may have succeeded or the
	// instrumentation did not record the outcome.
	SpanStatusUnset SpanStatus = 0

	// SpanStatusOK means the operation completed successfully.
	SpanStatusOK SpanStatus = 1

	// SpanStatusError means the operation failed. When set, the span's
	// StatusMessage field should contain a description of the error.
	SpanStatusError SpanStatus = 2
)

// MetricKind distinguishes how a metric point's value should be
// interpreted: as an instantaneous measurement, a monotonically
// increasing count, or a distribution summary.
type MetricKind uint8

const (
	// MetricKindGauge represents an instantaneous value that can go
	// up or down (e.g., queue depth, memory usage, active connections).
	MetricKindGauge MetricKind = 0

	// MetricKindCounter represents a monotonically increasing value
	// (e.g., total requests, total bytes sent). Counters only go up;
	// resets are detected by the consumer.
	MetricKindCounter MetricKind = 1

	// MetricKindHistogram represents a distribution summary with
	// bucket counts, sum, and total count. When Kind is Histogram, the
	// metric's Histogram field is populated instead of Value.
	MetricKindHistogram MetricKind = 2
)

// Severity constants for log records, following the OpenTelemetry
// severity numbering. Each named level is the minimum of its range:
// TRACE=1-4, DEBUG=5-8, INFO=9-12, WARN=13-16, ERROR=17-20,
// FATAL=21-24. Use these for filtering (e.g., "severity >= SeverityWarn"
// matches WARN, ERROR, and FATAL).
const (
	SeverityTrace uint8 = 1
	SeverityDebug uint8 = 5
	SeverityInfo  uint8 = 9
	SeverityWarn  uint8 = 13
	SeverityError uint8 = 17
	SeverityFatal uint8 = 21
)

// Span represents a unit of work within a distributed trace. A single
// agent action (e.g., "create workspace") generates spans in the proxy,
// daemon, launcher, and pipeline executor. The TraceID connects them
// into a causal chain; ParentSpanID establishes the parent-child
// relationship within that chain.
type Span struct {
	// TraceID is the globally unique identifier for the trace this
	// span belongs to. All spans in a distributed operation share the
	// same TraceID.
	TraceID TraceID `json:"trace_id"`

	// SpanID uniquely identifies this span within its trace.
	SpanID SpanID `json:"span_id"`

	// ParentSpanID identifies this span's parent. Zero for root spans
	// (the first span in a trace or a new local root).
	ParentSpanID SpanID `json:"parent_span_id"`

	// Fleet identifies the fleet this span originated in.
	Fleet ref.Fleet `json:"fleet"`

	// Machine identifies the machine where this span was recorded.
	Machine ref.Machine `json:"machine"`

	// Source identifies the process that emitted this span (a service,
	// agent, or machine-level daemon).
	Source ref.Entity `json:"source"`

	// Operation names the work this span represents, scoped by
	// convention: "socket.handle", "sync.loop", "proxy.forward",
	// "pipeline.step.shell".
	Operation string `json:"operation"`

	// StartTime is when the operation began, as Unix nanoseconds.
	StartTime int64 `json:"start_time"`

	// Duration is how long the operation took, in nanoseconds.
	Duration int64 `json:"duration"`

	// Status indicates the outcome: unset (0), ok (1), or error (2).
	Status SpanStatus `json:"status"`

	// StatusMessage describes the error when Status is SpanStatusError.
	// Empty for non-error spans.
	StatusMessage string `json:"status_message,omitempty"`

	// Attributes are operation-specific key-value pairs (e.g.,
	// "action": "list", "http.method": "POST"). Stored as JSON in
	// SQLite for queryability via json_extract().
	Attributes map[string]any `json:"attributes,omitempty"`

	// Events are timestamped annotations within the span (e.g.,
	// "retry attempt 2", "cache miss").
	Events []SpanEvent `json:"events,omitempty"`
}

// SpanEvent is a timestamped annotation within a span. Events mark
// notable points during a span's lifetime without creating a child span.
type SpanEvent struct {
	// Name identifies the event (e.g., "retry", "cache.miss",
	// "connection.established").
	Name string `json:"name"`

	// Timestamp is when the event occurred, as Unix nanoseconds.
	Timestamp int64 `json:"timestamp"`

	// Attributes are event-specific key-value pairs.
	Attributes map[string]any `json:"attributes,omitempty"`
}

// MetricPoint is a single metric observation at a point in time.
// Metrics follow Prometheus naming conventions (e.g.,
// "bureau_proxy_request_duration_seconds",
// "bureau_socket_request_total").
type MetricPoint struct {
	// Fleet identifies the fleet this metric originated in.
	Fleet ref.Fleet `json:"fleet"`

	// Machine identifies the machine where this metric was recorded.
	Machine ref.Machine `json:"machine"`

	// Source identifies the process that emitted this metric.
	Source ref.Entity `json:"source"`

	// Name is the metric name, following Prometheus conventions:
	// bureau_proxy_request_duration_seconds,
	// bureau_socket_request_total, bureau_sandbox_count.
	Name string `json:"name"`

	// Labels are typed key-value pairs for metric dimensions. Standard
	// labels include "service", "action", "status_code". Bureau ref
	// fields (Fleet, Machine, Source) are the struct fields above, not
	// labels.
	Labels map[string]string `json:"labels,omitempty"`

	// Kind distinguishes value interpretation: gauge (0), counter (1),
	// or histogram (2).
	Kind MetricKind `json:"kind"`

	// Timestamp is when this observation was recorded, as Unix
	// nanoseconds.
	Timestamp int64 `json:"timestamp"`

	// Value is the metric value for gauge and counter kinds. Zero is a
	// valid measurement (e.g., 0 active connections), so this field is
	// always serialized. Ignored when Kind is MetricKindHistogram.
	Value float64 `json:"value"`

	// Histogram is the distribution summary for histogram metrics.
	// Nil when Kind is MetricKindGauge or MetricKindCounter.
	Histogram *HistogramValue `json:"histogram,omitempty"`
}

// HistogramValue is a distribution summary for histogram metrics.
// It captures the shape of a value distribution using configurable
// bucket boundaries.
type HistogramValue struct {
	// Boundaries are the upper bounds of each bucket, in ascending
	// order. The final implicit +Inf bucket is not stored here.
	Boundaries []float64 `json:"boundaries"`

	// BucketCounts are the observation counts per bucket. Length is
	// len(Boundaries) + 1 because the +Inf bucket is explicit in
	// counts.
	BucketCounts []uint64 `json:"bucket_counts"`

	// Sum is the sum of all observed values.
	Sum float64 `json:"sum"`

	// Count is the total number of observations across all buckets.
	Count uint64 `json:"count"`
}

// LogRecord is a structured log entry with optional trace correlation.
// When TraceID and SpanID are set, the log is linked to the span that
// produced it, giving narrative context that metrics lack.
type LogRecord struct {
	// Fleet identifies the fleet this log originated in.
	Fleet ref.Fleet `json:"fleet"`

	// Machine identifies the machine where this log was recorded.
	Machine ref.Machine `json:"machine"`

	// Source identifies the process that emitted this log.
	Source ref.Entity `json:"source"`

	// Severity follows OpenTelemetry severity numbering: 1-4=TRACE,
	// 5-8=DEBUG, 9-12=INFO, 13-16=WARN, 17-20=ERROR, 21-24=FATAL.
	Severity uint8 `json:"severity"`

	// Body is the log message text.
	Body string `json:"body"`

	// TraceID links this log to a distributed trace. Zero when the log
	// is not correlated with any trace.
	TraceID TraceID `json:"trace_id"`

	// SpanID links this log to a specific span within the trace. Zero
	// when the log is not correlated with any span.
	SpanID SpanID `json:"span_id"`

	// Timestamp is when this log was recorded, as Unix nanoseconds.
	Timestamp int64 `json:"timestamp"`

	// Attributes are structured fields from the log context (e.g.,
	// "error": "connection refused", "sandbox_id": "abc123").
	Attributes map[string]any `json:"attributes,omitempty"`
}

// TelemetryBatch is the wire format for relay → telemetry service
// communication. A single CBOR message containing mixed signal types
// from one machine. The relay accumulates telemetry from local
// processes and flushes batches on a timer (default 5s) or size
// threshold (default 256 KB of CBOR).
type TelemetryBatch struct {
	// Machine identifies the originating machine. All records in this
	// batch are from processes on this machine.
	Machine ref.Machine `json:"machine"`

	// Fleet identifies the fleet. All records in this batch belong to
	// this fleet.
	Fleet ref.Fleet `json:"fleet"`

	// Spans are trace spans collected since the last flush.
	Spans []Span `json:"spans,omitempty"`

	// Metrics are metric observations collected since the last flush.
	Metrics []MetricPoint `json:"metrics,omitempty"`

	// Logs are log records collected since the last flush.
	Logs []LogRecord `json:"logs,omitempty"`

	// SequenceNumber is monotonically increasing per relay instance.
	// The telemetry service uses it to detect gaps (dropped data
	// during buffer overflow) and duplicates (retry after timeout).
	SequenceNumber uint64 `json:"sequence_number"`
}
