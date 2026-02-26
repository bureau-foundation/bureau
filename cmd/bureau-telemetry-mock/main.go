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
// The binary exposes seven actions:
//   - status (unauthenticated): relay-compatible operational stats plus stored counts
//   - submit (authenticated): accepts spans, metrics, logs, output deltas — matches relay wire format
//   - ingest (authenticated, streaming): accepts the relay-to-service streaming
//     TelemetryBatch protocol, storing records and forwarding to subscribers
//   - subscribe (authenticated, streaming): pushes records to the client as
//     they arrive via submit or ingest — the event-driven wait mechanism for tests
//   - query-spans (authenticated): filter stored spans by source, operation, or trace ID
//   - query-metrics (authenticated): filter stored metrics by source or name
//   - query-logs (authenticated): filter stored logs by source, severity, body substring, or trace ID
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/version"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if showVersion {
		version.Print("bureau-telemetry-mock")
		return nil
	}

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

	socketServer := boot.NewSocketServer()
	socketServer.RegisterRevocationHandler()

	socketServer.Handle("status", mock.handleStatus)
	socketServer.HandleAuth("submit", mock.handleSubmit)
	socketServer.HandleAuth("query-spans", mock.handleQuerySpans)
	socketServer.HandleAuth("query-metrics", mock.handleQueryMetrics)
	socketServer.HandleAuth("query-logs", mock.handleQueryLogs)
	socketServer.HandleAuthStream("ingest", mock.handleIngest)
	socketServer.HandleAuthStream("subscribe", mock.handleSubscribe)

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
	mu           sync.Mutex
	spans        []telemetry.Span
	metrics      []telemetry.MetricPoint
	logs         []telemetry.LogRecord
	outputDeltas []telemetry.OutputDelta

	submits       atomic.Uint64
	ingestBatches atomic.Uint64

	// subscriberMu protects subscribers. The submit and ingest
	// handlers read under RLock to fan out; the subscribe handler
	// writes under Lock to add/remove subscribers.
	subscriberMu sync.RWMutex
	subscribers  []*mockSubscriber
}

// mockSubscriber represents a connected subscribe stream client.
// Records are pushed on the events channel; the subscribe handler
// reads from it and writes to the client connection.
type mockSubscriber struct {
	events chan subscribeFrame
}

// subscribeFrame is the CBOR frame pushed to subscribe stream clients.
// Contains the records from a single submit or ingest batch.
type subscribeFrame struct {
	Spans        []telemetry.Span        `cbor:"spans,omitempty"`
	Metrics      []telemetry.MetricPoint `cbor:"metrics,omitempty"`
	Logs         []telemetry.LogRecord   `cbor:"logs,omitempty"`
	OutputDeltas []telemetry.OutputDelta `cbor:"output_deltas,omitempty"`
}

// streamAck is the acknowledgment frame for streaming connections
// (ingest, subscribe). Matches the real telemetry service's wire format.
type streamAck struct {
	OK    bool   `cbor:"ok"`
	Error string `cbor:"error,omitempty"`
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
	StoredOutputDeltas   int     `cbor:"stored_output_deltas"`
	Submits              uint64  `cbor:"submits"`
	IngestBatches        uint64  `cbor:"ingest_batches"`
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
	outputDeltaCount := len(m.outputDeltas)
	m.mu.Unlock()

	return &statusResponse{
		StoredSpans:        spanCount,
		StoredMetrics:      metricCount,
		StoredLogs:         logCount,
		StoredOutputDeltas: outputDeltaCount,
		Submits:            m.submits.Load(),
		IngestBatches:      m.ingestBatches.Load(),
	}, nil
}

func (m *telemetryMock) handleSubmit(_ context.Context, _ *servicetoken.Token, raw []byte) (any, error) {
	var request telemetry.SubmitRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid submit request")
	}

	if len(request.Spans) == 0 && len(request.Metrics) == 0 && len(request.Logs) == 0 && len(request.OutputDeltas) == 0 {
		return nil, errors.New("submit request must contain at least one span, metric, log, or output delta record")
	}

	request.StampIdentity()

	m.mu.Lock()
	m.spans = append(m.spans, request.Spans...)
	m.metrics = append(m.metrics, request.Metrics...)
	m.logs = append(m.logs, request.Logs...)
	m.outputDeltas = append(m.outputDeltas, request.OutputDeltas...)
	m.mu.Unlock()

	m.submits.Add(1)

	m.notifySubscribers(subscribeFrame{
		Spans:        request.Spans,
		Metrics:      request.Metrics,
		Logs:         request.Logs,
		OutputDeltas: request.OutputDeltas,
	})

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

// notifySubscribers pushes a subscribeFrame to all connected subscribe
// stream clients. Uses non-blocking sends: if a subscriber's channel
// is full (slow reader), the frame is dropped for that subscriber.
func (m *telemetryMock) notifySubscribers(frame subscribeFrame) {
	m.subscriberMu.RLock()
	defer m.subscriberMu.RUnlock()

	for _, subscriber := range m.subscribers {
		select {
		case subscriber.events <- frame:
		default:
		}
	}
}

// handleIngest is the streaming handler for the "ingest" action.
// Accepts the same wire protocol as the real telemetry service:
// readiness ack, then a stream of TelemetryBatch CBOR values with
// per-batch acks. Records are stored in memory and forwarded to
// subscribe stream clients.
func (m *telemetryMock) handleIngest(ctx context.Context, token *servicetoken.Token, _ []byte, conn net.Conn) {
	encoder := codec.NewEncoder(conn)

	// Send readiness ack.
	if err := encoder.Encode(streamAck{OK: true}); err != nil {
		return
	}

	// Close the connection on context cancellation to unblock reads.
	handlerDone := make(chan struct{})
	defer close(handlerDone)

	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	decoder := codec.NewDecoder(conn)

	for {
		var batch telemetry.TelemetryBatch
		if err := decoder.Decode(&batch); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			if opError := (*net.OpError)(nil); errors.As(err, &opError) && opError.Err.Error() == "use of closed network connection" {
				return
			}
			encoder.Encode(streamAck{Error: "decode error"})
			return
		}

		m.mu.Lock()
		m.spans = append(m.spans, batch.Spans...)
		m.metrics = append(m.metrics, batch.Metrics...)
		m.logs = append(m.logs, batch.Logs...)
		m.outputDeltas = append(m.outputDeltas, batch.OutputDeltas...)
		m.mu.Unlock()

		m.ingestBatches.Add(1)

		if err := encoder.Encode(streamAck{OK: true}); err != nil {
			return
		}

		m.notifySubscribers(subscribeFrame{
			Spans:        batch.Spans,
			Metrics:      batch.Metrics,
			Logs:         batch.Logs,
			OutputDeltas: batch.OutputDeltas,
		})
	}
}

// handleSubscribe is the streaming handler for the "subscribe" action.
// After authentication, the client receives a readiness ack and then
// subscribeFrame CBOR values whenever new telemetry data is stored
// (via submit or ingest). The stream stays open until the client
// disconnects or the context is cancelled. This is the event-driven
// wait mechanism for integration tests.
func (m *telemetryMock) handleSubscribe(ctx context.Context, token *servicetoken.Token, _ []byte, conn net.Conn) {
	encoder := codec.NewEncoder(conn)

	// Register the subscriber BEFORE sending the readiness ack.
	// This guarantees no events are missed between the client
	// receiving the ack and starting to read: by the time the
	// client sees the ack, the subscriber channel is already
	// receiving events from any concurrent submit/ingest handlers.
	subscriber := &mockSubscriber{
		events: make(chan subscribeFrame, 64),
	}

	m.subscriberMu.Lock()
	m.subscribers = append(m.subscribers, subscriber)
	m.subscriberMu.Unlock()

	defer func() {
		m.subscriberMu.Lock()
		for i, existing := range m.subscribers {
			if existing == subscriber {
				m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
				break
			}
		}
		m.subscriberMu.Unlock()
	}()

	// Send readiness ack after registration.
	if err := encoder.Encode(streamAck{OK: true}); err != nil {
		return
	}

	// Close the connection on context cancellation to unblock the
	// client's read (if it's doing bidirectional communication).
	handlerDone := make(chan struct{})
	defer close(handlerDone)

	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	for {
		select {
		case frame := <-subscriber.events:
			if err := encoder.Encode(frame); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
