// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// testTimestamp is a fixed timestamp for metric collection in tests.
// The actual value is irrelevant — collect() writes it into MetricPoint
// output but no test logic depends on it.
const testTimestamp int64 = 1735689600_000000000 // 2025-01-01T00:00:00Z in nanos

func TestProxyMetricsObserve(t *testing.T) {
	metrics := newProxyMetrics()

	// Record a few requests.
	metrics.observe("POST", "openai:/v1/chat", "200", 100*time.Millisecond, 1)
	metrics.observe("POST", "openai:/v1/chat", "200", 200*time.Millisecond, 1)
	metrics.observe("GET", "matrix:/_matrix/client", "404", 5*time.Millisecond, 1)
	metrics.observe("POST", "openai:/v1/chat", "500", 2*time.Second, 1)

	points := metrics.collect(testTimestamp)

	if len(points) == 0 {
		t.Fatal("expected metric points, got none")
	}

	// Categorize the points.
	var counterPoints, histogramPoints, credentialPoints []telemetry.MetricPoint
	for _, point := range points {
		switch point.Name {
		case "bureau_proxy_request_total":
			counterPoints = append(counterPoints, point)
		case "bureau_proxy_request_duration_seconds":
			histogramPoints = append(histogramPoints, point)
		case "bureau_proxy_credential_injection_total":
			credentialPoints = append(credentialPoints, point)
		default:
			t.Errorf("unexpected metric name: %s", point.Name)
		}
	}

	// Verify counters: we had 3 unique method+status combos.
	if len(counterPoints) != 3 {
		t.Errorf("expected 3 counter points, got %d", len(counterPoints))
	}
	for _, point := range counterPoints {
		if point.Kind != telemetry.MetricKindCounter {
			t.Errorf("counter point has wrong kind: %d", point.Kind)
		}
		method := point.Labels["method"]
		status := point.Labels["status"]
		switch {
		case method == "POST" && status == "200":
			if point.Value != 2 {
				t.Errorf("POST/200 counter: want 2, got %v", point.Value)
			}
		case method == "GET" && status == "404":
			if point.Value != 1 {
				t.Errorf("GET/404 counter: want 1, got %v", point.Value)
			}
		case method == "POST" && status == "500":
			if point.Value != 1 {
				t.Errorf("POST/500 counter: want 1, got %v", point.Value)
			}
		default:
			t.Errorf("unexpected counter labels: method=%s status=%s", method, status)
		}
	}

	// Verify credential injection counter: 4 requests, each with 1 injection.
	if len(credentialPoints) != 1 {
		t.Fatalf("expected 1 credential point, got %d", len(credentialPoints))
	}
	if credentialPoints[0].Value != 4 {
		t.Errorf("credential injections: want 4, got %v", credentialPoints[0].Value)
	}

	// Verify histograms: 3 unique method+pathPattern+status combos.
	if len(histogramPoints) != 3 {
		t.Errorf("expected 3 histogram points, got %d", len(histogramPoints))
	}
	for _, point := range histogramPoints {
		if point.Kind != telemetry.MetricKindHistogram {
			t.Errorf("histogram point has wrong kind: %d", point.Kind)
		}
		if point.Histogram == nil {
			t.Error("histogram point has nil Histogram value")
			continue
		}
		if len(point.Histogram.Boundaries) != len(durationBoundaries) {
			t.Errorf("expected %d boundaries, got %d", len(durationBoundaries), len(point.Histogram.Boundaries))
		}
		if len(point.Histogram.BucketCounts) != len(durationBoundaries)+1 {
			t.Errorf("expected %d bucket counts, got %d", len(durationBoundaries)+1, len(point.Histogram.BucketCounts))
		}
	}
}

func TestProxyMetricsCollectEmpty(t *testing.T) {
	metrics := newProxyMetrics()
	points := metrics.collect(testTimestamp)
	if points != nil {
		t.Errorf("expected nil from empty metrics, got %d points", len(points))
	}
}

func TestProxyMetricsHistogramBuckets(t *testing.T) {
	metrics := newProxyMetrics()

	// Record durations that fall into specific buckets:
	// 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60
	metrics.observe("GET", "test:/path", "200", 3*time.Millisecond, 0)  // <= 0.005 bucket
	metrics.observe("GET", "test:/path", "200", 50*time.Millisecond, 0) // <= 0.05 bucket
	metrics.observe("GET", "test:/path", "200", 90*time.Second, 0)      // > 60 (overflow bucket)

	points := metrics.collect(testTimestamp)

	// Find the histogram point.
	var histogram *telemetry.HistogramValue
	for _, point := range points {
		if point.Name == "bureau_proxy_request_duration_seconds" {
			histogram = point.Histogram
			break
		}
	}

	if histogram == nil {
		t.Fatal("no histogram point found")
	}

	if histogram.Count != 3 {
		t.Errorf("histogram count: want 3, got %d", histogram.Count)
	}

	// Check sum: 0.003 + 0.05 + 90.0 = 90.053
	expectedSum := 0.003 + 0.05 + 90.0
	if histogram.Sum < expectedSum-0.001 || histogram.Sum > expectedSum+0.001 {
		t.Errorf("histogram sum: want ~%f, got %f", expectedSum, histogram.Sum)
	}

	// First bucket (<=0.005) should have 1 observation.
	if histogram.BucketCounts[0] != 1 {
		t.Errorf("bucket[0] (<=0.005): want 1, got %d", histogram.BucketCounts[0])
	}

	// Bucket for <=0.05 (index 3) should have 1 observation.
	if histogram.BucketCounts[3] != 1 {
		t.Errorf("bucket[3] (<=0.05): want 1, got %d", histogram.BucketCounts[3])
	}

	// Overflow bucket (last) should have 1 observation.
	overflowIndex := len(histogram.BucketCounts) - 1
	if histogram.BucketCounts[overflowIndex] != 1 {
		t.Errorf("overflow bucket: want 1, got %d", histogram.BucketCounts[overflowIndex])
	}
}

func TestNormalizePathPattern(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "/v1/chat"},
		{"/v1/embeddings", "/v1/embeddings"},
		{"/_matrix/client/v3/rooms/!abc:server/send/m.room.message/txn123", "/_matrix/client"},
		{"/", "/"},
		{"/health", "/health"},
		{"/a/b", "/a/b"},
		{"/a/b/c/d/e", "/a/b"},
	}

	for _, test := range tests {
		got := normalizePathPattern(test.path)
		if got != test.want {
			t.Errorf("normalizePathPattern(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestFormatTraceparent(t *testing.T) {
	traceID := telemetry.NewTraceID()
	spanID := telemetry.NewSpanID()

	traceparent := formatTraceparent(traceID, spanID)

	// Format: 00-{32 hex chars}-{16 hex chars}-01
	if len(traceparent) != 2+1+32+1+16+1+2 {
		t.Errorf("traceparent length: want %d, got %d: %q", 55, len(traceparent), traceparent)
	}

	// Must start with version 00
	if traceparent[:3] != "00-" {
		t.Errorf("traceparent must start with '00-', got %q", traceparent[:3])
	}

	// Must end with trace-flags 01 (sampled)
	if traceparent[len(traceparent)-2:] != "01" {
		t.Errorf("traceparent must end with '01', got %q", traceparent[len(traceparent)-2:])
	}
}

func TestHTTPServiceTraceparentInjection(t *testing.T) {
	var receivedTraceparent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceparent = r.Header.Get("Traceparent")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	// Verify traceparent was injected.
	if receivedTraceparent == "" {
		t.Fatal("upstream did not receive a Traceparent header")
	}

	// Validate format: 00-{32hex}-{16hex}-01
	if len(receivedTraceparent) != 55 {
		t.Errorf("traceparent length: want 55, got %d: %q", len(receivedTraceparent), receivedTraceparent)
	}
	if receivedTraceparent[:3] != "00-" {
		t.Errorf("traceparent version: want '00-', got %q", receivedTraceparent[:3])
	}
	if receivedTraceparent[len(receivedTraceparent)-3:] != "-01" {
		t.Errorf("traceparent flags: want '-01', got %q", receivedTraceparent[len(receivedTraceparent)-3:])
	}
}

func TestHTTPServiceTelemetryRecording(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer upstream.Close()

	// Create a test telemetry with direct metrics access. The emitter
	// is nil (no relay socket needed); recordRequest's span recording
	// is nil-safe, and we verify metrics directly.
	testTelemetry := &ProxyTelemetry{
		emitter: nil,
		metrics: newProxyMetrics(),
	}

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "api-key",
		},
		Credential: testCredentials(t, map[string]string{
			"api-key": "Bearer test",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	service.SetTelemetry(testTelemetry)

	// Make a few requests.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		service.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}

	// Verify metrics were recorded.
	points := testTelemetry.metrics.collect(testTimestamp)
	if len(points) == 0 {
		t.Fatal("expected metric points after requests")
	}

	// Find the request counter.
	var foundCounter bool
	for _, point := range points {
		if point.Name == "bureau_proxy_request_total" {
			foundCounter = true
			if point.Labels["method"] != "POST" {
				t.Errorf("counter method: want POST, got %s", point.Labels["method"])
			}
			if point.Labels["status"] != "200" {
				t.Errorf("counter status: want 200, got %s", point.Labels["status"])
			}
			if point.Value != 3 {
				t.Errorf("counter value: want 3, got %v", point.Value)
			}
		}
	}
	if !foundCounter {
		t.Error("no bureau_proxy_request_total point found")
	}

	// Find the credential injection counter (1 header per request, 3 requests).
	var foundCredential bool
	for _, point := range points {
		if point.Name == "bureau_proxy_credential_injection_total" {
			foundCredential = true
			if point.Value != 3 {
				t.Errorf("credential injections: want 3, got %v", point.Value)
			}
		}
	}
	if !foundCredential {
		t.Error("no bureau_proxy_credential_injection_total point found")
	}
}

func TestNilTelemetryIsNoOp(t *testing.T) {
	// Verify that nil ProxyTelemetry doesn't panic. The startTime
	// argument is a fixed value — recordRequest short-circuits on
	// nil receiver before using it.
	fixedStart := time.Unix(1735689600, 0)
	var nilTelemetry *ProxyTelemetry
	nilTelemetry.recordRequest(
		telemetry.NewTraceID(),
		telemetry.NewSpanID(),
		"test", "GET", "/path", 200,
		fixedStart, 100*time.Millisecond, 1, "",
	)
}

// testSpanCollector is a simple collector for testing.
type testSpanCollector struct {
	spans []telemetry.Span
}
