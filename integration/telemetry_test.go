// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Wire protocol types for streaming telemetry actions ---

// telemetryStreamAck is the readiness/batch acknowledgment frame used
// by the ingest and tail protocols on the telemetry service and mock.
type telemetryStreamAck struct {
	OK    bool   `cbor:"ok"`
	Error string `cbor:"error,omitempty"`
}

// telemetryTailFrame is the CBOR frame received from the telemetry
// service's tail streaming action. Type "batch" carries a raw CBOR
// TelemetryBatch; type "heartbeat" is a keepalive signal.
type telemetryTailFrame struct {
	Type  string           `cbor:"type"`
	Batch codec.RawMessage `cbor:"batch,omitempty"`
}

// telemetryTailControl is the CBOR message sent to the telemetry
// service's tail streaming action to dynamically adjust the source
// filter. Patterns use principal.MatchPattern glob syntax.
type telemetryTailControl struct {
	Subscribe   []string `cbor:"subscribe,omitempty"`
	Unsubscribe []string `cbor:"unsubscribe,omitempty"`
}

// telemetryMockSubscribeFrame is the CBOR frame received from the
// telemetry mock's subscribe streaming action. Contains the records
// from a single submit or ingest batch.
type telemetryMockSubscribeFrame struct {
	Spans        []telemetry.Span        `cbor:"spans,omitempty"`
	Metrics      []telemetry.MetricPoint `cbor:"metrics,omitempty"`
	Logs         []telemetry.LogRecord   `cbor:"logs,omitempty"`
	OutputDeltas []telemetry.OutputDelta `cbor:"output_deltas,omitempty"`
}

// telemetryServiceStatus mirrors the telemetry service's CBOR status
// response. Defined locally in the test file — standard pattern.
type telemetryServiceStatus struct {
	BatchesReceived      uint64  `cbor:"batches_received"`
	SpansReceived        uint64  `cbor:"spans_received"`
	MetricsReceived      uint64  `cbor:"metrics_received"`
	LogsReceived         uint64  `cbor:"logs_received"`
	OutputDeltasReceived uint64  `cbor:"output_deltas_received"`
	ConnectedRelays      int     `cbor:"connected_relays"`
	UptimeSeconds        float64 `cbor:"uptime_seconds"`
}

// openTelemetryStream dials a Unix socket, sends the CBOR streaming
// handshake (action + service token), and reads the readiness ack.
// Returns the open connection for the caller to use with codec
// Encoder/Decoder. The connection is registered for cleanup on test
// completion.
func openTelemetryStream(t *testing.T, socketPath, action string, token []byte) net.Conn {
	t.Helper()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s for %s: %v", socketPath, action, err)
	}
	t.Cleanup(func() { conn.Close() })

	encoder := codec.NewEncoder(conn)
	decoder := codec.NewDecoder(conn)

	// Send the streaming handshake: action name + service token.
	handshake := map[string]any{
		"action": action,
		"token":  token,
	}
	if err := encoder.Encode(handshake); err != nil {
		t.Fatalf("send %s handshake: %v", action, err)
	}

	// Read readiness ack from the server.
	var ack telemetryStreamAck
	if err := decoder.Decode(&ack); err != nil {
		t.Fatalf("read %s readiness ack: %v", action, err)
	}
	if !ack.OK {
		t.Fatalf("%s readiness ack rejected: %s", action, ack.Error)
	}

	return conn
}

// TestTelemetryServiceTail exercises the production telemetry service's
// tail streaming action end-to-end. The test deploys the real telemetry
// service, opens an ingest stream to push a TelemetryBatch, opens a
// tail stream with a glob-pattern subscription, and verifies the batch
// arrives on the tail stream with the correct content.
//
// This tests the full production path: ingest decode → counter update →
// raw CBOR fan-out → per-subscriber pattern filter → tail frame encode.
func TestTelemetryServiceTail(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "tailtelem")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the real telemetry service.
	telemetrySvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    resolvedBinary(t, "TELEMETRY_SERVICE_BINARY"),
		Name:      "telemetry-tail",
		Localpart: "service/telemetry/tail",
	})

	// Build a caller entity for minting service tokens.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/tail-test")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}

	// Mint tokens with the grants each action requires.
	ingestToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{"telemetry/ingest"}}})
	tailToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{"telemetry/tail"}}})

	// Open the tail stream and subscribe to all sources BEFORE opening
	// the ingest stream. This ensures the subscriber is registered and
	// will receive events from the batch we're about to send. The
	// subscribe-before-ack ordering in the service's handleTail means
	// the subscriber channel is active by the time we see the ack.
	tailConn := openTelemetryStream(t, telemetrySvc.SocketPath, "tail", tailToken)
	tailEncoder := codec.NewEncoder(tailConn)
	tailDecoder := codec.NewDecoder(tailConn)

	// Subscribe to all sources via the "**" wildcard pattern.
	if err := tailEncoder.Encode(telemetryTailControl{
		Subscribe: []string{"**"},
	}); err != nil {
		t.Fatalf("send tail subscribe: %v", err)
	}

	// Open the ingest stream and send a TelemetryBatch containing
	// a span and a log record.
	ingestConn := openTelemetryStream(t, telemetrySvc.SocketPath, "ingest", ingestToken)
	ingestEncoder := codec.NewEncoder(ingestConn)
	ingestDecoder := codec.NewDecoder(ingestConn)

	batch := telemetry.TelemetryBatch{
		Fleet:          fleet.Ref,
		Machine:        machine.Ref,
		SequenceNumber: 1,
		Spans: []telemetry.Span{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Operation: "test.tail.operation",
			StartTime: 1000000000,
			Duration:  500000000,
			Status:    telemetry.SpanStatusOK,
		}},
		Logs: []telemetry.LogRecord{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Body:      "tail test log",
			Severity:  telemetry.SeverityInfo,
			Timestamp: 1000000000,
		}},
		OutputDeltas: []telemetry.OutputDelta{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			SessionID: "tail-test-session",
			Sequence:  0,
			Stream:    telemetry.OutputStreamCombined,
			Timestamp: 1000000000,
			Data:      []byte("tail test output\n"),
		}},
	}

	if err := ingestEncoder.Encode(batch); err != nil {
		t.Fatalf("send ingest batch: %v", err)
	}

	// Read the per-batch ack from the ingest stream.
	var batchAck telemetryStreamAck
	if err := ingestDecoder.Decode(&batchAck); err != nil {
		t.Fatalf("read ingest batch ack: %v", err)
	}
	if !batchAck.OK {
		t.Fatalf("ingest batch ack rejected: %s", batchAck.Error)
	}

	// Read from the tail stream, skipping any heartbeat frames.
	var frame telemetryTailFrame
	for {
		if err := tailDecoder.Decode(&frame); err != nil {
			t.Fatalf("read tail frame: %v", err)
		}
		if frame.Type == "batch" {
			break
		}
		if frame.Type != "heartbeat" {
			t.Fatalf("unexpected tail frame type: %q", frame.Type)
		}
	}

	// Decode the raw batch from the tail frame and verify content.
	var receivedBatch telemetry.TelemetryBatch
	if err := codec.Unmarshal(frame.Batch, &receivedBatch); err != nil {
		t.Fatalf("unmarshal tail batch: %v", err)
	}

	if length := len(receivedBatch.Spans); length != 1 {
		t.Fatalf("expected 1 span in tail batch, got %d", length)
	}
	if receivedBatch.Spans[0].Operation != "test.tail.operation" {
		t.Fatalf("expected span operation %q, got %q",
			"test.tail.operation", receivedBatch.Spans[0].Operation)
	}
	if length := len(receivedBatch.Logs); length != 1 {
		t.Fatalf("expected 1 log in tail batch, got %d", length)
	}
	if receivedBatch.Logs[0].Body != "tail test log" {
		t.Fatalf("expected log body %q, got %q",
			"tail test log", receivedBatch.Logs[0].Body)
	}
	if length := len(receivedBatch.OutputDeltas); length != 1 {
		t.Fatalf("expected 1 output delta in tail batch, got %d", length)
	}
	if receivedBatch.OutputDeltas[0].SessionID != "tail-test-session" {
		t.Fatalf("expected output delta session_id %q, got %q",
			"tail-test-session", receivedBatch.OutputDeltas[0].SessionID)
	}
	if string(receivedBatch.OutputDeltas[0].Data) != "tail test output\n" {
		t.Fatalf("expected output delta data %q, got %q",
			"tail test output\n", string(receivedBatch.OutputDeltas[0].Data))
	}

	// Verify the service's status endpoint reflects the ingested data.
	unauthClient := service.NewServiceClientFromToken(telemetrySvc.SocketPath, nil)
	var status telemetryServiceStatus
	if err := unauthClient.Call(t.Context(), "status", nil, &status); err != nil {
		t.Fatalf("status after ingest: %v", err)
	}
	if status.BatchesReceived != 1 {
		t.Fatalf("expected 1 batch received, got %d", status.BatchesReceived)
	}
	if status.SpansReceived != 1 {
		t.Fatalf("expected 1 span received, got %d", status.SpansReceived)
	}
	if status.LogsReceived != 1 {
		t.Fatalf("expected 1 log received, got %d", status.LogsReceived)
	}
	if status.OutputDeltasReceived != 1 {
		t.Fatalf("expected 1 output delta received, got %d", status.OutputDeltasReceived)
	}
}

// TestTelemetryRelayPipeline exercises the full telemetry data flow:
// agent submit → relay accumulation → relay shipping → service ingest.
// The telemetry mock stands in for the fleet-wide telemetry service so
// we can verify content without a real storage backend. The relay is
// deployed with --flush-threshold-bytes=1 so any submit exceeds the
// threshold and triggers immediate shipping.
//
// Event-driven verification uses the mock's subscribe streaming action:
// the test opens a subscribe stream before deploying the relay, submits
// telemetry to the relay, and verifies the exact content arrives on the
// subscribe stream.
func TestTelemetryRelayPipeline(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "relaypipe")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the telemetry mock as the fleet-wide telemetry service.
	// deployTelemetryMock also publishes a service binding so the
	// daemon resolves RequiredServices: ["telemetry"] to the mock's
	// socket for any subsequent principal deployed on this machine.
	mockService := deployTelemetryMock(t, admin, fleet, machine, "pipeline")

	// Build a caller entity for minting service tokens.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/relay-test")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}

	// Open a subscribe stream on the mock BEFORE deploying the relay.
	// This guarantees the subscriber is registered and will receive
	// events from any batch the relay ships, regardless of timing.
	subscribeToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	subscribeConn := openTelemetryStream(t, mockService.SocketPath, "subscribe", subscribeToken)
	subscribeDecoder := codec.NewDecoder(subscribeConn)

	// Deploy the telemetry relay with immediate flushing. The relay's
	// RequiredServices: ["telemetry"] tells the daemon to mount the
	// mock's socket at /run/bureau/service/telemetry.sock inside the
	// relay's sandbox. The env vars tell the relay binary where to
	// find the mounted socket and token file.
	relayBinary := resolvedBinary(t, "TELEMETRY_RELAY_BINARY")
	relayService := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           relayBinary,
		Name:             "relay-pipeline",
		Localpart:        "service/telemetry-relay/pipeline",
		Command:          []string{relayBinary, "--flush-threshold-bytes", "1"},
		RequiredServices: []string{"telemetry"},
		ExtraEnvironmentVariables: map[string]string{
			"BUREAU_TELEMETRY_SERVICE_SOCKET": "/run/bureau/service/telemetry.sock",
			"BUREAU_TELEMETRY_TOKEN_PATH":     "/run/bureau/service/token/telemetry.token",
		},
	})

	// Submit telemetry to the relay's CBOR socket. The relay accepts
	// submit as an authenticated action (no grants required). The
	// --flush-threshold-bytes=1 ensures the relay immediately ships
	// the batch to the mock via its ingest stream.
	submitToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	relayClient := service.NewServiceClientFromToken(relayService.SocketPath, submitToken)

	submitRequest := telemetry.SubmitRequest{
		Fleet:   fleet.Ref,
		Machine: machine.Ref,
		Source:  callerEntity,
		Spans: []telemetry.Span{{
			Operation: "test.relay.pipeline",
			StartTime: 2000000000,
			Duration:  100000000,
			Status:    telemetry.SpanStatusOK,
		}},
		Metrics: []telemetry.MetricPoint{{
			Name:      "relay_test_gauge",
			Kind:      telemetry.MetricKindGauge,
			Value:     99,
			Timestamp: 2000000000,
		}},
		OutputDeltas: []telemetry.OutputDelta{{
			SessionID: "relay-test-session",
			Sequence:  0,
			Stream:    telemetry.OutputStreamStdout,
			Timestamp: 2000000000,
			Data:      []byte("relay pipeline output\n"),
		}},
	}
	if err := relayClient.Call(t.Context(), "submit", submitRequest, nil); err != nil {
		t.Fatalf("submit to relay: %v", err)
	}

	// The relay accumulates the submit, exceeds --flush-threshold-bytes=1,
	// and ships a TelemetryBatch to the mock via its ingest stream. The
	// mock stores the batch and pushes a subscribeFrame to our subscriber.
	var frame telemetryMockSubscribeFrame
	if err := subscribeDecoder.Decode(&frame); err != nil {
		t.Fatalf("read subscribe frame from mock: %v", err)
	}

	// Verify the span content arrived intact through the relay pipeline.
	if length := len(frame.Spans); length != 1 {
		t.Fatalf("expected 1 span on subscribe stream, got %d", length)
	}
	if frame.Spans[0].Operation != "test.relay.pipeline" {
		t.Fatalf("expected span operation %q, got %q",
			"test.relay.pipeline", frame.Spans[0].Operation)
	}

	// Verify the metric content arrived intact.
	if length := len(frame.Metrics); length != 1 {
		t.Fatalf("expected 1 metric on subscribe stream, got %d", length)
	}
	if frame.Metrics[0].Name != "relay_test_gauge" {
		t.Fatalf("expected metric name %q, got %q",
			"relay_test_gauge", frame.Metrics[0].Name)
	}

	// Verify the output delta content arrived intact through the relay.
	if length := len(frame.OutputDeltas); length != 1 {
		t.Fatalf("expected 1 output delta on subscribe stream, got %d", length)
	}
	if frame.OutputDeltas[0].SessionID != "relay-test-session" {
		t.Fatalf("expected output delta session_id %q, got %q",
			"relay-test-session", frame.OutputDeltas[0].SessionID)
	}
	if string(frame.OutputDeltas[0].Data) != "relay pipeline output\n" {
		t.Fatalf("expected output delta data %q, got %q",
			"relay pipeline output\n", string(frame.OutputDeltas[0].Data))
	}

	// Cross-check: the mock's status should show the ingested batch.
	unauthClient := service.NewServiceClientFromToken(mockService.SocketPath, nil)
	var mockStatus telemetryMockStatus
	if err := unauthClient.Call(t.Context(), "status", nil, &mockStatus); err != nil {
		t.Fatalf("mock status after relay pipeline: %v", err)
	}
	if mockStatus.IngestBatches != 1 {
		t.Fatalf("expected 1 ingest batch on mock, got %d", mockStatus.IngestBatches)
	}
	if mockStatus.StoredSpans != 1 {
		t.Fatalf("expected 1 stored span on mock, got %d", mockStatus.StoredSpans)
	}
}

// TestSocketServerTelemetryEmission verifies that a Bureau service with
// telemetry enabled (RequiredServices: ["telemetry"]) automatically emits
// socket.handle spans when processing CBOR socket requests. This tests
// the full production path end-to-end:
//
//   - BootstrapViaProxy detects the telemetry relay socket (bind-mounted
//     by the daemon from the telemetry mock's socket)
//   - BootstrapResult.NewSocketServer() attaches the TelemetryEmitter to
//     the SocketServer
//   - SocketServer.handleConnection records a socket.handle span for
//     each request
//   - TelemetryEmitter flushes the buffered span to the mock via the
//     CBOR submit action
//   - The mock stores it and pushes it to the subscribe stream
//
// Verification is event-driven via the mock's subscribe streaming action:
// the test opens a subscriber before making the request, so the span
// arrives on the stream as soon as the emitter flushes (5-second interval
// from BootstrapViaProxy).
func TestSocketServerTelemetryEmission(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "sockspan")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the telemetry mock and publish a service binding so the
	// daemon resolves RequiredServices: ["telemetry"] to the mock's
	// socket for subsequent principals.
	mockService := deployTelemetryMock(t, admin, fleet, machine, "sockspan")

	// Open a subscribe stream on the mock BEFORE deploying the test
	// service. This ensures the subscriber is registered and will
	// receive any spans the test service's emitter flushes.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/sockspan-test")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}
	subscribeToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	subscribeConn := openTelemetryStream(t, mockService.SocketPath, "subscribe", subscribeToken)
	subscribeDecoder := codec.NewDecoder(subscribeConn)

	// Deploy the test service WITH RequiredServices: ["telemetry"].
	// The daemon bind-mounts the telemetry mock's socket at
	// /run/bureau/service/telemetry.sock inside the test service's
	// sandbox. BootstrapViaProxy probes that path, creates a
	// TelemetryEmitter, and NewSocketServer() attaches it.
	testSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           resolvedBinary(t, "TEST_SERVICE_BINARY"),
		Name:             "test-sockspan",
		Localpart:        "service/test/sockspan",
		RequiredServices: []string{"telemetry"},
	})

	// Make an unauthenticated "status" request to the test service.
	// This triggers handleConnection → recordRequestSpan → RecordSpan.
	unauthClient := service.NewServiceClientFromToken(testSvc.SocketPath, nil)
	var statusResp struct {
		UptimeSeconds float64 `cbor:"uptime_seconds"`
		Principal     string  `cbor:"principal"`
	}
	if err := unauthClient.Call(t.Context(), "status", nil, &statusResp); err != nil {
		t.Fatalf("status call to test service: %v", err)
	}
	if statusResp.Principal == "" {
		t.Fatal("status response missing principal")
	}

	// Read from the subscribe stream. The emitter flushes every 5s
	// (configured in BootstrapViaProxy). When the batch arrives, the
	// mock pushes a subscribeFrame containing the spans.
	var frame telemetryMockSubscribeFrame
	if err := subscribeDecoder.Decode(&frame); err != nil {
		t.Fatalf("read subscribe frame from mock: %v", err)
	}

	// The frame should contain at least one socket.handle span from
	// our status request. There may be additional spans if the emitter
	// flushed other requests (e.g., the deployService helper's
	// readiness probing), so find our span by operation + action.
	var found *telemetry.Span
	for index := range frame.Spans {
		span := &frame.Spans[index]
		if span.Operation == "socket.handle" {
			action, _ := span.Attributes["action"].(string)
			if action == "status" {
				found = span
				break
			}
		}
	}
	if found == nil {
		t.Fatalf("no socket.handle span with action=status in subscribe frame "+
			"(got %d spans: %s)", len(frame.Spans), summarizeSpans(frame.Spans))
	}

	// Verify span fields stamped by the emitter and socket server.
	if found.Status != telemetry.SpanStatusOK {
		t.Fatalf("expected span status OK, got %d", found.Status)
	}
	handlerType, _ := found.Attributes["handler_type"].(string)
	if handlerType != "unauthenticated" {
		t.Fatalf("expected handler_type %q, got %q", "unauthenticated", handlerType)
	}
	if found.Duration <= 0 {
		t.Fatalf("expected positive duration, got %d", found.Duration)
	}
	if found.Fleet.IsZero() {
		t.Fatal("span Fleet is zero (emitter did not stamp identity)")
	}
	if found.Machine.IsZero() {
		t.Fatal("span Machine is zero (emitter did not stamp identity)")
	}
	if found.Source.IsZero() {
		t.Fatal("span Source is zero (emitter did not stamp identity)")
	}
	if found.TraceID.IsZero() {
		t.Fatal("span TraceID is zero")
	}
	if found.SpanID.IsZero() {
		t.Fatal("span SpanID is zero")
	}
}

// summarizeSpans produces a compact description of spans for diagnostic
// messages when an expected span is not found.
func summarizeSpans(spans []telemetry.Span) string {
	if len(spans) == 0 {
		return "none"
	}
	result := ""
	for index, span := range spans {
		if index > 0 {
			result += ", "
		}
		action, _ := span.Attributes["action"].(string)
		result += fmt.Sprintf("%s[action=%s,status=%d]", span.Operation, action, span.Status)
	}
	return result
}
