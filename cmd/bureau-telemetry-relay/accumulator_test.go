// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// testFleet constructs a ref.Fleet for test use.
func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	fleet, err := ref.ParseFleetRoomAlias("#test_bureau/fleet/prod:bureau.test")
	if err != nil {
		t.Fatalf("parse fleet: %v", err)
	}
	return fleet
}

// testMachine constructs a ref.Machine for test use.
func testMachine(t *testing.T) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID("@test_bureau/fleet/prod/machine/gpu-box:bureau.test")
	if err != nil {
		t.Fatalf("parse machine: %v", err)
	}
	return machine
}

// testEntity constructs a ref.Entity for test use.
func testEntity(t *testing.T) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID("@test_bureau/fleet/prod/service/proxy:bureau.test")
	if err != nil {
		t.Fatalf("parse entity: %v", err)
	}
	return entity
}

func TestAccumulatorEmptyFlush(t *testing.T) {
	accumulator := NewAccumulator(testFleet(t), testMachine(t), 1024)

	batch := accumulator.Flush()
	if batch != nil {
		t.Fatal("expected nil batch from empty accumulator")
	}

	if accumulator.SequenceNumber() != 0 {
		t.Fatalf("expected sequence number 0 after empty flush, got %d", accumulator.SequenceNumber())
	}
}

func TestAccumulatorAddSpansAndFlush(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	spans := []telemetry.Span{
		{
			TraceID:   telemetry.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanID:    telemetry.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			Operation: "test.span",
			StartTime: 1000,
			Duration:  500,
			Status:    telemetry.SpanStatusOK,
		},
	}

	crossed, err := accumulator.AddSpans(spans)
	if err != nil {
		t.Fatalf("AddSpans: %v", err)
	}
	if crossed {
		t.Fatal("threshold should not be crossed with threshold=0")
	}

	batch := accumulator.Flush()
	if batch == nil {
		t.Fatal("expected non-nil batch")
	}

	if len(batch.Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(batch.Spans))
	}
	if batch.Spans[0].Operation != "test.span" {
		t.Fatalf("expected operation %q, got %q", "test.span", batch.Spans[0].Operation)
	}
	if batch.Fleet != fleet {
		t.Fatal("batch fleet does not match")
	}
	if batch.Machine != machine {
		t.Fatal("batch machine does not match")
	}
	if batch.SequenceNumber != 0 {
		t.Fatalf("expected sequence number 0, got %d", batch.SequenceNumber)
	}

	// After flush, accumulator is empty and sequence number incremented.
	if accumulator.SizeBytes() != 0 {
		t.Fatalf("expected 0 size after flush, got %d", accumulator.SizeBytes())
	}
	if accumulator.SequenceNumber() != 1 {
		t.Fatalf("expected sequence number 1, got %d", accumulator.SequenceNumber())
	}

	// Second flush should be nil.
	if accumulator.Flush() != nil {
		t.Fatal("expected nil batch after double flush")
	}
	// Sequence number should NOT increment on nil flush.
	if accumulator.SequenceNumber() != 1 {
		t.Fatalf("expected sequence number 1 after nil flush, got %d", accumulator.SequenceNumber())
	}
}

func TestAccumulatorAddMetricsAndLogs(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	metrics := []telemetry.MetricPoint{
		{
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			Name:      "bureau_test_gauge",
			Kind:      telemetry.MetricKindGauge,
			Timestamp: 1000,
			Value:     42.5,
		},
	}

	logs := []telemetry.LogRecord{
		{
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			Severity:  telemetry.SeverityInfo,
			Body:      "test log message",
			Timestamp: 2000,
		},
	}

	if _, err := accumulator.AddMetrics(metrics); err != nil {
		t.Fatalf("AddMetrics: %v", err)
	}
	if _, err := accumulator.AddLogs(logs); err != nil {
		t.Fatalf("AddLogs: %v", err)
	}

	batch := accumulator.Flush()
	if batch == nil {
		t.Fatal("expected non-nil batch")
	}
	if len(batch.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(batch.Metrics))
	}
	if len(batch.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(batch.Logs))
	}
	if batch.Metrics[0].Name != "bureau_test_gauge" {
		t.Fatalf("wrong metric name: %q", batch.Metrics[0].Name)
	}
	if batch.Logs[0].Body != "test log message" {
		t.Fatalf("wrong log body: %q", batch.Logs[0].Body)
	}
}

func TestAccumulatorAddOutputDeltasAndFlush(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	deltas := []telemetry.OutputDelta{
		{
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			SessionID: "session-abc",
			Sequence:  0,
			Stream:    telemetry.OutputStreamCombined,
			Timestamp: 1000,
			Data:      []byte("hello world\n"),
		},
		{
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			SessionID: "session-abc",
			Sequence:  1,
			Stream:    telemetry.OutputStreamCombined,
			Timestamp: 2000,
			Data:      []byte("second line\n"),
		},
	}

	crossed, err := accumulator.AddOutputDeltas(deltas)
	if err != nil {
		t.Fatalf("AddOutputDeltas: %v", err)
	}
	if crossed {
		t.Fatal("threshold should not be crossed with threshold=0")
	}

	batch := accumulator.Flush()
	if batch == nil {
		t.Fatal("expected non-nil batch")
	}

	if len(batch.OutputDeltas) != 2 {
		t.Fatalf("expected 2 output deltas, got %d", len(batch.OutputDeltas))
	}
	if batch.OutputDeltas[0].SessionID != "session-abc" {
		t.Fatalf("expected session_id %q, got %q", "session-abc", batch.OutputDeltas[0].SessionID)
	}
	if string(batch.OutputDeltas[0].Data) != "hello world\n" {
		t.Fatalf("expected data %q, got %q", "hello world\n", string(batch.OutputDeltas[0].Data))
	}
	if batch.OutputDeltas[1].Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", batch.OutputDeltas[1].Sequence)
	}
	if batch.Fleet != fleet {
		t.Fatal("batch fleet does not match")
	}
	if batch.Machine != machine {
		t.Fatal("batch machine does not match")
	}
	if batch.SequenceNumber != 0 {
		t.Fatalf("expected sequence number 0, got %d", batch.SequenceNumber)
	}

	// After flush, accumulator is empty.
	if accumulator.SizeBytes() != 0 {
		t.Fatalf("expected 0 size after flush, got %d", accumulator.SizeBytes())
	}
	if accumulator.Flush() != nil {
		t.Fatal("expected nil batch after double flush")
	}
}

func TestAccumulatorSizeTracking(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	if accumulator.SizeBytes() != 0 {
		t.Fatalf("expected 0 initial size, got %d", accumulator.SizeBytes())
	}

	spans := []telemetry.Span{
		{
			TraceID:   telemetry.TraceID{1},
			SpanID:    telemetry.SpanID{1},
			Fleet:     fleet,
			Machine:   machine,
			Source:    testEntity(t),
			Operation: "test.op",
			StartTime: 1000,
			Duration:  100,
		},
	}

	if _, err := accumulator.AddSpans(spans); err != nil {
		t.Fatalf("AddSpans: %v", err)
	}

	sizeAfterSpan := accumulator.SizeBytes()
	if sizeAfterSpan <= 0 {
		t.Fatalf("expected positive size after adding span, got %d", sizeAfterSpan)
	}

	// Add more records — size should increase.
	if _, err := accumulator.AddSpans(spans); err != nil {
		t.Fatalf("AddSpans: %v", err)
	}
	sizeAfterTwo := accumulator.SizeBytes()
	if sizeAfterTwo <= sizeAfterSpan {
		t.Fatalf("expected size to increase after second add: %d <= %d", sizeAfterTwo, sizeAfterSpan)
	}

	// Flush resets size.
	accumulator.Flush()
	if accumulator.SizeBytes() != 0 {
		t.Fatalf("expected 0 size after flush, got %d", accumulator.SizeBytes())
	}
}

func TestAccumulatorThresholdCrossing(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)

	// Use a small threshold so we can cross it with a few records.
	accumulator := NewAccumulator(fleet, machine, 100)

	span := telemetry.Span{
		TraceID:   telemetry.TraceID{1},
		SpanID:    telemetry.SpanID{1},
		Fleet:     fleet,
		Machine:   machine,
		Source:    testEntity(t),
		Operation: "test.op",
		StartTime: 1000,
		Duration:  100,
	}

	// Keep adding spans until the threshold is crossed.
	crossedEventually := false
	for i := 0; i < 100; i++ {
		crossed, err := accumulator.AddSpans([]telemetry.Span{span})
		if err != nil {
			t.Fatalf("AddSpans: %v", err)
		}
		if crossed {
			crossedEventually = true
			break
		}
	}

	if !crossedEventually {
		t.Fatalf("threshold of 100 bytes was never crossed after 100 spans (final size: %d)", accumulator.SizeBytes())
	}
}

func TestAccumulatorAddEmptySlices(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	crossed, err := accumulator.AddSpans(nil)
	if err != nil {
		t.Fatalf("AddSpans(nil): %v", err)
	}
	if crossed {
		t.Fatal("empty add should not cross threshold")
	}

	crossed, err = accumulator.AddMetrics(nil)
	if err != nil {
		t.Fatalf("AddMetrics(nil): %v", err)
	}
	if crossed {
		t.Fatal("empty add should not cross threshold")
	}

	crossed, err = accumulator.AddLogs(nil)
	if err != nil {
		t.Fatalf("AddLogs(nil): %v", err)
	}
	if crossed {
		t.Fatal("empty add should not cross threshold")
	}

	crossed, err = accumulator.AddOutputDeltas(nil)
	if err != nil {
		t.Fatalf("AddOutputDeltas(nil): %v", err)
	}
	if crossed {
		t.Fatal("empty add should not cross threshold")
	}

	if accumulator.SizeBytes() != 0 {
		t.Fatalf("expected 0 size after empty adds, got %d", accumulator.SizeBytes())
	}

	if accumulator.Flush() != nil {
		t.Fatal("expected nil flush after empty adds")
	}
}

func TestAccumulatorSequenceNumberIncrements(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	entity := testEntity(t)
	span := telemetry.Span{
		TraceID:   telemetry.TraceID{1},
		SpanID:    telemetry.SpanID{1},
		Fleet:     fleet,
		Machine:   machine,
		Source:    entity,
		Operation: "test.op",
	}

	for i := uint64(0); i < 5; i++ {
		if _, err := accumulator.AddSpans([]telemetry.Span{span}); err != nil {
			t.Fatalf("AddSpans: %v", err)
		}
		batch := accumulator.Flush()
		if batch == nil {
			t.Fatalf("iteration %d: expected non-nil batch", i)
		}
		if batch.SequenceNumber != i {
			t.Fatalf("iteration %d: expected sequence number %d, got %d", i, i, batch.SequenceNumber)
		}
	}

	if accumulator.SequenceNumber() != 5 {
		t.Fatalf("expected sequence number 5, got %d", accumulator.SequenceNumber())
	}
}

func TestAccumulatorMixedRecordTypes(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	entity := testEntity(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	if _, err := accumulator.AddSpans([]telemetry.Span{
		{TraceID: telemetry.TraceID{1}, SpanID: telemetry.SpanID{1}, Fleet: fleet, Machine: machine, Source: entity, Operation: "s1"},
		{TraceID: telemetry.TraceID{2}, SpanID: telemetry.SpanID{2}, Fleet: fleet, Machine: machine, Source: entity, Operation: "s2"},
	}); err != nil {
		t.Fatalf("AddSpans: %v", err)
	}

	if _, err := accumulator.AddMetrics([]telemetry.MetricPoint{
		{Fleet: fleet, Machine: machine, Source: entity, Name: "m1", Kind: telemetry.MetricKindCounter},
	}); err != nil {
		t.Fatalf("AddMetrics: %v", err)
	}

	if _, err := accumulator.AddLogs([]telemetry.LogRecord{
		{Fleet: fleet, Machine: machine, Source: entity, Severity: telemetry.SeverityError, Body: "l1"},
		{Fleet: fleet, Machine: machine, Source: entity, Severity: telemetry.SeverityWarn, Body: "l2"},
		{Fleet: fleet, Machine: machine, Source: entity, Severity: telemetry.SeverityInfo, Body: "l3"},
	}); err != nil {
		t.Fatalf("AddLogs: %v", err)
	}

	if _, err := accumulator.AddOutputDeltas([]telemetry.OutputDelta{
		{Fleet: fleet, Machine: machine, Source: entity, SessionID: "s1", Sequence: 0, Data: []byte("d1")},
		{Fleet: fleet, Machine: machine, Source: entity, SessionID: "s1", Sequence: 1, Data: []byte("d2")},
	}); err != nil {
		t.Fatalf("AddOutputDeltas: %v", err)
	}

	batch := accumulator.Flush()
	if batch == nil {
		t.Fatal("expected non-nil batch")
	}
	if len(batch.Spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(batch.Spans))
	}
	if len(batch.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(batch.Metrics))
	}
	if len(batch.Logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(batch.Logs))
	}
	if len(batch.OutputDeltas) != 2 {
		t.Fatalf("expected 2 output deltas, got %d", len(batch.OutputDeltas))
	}
}

func TestAccumulatorConcurrentAddAndFlush(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	entity := testEntity(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	const goroutines = 10
	const recordsPerGoroutine = 50

	var waitGroup sync.WaitGroup
	waitGroup.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer waitGroup.Done()
			for j := 0; j < recordsPerGoroutine; j++ {
				span := telemetry.Span{
					TraceID:   telemetry.TraceID{1},
					SpanID:    telemetry.SpanID{1},
					Fleet:     fleet,
					Machine:   machine,
					Source:    entity,
					Operation: "concurrent.span",
				}
				if _, err := accumulator.AddSpans([]telemetry.Span{span}); err != nil {
					t.Errorf("AddSpans: %v", err)
				}
			}
		}()
	}

	waitGroup.Wait()

	// All records should be present in exactly one flush.
	batch := accumulator.Flush()
	if batch == nil {
		t.Fatal("expected non-nil batch after concurrent adds")
	}
	expectedSpans := goroutines * recordsPerGoroutine
	if len(batch.Spans) != expectedSpans {
		t.Fatalf("expected %d spans, got %d", expectedSpans, len(batch.Spans))
	}
}

func TestAccumulatorConcurrentAddWithInterleavedFlush(t *testing.T) {
	fleet := testFleet(t)
	machine := testMachine(t)
	entity := testEntity(t)
	accumulator := NewAccumulator(fleet, machine, 0)

	const writers = 5
	const recordsPerWriter = 100

	var waitGroup sync.WaitGroup
	waitGroup.Add(writers + 1) // writers + 1 flusher

	// Collect all flushed batches.
	var batchMutex sync.Mutex
	var allBatches []*telemetry.TelemetryBatch

	// Writers: add records concurrently.
	for i := 0; i < writers; i++ {
		go func() {
			defer waitGroup.Done()
			for j := 0; j < recordsPerWriter; j++ {
				span := telemetry.Span{
					TraceID:   telemetry.TraceID{1},
					SpanID:    telemetry.SpanID{1},
					Fleet:     fleet,
					Machine:   machine,
					Source:    entity,
					Operation: "interleaved.span",
				}
				if _, err := accumulator.AddSpans([]telemetry.Span{span}); err != nil {
					t.Errorf("AddSpans: %v", err)
				}
			}
		}()
	}

	// Flusher: flush repeatedly while writers are active.
	go func() {
		defer waitGroup.Done()
		for i := 0; i < recordsPerWriter; i++ {
			batch := accumulator.Flush()
			if batch != nil {
				batchMutex.Lock()
				allBatches = append(allBatches, batch)
				batchMutex.Unlock()
			}
		}
	}()

	waitGroup.Wait()

	// Final flush to collect any remaining records.
	if finalBatch := accumulator.Flush(); finalBatch != nil {
		allBatches = append(allBatches, finalBatch)
	}

	// Total records across all batches must equal total written.
	totalSpans := 0
	for _, batch := range allBatches {
		totalSpans += len(batch.Spans)
	}
	expectedTotal := writers * recordsPerWriter
	if totalSpans != expectedTotal {
		t.Fatalf("expected %d total spans across all batches, got %d", expectedTotal, totalSpans)
	}

	// Sequence numbers must be consecutive starting from 0.
	for i, batch := range allBatches {
		if batch.SequenceNumber != uint64(i) {
			t.Fatalf("batch %d: expected sequence number %d, got %d", i, i, batch.SequenceNumber)
		}
	}
}
