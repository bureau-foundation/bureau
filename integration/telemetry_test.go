// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/schema/log"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Wire protocol types for streaming telemetry actions ---

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
	var ack telemetry.StreamAck
	if err := decoder.Decode(&ack); err != nil {
		t.Fatalf("read %s readiness ack: %v", action, err)
	}
	if !ack.OK {
		t.Fatalf("%s readiness ack rejected: %s", action, ack.Error)
	}

	return conn
}

// queryTelemetryStatus queries the telemetry service's unauthenticated
// status endpoint and returns the parsed response.
func queryTelemetryStatus(t *testing.T, socketPath string) telemetry.ServiceStatus {
	t.Helper()
	client := service.NewServiceClientFromToken(socketPath, nil)
	var status telemetry.ServiceStatus
	if err := client.Call(t.Context(), "status", nil, &status); err != nil {
		t.Fatalf("telemetry status call: %v", err)
	}
	return status
}

// fetchLogMetadata resolves a log metadata tag and returns the
// deserialized LogContent. The tag names follow the pattern
// "log/<source-localpart>/<session-id>".
func fetchLogMetadata(t *testing.T, client *artifactstore.Client, tagName string) log.LogContent {
	t.Helper()
	ctx := t.Context()

	resolved, err := client.Resolve(ctx, tagName)
	if err != nil {
		t.Fatalf("resolve tag %q: %v", tagName, err)
	}

	result, err := client.Fetch(ctx, resolved.Hash)
	if err != nil {
		t.Fatalf("fetch metadata artifact %s (tag %q): %v", resolved.Hash, tagName, err)
	}
	defer result.Content.Close()

	data, err := io.ReadAll(result.Content)
	if err != nil {
		t.Fatalf("read metadata artifact content: %v", err)
	}

	var content log.LogContent
	if err := codec.Unmarshal(data, &content); err != nil {
		t.Fatalf("unmarshal log metadata from tag %q: %v", tagName, err)
	}
	return content
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
	telemetryBinary := resolvedBinary(t, "TELEMETRY_SERVICE_BINARY")
	telemetrySvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    telemetryBinary,
		Name:      "telemetry-tail",
		Localpart: "service/telemetry/tail",
		Command:   []string{telemetryBinary, "--storage-path", "/tmp/telemetry.db"},
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

	// Fixed timestamp used for all telemetry records. The "top"
	// query uses Start/End to specify the range, so the actual
	// timestamp value doesn't need to be "recent" — it just needs
	// to be consistent with the query's Start parameter.
	const batchTimestamp int64 = 1_735_689_600_000_000_000 // 2025-01-01T00:00:00Z

	batch := telemetry.TelemetryBatch{
		Fleet:          fleet.Ref,
		Machine:        machine.Ref,
		SequenceNumber: 1,
		Spans: []telemetry.Span{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Operation: "test.tail.operation",
			StartTime: batchTimestamp,
			Duration:  500000000,
			Status:    telemetry.SpanStatusOK,
		}},
		Logs: []telemetry.LogRecord{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			Body:      "tail test log",
			Severity:  telemetry.SeverityInfo,
			Timestamp: batchTimestamp,
		}},
		OutputDeltas: []telemetry.OutputDelta{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			SessionID: "tail-test-session",
			Sequence:  0,
			Stream:    telemetry.OutputStreamCombined,
			Timestamp: batchTimestamp,
			Data:      []byte("tail test output\n"),
		}},
	}

	if err := ingestEncoder.Encode(batch); err != nil {
		t.Fatalf("send ingest batch: %v", err)
	}

	// Read the per-batch ack from the ingest stream.
	var batchAck telemetry.StreamAck
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
	var status telemetry.ServiceStatus
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

	// Verify storage stats reflect the persisted data. The ingest
	// handler writes to SQLite before fanning out to tail subscribers,
	// so by the time we received the batch on the tail stream the
	// data is committed. Output deltas go to the log manager, not
	// the store, so they don't appear in storage stats.
	if status.Storage.PartitionCount != 1 {
		t.Fatalf("expected 1 storage partition, got %d", status.Storage.PartitionCount)
	}
	if status.Storage.SpanCount != 1 {
		t.Fatalf("expected 1 stored span, got %d", status.Storage.SpanCount)
	}
	if status.Storage.LogCount != 1 {
		t.Fatalf("expected 1 stored log, got %d", status.Storage.LogCount)
	}
	if status.Storage.MetricCount != 0 {
		t.Fatalf("expected 0 stored metrics, got %d", status.Storage.MetricCount)
	}
	if status.Storage.DatabaseSizeBytes <= 0 {
		t.Fatalf("expected positive database size, got %d", status.Storage.DatabaseSizeBytes)
	}
	if status.Storage.NewestPartition == "" {
		t.Fatal("expected non-empty newest partition suffix")
	}

	// Exercise the four query actions against the stored data.
	queryToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{
			"telemetry/traces", "telemetry/metrics",
			"telemetry/logs", "telemetry/top",
		}}})
	queryClient := service.NewServiceClientFromToken(telemetrySvc.SocketPath, queryToken)

	// traces: query all, expect the 1 span we submitted.
	var tracesResponse telemetry.TracesResponse
	if err := queryClient.Call(t.Context(), "traces", telemetry.TracesRequest{}, &tracesResponse); err != nil {
		t.Fatalf("traces query: %v", err)
	}
	if len(tracesResponse.Spans) != 1 {
		t.Fatalf("traces: got %d spans, want 1", len(tracesResponse.Spans))
	}
	if tracesResponse.Spans[0].Operation != "test.tail.operation" {
		t.Errorf("traces[0].Operation = %q, want test.tail.operation",
			tracesResponse.Spans[0].Operation)
	}

	// logs: query all, expect the 1 log we submitted.
	var logsResponse telemetry.LogsResponse
	if err := queryClient.Call(t.Context(), "logs", telemetry.LogsRequest{}, &logsResponse); err != nil {
		t.Fatalf("logs query: %v", err)
	}
	if len(logsResponse.Logs) != 1 {
		t.Fatalf("logs: got %d logs, want 1", len(logsResponse.Logs))
	}
	if logsResponse.Logs[0].Body != "tail test log" {
		t.Errorf("logs[0].Body = %q, want %q", logsResponse.Logs[0].Body, "tail test log")
	}

	// metrics: query by name, expect empty (no metrics in the batch).
	var metricsResponse telemetry.MetricsResponse
	if err := queryClient.Call(t.Context(), "metrics", telemetry.MetricsRequest{
		Name: "nonexistent.metric",
	}, &metricsResponse); err != nil {
		t.Fatalf("metrics query: %v", err)
	}
	if len(metricsResponse.Metrics) != 0 {
		t.Errorf("metrics: got %d points, want 0", len(metricsResponse.Metrics))
	}

	// top: range covering our batch timestamp, expect the operation
	// from our span.
	var topResponse telemetry.TopResponse
	if err := queryClient.Call(t.Context(), "top", telemetry.TopRequest{
		Start: batchTimestamp - int64(time.Hour),
		End:   batchTimestamp + int64(time.Hour),
	}, &topResponse); err != nil {
		t.Fatalf("top query: %v", err)
	}
	if len(topResponse.HighestThroughput) != 1 {
		t.Fatalf("top throughput: got %d entries, want 1", len(topResponse.HighestThroughput))
	}
	if topResponse.HighestThroughput[0].Operation != "test.tail.operation" {
		t.Errorf("top throughput[0].Operation = %q, want test.tail.operation",
			topResponse.HighestThroughput[0].Operation)
	}
	if topResponse.HighestThroughput[0].Count != 1 {
		t.Errorf("top throughput[0].Count = %d, want 1", topResponse.HighestThroughput[0].Count)
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

	// Read from the subscribe stream until we find the socket.handle
	// span. The emitter flushes every 5s (configured in
	// BootstrapViaProxy). Multiple frames may arrive: the proxy also
	// emits proxy.forward spans for HTTP requests it handles (invite
	// acceptance, sync calls, etc.), and these may appear in earlier
	// frames before the socket server's socket.handle span.
	var found *telemetry.Span
	for found == nil {
		var frame telemetryMockSubscribeFrame
		if err := subscribeDecoder.Decode(&frame); err != nil {
			t.Fatalf("read subscribe frame from mock: %v", err)
		}

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

// TestProxyTelemetryEmission verifies that the proxy emits telemetry spans
// through the telemetry relay when the daemon wires up the relay connection.
//
// End-to-end flow: daemon resolves telemetry service binding in the config
// room → mints a telemetry service token for the proxy → passes socket path
// and token path to the launcher via IPC → launcher pipes them into the
// proxy's credential payload → proxy creates a TelemetryEmitter and records
// a proxy.forward span for each HTTP request → emitter flushes spans to the
// relay socket → telemetry mock's subscribe stream delivers them here.
func TestProxyTelemetryEmission(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "proxytel")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the telemetry mock and publish a service binding so the
	// daemon resolves the telemetry relay for all subsequent principals.
	mockService := deployTelemetryMock(t, admin, fleet, machine, "proxytel")

	// Open a subscribe stream on the mock BEFORE deploying the agent.
	// The subscriber must be registered before the proxy's emitter
	// flushes so we don't miss spans from the agent's startup activity.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/proxytel-observer")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}
	subscribeToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	subscribeConn := openTelemetryStream(t, mockService.SocketPath, "subscribe", subscribeToken)
	subscribeDecoder := codec.NewDecoder(subscribeConn)

	// Deploy the test agent. The daemon sees the telemetry service
	// binding, mints a telemetry token, and passes both the socket
	// path and token path to the launcher. The proxy starts with
	// telemetry enabled and emits proxy.forward spans for every HTTP
	// request it processes.
	//
	// SkipWaitForReady: the test exercises the proxy from the host side
	// (proxyWhoami below), not through the sandboxed agent process.
	// bureau-test-agent's ready message ("quickstart-test-ready") also
	// doesn't match deployAgent's "agent-ready" substring check.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           resolvedBinary(t, "TEST_AGENT_BINARY"),
		Localpart:        "agent/proxytel-test",
		SkipWaitForReady: true,
	})

	// Make an HTTP request through the proxy. The proxy records a
	// proxy.forward span for this Matrix whoami call.
	proxyClient := proxyHTTPClient(agent.ProxySocketPath)
	userID := proxyWhoami(t, proxyClient)
	if userID == "" {
		t.Fatal("proxyWhoami returned empty user ID")
	}

	// Read from the subscribe stream. The proxy's emitter flushes
	// every 5 seconds. When the batch arrives, the mock pushes a
	// frame containing the spans. There may be multiple frames if
	// the test agent's startup activity triggered earlier flushes.
	var found *telemetry.Span
	for found == nil {
		var frame telemetryMockSubscribeFrame
		if err := subscribeDecoder.Decode(&frame); err != nil {
			t.Fatalf("read subscribe frame from mock: %v", err)
		}

		for index := range frame.Spans {
			span := &frame.Spans[index]
			if span.Operation == "proxy.forward" {
				serviceName, _ := span.Attributes["service"].(string)
				httpPath, _ := span.Attributes["http.path"].(string)
				if serviceName == "matrix" && httpPath == "/_matrix/client/v3/account/whoami" {
					found = span
					break
				}
			}
		}
	}

	// Verify the span carries the expected attributes and identity.
	if found.Status != telemetry.SpanStatusOK {
		t.Errorf("expected span status OK, got %d", found.Status)
	}
	httpMethod, _ := found.Attributes["http.method"].(string)
	if httpMethod != "GET" {
		t.Errorf("expected http.method %q, got %q", "GET", httpMethod)
	}
	httpStatusCode, _ := found.Attributes["http.status_code"]
	if statusUint, ok := httpStatusCode.(uint64); !ok || statusUint != 200 {
		t.Errorf("expected http.status_code 200 (uint64), got %v (%T)", httpStatusCode, httpStatusCode)
	}
	if found.Duration <= 0 {
		t.Errorf("expected positive duration, got %d", found.Duration)
	}
	if found.Fleet.IsZero() {
		t.Error("span Fleet is zero (proxy emitter did not stamp identity)")
	}
	if found.Machine.IsZero() {
		t.Error("span Machine is zero (proxy emitter did not stamp identity)")
	}
	if found.Source.IsZero() {
		t.Error("span Source is zero (proxy emitter did not stamp identity)")
	}
	if found.TraceID.IsZero() {
		t.Error("span TraceID is zero")
	}
	if found.SpanID.IsZero() {
		t.Error("span SpanID is zero")
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

// TestTelemetryOutputDeltaPersistence exercises the full output delta
// persistence pipeline: ingest → log manager → artifact store → m.bureau.log
// state event. Verifies:
//   - Output deltas exceeding the 1 MB chunk threshold trigger an immediate
//     flush to the artifact service
//   - The m.bureau.log state event appears in the machine config room with
//     correct session, source, and chunk metadata
//   - The artifact content matches the original delta data
//   - The complete-log action transitions the log entity to "complete"
func TestTelemetryOutputDeltaPersistence(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)
	ctx := t.Context()

	machine := newTestMachine(t, fleet, "logpersist")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Deploy the artifact service ---

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	artifactSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-logtest",
		Localpart: "service/artifact/logtest",
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	// Publish the artifact service binding so the daemon resolves
	// RequiredServices: ["artifact"] to this service's socket.
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "artifact",
		schema.ServiceBindingContent{Principal: artifactSvc.Entity}); err != nil {
		t.Fatalf("publish artifact service binding: %v", err)
	}

	// --- Deploy the telemetry service with artifact dependency ---

	telemetryBinary := resolvedBinary(t, "TELEMETRY_SERVICE_BINARY")
	telemetrySvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           telemetryBinary,
		Name:             "telemetry-logtest",
		Localpart:        "service/telemetry/logtest",
		Command:          []string{telemetryBinary, "--storage-path", "/tmp/telemetry.db"},
		RequiredServices: []string{"artifact"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{artifact.ActionStore}},
			},
		},
	})

	// Publish the telemetry service binding so daemon routes
	// telemetry to this service.
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "telemetry",
		schema.ServiceBindingContent{Principal: telemetrySvc.Entity}); err != nil {
		t.Fatalf("publish telemetry service binding: %v", err)
	}

	// --- Verify artifact persistence is active ---

	// Query the telemetry service status to confirm that the artifact
	// client was successfully created inside the sandbox. If this is
	// false, the bind-mount of the artifact socket or token into the
	// telemetry sandbox failed and all persistence will be skipped.
	telemetryStatus := queryTelemetryStatus(t, telemetrySvc.SocketPath)
	if !telemetryStatus.ArtifactPersistence {
		t.Fatal("telemetry service started without artifact persistence — artifact socket or token not available inside sandbox")
	}

	// --- Mint tokens ---

	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/logtest-caller")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}

	ingestToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{"telemetry/ingest"}}})

	artifactFetchToken := mintTestServiceToken(t, machine, callerEntity, "artifact",
		[]servicetoken.Grant{{Actions: []string{artifact.ActionFetch}}})

	// --- Send output deltas exceeding the 1 MB chunk threshold ---

	// Build a 1.1 MB payload. This exceeds the log manager's default
	// chunkSizeThreshold (1 MB), triggering an immediate synchronous
	// flush within HandleDeltas. By the time the batch ack returns,
	// the artifact is stored and the state event is written.
	const payloadSize = 1_100_000
	deltaData := bytes.Repeat([]byte("output-persistence-test\n"), payloadSize/24+1)
	deltaData = deltaData[:payloadSize]

	sessionID := "logtest-session-001"

	ingestConn := openTelemetryStream(t, telemetrySvc.SocketPath, "ingest", ingestToken)
	ingestEncoder := codec.NewEncoder(ingestConn)
	ingestDecoder := codec.NewDecoder(ingestConn)

	batch := telemetry.TelemetryBatch{
		Fleet:          fleet.Ref,
		Machine:        machine.Ref,
		SequenceNumber: 1,
		OutputDeltas: []telemetry.OutputDelta{{
			Fleet:     fleet.Ref,
			Machine:   machine.Ref,
			Source:    callerEntity,
			SessionID: sessionID,
			Sequence:  0,
			Stream:    telemetry.OutputStreamCombined,
			Timestamp: 1000000000,
			Data:      deltaData,
		}},
	}

	if err := ingestEncoder.Encode(batch); err != nil {
		t.Fatalf("send ingest batch: %v", err)
	}

	var batchAck telemetry.StreamAck
	if err := ingestDecoder.Decode(&batchAck); err != nil {
		t.Fatalf("read ingest batch ack: %v", err)
	}
	if !batchAck.OK {
		t.Fatalf("ingest batch ack rejected: %s", batchAck.Error)
	}

	// --- Verify the log manager stats after ingestion ---

	postIngestStatus := queryTelemetryStatus(t, telemetrySvc.SocketPath)
	t.Logf("post-ingest status: batches=%d output_deltas=%d log_manager=%+v",
		postIngestStatus.BatchesReceived,
		postIngestStatus.OutputDeltasReceived,
		postIngestStatus.LogManager)

	if postIngestStatus.LogManager.FlushErrors > 0 {
		t.Fatalf("log manager had %d flush errors (store_errors=%d, metadata_errors=%d, last_error=%q)",
			postIngestStatus.LogManager.FlushErrors,
			postIngestStatus.LogManager.StoreErrors,
			postIngestStatus.LogManager.MetadataErrors,
			postIngestStatus.LogManager.LastError)
	}
	if postIngestStatus.LogManager.FlushCount == 0 {
		t.Fatal("log manager flush_count is 0 — HandleDeltas did not trigger a flush despite exceeding the 1 MB threshold")
	}
	if postIngestStatus.LogManager.StoreCount == 0 {
		t.Fatal("log manager store_count is 0 — artifact store was never called")
	}

	// --- Verify the log metadata artifact ---

	// HandleDeltas flushes synchronously when the buffer exceeds the
	// threshold, so by the time we receive the ack the metadata
	// artifact should be persisted. Read it via the mutable tag.
	artifactClient := artifactstore.NewClientFromToken(artifactSvc.SocketPath, artifactFetchToken)
	tagName := "log/" + callerEntity.Localpart() + "/" + sessionID
	logContent := fetchLogMetadata(t, artifactClient, tagName)

	if logContent.Version != log.LogContentVersion {
		t.Errorf("log version = %d, want %d", logContent.Version, log.LogContentVersion)
	}
	if logContent.SessionID != sessionID {
		t.Errorf("log session_id = %q, want %q", logContent.SessionID, sessionID)
	}
	if logContent.Source.IsZero() {
		t.Error("log source is zero")
	}
	if logContent.Format != log.LogFormatRaw {
		t.Errorf("log format = %q, want %q", logContent.Format, log.LogFormatRaw)
	}
	if logContent.Status != log.LogStatusActive {
		t.Errorf("log status = %q, want %q", logContent.Status, log.LogStatusActive)
	}
	if logContent.TotalBytes != int64(payloadSize) {
		t.Errorf("log total_bytes = %d, want %d", logContent.TotalBytes, payloadSize)
	}
	if length := len(logContent.Chunks); length != 1 {
		t.Fatalf("log chunks length = %d, want 1", length)
	}

	chunk := logContent.Chunks[0]
	if chunk.Ref == "" {
		t.Fatal("chunk ref is empty")
	}
	if chunk.Size != int64(payloadSize) {
		t.Errorf("chunk size = %d, want %d", chunk.Size, payloadSize)
	}
	if chunk.Timestamp != 1000000000 {
		t.Errorf("chunk timestamp = %d, want 1000000000", chunk.Timestamp)
	}

	// --- Fetch the data artifact and verify content ---

	fetchResult, err := artifactClient.Fetch(ctx, chunk.Ref)
	if err != nil {
		t.Fatalf("fetch artifact %s: %v", chunk.Ref, err)
	}
	defer fetchResult.Content.Close()

	fetchedData, err := io.ReadAll(fetchResult.Content)
	if err != nil {
		t.Fatalf("read artifact content: %v", err)
	}

	if !bytes.Equal(fetchedData, deltaData) {
		t.Errorf("artifact content mismatch: got %d bytes, want %d bytes", len(fetchedData), len(deltaData))
		if len(fetchedData) > 0 && len(deltaData) > 0 {
			// Show first divergence for diagnostics.
			for index := 0; index < len(fetchedData) && index < len(deltaData); index++ {
				if fetchedData[index] != deltaData[index] {
					t.Errorf("first divergence at byte %d: got 0x%02x, want 0x%02x",
						index, fetchedData[index], deltaData[index])
					break
				}
			}
		}
	}

	// --- Call complete-log and verify status transition ---

	completeLogToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{"telemetry/ingest"}}})
	telemetryClient := service.NewServiceClientFromToken(telemetrySvc.SocketPath, completeLogToken)

	var completeResponse telemetry.CompleteLogResponse
	if err := telemetryClient.Call(ctx, "complete-log", telemetry.CompleteLogRequest{
		Source:    callerEntity.UserID(),
		SessionID: sessionID,
	}, &completeResponse); err != nil {
		t.Fatalf("complete-log call: %v", err)
	}
	if !completeResponse.Completed {
		t.Error("complete-log response: completed = false, want true")
	}

	// Re-read the tag and verify status transitioned.
	completedLogContent := fetchLogMetadata(t, artifactClient, tagName)

	if completedLogContent.Status != log.LogStatusComplete {
		t.Errorf("completed log status = %q, want %q", completedLogContent.Status, log.LogStatusComplete)
	}
	if completedLogContent.TotalBytes != int64(payloadSize) {
		t.Errorf("completed log total_bytes = %d, want %d", completedLogContent.TotalBytes, payloadSize)
	}
	if length := len(completedLogContent.Chunks); length != 1 {
		t.Errorf("completed log chunks length = %d, want 1", length)
	}

	// Verify status endpoint reflects the output delta count.
	unauthClient := service.NewServiceClientFromToken(telemetrySvc.SocketPath, nil)
	var status telemetry.ServiceStatus
	if err := unauthClient.Call(ctx, "status", nil, &status); err != nil {
		t.Fatalf("status call: %v", err)
	}
	if status.OutputDeltasReceived != 1 {
		t.Errorf("output_deltas_received = %d, want 1", status.OutputDeltasReceived)
	}
}

// TestTelemetryOutputRotation exercises the log manager's eviction
// pipeline end-to-end: ingest enough output to exceed a small
// --max-bytes-per-session limit, trigger the reaper via the explicit
// "reap" action, and verify the m.bureau.log state event reflects
// chunk eviction (trimmed chunk list, reduced totalBytes, "rotating"
// status).
//
// The telemetry service is deployed with --chunk-size-threshold=5000
// and --max-bytes-per-session=12000. Each batch sends 6000 bytes,
// which immediately exceeds the threshold and triggers a synchronous
// flush. After 4 batches (24000 bytes across 4 chunks), the reaper
// evicts the oldest 2 chunks to bring the session back under 12000.
func TestTelemetryOutputRotation(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)
	ctx := t.Context()

	machine := newTestMachine(t, fleet, "logrotate")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Deploy artifact service ---

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	artifactSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-rotate",
		Localpart: "service/artifact/rotate",
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "artifact",
		schema.ServiceBindingContent{Principal: artifactSvc.Entity}); err != nil {
		t.Fatalf("publish artifact service binding: %v", err)
	}

	// --- Deploy telemetry service with small limits ---

	telemetryBinary := resolvedBinary(t, "TELEMETRY_SERVICE_BINARY")
	telemetrySvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    telemetryBinary,
		Name:      "telemetry-rotate",
		Localpart: "service/telemetry/rotate",
		Command: []string{
			telemetryBinary,
			"--storage-path", "/tmp/telemetry.db",
			"--chunk-size-threshold", "5000",
			"--max-bytes-per-session", "12000",
		},
		RequiredServices: []string{"artifact"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{artifact.ActionStore}},
			},
		},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "telemetry",
		schema.ServiceBindingContent{Principal: telemetrySvc.Entity}); err != nil {
		t.Fatalf("publish telemetry service binding: %v", err)
	}

	// Verify artifact persistence is active.
	telemetryStatus := queryTelemetryStatus(t, telemetrySvc.SocketPath)
	if !telemetryStatus.ArtifactPersistence {
		t.Fatal("telemetry service started without artifact persistence")
	}

	// --- Mint tokens ---

	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/rotate-caller")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}

	ingestToken := mintTestServiceToken(t, machine, callerEntity, "telemetry",
		[]servicetoken.Grant{{Actions: []string{"telemetry/ingest"}}})

	// --- Send 4 batches of 6000 bytes each ---

	const chunkSize = 6000
	const batchCount = 4
	sessionID := "rotate-session-001"

	ingestConn := openTelemetryStream(t, telemetrySvc.SocketPath, "ingest", ingestToken)
	ingestEncoder := codec.NewEncoder(ingestConn)
	ingestDecoder := codec.NewDecoder(ingestConn)

	for batchIndex := 0; batchIndex < batchCount; batchIndex++ {
		// Each batch has a unique pattern so we can identify which
		// chunks survived eviction by fetching their artifact content.
		pattern := fmt.Sprintf("batch-%d-data-", batchIndex)
		deltaData := bytes.Repeat([]byte(pattern), chunkSize/len(pattern)+1)
		deltaData = deltaData[:chunkSize]

		batch := telemetry.TelemetryBatch{
			Fleet:          fleet.Ref,
			Machine:        machine.Ref,
			SequenceNumber: uint64(batchIndex + 1),
			OutputDeltas: []telemetry.OutputDelta{{
				Fleet:     fleet.Ref,
				Machine:   machine.Ref,
				Source:    callerEntity,
				SessionID: sessionID,
				Sequence:  uint64(batchIndex * chunkSize),
				Stream:    telemetry.OutputStreamCombined,
				Timestamp: int64(1000000000 + batchIndex*1000),
				Data:      deltaData,
			}},
		}

		if err := ingestEncoder.Encode(batch); err != nil {
			t.Fatalf("send ingest batch %d: %v", batchIndex, err)
		}

		var batchAck telemetry.StreamAck
		if err := ingestDecoder.Decode(&batchAck); err != nil {
			t.Fatalf("read ingest batch %d ack: %v", batchIndex, err)
		}
		if !batchAck.OK {
			t.Fatalf("ingest batch %d ack rejected: %s", batchIndex, batchAck.Error)
		}
	}

	// --- Verify pre-eviction state ---

	// Each 6000-byte batch exceeds the 5000-byte chunk threshold, so
	// HandleDeltas flushes synchronously on every batch. After 4
	// batches the metadata should have 4 chunks totaling 24000 bytes.
	artifactFetchToken := mintTestServiceToken(t, machine, callerEntity, "artifact",
		[]servicetoken.Grant{{Actions: []string{artifact.ActionFetch}}})
	artifactClient := artifactstore.NewClientFromToken(artifactSvc.SocketPath, artifactFetchToken)

	tagName := "log/" + callerEntity.Localpart() + "/" + sessionID
	preEvictionLog := fetchLogMetadata(t, artifactClient, tagName)

	if preEvictionLog.Status != log.LogStatusActive {
		t.Errorf("pre-eviction status = %q, want %q", preEvictionLog.Status, log.LogStatusActive)
	}
	if length := len(preEvictionLog.Chunks); length != batchCount {
		t.Fatalf("pre-eviction chunks = %d, want %d", length, batchCount)
	}
	expectedPreTotalBytes := int64(chunkSize * batchCount)
	if preEvictionLog.TotalBytes != expectedPreTotalBytes {
		t.Errorf("pre-eviction total_bytes = %d, want %d", preEvictionLog.TotalBytes, expectedPreTotalBytes)
	}

	// Record the last two chunk refs — these should survive eviction.
	survivingRefs := make([]string, 2)
	survivingRefs[0] = preEvictionLog.Chunks[2].Ref
	survivingRefs[1] = preEvictionLog.Chunks[3].Ref

	// --- Trigger eviction via the reap action ---

	telemetryClient := service.NewServiceClientFromToken(telemetrySvc.SocketPath, ingestToken)
	if err := telemetryClient.Call(ctx, "reap", nil, nil); err != nil {
		t.Fatalf("reap call: %v", err)
	}

	// --- Verify post-eviction state ---

	postEvictionLog := fetchLogMetadata(t, artifactClient, tagName)

	// With max=12000 and chunks of 6000, eviction removes the oldest
	// 2 chunks (24000-6000=18000 > 12000, 18000-6000=12000 ≤ 12000).
	if postEvictionLog.Status != log.LogStatusRotating {
		t.Errorf("post-eviction status = %q, want %q", postEvictionLog.Status, log.LogStatusRotating)
	}
	if length := len(postEvictionLog.Chunks); length != 2 {
		t.Fatalf("post-eviction chunks = %d, want 2", length)
	}
	expectedPostTotalBytes := int64(chunkSize * 2)
	if postEvictionLog.TotalBytes != expectedPostTotalBytes {
		t.Errorf("post-eviction total_bytes = %d, want %d", postEvictionLog.TotalBytes, expectedPostTotalBytes)
	}

	// Verify the surviving chunks are the last two (most recent).
	if postEvictionLog.Chunks[0].Ref != survivingRefs[0] {
		t.Errorf("surviving chunk[0] ref = %q, want %q", postEvictionLog.Chunks[0].Ref, survivingRefs[0])
	}
	if postEvictionLog.Chunks[1].Ref != survivingRefs[1] {
		t.Errorf("surviving chunk[1] ref = %q, want %q", postEvictionLog.Chunks[1].Ref, survivingRefs[1])
	}

	// Verify the eviction counter in the status endpoint.
	postEvictionStatus := queryTelemetryStatus(t, telemetrySvc.SocketPath)
	if postEvictionStatus.LogManager.EvictionCount == 0 {
		t.Error("eviction_count is 0 after reap — eviction did not fire")
	}

	// --- Verify surviving artifacts are still fetchable ---

	for index, chunkRef := range survivingRefs {
		fetchResult, err := artifactClient.Fetch(ctx, chunkRef)
		if err != nil {
			t.Fatalf("fetch surviving chunk[%d] artifact %s: %v", index, chunkRef, err)
		}
		fetchedData, err := io.ReadAll(fetchResult.Content)
		fetchResult.Content.Close()
		if err != nil {
			t.Fatalf("read surviving chunk[%d] content: %v", index, err)
		}
		if len(fetchedData) != chunkSize {
			t.Errorf("surviving chunk[%d] size = %d, want %d", index, len(fetchedData), chunkSize)
		}
	}

	// --- Complete the session and verify final state ---

	var completeResponse telemetry.CompleteLogResponse
	if err := telemetryClient.Call(ctx, "complete-log", telemetry.CompleteLogRequest{
		Source:    callerEntity.UserID(),
		SessionID: sessionID,
	}, &completeResponse); err != nil {
		t.Fatalf("complete-log call: %v", err)
	}
	if !completeResponse.Completed {
		t.Error("complete-log response: completed = false, want true")
	}

	completedLog := fetchLogMetadata(t, artifactClient, tagName)

	if completedLog.Status != log.LogStatusComplete {
		t.Errorf("completed status = %q, want %q", completedLog.Status, log.LogStatusComplete)
	}
	// After completion, the chunk list should still be the 2 surviving
	// chunks (complete-log doesn't re-add evicted chunks).
	if length := len(completedLog.Chunks); length != 2 {
		t.Errorf("completed chunks = %d, want 2", length)
	}
	if completedLog.TotalBytes != expectedPostTotalBytes {
		t.Errorf("completed total_bytes = %d, want %d", completedLog.TotalBytes, expectedPostTotalBytes)
	}
}

// --- Full-stack output capture tests (relay + service + artifact persistence) ---

// telemetryLogInfra holds the deployed three-service stack for
// full-stack output capture tests: artifact service → telemetry
// service → telemetry relay.
type telemetryLogInfra struct {
	artifactService  serviceDeployResult
	telemetryService serviceDeployResult
	telemetryRelay   serviceDeployResult

	// artifactClient is pre-authenticated for fetching artifacts and
	// resolving tags. Tests use it to verify persisted output data
	// and log metadata.
	artifactClient *artifactstore.Client
}

// deployTelemetryLogInfra deploys the three-service stack for full
// output capture persistence:
//
//  1. Artifact service (CAS storage)
//  2. Telemetry service (log manager + persistence, RequiredServices: ["artifact"])
//  3. Telemetry relay (accumulate + ship, RequiredServices: ["telemetry"])
//
// The relay is bound as "telemetry-relay" in the config room so the
// daemon's resolveTelemetrySocket finds it (preferring relay over
// service). The service is bound as "telemetry" so the relay's
// RequiredServices resolution finds the service (no circular
// dependency). The relay runs with --flush-threshold-bytes=1 for
// immediate shipping.
func deployTelemetryLogInfra(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	machine *testMachine,
	suffix string,
) telemetryLogInfra {
	t.Helper()
	ctx := t.Context()

	// --- Deploy the artifact service ---

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	artifactSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-" + suffix,
		Localpart: "service/artifact/" + suffix,
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "artifact",
		schema.ServiceBindingContent{Principal: artifactSvc.Entity}); err != nil {
		t.Fatalf("publish artifact service binding: %v", err)
	}

	// --- Deploy the telemetry service ---

	telemetryBinary := resolvedBinary(t, "TELEMETRY_SERVICE_BINARY")
	telemetrySvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           telemetryBinary,
		Name:             "telemetry-" + suffix,
		Localpart:        "service/telemetry/" + suffix,
		Command:          []string{telemetryBinary, "--storage-path", "/tmp/telemetry.db"},
		RequiredServices: []string{"artifact"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{artifact.ActionStore}},
			},
		},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "telemetry",
		schema.ServiceBindingContent{Principal: telemetrySvc.Entity}); err != nil {
		t.Fatalf("publish telemetry service binding: %v", err)
	}

	// Verify artifact persistence is active before deploying the
	// relay. If this fails, the artifact socket or token was not
	// correctly bind-mounted into the telemetry service sandbox.
	telemetryStatus := queryTelemetryStatus(t, telemetrySvc.SocketPath)
	if !telemetryStatus.ArtifactPersistence {
		t.Fatal("telemetry service started without artifact persistence — artifact socket or token not available inside sandbox")
	}

	// --- Deploy the telemetry relay ---

	relayBinary := resolvedBinary(t, "TELEMETRY_RELAY_BINARY")
	relayService := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           relayBinary,
		Name:             "relay-" + suffix,
		Localpart:        "service/telemetry-relay/" + suffix,
		Command:          []string{relayBinary, "--flush-threshold-bytes", "1"},
		RequiredServices: []string{"telemetry"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{"telemetry/ingest"}},
			},
		},
		ExtraEnvironmentVariables: map[string]string{
			"BUREAU_TELEMETRY_SERVICE_SOCKET": "/run/bureau/service/telemetry.sock",
			"BUREAU_TELEMETRY_TOKEN_PATH":     "/run/bureau/service/token/telemetry.token",
		},
	})

	// Publish the relay binding as "telemetry-relay" so the daemon's
	// resolveTelemetrySocket finds it. This is the socket path the
	// daemon passes to sandboxes for output capture and sends
	// complete-log to on sandbox exit.
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "telemetry-relay",
		schema.ServiceBindingContent{Principal: relayService.Entity}); err != nil {
		t.Fatalf("publish telemetry-relay service binding: %v", err)
	}

	// --- Mint artifact fetch token for verification ---

	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/"+suffix+"-verifier")
	if err != nil {
		t.Fatalf("construct verifier entity: %v", err)
	}

	artifactFetchToken := mintTestServiceToken(t, machine, callerEntity, "artifact",
		[]servicetoken.Grant{{Actions: []string{artifact.ActionFetch}}})

	return telemetryLogInfra{
		artifactService:  artifactSvc,
		telemetryService: telemetrySvc,
		telemetryRelay:   relayService,
		artifactClient:   artifactstore.NewClientFromToken(artifactSvc.SocketPath, artifactFetchToken),
	}
}

// waitForLogStoreCount polls the telemetry service status until the
// log manager's StoreCount reaches or exceeds the target. Used to
// confirm that the output capture chain is active and data is being
// persisted before proceeding with drain or exit verification.
func waitForLogStoreCount(t *testing.T, socketPath string, target uint64) {
	t.Helper()
	for {
		status := queryTelemetryStatus(t, socketPath)
		if status.LogManager.StoreCount >= target {
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("timed out waiting for log manager store count >= %d (current: %d, flush_errors: %d, last_error: %q)",
				target, status.LogManager.StoreCount, status.LogManager.FlushErrors, status.LogManager.LastError)
		}
		runtime.Gosched()
	}
}

// discoverLogTag uses the artifact service's tag listing to find
// the log metadata tag for a given source entity. The tag prefix
// is "log/<entity-localpart>/" matching the log manager's tag
// naming scheme. Returns the full tag name and the session ID
// extracted from it.
func discoverLogTag(t *testing.T, artifactClient *artifactstore.Client, source ref.Entity) (string, string) {
	t.Helper()
	ctx := t.Context()

	prefix := "log/" + source.Localpart() + "/"

	for {
		tags, err := artifactClient.Tags(ctx, prefix)
		if err != nil {
			t.Fatalf("list tags with prefix %q: %v", prefix, err)
		}
		if len(tags.Tags) > 0 {
			tagName := tags.Tags[0].Name
			sessionID := strings.TrimPrefix(tagName, prefix)
			return tagName, sessionID
		}
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for log tag with prefix %q", prefix)
		}
		runtime.Gosched()
	}
}

// TestTelemetryOutputLogPipeline exercises the full production output
// capture path end-to-end:
//
//	sandbox process → bureau-log-relay (PTY capture)
//	→ bureau-telemetry-relay (accumulate, ingest stream)
//	→ bureau-telemetry-service (log manager → artifact store → CAS metadata)
//	→ daemon watchSandboxExit → completeLogForPrincipal → status=complete
//
// Two scenarios run sequentially on the same infrastructure:
//   - Normal exit (exit code 0): agent produces ~1.7MB output and exits cleanly
//   - Crash exit (exit code 42): agent produces output and crashes
//
// Both verify: SandboxExitedMessage is received, log metadata tag
// exists with status=complete, chunks are non-empty, artifact content
// is retrievable.
func TestTelemetryOutputLogPipeline(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	runnerEnv := findRunnerEnv(t)
	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")

	machine := newTestMachine(t, fleet, "logpipe")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	infra := deployTelemetryLogInfra(t, admin, fleet, machine, "logpipe")

	// --- Scenario A: Normal exit (exit code 0) ---

	t.Run("normal-exit", func(t *testing.T) {
		exitWatch := watchRoom(t, admin, machine.ConfigRoomID)

		deployAgent(t, admin, machine, agentOptions{
			Binary:    testAgentBinary,
			Localpart: "agent/logpipe-normal",
			Command: []string{
				"sh", "-c",
				"dd if=/dev/urandom bs=64k count=20 2>/dev/null | base64; echo LOGTEST_NORMAL_DONE",
			},
			OutputCapture:    &schema.OutputCapture{Enabled: true},
			EnvironmentPath:  runnerEnv,
			RestartPolicy:    schema.RestartPolicyNever,
			SkipWaitForReady: true,
		})

		// Wait for the sandbox exit notification. The daemon fires
		// completeLogForPrincipal as part of the exit handling, so
		// by the time we see this message the complete-log request
		// has been sent to the relay (and proxied to the service).
		exitMessage := waitForNotification[schema.SandboxExitedMessage](
			t, &exitWatch, schema.MsgTypeSandboxExited, machine.UserID,
			func(message schema.SandboxExitedMessage) bool {
				return strings.Contains(message.Principal, "logpipe-normal")
			}, "sandbox exit for logpipe-normal")

		if exitMessage.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", exitMessage.ExitCode)
		}

		// Verify the telemetry service status reflects successful persistence.
		status := queryTelemetryStatus(t, infra.telemetryService.SocketPath)
		t.Logf("post-exit status: store=%d metadata=%d flush_errors=%d active=%d last_error=%q",
			status.LogManager.StoreCount,
			status.LogManager.MetadataWrites,
			status.LogManager.FlushErrors,
			status.LogManager.ActiveSessions,
			status.LogManager.LastError)

		if status.LogManager.StoreCount == 0 {
			t.Fatal("log manager store_count is 0 — no artifacts were stored")
		}
		if status.LogManager.FlushErrors > 0 {
			t.Fatalf("log manager had %d flush errors (last: %q)",
				status.LogManager.FlushErrors, status.LogManager.LastError)
		}

		// Discover the log metadata tag and verify content.
		agentEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/logpipe-normal")
		if err != nil {
			t.Fatalf("construct agent entity: %v", err)
		}
		tagName, sessionID := discoverLogTag(t, infra.artifactClient, agentEntity)
		t.Logf("discovered log tag: %s (session: %s)", tagName, sessionID)

		logContent := fetchLogMetadata(t, infra.artifactClient, tagName)

		if logContent.Status != log.LogStatusComplete {
			t.Errorf("log status = %q, want %q", logContent.Status, log.LogStatusComplete)
		}
		if logContent.TotalBytes < 1_000_000 {
			t.Errorf("log total_bytes = %d, want >= 1,000,000", logContent.TotalBytes)
		}
		if length := len(logContent.Chunks); length < 1 {
			t.Fatalf("log chunks length = %d, want >= 1", length)
		}

		// Fetch the first data chunk and verify it has content.
		firstChunk := logContent.Chunks[0]
		fetchResult, err := infra.artifactClient.Fetch(t.Context(), firstChunk.Ref)
		if err != nil {
			t.Fatalf("fetch chunk artifact %s: %v", firstChunk.Ref, err)
		}
		chunkData, err := io.ReadAll(fetchResult.Content)
		fetchResult.Content.Close()
		if err != nil {
			t.Fatalf("read chunk content: %v", err)
		}
		if len(chunkData) == 0 {
			t.Fatal("first chunk artifact has empty content")
		}
		t.Logf("first chunk: %d bytes, hash=%s", len(chunkData), firstChunk.Ref)
	})

	// --- Scenario B: Crash exit (exit code 42) ---

	t.Run("crash-exit", func(t *testing.T) {
		exitWatch := watchRoom(t, admin, machine.ConfigRoomID)

		deployAgent(t, admin, machine, agentOptions{
			Binary:    testAgentBinary,
			Localpart: "agent/logpipe-crash",
			Command: []string{
				"sh", "-c",
				"dd if=/dev/urandom bs=64k count=20 2>/dev/null | base64; exit 42",
			},
			OutputCapture:    &schema.OutputCapture{Enabled: true},
			EnvironmentPath:  runnerEnv,
			RestartPolicy:    schema.RestartPolicyNever,
			SkipWaitForReady: true,
		})

		exitMessage := waitForNotification[schema.SandboxExitedMessage](
			t, &exitWatch, schema.MsgTypeSandboxExited, machine.UserID,
			func(message schema.SandboxExitedMessage) bool {
				return strings.Contains(message.Principal, "logpipe-crash")
			}, "sandbox exit for logpipe-crash")

		if exitMessage.ExitCode != 42 {
			t.Errorf("expected exit code 42, got %d", exitMessage.ExitCode)
		}

		// Verify log metadata was persisted with status=complete.
		crashEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/logpipe-crash")
		if err != nil {
			t.Fatalf("construct agent entity: %v", err)
		}
		tagName, sessionID := discoverLogTag(t, infra.artifactClient, crashEntity)
		t.Logf("discovered log tag: %s (session: %s)", tagName, sessionID)

		logContent := fetchLogMetadata(t, infra.artifactClient, tagName)

		if logContent.Status != log.LogStatusComplete {
			t.Errorf("log status = %q, want %q", logContent.Status, log.LogStatusComplete)
		}
		if logContent.TotalBytes < 1_000_000 {
			t.Errorf("log total_bytes = %d, want >= 1,000,000", logContent.TotalBytes)
		}
		if length := len(logContent.Chunks); length < 1 {
			t.Fatalf("log chunks length = %d, want >= 1", length)
		}
	})
}

// TestTelemetryOutputLogDrain exercises the graceful drain path:
// removing a principal from the machine config triggers the daemon
// to send SIGTERM and call completeLogForPrincipal, which flushes
// the relay and transitions the log session to "complete".
//
// This is the drain path in watchSandboxExit (wasDraining=true),
// which destroys the proxy first, then calls completeLogForPrincipal.
func TestTelemetryOutputLogDrain(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()
	fleet := createTestFleet(t, admin)

	runnerEnv := findRunnerEnv(t)
	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")

	machine := newTestMachine(t, fleet, "logdrain")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	infra := deployTelemetryLogInfra(t, admin, fleet, machine, "logdrain")

	// Deploy a long-running agent that continuously produces output.
	exitWatch := watchRoom(t, admin, machine.ConfigRoomID)

	deployment := deployAgent(t, admin, machine, agentOptions{
		Binary:    testAgentBinary,
		Localpart: "agent/logdrain-producer",
		Command: []string{
			"sh", "-c",
			// Initial burst crosses the 1MB chunk threshold for
			// immediate artifact storage. The loop keeps the
			// process alive so the drain path can be exercised.
			"dd if=/dev/urandom bs=64k count=20 2>/dev/null | base64; while true; do sleep 1; done",
		},
		OutputCapture:    &schema.OutputCapture{Enabled: true},
		EnvironmentPath:  runnerEnv,
		RestartPolicy:    schema.RestartPolicyNever,
		SkipWaitForReady: true,
	})

	// Wait for output to actually reach the telemetry service and
	// be stored as an artifact. This confirms the full capture chain
	// (sandbox → log relay → telemetry relay → service → artifact)
	// is functioning before we trigger the drain.
	waitForLogStoreCount(t, infra.telemetryService.SocketPath, 1)
	t.Log("output persistence confirmed, triggering drain")

	// Trigger drain: push a machine config that excludes the agent.
	// The daemon will send SIGTERM, wait for exit, and call
	// completeLogForPrincipal.
	_ = deployment // suppress unused warning; we just need the side effects

	pushMachineConfig(t, admin, machine, deploymentConfig{})

	// Wait for the sandbox exit notification.
	waitForNotification[schema.SandboxExitedMessage](
		t, &exitWatch, schema.MsgTypeSandboxExited, machine.UserID,
		func(message schema.SandboxExitedMessage) bool {
			return strings.Contains(message.Principal, "logdrain-producer")
		}, "sandbox exit for logdrain-producer")

	// Verify the session was completed.
	status := queryTelemetryStatus(t, infra.telemetryService.SocketPath)
	t.Logf("post-drain status: store=%d metadata=%d flush_errors=%d active=%d",
		status.LogManager.StoreCount,
		status.LogManager.MetadataWrites,
		status.LogManager.FlushErrors,
		status.LogManager.ActiveSessions)

	if status.LogManager.StoreCount == 0 {
		t.Fatal("log manager store_count is 0 — no artifacts stored before drain")
	}
	if status.LogManager.FlushErrors > 0 {
		t.Fatalf("log manager had %d flush errors (last: %q)",
			status.LogManager.FlushErrors, status.LogManager.LastError)
	}

	// Discover and verify the log metadata.
	drainEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/logdrain-producer")
	if err != nil {
		t.Fatalf("construct agent entity: %v", err)
	}
	tagName, sessionID := discoverLogTag(t, infra.artifactClient, drainEntity)
	t.Logf("discovered log tag: %s (session: %s)", tagName, sessionID)

	logContent := fetchLogMetadata(t, infra.artifactClient, tagName)

	if logContent.Status != log.LogStatusComplete {
		t.Errorf("log status = %q, want %q", logContent.Status, log.LogStatusComplete)
	}
	if logContent.TotalBytes == 0 {
		t.Error("log total_bytes = 0, expected some output before drain")
	}
	if length := len(logContent.Chunks); length < 1 {
		t.Fatalf("log chunks length = %d, want >= 1", length)
	}
}
