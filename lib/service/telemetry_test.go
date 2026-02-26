// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// testRelay is a mock telemetry relay that captures submitted spans.
// Tests use it to verify that the TelemetryEmitter flushes spans
// correctly.
type testRelay struct {
	socketPath string
	tokenPath  string

	mu       sync.Mutex
	spans    []telemetry.Span
	submits  int
	received chan struct{} // signaled on each submit
}

// setupTestRelay creates a SocketServer mimicking the telemetry relay's
// "submit" action. The server runs until t.Cleanup fires. Returns the
// relay with socket and token paths ready for use by a TelemetryEmitter.
func setupTestRelay(t *testing.T) *testRelay {
	t.Helper()

	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	relay := &testRelay{
		socketPath: socketPath,
		received:   make(chan struct{}, 10),
	}

	server.HandleAuth("submit", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		var request struct {
			Spans []telemetry.Span `cbor:"spans"`
		}
		if err := codec.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		relay.mu.Lock()
		relay.spans = append(relay.spans, request.Spans...)
		relay.submits++
		relay.mu.Unlock()
		select {
		case relay.received <- struct{}{}:
		default:
		}
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	// Cleanup order is LIFO: cancel fires first (stops the server),
	// then wg.Wait blocks until Serve returns.
	t.Cleanup(func() { wg.Wait() })
	t.Cleanup(cancel)

	waitForSocket(t, socketPath)

	// Write a valid service token for the emitter to read.
	tokenBytes := mintTestToken(t, privateKey, "service/ticket/basic")
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "telemetry.token")
	if err := os.WriteFile(tokenPath, tokenBytes, 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	relay.tokenPath = tokenPath

	return relay
}

// testFleet creates a ref.Fleet for test use, scoped to the standard
// "bureau/fleet/test" namespace on "test.local".
func testFleet(t *testing.T) ref.Fleet {
	t.Helper()
	serverName, err := ref.ParseServerName("test.local")
	if err != nil {
		t.Fatalf("ParseServerName: %v", err)
	}
	fleet, err := ref.ParseFleet("bureau/fleet/test", serverName)
	if err != nil {
		t.Fatalf("ParseFleet: %v", err)
	}
	return fleet
}

// testEmitterConfig returns a TelemetryConfig populated with valid
// test values. The caller may override individual fields before
// passing it to NewTelemetryEmitter.
func testEmitterConfig(t *testing.T, relay *testRelay, fakeClock *clock.FakeClock) TelemetryConfig {
	t.Helper()
	fleet := testFleet(t)
	machine := testMachine(t, "@bureau/fleet/test/machine/test:test.local")
	source, err := ref.NewEntityFromAccountLocalpart(fleet, "service/ticket/basic")
	if err != nil {
		t.Fatalf("NewEntityFromAccountLocalpart: %v", err)
	}
	return TelemetryConfig{
		SocketPath: relay.socketPath,
		TokenPath:  relay.tokenPath,
		Fleet:      fleet,
		Machine:    machine,
		Source:     source,
		Clock:      fakeClock,
		Logger:     testLogger(),
	}
}

func TestTelemetryEmitterRecordSpanNilReceiver(t *testing.T) {
	t.Parallel()
	var emitter *TelemetryEmitter
	// Must not panic.
	emitter.RecordSpan(telemetry.Span{Operation: "test"})
}

func TestTelemetryEmitterFlush(t *testing.T) {
	t.Parallel()

	relay := setupTestRelay(t)
	fakeClock := clock.Fake(testClockEpoch)
	config := testEmitterConfig(t, relay, fakeClock)

	emitter, err := NewTelemetryEmitter(config)
	if err != nil {
		t.Fatalf("NewTelemetryEmitter: %v", err)
	}

	emitterContext, emitterCancel := context.WithCancel(context.Background())
	go emitter.Run(emitterContext, 5*time.Second)

	// Wait for the ticker to register before advancing the clock.
	fakeClock.WaitForTimers(1)

	// Record a span. The emitter should stamp identity fields.
	traceID := telemetry.NewTraceID()
	spanID := telemetry.NewSpanID()
	emitter.RecordSpan(telemetry.Span{
		TraceID:   traceID,
		SpanID:    spanID,
		Operation: "socket.handle",
		StartTime: 1000000000,
		Duration:  500000,
		Status:    telemetry.SpanStatusOK,
		Attributes: map[string]any{
			"action":       "status",
			"handler_type": "unauthenticated",
		},
	})

	// Advance the clock to trigger a periodic flush.
	fakeClock.Advance(5 * time.Second)

	// Wait for the relay to receive the spans.
	select {
	case <-relay.received:
	case <-t.Context().Done():
		t.Fatal("timeout waiting for relay to receive spans")
	}

	relay.mu.Lock()
	defer relay.mu.Unlock()

	if len(relay.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(relay.spans))
	}
	span := relay.spans[0]

	// Verify identity was stamped by the emitter.
	if span.Fleet != config.Fleet {
		t.Errorf("Fleet: got %v, want %v", span.Fleet, config.Fleet)
	}
	if span.Machine != config.Machine {
		t.Errorf("Machine: got %v, want %v", span.Machine, config.Machine)
	}
	if span.Source != config.Source {
		t.Errorf("Source: got %v, want %v", span.Source, config.Source)
	}

	// Verify caller-provided fields are preserved.
	if span.TraceID != traceID {
		t.Errorf("TraceID: got %s, want %s", span.TraceID, traceID)
	}
	if span.SpanID != spanID {
		t.Errorf("SpanID: got %s, want %s", span.SpanID, spanID)
	}
	if span.Operation != "socket.handle" {
		t.Errorf("Operation: got %q, want %q", span.Operation, "socket.handle")
	}
	if span.Status != telemetry.SpanStatusOK {
		t.Errorf("Status: got %d, want %d", span.Status, telemetry.SpanStatusOK)
	}
	if span.Attributes["action"] != "status" {
		t.Errorf("Attributes[action]: got %v, want %q", span.Attributes["action"], "status")
	}

	emitterCancel()
	<-emitter.Done()
}

func TestTelemetryEmitterMultipleSpansBatched(t *testing.T) {
	t.Parallel()

	relay := setupTestRelay(t)
	fakeClock := clock.Fake(testClockEpoch)
	config := testEmitterConfig(t, relay, fakeClock)

	emitter, err := NewTelemetryEmitter(config)
	if err != nil {
		t.Fatalf("NewTelemetryEmitter: %v", err)
	}

	emitterContext, emitterCancel := context.WithCancel(context.Background())
	go emitter.Run(emitterContext, 5*time.Second)
	fakeClock.WaitForTimers(1)

	// Record three spans before any flush.
	for _, operation := range []string{"op.alpha", "op.beta", "op.gamma"} {
		emitter.RecordSpan(telemetry.Span{
			TraceID:   telemetry.NewTraceID(),
			SpanID:    telemetry.NewSpanID(),
			Operation: operation,
			Status:    telemetry.SpanStatusOK,
		})
	}

	fakeClock.Advance(5 * time.Second)

	select {
	case <-relay.received:
	case <-t.Context().Done():
		t.Fatal("timeout waiting for relay to receive spans")
	}

	relay.mu.Lock()
	defer relay.mu.Unlock()

	// All three should arrive in a single submit call.
	if relay.submits != 1 {
		t.Errorf("expected 1 submit call (batched), got %d", relay.submits)
	}
	if len(relay.spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(relay.spans))
	}

	operations := make(map[string]bool)
	for _, span := range relay.spans {
		operations[span.Operation] = true
	}
	for _, want := range []string{"op.alpha", "op.beta", "op.gamma"} {
		if !operations[want] {
			t.Errorf("missing span with operation %q", want)
		}
	}

	emitterCancel()
	<-emitter.Done()
}

func TestTelemetryEmitterDrainFlush(t *testing.T) {
	t.Parallel()

	relay := setupTestRelay(t)
	fakeClock := clock.Fake(testClockEpoch)
	config := testEmitterConfig(t, relay, fakeClock)

	emitter, err := NewTelemetryEmitter(config)
	if err != nil {
		t.Fatalf("NewTelemetryEmitter: %v", err)
	}

	emitterContext, emitterCancel := context.WithCancel(context.Background())
	go emitter.Run(emitterContext, 5*time.Second)
	fakeClock.WaitForTimers(1)

	// Record a span but do NOT advance the clock. The span sits in
	// the buffer with no periodic flush to send it.
	emitter.RecordSpan(telemetry.Span{
		TraceID:   telemetry.NewTraceID(),
		SpanID:    telemetry.NewSpanID(),
		Operation: "socket.handle",
		Status:    telemetry.SpanStatusOK,
	})

	// Cancel triggers the drain flush, which should send the buffered
	// span before closing Done.
	emitterCancel()
	<-emitter.Done()

	relay.mu.Lock()
	defer relay.mu.Unlock()

	if len(relay.spans) != 1 {
		t.Fatalf("drain flush: expected 1 span, got %d", len(relay.spans))
	}
	if relay.spans[0].Operation != "socket.handle" {
		t.Errorf("drain flush: expected operation 'socket.handle', got %q", relay.spans[0].Operation)
	}
}

func TestTelemetryEmitterDrainEmptyBuffer(t *testing.T) {
	t.Parallel()

	relay := setupTestRelay(t)
	fakeClock := clock.Fake(testClockEpoch)
	config := testEmitterConfig(t, relay, fakeClock)

	emitter, err := NewTelemetryEmitter(config)
	if err != nil {
		t.Fatalf("NewTelemetryEmitter: %v", err)
	}

	emitterContext, emitterCancel := context.WithCancel(context.Background())
	go emitter.Run(emitterContext, 5*time.Second)
	fakeClock.WaitForTimers(1)

	// Cancel without recording any spans.
	emitterCancel()
	<-emitter.Done()

	relay.mu.Lock()
	defer relay.mu.Unlock()

	if relay.submits != 0 {
		t.Errorf("expected 0 submits for empty buffer, got %d", relay.submits)
	}
}

func TestNewTelemetryEmitterValidation(t *testing.T) {
	t.Parallel()

	relay := setupTestRelay(t)
	fakeClock := clock.Fake(testClockEpoch)
	config := testEmitterConfig(t, relay, fakeClock)

	// Valid config should succeed.
	emitter, err := NewTelemetryEmitter(config)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if emitter == nil {
		t.Fatal("valid config returned nil emitter")
	}

	// Each required field, when zeroed, should produce an error.
	tests := []struct {
		name   string
		mutate func(*TelemetryConfig)
	}{
		{"missing SocketPath", func(c *TelemetryConfig) { c.SocketPath = "" }},
		{"missing TokenPath", func(c *TelemetryConfig) { c.TokenPath = "" }},
		{"zero Fleet", func(c *TelemetryConfig) { c.Fleet = ref.Fleet{} }},
		{"zero Machine", func(c *TelemetryConfig) { c.Machine = ref.Machine{} }},
		{"zero Source", func(c *TelemetryConfig) { c.Source = ref.Entity{} }},
		{"nil Clock", func(c *TelemetryConfig) { c.Clock = nil }},
		{"nil Logger", func(c *TelemetryConfig) { c.Logger = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := config
			tt.mutate(&invalid)
			_, err := NewTelemetryEmitter(invalid)
			if err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

func TestNewTelemetryEmitterBadTokenFile(t *testing.T) {
	t.Parallel()

	fakeClock := clock.Fake(testClockEpoch)
	fleet := testFleet(t)
	machine := testMachine(t, "@bureau/fleet/test/machine/test:test.local")
	source, err := ref.NewEntityFromAccountLocalpart(fleet, "service/ticket/basic")
	if err != nil {
		t.Fatalf("NewEntityFromAccountLocalpart: %v", err)
	}

	_, err = NewTelemetryEmitter(TelemetryConfig{
		SocketPath: "/tmp/nonexistent.sock",
		TokenPath:  "/tmp/nonexistent-token-file",
		Fleet:      fleet,
		Machine:    machine,
		Source:     source,
		Clock:      fakeClock,
		Logger:     testLogger(),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent token file")
	}
}

// --- Socket server telemetry integration tests ---
//
// These verify that handleConnection produces spans with correct
// attributes when a TelemetryEmitter is attached. We inspect the
// emitter's buffer directly (same package) rather than going through
// the relay — the relay flush path is already tested above.

// testTelemetryEmitter creates a TelemetryEmitter with a dummy token
// file for socket server tests. The emitter is never Run (no flush
// loop), so it doesn't need a real relay socket — we just inspect
// its span buffer.
func testTelemetryEmitter(t *testing.T) *TelemetryEmitter {
	t.Helper()

	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "test.token")
	if err := os.WriteFile(tokenPath, []byte("dummy-token-bytes"), 0600); err != nil {
		t.Fatalf("writing dummy token: %v", err)
	}

	fleet := testFleet(t)
	machine := testMachine(t, "@bureau/fleet/test/machine/test:test.local")
	source, err := ref.NewEntityFromAccountLocalpart(fleet, "service/ticket/basic")
	if err != nil {
		t.Fatalf("NewEntityFromAccountLocalpart: %v", err)
	}

	emitter, err := NewTelemetryEmitter(TelemetryConfig{
		SocketPath: "/tmp/nonexistent-relay.sock",
		TokenPath:  tokenPath,
		Fleet:      fleet,
		Machine:    machine,
		Source:     source,
		Clock:      clock.Fake(testClockEpoch),
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("NewTelemetryEmitter: %v", err)
	}
	return emitter
}

// collectEmitterSpans returns a snapshot of the emitter's buffered
// spans. Safe for concurrent use.
func collectEmitterSpans(emitter *TelemetryEmitter) []telemetry.Span {
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	spans := make([]telemetry.Span, len(emitter.spans))
	copy(spans, emitter.spans)
	return spans
}

func TestSocketServerTelemetryUnauthenticatedSuccess(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	emitter := testTelemetryEmitter(t)
	server := NewSocketServer(socketPath, testLogger(), nil)
	server.SetTelemetry(emitter)

	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
		return map[string]any{"healthy": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "status"})
	if !response.OK {
		t.Fatalf("request failed: %s", response.Error)
	}

	spans := collectEmitterSpans(emitter)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Operation != "socket.handle" {
		t.Errorf("Operation: got %q, want 'socket.handle'", span.Operation)
	}
	if span.Status != telemetry.SpanStatusOK {
		t.Errorf("Status: got %d, want OK (%d)", span.Status, telemetry.SpanStatusOK)
	}
	if span.Attributes["action"] != "status" {
		t.Errorf("Attributes[action]: got %v, want 'status'", span.Attributes["action"])
	}
	if span.Attributes["handler_type"] != "unauthenticated" {
		t.Errorf("Attributes[handler_type]: got %v, want 'unauthenticated'", span.Attributes["handler_type"])
	}
	if span.Duration <= 0 {
		t.Errorf("Duration should be positive, got %d", span.Duration)
	}
	if span.TraceID.IsZero() {
		t.Error("TraceID should not be zero")
	}
	if span.SpanID.IsZero() {
		t.Error("SpanID should not be zero")
	}

	cancel()
	wg.Wait()
}

func TestSocketServerTelemetryHandlerError(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	emitter := testTelemetryEmitter(t)
	server := NewSocketServer(socketPath, testLogger(), nil)
	server.SetTelemetry(emitter)

	server.Handle("fail", func(ctx context.Context, raw []byte) (any, error) {
		return nil, fmt.Errorf("handler broke")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "fail"})
	if response.OK {
		t.Fatal("expected request to fail")
	}

	spans := collectEmitterSpans(emitter)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status != telemetry.SpanStatusError {
		t.Errorf("Status: got %d, want Error (%d)", span.Status, telemetry.SpanStatusError)
	}
	if span.StatusMessage != "handler broke" {
		t.Errorf("StatusMessage: got %q, want 'handler broke'", span.StatusMessage)
	}
	if span.Attributes["action"] != "fail" {
		t.Errorf("Attributes[action]: got %v, want 'fail'", span.Attributes["action"])
	}

	cancel()
	wg.Wait()
}

func TestSocketServerTelemetryAuthenticatedSuccess(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	emitter := testTelemetryEmitter(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)
	server.SetTelemetry(emitter)

	server.HandleAuth("query", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		return map[string]any{"found": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintTestToken(t, privateKey, "agent/coder")
	response := sendRequest(t, socketPath, map[string]any{
		"action": "query",
		"token":  tokenBytes,
	})
	if !response.OK {
		t.Fatalf("request failed: %s", response.Error)
	}

	spans := collectEmitterSpans(emitter)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status != telemetry.SpanStatusOK {
		t.Errorf("Status: got %d, want OK", span.Status)
	}
	if span.Attributes["action"] != "query" {
		t.Errorf("Attributes[action]: got %v, want 'query'", span.Attributes["action"])
	}
	if span.Attributes["handler_type"] != "authenticated" {
		t.Errorf("Attributes[handler_type]: got %v, want 'authenticated'", span.Attributes["handler_type"])
	}
	wantSubject := "@bureau/fleet/test/agent/coder:test.local"
	if span.Attributes["subject"] != wantSubject {
		t.Errorf("Attributes[subject]: got %v, want %q", span.Attributes["subject"], wantSubject)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerTelemetryAuthFailure(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	emitter := testTelemetryEmitter(t)
	authConfig, _ := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)
	server.SetTelemetry(emitter)

	server.HandleAuth("query", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with missing token")
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// Send request without a token.
	response := sendRequest(t, socketPath, map[string]string{"action": "query"})
	if response.OK {
		t.Fatal("expected auth failure")
	}

	spans := collectEmitterSpans(emitter)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status != telemetry.SpanStatusError {
		t.Errorf("Status: got %d, want Error", span.Status)
	}
	if span.Attributes["action"] != "query" {
		t.Errorf("Attributes[action]: got %v, want 'query'", span.Attributes["action"])
	}
	if span.Attributes["handler_type"] != "authenticated" {
		t.Errorf("Attributes[handler_type]: got %v, want 'authenticated'", span.Attributes["handler_type"])
	}
	// Subject should not be set on auth failure (token never decoded).
	if _, hasSubject := span.Attributes["subject"]; hasSubject {
		t.Errorf("Attributes[subject] should not be set on auth failure, got %v", span.Attributes["subject"])
	}

	cancel()
	wg.Wait()
}

func TestSocketServerTelemetryUnknownAction(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	emitter := testTelemetryEmitter(t)
	server := NewSocketServer(socketPath, testLogger(), nil)
	server.SetTelemetry(emitter)

	server.Handle("known", func(ctx context.Context, raw []byte) (any, error) {
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "nonexistent"})
	if response.OK {
		t.Fatal("expected error for unknown action")
	}

	spans := collectEmitterSpans(emitter)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status != telemetry.SpanStatusError {
		t.Errorf("Status: got %d, want Error", span.Status)
	}
	if span.Attributes["action"] != "nonexistent" {
		t.Errorf("Attributes[action]: got %v, want 'nonexistent'", span.Attributes["action"])
	}

	cancel()
	wg.Wait()
}

func TestSocketServerTelemetryNoEmitter(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	// No SetTelemetry call — telemetry is nil.
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
		return map[string]any{"healthy": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// Request should succeed without telemetry.
	response := sendRequest(t, socketPath, map[string]string{"action": "status"})
	if !response.OK {
		t.Fatalf("request failed: %s", response.Error)
	}

	// No panic, no span recorded — nothing to check beyond success.
	cancel()
	wg.Wait()
}
