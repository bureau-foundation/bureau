// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-telemetry-mock is a drop-in replacement for the telemetry relay
// in integration tests. It accepts the relay's submit protocol exactly
// (same submitRequest wire type), stores everything in memory, and
// exposes query actions so tests can verify telemetry was received.
//
// Other services get the mock's socket via standard m.bureau.service_binding
// resolution — zero code changes in the service under test.
//
// The binary exposes five actions:
//   - status (unauthenticated): relay-compatible operational stats plus stored counts
//   - submit (authenticated): accepts spans, metrics, logs — matches relay wire format
//   - query-spans (authenticated): filter stored spans by source, operation, or trace ID
//   - query-metrics (authenticated): filter stored metrics by source or name
//   - query-logs (authenticated): filter stored logs by source, severity, body substring, or trace ID
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, cleanup, err := service.BootstrapViaProxy(ctx, service.ProxyBootstrapConfig{
		Audience:     "telemetry",
		Description:  "Telemetry mock for integration tests",
		Capabilities: []string{"ingest"},
	})
	if err != nil {
		return err
	}
	defer cleanup()

	mock := &telemetryMock{}

	socketServer := service.NewSocketServer(boot.SocketPath, boot.Logger, boot.AuthConfig)
	socketServer.RegisterRevocationHandler()

	socketServer.Handle("status", mock.handleStatus)
	socketServer.HandleAuth("submit", mock.handleSubmit)
	socketServer.HandleAuth("query-spans", mock.handleQuerySpans)
	socketServer.HandleAuth("query-metrics", mock.handleQueryMetrics)
	socketServer.HandleAuth("query-logs", mock.handleQueryLogs)

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- socketServer.Serve(ctx)
	}()

	boot.Logger.Info("telemetry mock running",
		"principal", boot.PrincipalName,
		"socket", boot.SocketPath,
	)

	<-ctx.Done()
	boot.Logger.Info("shutting down")

	if err := <-socketDone; err != nil {
		boot.Logger.Error("socket server error", "error", err)
	}

	return nil
}

// telemetryMock stores telemetry data in memory for test assertions.
type telemetryMock struct {
	mu      sync.Mutex
	spans   []telemetry.Span
	metrics []telemetry.MetricPoint
	logs    []telemetry.LogRecord

	submits atomic.Uint64
}

// statusResponse is relay-compatible with additional stored-count
// fields for test assertions. The relay's relayStatusResponse fields
// are present (zeroed for mock-irrelevant ones like buffer_entries)
// so that callers expecting the relay wire format don't break.
type statusResponse struct {
	UptimeSeconds        float64 `cbor:"uptime_seconds"`
	AccumulatorSizeBytes int     `cbor:"accumulator_size_bytes"`
	BufferEntries        int     `cbor:"buffer_entries"`
	BufferSizeBytes      int     `cbor:"buffer_size_bytes"`
	BatchesShipped       uint64  `cbor:"batches_shipped"`
	BatchesDropped       uint64  `cbor:"batches_dropped"`
	SequenceNumber       uint64  `cbor:"sequence_number"`
	StoredSpans          int     `cbor:"stored_spans"`
	StoredMetrics        int     `cbor:"stored_metrics"`
	StoredLogs           int     `cbor:"stored_logs"`
	Submits              uint64  `cbor:"submits"`
}

// submitRequest matches the relay's wire format exactly. At least one
// of the three slices must be non-empty.
type submitRequest struct {
	Spans   []telemetry.Span        `cbor:"spans"`
	Metrics []telemetry.MetricPoint `cbor:"metrics"`
	Logs    []telemetry.LogRecord   `cbor:"logs"`
}

// spanQuery filters stored spans. All fields are optional — an empty
// query returns all spans.
type spanQuery struct {
	Source    string `cbor:"source"`
	Operation string `cbor:"operation"`
	TraceID   string `cbor:"trace_id"`
}

// spanQueryResponse is the wire format for query-spans results.
type spanQueryResponse struct {
	Spans []telemetry.Span `cbor:"spans"`
	Count int              `cbor:"count"`
}

// metricQuery filters stored metrics. All fields are optional.
type metricQuery struct {
	Source string `cbor:"source"`
	Name   string `cbor:"name"`
}

// metricQueryResponse is the wire format for query-metrics results.
type metricQueryResponse struct {
	Metrics []telemetry.MetricPoint `cbor:"metrics"`
	Count   int                     `cbor:"count"`
}

// logQuery filters stored logs. All fields are optional.
type logQuery struct {
	Source       string `cbor:"source"`
	MinSeverity  uint8  `cbor:"min_severity"`
	BodyContains string `cbor:"body_contains"`
	TraceID      string `cbor:"trace_id"`
}

// logQueryResponse is the wire format for query-logs results.
type logQueryResponse struct {
	Logs  []telemetry.LogRecord `cbor:"logs"`
	Count int                   `cbor:"count"`
}

func (m *telemetryMock) handleStatus(_ context.Context, _ []byte) (any, error) {
	m.mu.Lock()
	spanCount := len(m.spans)
	metricCount := len(m.metrics)
	logCount := len(m.logs)
	m.mu.Unlock()

	return &statusResponse{
		StoredSpans:   spanCount,
		StoredMetrics: metricCount,
		StoredLogs:    logCount,
		Submits:       m.submits.Load(),
	}, nil
}

func (m *telemetryMock) handleSubmit(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var request submitRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid submit request")
	}

	if len(request.Spans) == 0 && len(request.Metrics) == 0 && len(request.Logs) == 0 {
		return nil, errors.New("submit request must contain at least one span, metric, or log record")
	}

	m.mu.Lock()
	m.spans = append(m.spans, request.Spans...)
	m.metrics = append(m.metrics, request.Metrics...)
	m.logs = append(m.logs, request.Logs...)
	m.mu.Unlock()

	m.submits.Add(1)

	return nil, nil
}

func (m *telemetryMock) handleQuerySpans(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var query spanQuery
	if err := codec.Unmarshal(raw, &query); err != nil {
		return nil, errors.New("invalid span query")
	}

	m.mu.Lock()
	// Copy the slice under lock, then filter the copy.
	allSpans := make([]telemetry.Span, len(m.spans))
	copy(allSpans, m.spans)
	m.mu.Unlock()

	var matched []telemetry.Span
	for _, span := range allSpans {
		if query.Source != "" && span.Source.String() != query.Source {
			continue
		}
		if query.Operation != "" && span.Operation != query.Operation {
			continue
		}
		if query.TraceID != "" && span.TraceID.String() != query.TraceID {
			continue
		}
		matched = append(matched, span)
	}

	return &spanQueryResponse{
		Spans: matched,
		Count: len(matched),
	}, nil
}

func (m *telemetryMock) handleQueryMetrics(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var query metricQuery
	if err := codec.Unmarshal(raw, &query); err != nil {
		return nil, errors.New("invalid metric query")
	}

	m.mu.Lock()
	allMetrics := make([]telemetry.MetricPoint, len(m.metrics))
	copy(allMetrics, m.metrics)
	m.mu.Unlock()

	var matched []telemetry.MetricPoint
	for _, metric := range allMetrics {
		if query.Source != "" && metric.Source.String() != query.Source {
			continue
		}
		if query.Name != "" && metric.Name != query.Name {
			continue
		}
		matched = append(matched, metric)
	}

	return &metricQueryResponse{
		Metrics: matched,
		Count:   len(matched),
	}, nil
}

func (m *telemetryMock) handleQueryLogs(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var query logQuery
	if err := codec.Unmarshal(raw, &query); err != nil {
		return nil, errors.New("invalid log query")
	}

	m.mu.Lock()
	allLogs := make([]telemetry.LogRecord, len(m.logs))
	copy(allLogs, m.logs)
	m.mu.Unlock()

	var matched []telemetry.LogRecord
	for _, logRecord := range allLogs {
		if query.Source != "" && logRecord.Source.String() != query.Source {
			continue
		}
		if query.MinSeverity > 0 && logRecord.Severity < query.MinSeverity {
			continue
		}
		if query.BodyContains != "" && !strings.Contains(logRecord.Body, query.BodyContains) {
			continue
		}
		if query.TraceID != "" && logRecord.TraceID.String() != query.TraceID {
			continue
		}
		matched = append(matched, logRecord)
	}

	return &logQueryResponse{
		Logs:  matched,
		Count: len(matched),
	}, nil
}
