// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
)

// TestTelemetryMock exercises the submit→query roundtrip on the
// telemetry mock. It deploys the mock as a service, submits spans,
// metrics, and logs, then queries them back to verify storage and
// filtering.
func TestTelemetryMock(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "telem")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	mockService := deployTelemetryMock(t, admin, fleet, machine, "roundtrip")

	// Unauthenticated status: verify initial counts are zero.
	unauthClient := service.NewServiceClientFromToken(mockService.SocketPath, nil)
	var initialStatus telemetryMockStatus
	if err := unauthClient.Call(t.Context(), "status", nil, &initialStatus); err != nil {
		t.Fatalf("initial status call: %v", err)
	}
	if initialStatus.StoredSpans != 0 || initialStatus.StoredMetrics != 0 || initialStatus.StoredLogs != 0 {
		t.Fatalf("expected zero initial counts, got spans=%d metrics=%d logs=%d",
			initialStatus.StoredSpans, initialStatus.StoredMetrics, initialStatus.StoredLogs)
	}

	// Mint an authenticated token for submit and query actions.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/telemetry-test")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}
	token := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	authClient := service.NewServiceClientFromToken(mockService.SocketPath, token)

	// Submit a span, a metric, and a log in one request. Each telemetry
	// record requires Fleet, Machine, and Source — the ref types refuse
	// to marshal as zero values.
	submitFields := map[string]any{
		"spans": []telemetry.Span{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Operation: "test.operation",
			StartTime: 1000000000,
			Duration:  500000000,
			Status:    telemetry.SpanStatusOK,
		}},
		"metrics": []telemetry.MetricPoint{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Name:      "test_counter",
			Kind:      telemetry.MetricKindCounter,
			Value:     42,
			Timestamp: 1000000000,
		}},
		"logs": []telemetry.LogRecord{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Body:      "test log message",
			Severity:  telemetry.SeverityInfo,
			Timestamp: 1000000000,
		}},
	}
	if err := authClient.Call(t.Context(), "submit", submitFields, nil); err != nil {
		t.Fatalf("submit call: %v", err)
	}

	// Verify status shows updated counts.
	var afterSubmitStatus telemetryMockStatus
	if err := unauthClient.Call(t.Context(), "status", nil, &afterSubmitStatus); err != nil {
		t.Fatalf("status after submit: %v", err)
	}
	if afterSubmitStatus.StoredSpans != 1 {
		t.Fatalf("expected 1 stored span, got %d", afterSubmitStatus.StoredSpans)
	}
	if afterSubmitStatus.StoredMetrics != 1 {
		t.Fatalf("expected 1 stored metric, got %d", afterSubmitStatus.StoredMetrics)
	}
	if afterSubmitStatus.StoredLogs != 1 {
		t.Fatalf("expected 1 stored log, got %d", afterSubmitStatus.StoredLogs)
	}
	if afterSubmitStatus.Submits != 1 {
		t.Fatalf("expected 1 submit, got %d", afterSubmitStatus.Submits)
	}

	// query-spans: filter by operation.
	var spanResult telemetryMockSpanResult
	if err := authClient.Call(t.Context(), "query-spans",
		map[string]any{"operation": "test.operation"}, &spanResult); err != nil {
		t.Fatalf("query-spans by operation: %v", err)
	}
	if spanResult.Count != 1 {
		t.Fatalf("expected 1 span matching operation, got %d", spanResult.Count)
	}
	if spanResult.Spans[0].Operation != "test.operation" {
		t.Fatalf("expected operation %q, got %q", "test.operation", spanResult.Spans[0].Operation)
	}

	// query-spans: filter by operation that doesn't match.
	var noSpanResult telemetryMockSpanResult
	if err := authClient.Call(t.Context(), "query-spans",
		map[string]any{"operation": "nonexistent"}, &noSpanResult); err != nil {
		t.Fatalf("query-spans nonexistent: %v", err)
	}
	if noSpanResult.Count != 0 {
		t.Fatalf("expected 0 spans for nonexistent operation, got %d", noSpanResult.Count)
	}

	// query-metrics: filter by name.
	var metricResult telemetryMockMetricResult
	if err := authClient.Call(t.Context(), "query-metrics",
		map[string]any{"name": "test_counter"}, &metricResult); err != nil {
		t.Fatalf("query-metrics by name: %v", err)
	}
	if metricResult.Count != 1 {
		t.Fatalf("expected 1 metric matching name, got %d", metricResult.Count)
	}
	if metricResult.Metrics[0].Name != "test_counter" {
		t.Fatalf("expected metric name %q, got %q", "test_counter", metricResult.Metrics[0].Name)
	}

	// query-logs: filter by body substring.
	var logResult telemetryMockLogResult
	if err := authClient.Call(t.Context(), "query-logs",
		map[string]any{"body_contains": "test log"}, &logResult); err != nil {
		t.Fatalf("query-logs by body_contains: %v", err)
	}
	if logResult.Count != 1 {
		t.Fatalf("expected 1 log matching body, got %d", logResult.Count)
	}
	if logResult.Logs[0].Body != "test log message" {
		t.Fatalf("expected log body %q, got %q", "test log message", logResult.Logs[0].Body)
	}

	// query-logs: filter by body substring that doesn't match.
	var noLogResult telemetryMockLogResult
	if err := authClient.Call(t.Context(), "query-logs",
		map[string]any{"body_contains": "nonexistent"}, &noLogResult); err != nil {
		t.Fatalf("query-logs nonexistent: %v", err)
	}
	if noLogResult.Count != 0 {
		t.Fatalf("expected 0 logs for nonexistent body, got %d", noLogResult.Count)
	}

	// query-logs: filter by minimum severity (WARN and above).
	var severityResult telemetryMockLogResult
	if err := authClient.Call(t.Context(), "query-logs",
		map[string]any{"min_severity": telemetry.SeverityWarn}, &severityResult); err != nil {
		t.Fatalf("query-logs by min_severity: %v", err)
	}
	if severityResult.Count != 0 {
		t.Fatalf("expected 0 logs at WARN severity (our log is INFO), got %d", severityResult.Count)
	}
}

// Response types for the telemetry mock's CBOR protocol. Defined
// locally in the test file — standard pattern (fleet_test.go does the
// same for test service status).

type telemetryMockStatus struct {
	UptimeSeconds float64 `cbor:"uptime_seconds"`
	StoredSpans   int     `cbor:"stored_spans"`
	StoredMetrics int     `cbor:"stored_metrics"`
	StoredLogs    int     `cbor:"stored_logs"`
	Submits       uint64  `cbor:"submits"`
}

type telemetryMockSpanResult struct {
	Spans []telemetry.Span `cbor:"spans"`
	Count int              `cbor:"count"`
}

type telemetryMockMetricResult struct {
	Metrics []telemetry.MetricPoint `cbor:"metrics"`
	Count   int                     `cbor:"count"`
}

type telemetryMockLogResult struct {
	Logs  []telemetry.LogRecord `cbor:"logs"`
	Count int                   `cbor:"count"`
}
