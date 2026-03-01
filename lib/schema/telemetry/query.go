// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

// TracesRequest is the CBOR request for the telemetry service's
// "traces" action. All fields are optional; zero-valued fields are
// not applied as filters. Returns spans sorted by start time
// (newest first).
type TracesRequest struct {
	// TraceID filters to spans belonging to a specific trace.
	TraceID TraceID `cbor:"trace_id,omitempty"`

	// Machine filters by machine localpart (exact match).
	Machine string `cbor:"machine,omitempty"`

	// Source filters by entity localpart (exact match).
	Source string `cbor:"source,omitempty"`

	// Operation filters by operation name. Supports prefix
	// matching: "proxy." matches "proxy.forward", "proxy.auth", etc.
	Operation string `cbor:"operation,omitempty"`

	// MinDuration filters to spans with duration >= this value
	// (nanoseconds).
	MinDuration int64 `cbor:"min_duration,omitempty"`

	// Status filters by span status (0=unset, 1=ok, 2=error).
	Status *uint8 `cbor:"status,omitempty"`

	// Start is the earliest start_time to include (Unix nanoseconds).
	Start int64 `cbor:"start,omitempty"`

	// End is the latest start_time to include (Unix nanoseconds).
	End int64 `cbor:"end,omitempty"`

	// Limit is the maximum number of spans to return. Default 100.
	Limit int `cbor:"limit,omitempty"`
}

// TracesResponse is the CBOR response for the "traces" action.
type TracesResponse struct {
	Spans []Span `cbor:"spans"`
}

// MetricsRequest is the CBOR request for the telemetry service's
// "metrics" action. Name is required; all other fields are optional.
// Returns metric points sorted by timestamp (newest first).
type MetricsRequest struct {
	// Name is the metric name to query (exact match). Required.
	Name string `cbor:"name"`

	// Machine filters by machine localpart (exact match).
	Machine string `cbor:"machine,omitempty"`

	// Source filters by entity localpart (exact match).
	Source string `cbor:"source,omitempty"`

	// Labels filters by dimension labels. All specified labels
	// must match (AND semantics).
	Labels map[string]string `cbor:"labels,omitempty"`

	// Start is the earliest timestamp to include (Unix nanoseconds).
	Start int64 `cbor:"start,omitempty"`

	// End is the latest timestamp to include (Unix nanoseconds).
	End int64 `cbor:"end,omitempty"`

	// Limit is the maximum number of points to return. Default 1000.
	Limit int `cbor:"limit,omitempty"`
}

// MetricsResponse is the CBOR response for the "metrics" action.
type MetricsResponse struct {
	Metrics []MetricPoint `cbor:"metrics"`
}

// LogsRequest is the CBOR request for the telemetry service's
// "logs" action. All fields are optional. Returns log records sorted
// by timestamp (newest first).
type LogsRequest struct {
	// Machine filters by machine localpart (exact match).
	Machine string `cbor:"machine,omitempty"`

	// Source filters by entity localpart (exact match).
	Source string `cbor:"source,omitempty"`

	// MinSeverity filters to logs with severity >= this value.
	// Follows OpenTelemetry severity levels (1-24).
	MinSeverity *uint8 `cbor:"min_severity,omitempty"`

	// TraceID filters to logs correlated with a specific trace.
	TraceID TraceID `cbor:"trace_id,omitempty"`

	// Search filters to logs whose body contains this substring.
	Search string `cbor:"search,omitempty"`

	// Start is the earliest timestamp to include (Unix nanoseconds).
	Start int64 `cbor:"start,omitempty"`

	// End is the latest timestamp to include (Unix nanoseconds).
	End int64 `cbor:"end,omitempty"`

	// Limit is the maximum number of records to return. Default 100.
	Limit int `cbor:"limit,omitempty"`
}

// LogsResponse is the CBOR response for the "logs" action.
type LogsResponse struct {
	Logs []LogRecord `cbor:"logs"`
}

// TopRequest is the CBOR request for the telemetry service's "top"
// action. Returns an aggregated operational overview for a time window.
// Window is required.
type TopRequest struct {
	// Window is the lookback duration in nanoseconds (e.g.,
	// 3600000000000 for one hour). Required.
	Window int64 `cbor:"window"`

	// Machine restricts the overview to a single machine localpart.
	Machine string `cbor:"machine,omitempty"`
}

// TopResponse is the CBOR response for the "top" action. Contains
// ranked operational summaries derived from span data within the
// requested time window.
type TopResponse struct {
	// SlowestOperations lists operations ranked by P99 duration
	// (highest first). Limited to the top 10.
	SlowestOperations []OperationDuration `cbor:"slowest_operations"`

	// HighestErrorRate lists operations ranked by error rate
	// (highest first). Limited to the top 10. Only includes
	// operations with at least one error.
	HighestErrorRate []OperationErrorRate `cbor:"highest_error_rate"`

	// HighestThroughput lists operations ranked by span count
	// (highest first). Limited to the top 10.
	HighestThroughput []OperationThroughput `cbor:"highest_throughput"`

	// MachineActivity lists machines ranked by span count
	// (highest first). Omitted when the request filters to a
	// single machine.
	MachineActivity []MachineActivity `cbor:"machine_activity,omitempty"`
}

// OperationDuration is a single entry in the "slowest operations"
// ranking within a TopResponse.
type OperationDuration struct {
	Operation   string `cbor:"operation"`
	P99Duration int64  `cbor:"p99_duration"`
	Count       int64  `cbor:"count"`
}

// OperationErrorRate is a single entry in the "highest error rate"
// ranking within a TopResponse.
type OperationErrorRate struct {
	Operation  string  `cbor:"operation"`
	ErrorRate  float64 `cbor:"error_rate"`
	ErrorCount int64   `cbor:"error_count"`
	TotalCount int64   `cbor:"total_count"`
}

// OperationThroughput is a single entry in the "highest throughput"
// ranking within a TopResponse.
type OperationThroughput struct {
	Operation string `cbor:"operation"`
	Count     int64  `cbor:"count"`
}

// MachineActivity is a single entry in the "machine activity"
// ranking within a TopResponse.
type MachineActivity struct {
	Machine    string  `cbor:"machine"`
	SpanCount  int64   `cbor:"span_count"`
	ErrorCount int64   `cbor:"error_count"`
	ErrorRate  float64 `cbor:"error_rate"`
}
