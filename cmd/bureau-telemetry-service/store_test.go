// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

var storeTestClockEpoch = time.Date(2026, 2, 28, 14, 0, 0, 0, time.UTC)

// storeTestRefs holds the typed refs used across store tests.
type storeTestRefs struct {
	fleet   ref.Fleet
	machine ref.Machine
	source  ref.Entity
}

func newStoreTestRefs(t *testing.T) storeTestRefs {
	t.Helper()

	server := ref.MustParseServerName("bureau.local")
	namespace, err := ref.NewNamespace(server, "test_bureau")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := ref.NewFleet(namespace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	machine, err := ref.NewMachine(fleet, "gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	source, err := ref.ParseEntityUserID("@test_bureau/fleet/prod/service/proxy:bureau.local")
	if err != nil {
		t.Fatal(err)
	}

	return storeTestRefs{
		fleet:   fleet,
		machine: machine,
		source:  source,
	}
}

func openTestStore(t *testing.T) (*Store, *clock.FakeClock) {
	t.Helper()

	fakeClock := clock.Fake(storeTestClockEpoch)

	store, err := OpenStore(StoreConfig{
		Path:       filepath.Join(t.TempDir(), "telemetry_test.db"),
		PoolSize:   2,
		ServerName: ref.MustParseServerName("bureau.local"),
		Clock:      fakeClock,
		Logger:     testLogger(t),
	})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return store, fakeClock
}

func TestWriteAndQuerySpans(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	traceID := telemetry.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := telemetry.SpanID{1, 2, 3, 4, 5, 6, 7, 8}

	now := storeTestClockEpoch.UnixNano()

	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID:   traceID,
				SpanID:    spanID,
				Fleet:     refs.fleet,
				Machine:   refs.machine,
				Source:    refs.source,
				Operation: "proxy.forward",
				StartTime: now,
				Duration:  5_000_000, // 5ms
				Status:    telemetry.SpanStatusOK,
				Attributes: map[string]any{
					"http.method": "POST",
					"http.path":   "/_matrix/client/v3/rooms",
				},
			},
			{
				TraceID:       traceID,
				SpanID:        telemetry.SpanID{2, 2, 2, 2, 2, 2, 2, 2},
				Fleet:         refs.fleet,
				Machine:       refs.machine,
				Source:        refs.source,
				Operation:     "proxy.forward",
				StartTime:     now + 10_000_000,
				Duration:      50_000_000, // 50ms
				Status:        telemetry.SpanStatusError,
				StatusMessage: "upstream timeout",
			},
		},
	}

	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Query all spans.
	spans, err := store.QuerySpans(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("QuerySpans (all): %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}

	// Results should be newest first.
	if spans[0].Duration != 50_000_000 {
		t.Errorf("first span duration = %d, want 50_000_000 (newest)", spans[0].Duration)
	}

	// Verify ref fields round-trip through SQLite storage.
	for i, span := range spans {
		if span.Fleet.IsZero() {
			t.Errorf("span[%d].Fleet is zero after round-trip", i)
		} else if span.Fleet.Localpart() != refs.fleet.Localpart() {
			t.Errorf("span[%d].Fleet = %q, want %q", i, span.Fleet.Localpart(), refs.fleet.Localpart())
		}
		if span.Machine.IsZero() {
			t.Errorf("span[%d].Machine is zero after round-trip", i)
		} else if span.Machine.Localpart() != refs.machine.Localpart() {
			t.Errorf("span[%d].Machine = %q, want %q", i, span.Machine.Localpart(), refs.machine.Localpart())
		}
		if span.Source.IsZero() {
			t.Errorf("span[%d].Source is zero after round-trip", i)
		} else if span.Source.Localpart() != refs.source.Localpart() {
			t.Errorf("span[%d].Source = %q, want %q", i, span.Source.Localpart(), refs.source.Localpart())
		}
	}

	// Query by trace ID.
	spans, err = store.QuerySpans(ctx, SpanFilter{TraceID: traceID})
	if err != nil {
		t.Fatalf("QuerySpans (trace): %v", err)
	}
	if len(spans) != 2 {
		t.Errorf("trace query: got %d spans, want 2", len(spans))
	}

	// Query by operation prefix.
	spans, err = store.QuerySpans(ctx, SpanFilter{Operation: "proxy"})
	if err != nil {
		t.Fatalf("QuerySpans (operation): %v", err)
	}
	if len(spans) != 2 {
		t.Errorf("operation query: got %d spans, want 2", len(spans))
	}

	// Query errors only.
	errorStatus := uint8(telemetry.SpanStatusError)
	spans, err = store.QuerySpans(ctx, SpanFilter{Status: &errorStatus})
	if err != nil {
		t.Fatalf("QuerySpans (errors): %v", err)
	}
	if len(spans) != 1 {
		t.Errorf("error query: got %d spans, want 1", len(spans))
	}
	if len(spans) > 0 && spans[0].StatusMessage != "upstream timeout" {
		t.Errorf("error span message = %q, want %q", spans[0].StatusMessage, "upstream timeout")
	}

	// Query with min duration.
	spans, err = store.QuerySpans(ctx, SpanFilter{MinDuration: 10_000_000})
	if err != nil {
		t.Fatalf("QuerySpans (min_duration): %v", err)
	}
	if len(spans) != 1 {
		t.Errorf("min_duration query: got %d spans, want 1", len(spans))
	}

	// Verify attributes round-trip.
	allSpans, err := store.QuerySpans(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("QuerySpans (all for attrs): %v", err)
	}
	foundAttributes := false
	for _, span := range allSpans {
		if len(span.Attributes) > 0 {
			foundAttributes = true
			if method, ok := span.Attributes["http.method"]; !ok || method != "POST" {
				t.Errorf("attribute http.method = %v, want POST", method)
			}
		}
	}
	if !foundAttributes {
		t.Error("no span had attributes after round-trip")
	}
}

func TestWriteAndQueryMetrics(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Metrics: []telemetry.MetricPoint{
			{
				Fleet:     refs.fleet,
				Machine:   refs.machine,
				Source:    refs.source,
				Name:      "bureau_proxy_request_total",
				Labels:    map[string]string{"status": "ok"},
				Kind:      telemetry.MetricKindCounter,
				Timestamp: now,
				Value:     142,
			},
			{
				Fleet:     refs.fleet,
				Machine:   refs.machine,
				Source:    refs.source,
				Name:      "bureau_proxy_request_total",
				Labels:    map[string]string{"status": "error"},
				Kind:      telemetry.MetricKindCounter,
				Timestamp: now + 1_000_000,
				Value:     3,
			},
		},
	}

	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Query all metrics by name.
	metrics, err := store.QueryMetrics(ctx, MetricFilter{
		Name: "bureau_proxy_request_total",
	})
	if err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("got %d metrics, want 2", len(metrics))
	}

	// Query with label filter.
	metrics, err = store.QueryMetrics(ctx, MetricFilter{
		Name:   "bureau_proxy_request_total",
		Labels: map[string]string{"status": "error"},
	})
	if err != nil {
		t.Fatalf("QueryMetrics (label filter): %v", err)
	}
	if len(metrics) != 1 {
		t.Errorf("label filter: got %d metrics, want 1", len(metrics))
	}
	if len(metrics) > 0 && metrics[0].Value != 3 {
		t.Errorf("metric value = %f, want 3", metrics[0].Value)
	}
}

func TestWriteAndQueryLogs(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	traceID := telemetry.TraceID{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	now := storeTestClockEpoch.UnixNano()

	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Logs: []telemetry.LogRecord{
			{
				Fleet:     refs.fleet,
				Machine:   refs.machine,
				Source:    refs.source,
				Severity:  telemetry.SeverityInfo,
				Body:      "pipeline step completed",
				TraceID:   traceID,
				Timestamp: now,
			},
			{
				Fleet:     refs.fleet,
				Machine:   refs.machine,
				Source:    refs.source,
				Severity:  telemetry.SeverityError,
				Body:      "connection refused: upstream service unreachable",
				TraceID:   traceID,
				Timestamp: now + 5_000_000,
			},
		},
	}

	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Query all logs.
	logs, err := store.QueryLogs(ctx, LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("got %d logs, want 2", len(logs))
	}

	// Query by severity.
	minSeverity := telemetry.SeverityError
	logs, err = store.QueryLogs(ctx, LogFilter{MinSeverity: &minSeverity})
	if err != nil {
		t.Fatalf("QueryLogs (severity): %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("severity filter: got %d logs, want 1", len(logs))
	}

	// Query by trace ID.
	logs, err = store.QueryLogs(ctx, LogFilter{TraceID: traceID})
	if err != nil {
		t.Fatalf("QueryLogs (trace): %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("trace filter: got %d logs, want 2", len(logs))
	}

	// Search by body substring.
	logs, err = store.QueryLogs(ctx, LogFilter{Search: "connection refused"})
	if err != nil {
		t.Fatalf("QueryLogs (search): %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("search filter: got %d logs, want 1", len(logs))
	}
}

func TestRetention(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	// Write data for "today" (Feb 28, 2026).
	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "today.op", StartTime: now, Duration: 1_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}
	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch (today): %v", err)
	}

	// Write data for 10 days ago (Feb 18, 2026).
	oldTime := storeTestClockEpoch.Add(-10 * 24 * time.Hour).UnixNano()
	oldBatch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{2}, SpanID: telemetry.SpanID{2},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "old.op", StartTime: oldTime, Duration: 1_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}
	if err := store.WriteBatch(ctx, oldBatch); err != nil {
		t.Fatalf("WriteBatch (old): %v", err)
	}

	// Verify both partitions exist.
	partitions := store.activePartitions()
	if len(partitions) != 2 {
		t.Fatalf("got %d partitions before retention, want 2", len(partitions))
	}

	// Run retention. The old partition (10 days ago) should be
	// dropped: 7-day span retention + 24h buffer = 8 days, and
	// the partition is 10 days old.
	if err := store.RunRetention(ctx, DefaultRetention()); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}

	partitions = store.activePartitions()
	if len(partitions) != 1 {
		t.Fatalf("got %d partitions after retention, want 1", len(partitions))
	}

	// Verify the old data is gone.
	spans, err := store.QuerySpans(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("QuerySpans after retention: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans after retention, want 1", len(spans))
	}
	if spans[0].Operation != "today.op" {
		t.Errorf("remaining span operation = %q, want %q", spans[0].Operation, "today.op")
	}
}

func TestEmptyBatchIsNoOp(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()

	batch := &telemetry.TelemetryBatch{}
	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch (empty): %v", err)
	}

	partitions := store.activePartitions()
	if len(partitions) != 0 {
		t.Errorf("empty batch created %d partitions, want 0", len(partitions))
	}
}

func TestStoreStats(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "op", StartTime: now, Duration: 1, Status: 0,
			},
		},
		Metrics: []telemetry.MetricPoint{
			{
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Name: "test_metric", Kind: 0, Timestamp: now, Value: 1,
			},
		},
		Logs: []telemetry.LogRecord{
			{
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Severity: 9, Body: "test", Timestamp: now,
			},
		},
	}

	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.PartitionCount != 1 {
		t.Errorf("PartitionCount = %d, want 1", stats.PartitionCount)
	}
	if stats.SpanCount != 1 {
		t.Errorf("SpanCount = %d, want 1", stats.SpanCount)
	}
	if stats.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1", stats.MetricCount)
	}
	if stats.LogCount != 1 {
		t.Errorf("LogCount = %d, want 1", stats.LogCount)
	}
	if stats.DatabaseSizeBytes <= 0 {
		t.Errorf("DatabaseSizeBytes = %d, want > 0", stats.DatabaseSizeBytes)
	}
}

func TestPartitionDiscoveryOnReopen(t *testing.T) {
	refs := newStoreTestRefs(t)
	dbPath := filepath.Join(t.TempDir(), "telemetry_reopen.db")
	fakeClock := clock.Fake(storeTestClockEpoch)
	ctx := context.Background()

	server := ref.MustParseServerName("bureau.local")

	// First open: write data.
	store1, err := OpenStore(StoreConfig{
		Path: dbPath, PoolSize: 2, ServerName: server, Clock: fakeClock, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("OpenStore (1): %v", err)
	}

	now := storeTestClockEpoch.UnixNano()
	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "survived.reopen", StartTime: now, Duration: 1,
				Status: telemetry.SpanStatusOK,
			},
		},
	}
	if err := store1.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close (1): %v", err)
	}

	// Second open: verify partition discovery and data survival.
	store2, err := OpenStore(StoreConfig{
		Path: dbPath, PoolSize: 2, ServerName: server, Clock: fakeClock, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("OpenStore (2): %v", err)
	}
	defer store2.Close()

	spans, err := store2.QuerySpans(ctx, SpanFilter{})
	if err != nil {
		t.Fatalf("QuerySpans after reopen: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans after reopen, want 1", len(spans))
	}
	if spans[0].Operation != "survived.reopen" {
		t.Errorf("span operation = %q, want %q", spans[0].Operation, "survived.reopen")
	}
}

func TestQueryTopBasic(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	// Insert spans with varied operations, durations, and statuses.
	batch := &telemetry.TelemetryBatch{
		Fleet:   refs.fleet,
		Machine: refs.machine,
		Spans: []telemetry.Span{
			// "proxy.forward": 3 spans, 1 error. Durations: 5ms, 50ms, 100ms.
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "proxy.forward", StartTime: now, Duration: 5_000_000,
				Status: telemetry.SpanStatusOK,
			},
			{
				TraceID: telemetry.TraceID{2}, SpanID: telemetry.SpanID{2},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "proxy.forward", StartTime: now + 1, Duration: 50_000_000,
				Status: telemetry.SpanStatusOK,
			},
			{
				TraceID: telemetry.TraceID{3}, SpanID: telemetry.SpanID{3},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "proxy.forward", StartTime: now + 2, Duration: 100_000_000,
				Status: telemetry.SpanStatusError, StatusMessage: "timeout",
			},
			// "db.query": 2 spans, 2 errors. Durations: 200ms, 300ms.
			{
				TraceID: telemetry.TraceID{4}, SpanID: telemetry.SpanID{4},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "db.query", StartTime: now + 3, Duration: 200_000_000,
				Status: telemetry.SpanStatusError, StatusMessage: "connection refused",
			},
			{
				TraceID: telemetry.TraceID{5}, SpanID: telemetry.SpanID{5},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "db.query", StartTime: now + 4, Duration: 300_000_000,
				Status: telemetry.SpanStatusError, StatusMessage: "deadlock",
			},
		},
	}

	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Query with a 1-hour window.
	result, err := store.QueryTop(ctx, TopFilter{
		Window: int64(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTop: %v", err)
	}

	// Throughput: proxy.forward=3, db.query=2.
	if len(result.HighestThroughput) != 2 {
		t.Fatalf("throughput: got %d entries, want 2", len(result.HighestThroughput))
	}
	if result.HighestThroughput[0].Operation != "proxy.forward" {
		t.Errorf("throughput[0].Operation = %q, want proxy.forward", result.HighestThroughput[0].Operation)
	}
	if result.HighestThroughput[0].Count != 3 {
		t.Errorf("throughput[0].Count = %d, want 3", result.HighestThroughput[0].Count)
	}
	if result.HighestThroughput[1].Operation != "db.query" {
		t.Errorf("throughput[1].Operation = %q, want db.query", result.HighestThroughput[1].Operation)
	}
	if result.HighestThroughput[1].Count != 2 {
		t.Errorf("throughput[1].Count = %d, want 2", result.HighestThroughput[1].Count)
	}

	// Error rate: db.query=100%, proxy.forward=33%.
	if len(result.HighestErrorRate) != 2 {
		t.Fatalf("error rate: got %d entries, want 2", len(result.HighestErrorRate))
	}
	if result.HighestErrorRate[0].Operation != "db.query" {
		t.Errorf("error_rate[0].Operation = %q, want db.query", result.HighestErrorRate[0].Operation)
	}
	if result.HighestErrorRate[0].ErrorRate != 1.0 {
		t.Errorf("error_rate[0].ErrorRate = %f, want 1.0", result.HighestErrorRate[0].ErrorRate)
	}
	if result.HighestErrorRate[0].ErrorCount != 2 {
		t.Errorf("error_rate[0].ErrorCount = %d, want 2", result.HighestErrorRate[0].ErrorCount)
	}

	// Slowest: db.query has P99=300ms, proxy.forward has P99=100ms.
	// With small counts, the P99 offset is 0, so the result is the max.
	if len(result.SlowestOperations) != 2 {
		t.Fatalf("slowest: got %d entries, want 2", len(result.SlowestOperations))
	}
	if result.SlowestOperations[0].Operation != "db.query" {
		t.Errorf("slowest[0].Operation = %q, want db.query", result.SlowestOperations[0].Operation)
	}
	if result.SlowestOperations[0].P99Duration != 300_000_000 {
		t.Errorf("slowest[0].P99Duration = %d, want 300_000_000", result.SlowestOperations[0].P99Duration)
	}
	if result.SlowestOperations[1].Operation != "proxy.forward" {
		t.Errorf("slowest[1].Operation = %q, want proxy.forward", result.SlowestOperations[1].Operation)
	}
	if result.SlowestOperations[1].P99Duration != 100_000_000 {
		t.Errorf("slowest[1].P99Duration = %d, want 100_000_000", result.SlowestOperations[1].P99Duration)
	}

	// Machine activity: one machine.
	if len(result.MachineActivity) != 1 {
		t.Fatalf("machine activity: got %d entries, want 1", len(result.MachineActivity))
	}
	if result.MachineActivity[0].SpanCount != 5 {
		t.Errorf("machine[0].SpanCount = %d, want 5", result.MachineActivity[0].SpanCount)
	}
	if result.MachineActivity[0].ErrorCount != 3 {
		t.Errorf("machine[0].ErrorCount = %d, want 3", result.MachineActivity[0].ErrorCount)
	}
}

func TestQueryTopMachineFilter(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	// Create a second machine ref.
	server := ref.MustParseServerName("bureau.local")
	namespace, err := ref.NewNamespace(server, "test_bureau")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := ref.NewFleet(namespace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	machine2, err := ref.NewMachine(fleet, "cpu-box")
	if err != nil {
		t.Fatal(err)
	}

	// Spans on machine 1.
	batch1 := &telemetry.TelemetryBatch{
		Fleet: refs.fleet, Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "proxy.forward", StartTime: now, Duration: 10_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}

	// Spans on machine 2.
	batch2 := &telemetry.TelemetryBatch{
		Fleet: refs.fleet, Machine: machine2,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{2}, SpanID: telemetry.SpanID{2},
				Fleet: refs.fleet, Machine: machine2, Source: refs.source,
				Operation: "db.query", StartTime: now + 1, Duration: 200_000_000,
				Status: telemetry.SpanStatusError,
			},
			{
				TraceID: telemetry.TraceID{3}, SpanID: telemetry.SpanID{3},
				Fleet: refs.fleet, Machine: machine2, Source: refs.source,
				Operation: "db.query", StartTime: now + 2, Duration: 300_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}

	if err := store.WriteBatch(ctx, batch1); err != nil {
		t.Fatalf("WriteBatch (machine 1): %v", err)
	}
	if err := store.WriteBatch(ctx, batch2); err != nil {
		t.Fatalf("WriteBatch (machine 2): %v", err)
	}

	// Filter to machine 2 only.
	result, err := store.QueryTop(ctx, TopFilter{
		Window:  int64(1 * time.Hour),
		Machine: machine2.Localpart(),
	})
	if err != nil {
		t.Fatalf("QueryTop (machine filter): %v", err)
	}

	// Should only see db.query operations.
	if len(result.HighestThroughput) != 1 {
		t.Fatalf("throughput: got %d entries, want 1", len(result.HighestThroughput))
	}
	if result.HighestThroughput[0].Operation != "db.query" {
		t.Errorf("throughput[0].Operation = %q, want db.query", result.HighestThroughput[0].Operation)
	}
	if result.HighestThroughput[0].Count != 2 {
		t.Errorf("throughput[0].Count = %d, want 2", result.HighestThroughput[0].Count)
	}

	// Machine activity is omitted when filtering to a single machine.
	if len(result.MachineActivity) != 0 {
		t.Errorf("machine activity: got %d entries, want 0 (single machine filter)", len(result.MachineActivity))
	}
}

func TestQueryTopEmptyStore(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()

	result, err := store.QueryTop(ctx, TopFilter{
		Window: int64(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTop (empty): %v", err)
	}

	if result.SlowestOperations == nil {
		t.Error("SlowestOperations is nil, want empty slice")
	}
	if result.HighestErrorRate == nil {
		t.Error("HighestErrorRate is nil, want empty slice")
	}
	if result.HighestThroughput == nil {
		t.Error("HighestThroughput is nil, want empty slice")
	}
	if result.MachineActivity == nil {
		t.Error("MachineActivity is nil, want empty slice")
	}
	if len(result.SlowestOperations) != 0 {
		t.Errorf("SlowestOperations has %d entries, want 0", len(result.SlowestOperations))
	}
}

func TestQueryTopWindowBoundary(t *testing.T) {
	store, fakeClock := openTestStore(t)
	ctx := context.Background()
	refs := newStoreTestRefs(t)

	now := storeTestClockEpoch.UnixNano()

	// Insert a span at the epoch.
	batch := &telemetry.TelemetryBatch{
		Fleet: refs.fleet, Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "old.op", StartTime: now, Duration: 1_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}
	if err := store.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch (old): %v", err)
	}

	// Advance clock by 2 hours and insert another span.
	fakeClock.Advance(2 * time.Hour)
	laterNow := fakeClock.Now().UnixNano()

	batch2 := &telemetry.TelemetryBatch{
		Fleet: refs.fleet, Machine: refs.machine,
		Spans: []telemetry.Span{
			{
				TraceID: telemetry.TraceID{2}, SpanID: telemetry.SpanID{2},
				Fleet: refs.fleet, Machine: refs.machine, Source: refs.source,
				Operation: "new.op", StartTime: laterNow, Duration: 2_000_000,
				Status: telemetry.SpanStatusOK,
			},
		},
	}
	if err := store.WriteBatch(ctx, batch2); err != nil {
		t.Fatalf("WriteBatch (new): %v", err)
	}

	// Query with a 1-hour window — should only see the newer span.
	result, err := store.QueryTop(ctx, TopFilter{
		Window: int64(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTop (1h window): %v", err)
	}

	if len(result.HighestThroughput) != 1 {
		t.Fatalf("throughput: got %d entries, want 1", len(result.HighestThroughput))
	}
	if result.HighestThroughput[0].Operation != "new.op" {
		t.Errorf("throughput[0].Operation = %q, want new.op", result.HighestThroughput[0].Operation)
	}

	// Query with a 3-hour window — should see both spans.
	result, err = store.QueryTop(ctx, TopFilter{
		Window: int64(3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTop (3h window): %v", err)
	}

	if len(result.HighestThroughput) != 2 {
		t.Fatalf("throughput: got %d entries, want 2", len(result.HighestThroughput))
	}
}
