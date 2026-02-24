// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// --- Mock implementation for event checkpoint tracker ---

type mockEventCheckpointer struct {
	calls    []CheckpointContextRequest
	response *CheckpointContextResponse
	err      error
}

func (mock *mockEventCheckpointer) CheckpointContext(_ context.Context, request CheckpointContextRequest) (*CheckpointContextResponse, error) {
	mock.calls = append(mock.calls, request)
	if mock.err != nil {
		return nil, mock.err
	}
	response := mock.response
	if response == nil {
		response = &CheckpointContextResponse{
			ID: fmt.Sprintf("ctx-%08d", len(mock.calls)),
		}
	}
	return response, nil
}

// testTimestamp is a fixed timestamp for test events. The tracker
// treats timestamps as opaque data — they're serialized into CBOR
// artifacts but never interpreted by checkpoint logic.
var testTimestamp = time.Unix(1735689600, 0) // 2025-01-01T00:00:00Z

func testEventTracker(checkpointer eventContextCheckpointer) *eventCheckpointTracker {
	return newEventCheckpointTracker(
		checkpointer,
		"events-v1",
		"test-session-1",
		"test-template",
		slog.Default(),
	)
}

// --- Event checkpoint delta tests ---

func TestEventCheckpointDelta(t *testing.T) {
	checkpointer := &mockEventCheckpointer{}
	tracker := testEventTracker(checkpointer)

	tracker.appendEvent(Event{
		Timestamp: testTimestamp,
		Type:      EventTypePrompt,
		Prompt:    &PromptEvent{Content: "hello", Source: "initial"},
	})
	tracker.appendEvent(Event{
		Timestamp: testTimestamp,
		Type:      EventTypeResponse,
		Response:  &ResponseEvent{Content: "world"},
	})

	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Verify one checkpoint was created with inline CBOR data.
	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	request := checkpointer.calls[0]
	if len(request.Data) == 0 {
		t.Fatal("expected non-empty Data in checkpoint request")
	}

	// Verify the CBOR data decodes to 2 events.
	var decoded []Event
	if err := codec.Unmarshal(request.Data, &decoded); err != nil {
		t.Fatalf("decoding CBOR from checkpoint Data: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 events in checkpoint Data, got %d", len(decoded))
	}

	// Verify checkpoint metadata.
	if request.CommitType != "delta" {
		t.Errorf("commit type = %q, want %q", request.CommitType, "delta")
	}
	if request.Format != "events-v1" {
		t.Errorf("format = %q, want %q", request.Format, "events-v1")
	}
	if request.Checkpoint != agent.CheckpointTurnBoundary {
		t.Errorf("checkpoint = %q, want %q", request.Checkpoint, agent.CheckpointTurnBoundary)
	}
	if request.SessionID != "test-session-1" {
		t.Errorf("session ID = %q, want %q", request.SessionID, "test-session-1")
	}
	if request.Template != "test-template" {
		t.Errorf("template = %q, want %q", request.Template, "test-template")
	}
	if request.MessageCount != 2 {
		t.Errorf("message count = %d, want %d", request.MessageCount, 2)
	}
	if request.Parent != "" {
		t.Errorf("parent = %q, want empty (first checkpoint)", request.Parent)
	}

	// Verify tracker state updated.
	if tracker.currentContextID != "ctx-00000001" {
		t.Errorf("currentContextID = %q, want %q", tracker.currentContextID, "ctx-00000001")
	}
	if tracker.lastCheckpointIndex != 2 {
		t.Errorf("lastCheckpointIndex = %d, want 2", tracker.lastCheckpointIndex)
	}
}

func TestEventCheckpointDelta_Empty(t *testing.T) {
	checkpointer := &mockEventCheckpointer{}
	tracker := testEventTracker(checkpointer)

	// No events appended — should be a no-op.
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(checkpointer.calls) != 0 {
		t.Errorf("expected 0 checkpoint calls, got %d", len(checkpointer.calls))
	}
}

func TestEventCheckpointDelta_CheckpointFailure(t *testing.T) {
	checkpointer := &mockEventCheckpointer{err: fmt.Errorf("service unavailable")}
	tracker := testEventTracker(checkpointer)

	tracker.appendEvent(Event{
		Timestamp: testTimestamp,
		Type:      EventTypeResponse,
		Response:  &ResponseEvent{Content: "test"},
	})
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Checkpoint was called but failed.
	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	// Tracker state unchanged — the event pump continues.
	if tracker.currentContextID != "" {
		t.Errorf("expected empty currentContextID after failure, got %q", tracker.currentContextID)
	}
	if tracker.lastCheckpointIndex != 0 {
		t.Errorf("expected lastCheckpointIndex=0 after failure, got %d", tracker.lastCheckpointIndex)
	}
}

func TestEventCheckpointParentChain(t *testing.T) {
	checkpointer := &mockEventCheckpointer{}
	tracker := testEventTracker(checkpointer)

	// First checkpoint.
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypePrompt, Prompt: &PromptEvent{Content: "msg1", Source: "initial"}})
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "resp1"}})
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[0].Parent != "" {
		t.Errorf("first checkpoint parent = %q, want empty", checkpointer.calls[0].Parent)
	}

	// Second checkpoint — should chain from the first.
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{ID: "tc-1", Name: "Read"}})
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypeToolResult, ToolResult: &ToolResultEvent{ID: "tc-1", Output: "contents"}})
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "resp2"}})
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(checkpointer.calls) != 2 {
		t.Fatalf("expected 2 checkpoint calls, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[1].Parent != "ctx-00000001" {
		t.Errorf("second checkpoint parent = %q, want %q", checkpointer.calls[1].Parent, "ctx-00000001")
	}
	if checkpointer.calls[1].MessageCount != 3 {
		t.Errorf("second checkpoint message count = %d, want 3", checkpointer.calls[1].MessageCount)
	}

	// Third checkpoint — session end.
	tracker.appendEvent(Event{Timestamp: testTimestamp, Type: EventTypeMetric, Metric: &MetricEvent{Status: "success"}})
	tracker.checkpointDelta(context.Background(), agent.CheckpointSessionEnd)

	if len(checkpointer.calls) != 3 {
		t.Fatalf("expected 3 checkpoint calls, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[2].Parent != "ctx-00000002" {
		t.Errorf("third checkpoint parent = %q, want %q", checkpointer.calls[2].Parent, "ctx-00000002")
	}
}

// TestEventCheckpointTriggers simulates a full event stream and verifies
// that checkpoints fire at the correct boundaries when driven by the
// same trigger logic used in Run()'s event consumer.
func TestEventCheckpointTriggers(t *testing.T) {
	checkpointer := &mockEventCheckpointer{}
	tracker := testEventTracker(checkpointer)

	// Simulate the event consumer trigger logic from Run().
	appendAndTrigger := func(event Event) {
		tracker.appendEvent(event)
		switch {
		case event.Type == EventTypeResponse:
			tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)
		case event.Type == EventTypeSystem &&
			event.System != nil &&
			event.System.Subtype == "compact_boundary":
			tracker.checkpointDelta(context.Background(), agent.CheckpointCompaction)
		case event.Type == EventTypeMetric:
			tracker.checkpointDelta(context.Background(), agent.CheckpointSessionEnd)
		}
	}

	// Turn 1: prompt -> tool call -> tool result -> response.
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypePrompt, Prompt: &PromptEvent{Content: "analyze this", Source: "initial"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{ID: "tc-1", Name: "Read"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeToolResult, ToolResult: &ToolResultEvent{ID: "tc-1", Output: "data"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "I see the data"}})

	// Should have 1 checkpoint (turn boundary at response).
	if len(checkpointer.calls) != 1 {
		t.Fatalf("after turn 1: expected 1 checkpoint, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[0].Checkpoint != agent.CheckpointTurnBoundary {
		t.Errorf("checkpoint 1 trigger = %q, want %q", checkpointer.calls[0].Checkpoint, agent.CheckpointTurnBoundary)
	}
	if checkpointer.calls[0].MessageCount != 4 {
		t.Errorf("checkpoint 1 event count = %d, want 4", checkpointer.calls[0].MessageCount)
	}

	// Turn 2: thinking -> tool call -> tool result -> response.
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeThinking, Thinking: &ThinkingEvent{Content: "let me think"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{ID: "tc-2", Name: "Edit"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeToolResult, ToolResult: &ToolResultEvent{ID: "tc-2", Output: "done"}})
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "I edited the file"}})

	// Should have 2 checkpoints (turn boundary each).
	if len(checkpointer.calls) != 2 {
		t.Fatalf("after turn 2: expected 2 checkpoints, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[1].MessageCount != 4 {
		t.Errorf("checkpoint 2 event count = %d, want 4", checkpointer.calls[1].MessageCount)
	}

	// Compaction event.
	appendAndTrigger(Event{
		Timestamp: testTimestamp,
		Type:      EventTypeSystem,
		System:    &SystemEvent{Subtype: "compact_boundary", Metadata: json.RawMessage(`{"trigger":"auto"}`)},
	})

	// Should have 3 checkpoints (compaction).
	if len(checkpointer.calls) != 3 {
		t.Fatalf("after compaction: expected 3 checkpoints, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[2].Checkpoint != agent.CheckpointCompaction {
		t.Errorf("checkpoint 3 trigger = %q, want %q", checkpointer.calls[2].Checkpoint, agent.CheckpointCompaction)
	}
	if checkpointer.calls[2].MessageCount != 1 {
		t.Errorf("checkpoint 3 event count = %d, want 1", checkpointer.calls[2].MessageCount)
	}

	// Turn 3 after compaction: response only.
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "resuming after compaction"}})

	// Should have 4 checkpoints.
	if len(checkpointer.calls) != 4 {
		t.Fatalf("after turn 3: expected 4 checkpoints, got %d", len(checkpointer.calls))
	}

	// Session end: metric event.
	appendAndTrigger(Event{Timestamp: testTimestamp, Type: EventTypeMetric, Metric: &MetricEvent{
		InputTokens:  5000,
		OutputTokens: 2000,
		CostUSD:      0.025,
		Status:       "success",
	}})

	// Should have 5 checkpoints (session end).
	if len(checkpointer.calls) != 5 {
		t.Fatalf("after metric: expected 5 checkpoints, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[4].Checkpoint != agent.CheckpointSessionEnd {
		t.Errorf("checkpoint 5 trigger = %q, want %q", checkpointer.calls[4].Checkpoint, agent.CheckpointSessionEnd)
	}

	// Verify the full parent chain is intact.
	for i := 1; i < len(checkpointer.calls); i++ {
		expectedParent := fmt.Sprintf("ctx-%08d", i)
		if checkpointer.calls[i].Parent != expectedParent {
			t.Errorf("checkpoint %d parent = %q, want %q", i+1, checkpointer.calls[i].Parent, expectedParent)
		}
	}
}

// TestEventCBORRoundTrip verifies that []Event with all event types
// (including ThinkingEvent, SystemEvent with Metadata, and
// ToolCallEvent with ServerTool) survive CBOR encode/decode.
func TestEventCBORRoundTrip(t *testing.T) {
	events := []Event{
		{Timestamp: testTimestamp, Type: EventTypePrompt, Prompt: &PromptEvent{Content: "analyze this", Source: "initial"}},
		{Timestamp: testTimestamp, Type: EventTypeThinking, Thinking: &ThinkingEvent{Content: "let me think about this", Signature: "sig-abc123"}},
		{Timestamp: testTimestamp, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{ID: "tc-1", Name: "Read", Input: json.RawMessage(`{"path":"/tmp/test"}`)}},
		{Timestamp: testTimestamp, Type: EventTypeToolResult, ToolResult: &ToolResultEvent{ID: "tc-1", Output: "file contents here"}},
		{Timestamp: testTimestamp, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{ID: "st-1", Name: "WebSearch", Input: json.RawMessage(`{"query":"test"}`), ServerTool: true}},
		{Timestamp: testTimestamp, Type: EventTypeToolResult, ToolResult: &ToolResultEvent{ID: "st-1", Output: "search results"}},
		{Timestamp: testTimestamp, Type: EventTypeResponse, Response: &ResponseEvent{Content: "Here is what I found."}},
		{Timestamp: testTimestamp, Type: EventTypeSystem, System: &SystemEvent{Subtype: "compact_boundary", Metadata: json.RawMessage(`{"trigger":"auto","pre_tokens":128000}`)}},
		{Timestamp: testTimestamp, Type: EventTypeError, Error: &ErrorEvent{Message: "something went wrong"}},
		{Timestamp: testTimestamp, Type: EventTypeOutput, Output: &OutputEvent{Raw: json.RawMessage(`{"unknown":"field"}`)}},
		{Timestamp: testTimestamp, Type: EventTypeMetric, Metric: &MetricEvent{
			InputTokens:     5000,
			OutputTokens:    2000,
			CacheReadTokens: 1000,
			CostUSD:         0.025,
			DurationSeconds: 45.5,
			TurnCount:       3,
			Status:          "success",
		}},
	}

	// Encode to CBOR.
	data, err := codec.Marshal(events)
	if err != nil {
		t.Fatalf("encoding events to CBOR: %v", err)
	}

	// Decode back to typed events.
	var decoded []Event
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding CBOR to events: %v", err)
	}
	if len(decoded) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(decoded))
	}

	// Verify prompt event.
	if decoded[0].Type != EventTypePrompt {
		t.Errorf("event[0].Type = %q, want %q", decoded[0].Type, EventTypePrompt)
	}
	if decoded[0].Prompt == nil || decoded[0].Prompt.Content != "analyze this" {
		t.Errorf("event[0].Prompt mismatch: %+v", decoded[0].Prompt)
	}

	// Verify thinking event.
	if decoded[1].Type != EventTypeThinking {
		t.Errorf("event[1].Type = %q, want %q", decoded[1].Type, EventTypeThinking)
	}
	if decoded[1].Thinking == nil {
		t.Fatal("event[1].Thinking is nil")
	}
	if decoded[1].Thinking.Content != "let me think about this" {
		t.Errorf("thinking content = %q", decoded[1].Thinking.Content)
	}
	if decoded[1].Thinking.Signature != "sig-abc123" {
		t.Errorf("thinking signature = %q", decoded[1].Thinking.Signature)
	}

	// Verify tool call.
	if decoded[2].ToolCall == nil || decoded[2].ToolCall.Name != "Read" {
		t.Errorf("event[2].ToolCall mismatch: %+v", decoded[2].ToolCall)
	}
	if decoded[2].ToolCall.ServerTool {
		t.Error("event[2] should not be a server tool")
	}

	// Verify server tool call.
	if decoded[4].ToolCall == nil || decoded[4].ToolCall.Name != "WebSearch" {
		t.Errorf("event[4].ToolCall mismatch: %+v", decoded[4].ToolCall)
	}
	if !decoded[4].ToolCall.ServerTool {
		t.Error("event[4] should be a server tool")
	}

	// Verify response.
	if decoded[6].Response == nil || decoded[6].Response.Content != "Here is what I found." {
		t.Errorf("event[6].Response mismatch: %+v", decoded[6].Response)
	}

	// Verify system event with metadata.
	if decoded[7].System == nil {
		t.Fatal("event[7].System is nil")
	}
	if decoded[7].System.Subtype != "compact_boundary" {
		t.Errorf("system subtype = %q", decoded[7].System.Subtype)
	}
	if len(decoded[7].System.Metadata) == 0 {
		t.Error("system metadata is empty")
	}

	// Verify metric with status.
	if decoded[10].Metric == nil {
		t.Fatal("event[10].Metric is nil")
	}
	if decoded[10].Metric.Status != "success" {
		t.Errorf("metric status = %q, want %q", decoded[10].Metric.Status, "success")
	}
	if decoded[10].Metric.InputTokens != 5000 {
		t.Errorf("metric input tokens = %d, want 5000", decoded[10].Metric.InputTokens)
	}
	if decoded[10].Metric.CostUSD != 0.025 {
		t.Errorf("metric cost = %f, want 0.025", decoded[10].Metric.CostUSD)
	}

	// Verify round-trip through raw message intermediary (what
	// materialization would do when concatenating delta artifacts).
	var rawItems []codec.RawMessage
	if err := codec.Unmarshal(data, &rawItems); err != nil {
		t.Fatalf("decoding to []RawMessage: %v", err)
	}
	if len(rawItems) != len(events) {
		t.Fatalf("expected %d raw items, got %d", len(events), len(rawItems))
	}

	merged, err := codec.Marshal(rawItems)
	if err != nil {
		t.Fatalf("re-encoding merged raw items: %v", err)
	}

	var reDecoded []Event
	if err := codec.Unmarshal(merged, &reDecoded); err != nil {
		t.Fatalf("decoding re-encoded CBOR: %v", err)
	}
	if len(reDecoded) != len(events) {
		t.Fatalf("expected %d re-decoded events, got %d", len(events), len(reDecoded))
	}
}
