// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// Well-known filesystem paths for the telemetry relay socket and its
// service token. When a service declares RequiredServices: ["telemetry"]
// in its template, the daemon bind-mounts the relay socket and token
// at these paths inside the sandbox.
const (
	TelemetryRelaySocketPath = "/run/bureau/service/telemetry.sock"
	TelemetryRelayTokenPath  = "/run/bureau/service/token/telemetry.token"
)

// drainFlushTimeout is how long the final drain flush waits for the
// relay to accept buffered spans before giving up. Two seconds is
// generous for a local Unix socket call; exceeding it means the relay
// is unresponsive and the data should be dropped rather than blocking
// service shutdown.
const drainFlushTimeout = 2 * time.Second

// TelemetryConfig holds the parameters for creating a
// [TelemetryEmitter]. All fields are required.
type TelemetryConfig struct {
	// SocketPath is the Unix socket path for the telemetry relay.
	SocketPath string

	// TokenPath is the path to the service token file for
	// authenticating with the telemetry relay.
	TokenPath string

	// Fleet identifies the fleet this service belongs to.
	Fleet ref.Fleet

	// Machine identifies the machine this service runs on.
	Machine ref.Machine

	// Source identifies the process emitting telemetry (the service
	// itself).
	Source ref.Entity

	// Clock provides time for the flush ticker. Production callers
	// pass clock.Real(); tests pass clock.Fake() for deterministic
	// control.
	Clock clock.Clock

	// Logger receives telemetry-related operational messages (flush
	// failures, connection errors).
	Logger *slog.Logger
}

// MetricsCollector is a function that produces metric points for
// inclusion in the next telemetry flush. The emitter calls this
// function during each flush cycle. The returned MetricPoints are
// submitted alongside buffered spans.
//
// Implementations must be safe for concurrent use and return quickly
// (the call happens on the flush path). Typical implementations
// snapshot in-memory counters and histograms.
type MetricsCollector func() []telemetry.MetricPoint

// TelemetryEmitter buffers telemetry spans and metrics, periodically
// flushing them to the telemetry relay via CBOR socket protocol. It
// stamps each recorded span with the emitter's Fleet, Machine, and
// Source identity.
//
// The emitter is safe for concurrent use. RecordSpan and RecordMetric
// are no-ops on a nil receiver, giving zero-cost opt-out when
// telemetry is unavailable.
//
// Lifecycle: call [TelemetryEmitter.Run] in a goroutine to start the
// flush loop, then cancel the context to stop it. Run performs a final
// drain flush before closing the Done channel.
type TelemetryEmitter struct {
	client  *ServiceClient
	clock   clock.Clock
	logger  *slog.Logger
	fleet   ref.Fleet
	machine ref.Machine
	source  ref.Entity

	mu               sync.Mutex
	spans            []telemetry.Span
	metrics          []telemetry.MetricPoint
	metricsCollector MetricsCollector

	done chan struct{}
}

// NewTelemetryEmitter creates an emitter that will flush spans to the
// relay at the given socket path, authenticated by the token at
// tokenPath. Returns an error if the token file cannot be read or any
// identity field is zero.
//
// The caller must start the flush loop by calling
// [TelemetryEmitter.Run] in a goroutine.
func NewTelemetryEmitter(config TelemetryConfig) (*TelemetryEmitter, error) {
	if config.SocketPath == "" {
		return nil, fmt.Errorf("telemetry emitter: SocketPath is required")
	}
	if config.TokenPath == "" {
		return nil, fmt.Errorf("telemetry emitter: TokenPath is required")
	}
	if config.Fleet.IsZero() {
		return nil, fmt.Errorf("telemetry emitter: Fleet is required")
	}
	if config.Machine.IsZero() {
		return nil, fmt.Errorf("telemetry emitter: Machine is required")
	}
	if config.Source.IsZero() {
		return nil, fmt.Errorf("telemetry emitter: Source is required")
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("telemetry emitter: Clock is required")
	}
	if config.Logger == nil {
		return nil, fmt.Errorf("telemetry emitter: Logger is required")
	}

	client, err := NewServiceClient(config.SocketPath, config.TokenPath)
	if err != nil {
		return nil, fmt.Errorf("telemetry emitter: creating relay client: %w", err)
	}

	return &TelemetryEmitter{
		client:  client,
		clock:   config.Clock,
		logger:  config.Logger,
		fleet:   config.Fleet,
		machine: config.Machine,
		source:  config.Source,
		done:    make(chan struct{}),
	}, nil
}

// RecordSpan buffers a span for the next flush cycle. Callers provide
// the operation-specific fields (TraceID, SpanID, Operation, StartTime,
// Duration, Status, Attributes, etc.); identity (Fleet, Machine,
// Source) is carried at the submit envelope level during flush, not
// stamped per-span.
//
// Safe to call on a nil receiver (no-op). This allows callers to
// unconditionally call RecordSpan without checking whether telemetry
// is enabled.
func (e *TelemetryEmitter) RecordSpan(span telemetry.Span) {
	if e == nil {
		return
	}

	e.mu.Lock()
	e.spans = append(e.spans, span)
	e.mu.Unlock()
}

// RecordMetric buffers a metric point for the next flush cycle.
// Like RecordSpan, identity fields are carried at the submit envelope
// level, not per-metric.
//
// Safe to call on a nil receiver (no-op).
func (e *TelemetryEmitter) RecordMetric(metric telemetry.MetricPoint) {
	if e == nil {
		return
	}

	e.mu.Lock()
	e.metrics = append(e.metrics, metric)
	e.mu.Unlock()
}

// SetMetricsCollector registers a function that the emitter calls
// during each flush to collect additional metric points. This is the
// integration point for services that maintain in-memory counters or
// histograms and want them flushed alongside buffered spans.
//
// The collector is called under the emitter's lock, so it must not
// call back into the emitter (RecordSpan, RecordMetric).
//
// Must be called before Run.
func (e *TelemetryEmitter) SetMetricsCollector(collector MetricsCollector) {
	e.metricsCollector = collector
}

// Run starts the flush loop, sending buffered spans to the relay at
// the given interval. Blocks until ctx is cancelled, then performs one
// final drain flush with a short deadline to capture any late spans
// from in-flight handlers. Closes the Done channel after the drain
// completes.
//
// Must be called exactly once per emitter.
func (e *TelemetryEmitter) Run(ctx context.Context, interval time.Duration) {
	defer close(e.done)

	ticker := e.clock.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.flush(ctx)
		case <-ctx.Done():
			drainContext, drainCancel := context.WithTimeout(context.Background(), drainFlushTimeout)
			e.flush(drainContext)
			drainCancel()
			return
		}
	}
}

// Done returns a channel that is closed after Run has fully exited,
// including the final drain flush. Callers can block on this to
// sequence cleanup.
func (e *TelemetryEmitter) Done() <-chan struct{} {
	return e.done
}

// flush drains the span and metric buffers, collects metrics from
// the optional MetricsCollector, and sends everything to the relay as
// a [telemetry.SubmitRequest] with envelope-level identity. Per-record
// identity fields are zero — the relay re-stamps them from the
// envelope after receiving. Buffers are swapped under the lock so
// RecordSpan/RecordMetric do not block during network I/O. Flush
// failures are logged but do not retry — the data is dropped. Lost
// telemetry is preferable to unbounded memory growth or degraded
// service latency.
func (e *TelemetryEmitter) flush(ctx context.Context) {
	e.mu.Lock()
	spans := e.spans
	e.spans = nil
	metrics := e.metrics
	e.metrics = nil
	if e.metricsCollector != nil {
		metrics = append(metrics, e.metricsCollector()...)
	}
	e.mu.Unlock()

	if len(spans) == 0 && len(metrics) == 0 {
		return
	}

	request := telemetry.SubmitRequest{
		Fleet:   e.fleet,
		Machine: e.machine,
		Source:  e.source,
		Spans:   spans,
		Metrics: metrics,
	}
	if err := e.client.Call(ctx, "submit", request, nil); err != nil {
		e.logger.Error("telemetry flush failed",
			"error", err,
			"dropped_spans", len(spans),
			"dropped_metrics", len(metrics),
		)
	}
}
