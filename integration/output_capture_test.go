// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"testing"

	pipelinecmd "github.com/bureau-foundation/bureau/cmd/bureau/pipeline"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
)

// telemetryMockOutputDeltaResult is the CBOR response from the telemetry
// mock's query-output-deltas action. Defined locally — standard pattern
// (see telemetryMockSpanResult, telemetryMockLogResult in telemetry_mock_test.go).
type telemetryMockOutputDeltaResult struct {
	OutputDeltas []telemetry.OutputDelta `cbor:"output_deltas"`
	Count        int                     `cbor:"count"`
}

// TestOutputCapturePipeline exercises end-to-end output capture for
// pipeline execution. The test proves the full capture chain:
//
//	sandbox process → bureau-log-relay (PTY capture) → telemetry mock → subscriber
//
// Two verification paths:
//
//   - Live streaming: a subscribe stream on the telemetry mock receives
//     OutputDelta frames as the pipeline runs, proving real-time tailing works.
//   - Post-completion reconstruction: query-output-deltas returns stored
//     deltas whose concatenated Data fields contain the pipeline's output,
//     proving historical log retrieval works.
//
// The test validates the complete wiring added for output capture:
//
//   - SandboxSpec.OutputCapture plumbing from daemon to launcher
//   - Telemetry token minting in startPipelineExecutor
//   - Sandbox script generation with capture flags (--relay, --token, etc.)
//   - bureau-log-relay capture mode (PTY interposition, OutputDelta streaming)
//   - Telemetry mock submit acceptance and subscribe broadcasting
func TestOutputCapturePipeline(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "outcap")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Deploy telemetry mock as the fleet-wide telemetry service ---
	//
	// The daemon resolves this via resolveTelemetrySocket when building
	// the pipeline executor's IPC request. bureau-log-relay sends
	// OutputDelta submit requests directly to the mock's socket.
	mockService := deployTelemetryMock(t, admin, fleet, machine, "outcap")

	// Build a caller entity for minting service tokens.
	callerEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/outcap-test")
	if err != nil {
		t.Fatalf("construct caller entity: %v", err)
	}

	// Open a subscribe stream on the mock BEFORE pipeline execution.
	// This guarantees the subscriber receives OutputDelta frames from
	// bureau-log-relay's submit calls, regardless of timing.
	subscribeToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	subscribeConn := openTelemetryStream(t, mockService.SocketPath, "subscribe", subscribeToken)
	subscribeDecoder := codec.NewDecoder(subscribeConn)

	// --- Ticket service setup ---
	ticketSvc := deployTicketService(t, admin, fleet, machine, "outcap")

	projectRoomID := createTicketProjectRoom(t, admin, "outcap-project",
		ticketSvc.Entity, machine.UserID.String())

	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

	// --- Publish pipeline definition ---
	//
	// The marker is unique per test run to prevent cross-test
	// interference if the mock receives telemetry from other sources.
	marker := fmt.Sprintf("OUTPUT_CAPTURE_MARKER_%s", t.Name())

	publishPipelineDefinition(t, admin, "test-outcap", pipeline.PipelineContent{
		Description: "Output capture integration test pipeline",
		Steps: []pipeline.PipelineStep{
			{Name: "emit-marker", Run: fmt.Sprintf("echo %s", marker)},
		},
	})

	// --- Execute pipeline ---
	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:test-outcap",
			"room":     projectRoomID.String(),
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for accepted result: %v", err)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("pipeline.execute failed: %v", err)
	}
	if result.TicketID.IsZero() {
		t.Fatal("accepted result has empty ticket_id")
	}
	t.Logf("pipeline.execute accepted, ticket: %s in %s", result.TicketID, result.TicketRoom)

	// --- Live streaming verification ---
	//
	// Read subscribe frames until an OutputDelta whose Data contains
	// the marker arrives. bureau-log-relay flushes every 1 second or
	// on child exit, so the marker should arrive promptly. Frames
	// without OutputDeltas (e.g., proxy telemetry spans) are skipped.
	markerFound := false
	for !markerFound {
		var frame telemetryMockSubscribeFrame
		if err := subscribeDecoder.Decode(&frame); err != nil {
			t.Fatalf("read subscribe frame from mock: %v", err)
		}
		for _, delta := range frame.OutputDeltas {
			if bytes.Contains(delta.Data, []byte(marker)) {
				markerFound = true
				t.Logf("live streaming: marker found in OutputDelta (session=%s, sequence=%d, %d bytes)",
					delta.SessionID, delta.Sequence, len(delta.Data))
				break
			}
		}
	}

	// --- Wait for pipeline ticket to close ---
	final, err := command.WatchTicket(ctx, command.WatchTicketParams{
		Session:  admin,
		RoomID:   result.TicketRoom,
		TicketID: result.TicketID,
	})
	if err != nil {
		t.Fatalf("watching pipeline ticket: %v", err)
	}
	if final.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want %s", final.Status, ticket.StatusClosed)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != pipeline.ConclusionSuccess {
		t.Errorf("pipeline conclusion = %q, want %s", final.Pipeline.Conclusion, pipeline.ConclusionSuccess)
	}

	// --- Post-completion reconstruction ---
	//
	// Query all stored output deltas from the mock, concatenate their
	// Data fields sorted by Sequence, and verify the marker appears in
	// the reconstructed output. An empty query returns all deltas —
	// this test is the only source producing OutputDeltas.
	queryToken := mintTestServiceToken(t, machine, callerEntity, "telemetry", nil)
	queryClient := service.NewServiceClientFromToken(mockService.SocketPath, queryToken)

	var queryResult telemetryMockOutputDeltaResult
	if err := queryClient.Call(ctx, "query-output-deltas", map[string]any{}, &queryResult); err != nil {
		t.Fatalf("query-output-deltas: %v", err)
	}
	if queryResult.Count == 0 {
		t.Fatal("query-output-deltas returned 0 deltas")
	}

	sort.Slice(queryResult.OutputDeltas, func(i, j int) bool {
		return queryResult.OutputDeltas[i].Sequence < queryResult.OutputDeltas[j].Sequence
	})

	var reconstructed bytes.Buffer
	for _, delta := range queryResult.OutputDeltas {
		reconstructed.Write(delta.Data)
	}

	if !bytes.Contains(reconstructed.Bytes(), []byte(marker)) {
		t.Fatalf("reconstructed output (%d bytes from %d deltas) does not contain marker %q:\n%s",
			reconstructed.Len(), queryResult.Count, marker, reconstructed.String())
	}
	t.Logf("post-completion reconstruction: marker found in %d bytes from %d deltas",
		reconstructed.Len(), queryResult.Count)

	t.Log("output capture pipeline verified: sandbox -> PTY capture -> telemetry submit -> live stream + historical query")
}
