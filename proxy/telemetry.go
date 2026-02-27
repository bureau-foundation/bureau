// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
)

// proxyMetrics accumulates request counters and duration histograms
// for periodic emission as telemetry MetricPoints. All methods are
// safe for concurrent use.
//
// Counters are cumulative (monotonically increasing). Histograms
// maintain bucket counts, sum, and total count across all
// observations since process start.
type proxyMetrics struct {
	mu sync.Mutex

	// requestTotal tracks total requests by method and status code.
	requestTotal map[requestKey]uint64

	// credentialInjections counts credential header injections.
	credentialInjections uint64

	// requestDuration tracks request duration distributions by
	// method, path pattern, and status code.
	requestDuration map[requestKey]*histogramState
}

// requestKey identifies a unique counter/histogram series.
type requestKey struct {
	Method      string
	PathPattern string
	Status      string
}

// histogramState accumulates observations into fixed buckets. The
// boundaries are shared across all histogram instances (set once at
// construction).
type histogramState struct {
	boundaries   []float64
	bucketCounts []uint64
	sum          float64
	count        uint64
}

// durationBoundaries are the histogram bucket upper bounds in seconds,
// chosen for HTTP request latency. Ranges from 5ms to 60s, covering
// fast cache hits through slow LLM streaming responses.
var durationBoundaries = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

func newProxyMetrics() *proxyMetrics {
	return &proxyMetrics{
		requestTotal:    make(map[requestKey]uint64),
		requestDuration: make(map[requestKey]*histogramState),
	}
}

// observe records a completed request. Called from the HTTP forwarding
// path after the upstream response is fully handled.
func (m *proxyMetrics) observe(method, pathPattern, status string, duration time.Duration, credentialCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Increment request counter.
	counterKey := requestKey{Method: method, Status: status}
	m.requestTotal[counterKey]++

	// Increment credential injection counter.
	m.credentialInjections += uint64(credentialCount)

	// Record duration histogram observation.
	histogramKey := requestKey{Method: method, PathPattern: pathPattern, Status: status}
	state := m.requestDuration[histogramKey]
	if state == nil {
		state = &histogramState{
			boundaries:   durationBoundaries,
			bucketCounts: make([]uint64, len(durationBoundaries)+1),
		}
		m.requestDuration[histogramKey] = state
	}

	durationSeconds := duration.Seconds()
	state.sum += durationSeconds
	state.count++

	// Find the bucket. bucketCounts has len(boundaries)+1 entries:
	// one per boundary plus the implicit +Inf bucket.
	placed := false
	for i, boundary := range state.boundaries {
		if durationSeconds <= boundary {
			state.bucketCounts[i]++
			placed = true
			break
		}
	}
	if !placed {
		state.bucketCounts[len(state.boundaries)]++
	}
}

// collect produces MetricPoint snapshots of all accumulated metrics.
// Called by the TelemetryEmitter's MetricsCollector callback during
// each flush cycle. Returns nil when there are no observations.
func (m *proxyMetrics) collect(timestamp int64) []telemetry.MetricPoint {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Count how many MetricPoints we'll produce.
	pointCount := len(m.requestTotal) + len(m.requestDuration)
	if m.credentialInjections > 0 {
		pointCount++
	}
	if pointCount == 0 {
		return nil
	}

	points := make([]telemetry.MetricPoint, 0, pointCount)

	// Request total counters.
	for key, count := range m.requestTotal {
		points = append(points, telemetry.MetricPoint{
			Name: "bureau_proxy_request_total",
			Labels: map[string]string{
				"method": key.Method,
				"status": key.Status,
			},
			Kind:      telemetry.MetricKindCounter,
			Timestamp: timestamp,
			Value:     float64(count),
		})
	}

	// Credential injection counter.
	if m.credentialInjections > 0 {
		points = append(points, telemetry.MetricPoint{
			Name:      "bureau_proxy_credential_injection_total",
			Kind:      telemetry.MetricKindCounter,
			Timestamp: timestamp,
			Value:     float64(m.credentialInjections),
		})
	}

	// Request duration histograms.
	for key, state := range m.requestDuration {
		// Copy bucket counts — the caller should not hold a reference
		// into the live histogram.
		bucketCounts := make([]uint64, len(state.bucketCounts))
		copy(bucketCounts, state.bucketCounts)
		boundaries := make([]float64, len(state.boundaries))
		copy(boundaries, state.boundaries)

		points = append(points, telemetry.MetricPoint{
			Name: "bureau_proxy_request_duration_seconds",
			Labels: map[string]string{
				"method":       key.Method,
				"path_pattern": key.PathPattern,
				"status":       key.Status,
			},
			Kind:      telemetry.MetricKindHistogram,
			Timestamp: timestamp,
			Histogram: &telemetry.HistogramValue{
				Boundaries:   boundaries,
				BucketCounts: bucketCounts,
				Sum:          state.sum,
				Count:        state.count,
			},
		})
	}

	return points
}

// ProxyTelemetry bundles the TelemetryEmitter with proxy-specific
// metrics collection. Nil-safe: all methods are no-ops when the
// receiver is nil, which happens when telemetry is not configured.
type ProxyTelemetry struct {
	emitter *service.TelemetryEmitter
	metrics *proxyMetrics
}

// NewProxyTelemetry creates a ProxyTelemetry from a TelemetryEmitter.
// Wires the metrics collector into the emitter's flush cycle.
func NewProxyTelemetry(emitter *service.TelemetryEmitter) *ProxyTelemetry {
	metrics := newProxyMetrics()
	emitter.SetMetricsCollector(func() []telemetry.MetricPoint {
		return metrics.collect(time.Now().UnixNano())
	})
	return &ProxyTelemetry{
		emitter: emitter,
		metrics: metrics,
	}
}

// recordRequest records a span and metrics for a completed HTTP proxy
// request. Safe to call on a nil receiver.
func (t *ProxyTelemetry) recordRequest(
	traceID telemetry.TraceID,
	spanID telemetry.SpanID,
	serviceName string,
	method string,
	path string,
	status int,
	startTime time.Time,
	duration time.Duration,
	credentialCount int,
	statusMessage string,
) {
	if t == nil {
		return
	}

	spanStatus := telemetry.SpanStatusOK
	if status >= 400 || status == 0 {
		spanStatus = telemetry.SpanStatusError
	}

	t.emitter.RecordSpan(telemetry.Span{
		TraceID:       traceID,
		SpanID:        spanID,
		Operation:     "proxy.forward",
		StartTime:     startTime.UnixNano(),
		Duration:      duration.Nanoseconds(),
		Status:        spanStatus,
		StatusMessage: statusMessage,
		Attributes: map[string]any{
			"http.method":      method,
			"http.path":        path,
			"http.status_code": status,
			"service":          serviceName,
		},
	})

	statusString := fmt.Sprintf("%d", status)
	pathPattern := serviceName + ":" + normalizePathPattern(path)
	t.metrics.observe(method, pathPattern, statusString, duration, credentialCount)
}

// normalizePathPattern reduces an HTTP path to a low-cardinality
// pattern suitable for metric labels. Replaces path segments that
// look like IDs (Matrix room IDs, transaction IDs, UUIDs) with "*".
func normalizePathPattern(path string) string {
	// For now, use the first two path segments. This gives patterns
	// like "/v1/chat" for OpenAI, "/_matrix/client" for Matrix,
	// which are low-cardinality enough for histogram labels.
	//
	// A more sophisticated normalizer would recognize Matrix API
	// patterns and parameterize room IDs and transaction IDs, but
	// that requires knowledge of each upstream's URL structure.
	segments := 0
	for i, char := range path {
		if char == '/' {
			segments++
			if segments == 3 {
				return path[:i]
			}
		}
	}
	return path
}

// formatTraceparent produces a W3C traceparent header value from a
// TraceID and SpanID. Format: "00-{trace_id}-{parent_id}-01"
// where 00 is the version and 01 means sampled.
func formatTraceparent(traceID telemetry.TraceID, spanID telemetry.SpanID) string {
	return "00-" + hex.EncodeToString(traceID[:]) + "-" + hex.EncodeToString(spanID[:]) + "-01"
}
