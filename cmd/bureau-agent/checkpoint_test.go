// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/llm"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// --- Mock implementations ---

type mockArtifactStore struct {
	calls    []artifactStoreCall
	response *artifactstore.StoreResponse
	err      error
}

type artifactStoreCall struct {
	Header *artifactstore.StoreHeader
}

func (mock *mockArtifactStore) Store(_ context.Context, header *artifactstore.StoreHeader, _ io.Reader) (*artifactstore.StoreResponse, error) {
	mock.calls = append(mock.calls, artifactStoreCall{Header: header})
	if mock.err != nil {
		return nil, mock.err
	}
	response := mock.response
	if response == nil {
		response = &artifactstore.StoreResponse{
			Ref:  fmt.Sprintf("blake3-%d", len(mock.calls)),
			Size: header.Size,
		}
	}
	return response, nil
}

type mockCheckpointer struct {
	calls    []agentdriver.CheckpointContextRequest
	response *agentdriver.CheckpointContextResponse
	err      error
}

func (mock *mockCheckpointer) CheckpointContext(_ context.Context, request agentdriver.CheckpointContextRequest) (*agentdriver.CheckpointContextResponse, error) {
	mock.calls = append(mock.calls, request)
	if mock.err != nil {
		return nil, mock.err
	}
	response := mock.response
	if response == nil {
		response = &agentdriver.CheckpointContextResponse{
			ID: fmt.Sprintf("ctx-%08d", len(mock.calls)),
		}
	}
	return response, nil
}

func testTracker(store artifactStorer, checkpointer contextCheckpointer) *checkpointTracker {
	return newCheckpointTracker(
		store,
		checkpointer,
		"test-session-1",
		"test-template",
		slog.Default(),
	)
}

// --- Checkpoint delta tests ---

func TestCheckpointDelta(t *testing.T) {
	store := &mockArtifactStore{}
	checkpointer := &mockCheckpointer{}
	tracker := testTracker(store, checkpointer)

	// Append a user message and assistant response.
	tracker.appendMessage(llm.UserMessage("hello"))
	tracker.appendMessage(llm.AssistantMessage("world"))

	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Verify one artifact was stored.
	if len(store.calls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(store.calls))
	}
	call := store.calls[0]
	if call.Header.ContentType != "application/cbor" {
		t.Errorf("content type = %q, want %q", call.Header.ContentType, "application/cbor")
	}
	if len(call.Header.Data) == 0 {
		t.Fatal("expected non-empty artifact data")
	}

	// Verify the CBOR data decodes to 2 messages.
	var decoded []llm.Message
	if err := codec.Unmarshal(call.Header.Data, &decoded); err != nil {
		t.Fatalf("decoding CBOR artifact: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 messages in artifact, got %d", len(decoded))
	}

	// Verify one checkpoint was created.
	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	request := checkpointer.calls[0]
	if request.CommitType != "delta" {
		t.Errorf("commit type = %q, want %q", request.CommitType, "delta")
	}
	if request.Format != "bureau-agent-v1" {
		t.Errorf("format = %q, want %q", request.Format, "bureau-agent-v1")
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

func TestCheckpointDelta_Empty(t *testing.T) {
	store := &mockArtifactStore{}
	checkpointer := &mockCheckpointer{}
	tracker := testTracker(store, checkpointer)

	// No messages appended — should be a no-op.
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(store.calls) != 0 {
		t.Errorf("expected 0 store calls, got %d", len(store.calls))
	}
	if len(checkpointer.calls) != 0 {
		t.Errorf("expected 0 checkpoint calls, got %d", len(checkpointer.calls))
	}
}

func TestCheckpointDelta_Disabled(t *testing.T) {
	// Nil clients → disabled tracker.
	tracker := newCheckpointTracker(nil, nil, "session", "template", slog.Default())

	tracker.appendMessage(llm.UserMessage("hello"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if tracker.currentContextID != "" {
		t.Errorf("expected empty currentContextID, got %q", tracker.currentContextID)
	}
}

func TestCheckpointDelta_StoreFailure(t *testing.T) {
	store := &mockArtifactStore{err: fmt.Errorf("disk full")}
	checkpointer := &mockCheckpointer{}
	tracker := testTracker(store, checkpointer)

	tracker.appendMessage(llm.UserMessage("hello"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Store was called but failed.
	if len(store.calls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(store.calls))
	}
	// Checkpoint was not called (store failed first).
	if len(checkpointer.calls) != 0 {
		t.Errorf("expected 0 checkpoint calls after store failure, got %d", len(checkpointer.calls))
	}
	// Tracker state unchanged.
	if tracker.currentContextID != "" {
		t.Errorf("expected empty currentContextID after failure, got %q", tracker.currentContextID)
	}
	if tracker.lastCheckpointIndex != 0 {
		t.Errorf("expected lastCheckpointIndex=0 after failure, got %d", tracker.lastCheckpointIndex)
	}
}

func TestCheckpointDelta_CheckpointFailure(t *testing.T) {
	store := &mockArtifactStore{}
	checkpointer := &mockCheckpointer{err: fmt.Errorf("service unavailable")}
	tracker := testTracker(store, checkpointer)

	tracker.appendMessage(llm.UserMessage("hello"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Store succeeded but checkpoint failed.
	if len(store.calls) != 1 {
		t.Fatalf("expected 1 store call, got %d", len(store.calls))
	}
	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	// Tracker state unchanged — the orphaned artifact is acceptable.
	if tracker.currentContextID != "" {
		t.Errorf("expected empty currentContextID after failure, got %q", tracker.currentContextID)
	}
	if tracker.lastCheckpointIndex != 0 {
		t.Errorf("expected lastCheckpointIndex=0 after failure, got %d", tracker.lastCheckpointIndex)
	}
}

func TestCheckpointParentChain(t *testing.T) {
	store := &mockArtifactStore{}
	checkpointer := &mockCheckpointer{}
	tracker := testTracker(store, checkpointer)

	// First checkpoint.
	tracker.appendMessage(llm.UserMessage("msg1"))
	tracker.appendMessage(llm.AssistantMessage("resp1"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(checkpointer.calls) != 1 {
		t.Fatalf("expected 1 checkpoint call, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[0].Parent != "" {
		t.Errorf("first checkpoint parent = %q, want empty", checkpointer.calls[0].Parent)
	}

	// Second checkpoint — should chain from the first.
	tracker.appendMessage(llm.UserMessage("msg2"))
	tracker.appendMessage(llm.AssistantMessage("resp2"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	if len(checkpointer.calls) != 2 {
		t.Fatalf("expected 2 checkpoint calls, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[1].Parent != "ctx-00000001" {
		t.Errorf("second checkpoint parent = %q, want %q", checkpointer.calls[1].Parent, "ctx-00000001")
	}

	// Third checkpoint.
	tracker.appendMessage(llm.UserMessage("msg3"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointSessionEnd)

	if len(checkpointer.calls) != 3 {
		t.Fatalf("expected 3 checkpoint calls, got %d", len(checkpointer.calls))
	}
	if checkpointer.calls[2].Parent != "ctx-00000002" {
		t.Errorf("third checkpoint parent = %q, want %q", checkpointer.calls[2].Parent, "ctx-00000002")
	}
}

func TestCheckpointCompaction(t *testing.T) {
	store := &mockArtifactStore{}
	checkpointer := &mockCheckpointer{}
	tracker := testTracker(store, checkpointer)

	// Simulate a conversation with an earlier checkpoint.
	tracker.appendMessage(llm.UserMessage("msg1"))
	tracker.appendMessage(llm.AssistantMessage("resp1"))
	tracker.checkpointDelta(context.Background(), agent.CheckpointTurnBoundary)

	// Append more messages.
	tracker.appendMessage(llm.UserMessage("msg2"))
	tracker.appendMessage(llm.AssistantMessage("resp2"))
	tracker.appendMessage(llm.UserMessage("msg3"))
	tracker.appendMessage(llm.AssistantMessage("resp3"))

	// Simulate truncation: only the last 2 messages survive.
	survivingMessages := []llm.Message{
		llm.UserMessage("msg3"),
		llm.AssistantMessage("resp3"),
	}
	tracker.checkpointCompaction(context.Background(), survivingMessages)

	// Should have created 3 checkpoints total:
	// 1. Initial turn boundary (from setup above)
	// 2. Pre-compaction delta (msg2, resp2, msg3, resp3)
	// 3. Compaction commit (surviving: msg3, resp3)
	if len(checkpointer.calls) != 3 {
		t.Fatalf("expected 3 checkpoint calls, got %d", len(checkpointer.calls))
	}

	// Verify pre-compaction delta.
	preCompaction := checkpointer.calls[1]
	if preCompaction.CommitType != "delta" {
		t.Errorf("pre-compaction commit type = %q, want %q", preCompaction.CommitType, "delta")
	}
	if preCompaction.Checkpoint != agent.CheckpointCompaction {
		t.Errorf("pre-compaction checkpoint = %q, want %q", preCompaction.Checkpoint, agent.CheckpointCompaction)
	}
	if preCompaction.MessageCount != 4 {
		t.Errorf("pre-compaction message count = %d, want 4", preCompaction.MessageCount)
	}
	if preCompaction.Parent != "ctx-00000001" {
		t.Errorf("pre-compaction parent = %q, want %q", preCompaction.Parent, "ctx-00000001")
	}

	// Verify compaction commit.
	compaction := checkpointer.calls[2]
	if compaction.CommitType != "compaction" {
		t.Errorf("compaction commit type = %q, want %q", compaction.CommitType, "compaction")
	}
	if compaction.Checkpoint != agent.CheckpointCompaction {
		t.Errorf("compaction checkpoint = %q, want %q", compaction.Checkpoint, agent.CheckpointCompaction)
	}
	if compaction.MessageCount != 2 {
		t.Errorf("compaction message count = %d, want 2", compaction.MessageCount)
	}
	// Compaction parent should be the pre-compaction delta.
	if compaction.Parent != "ctx-00000002" {
		t.Errorf("compaction parent = %q, want %q", compaction.Parent, "ctx-00000002")
	}
}

// --- CBOR round-trip test ---

func TestCBORRoundTrip(t *testing.T) {
	// Build a representative message slice with all content block types.
	messages := []llm.Message{
		llm.UserMessage("What files are in the directory?"),
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				llm.TextBlock("Let me check."),
				llm.ToolUseBlock("tool-1", "list_files", json.RawMessage(`{"path": "/tmp"}`)),
			},
		},
		llm.ToolResultMessage(llm.ToolResult{
			ToolUseID: "tool-1",
			Content:   "file1.txt\nfile2.txt",
		}),
		llm.AssistantMessage("The directory contains file1.txt and file2.txt."),
	}

	// Encode to CBOR (what the checkpoint tracker does).
	data, err := codec.Marshal(messages)
	if err != nil {
		t.Fatalf("encoding messages to CBOR: %v", err)
	}

	// Decode to []codec.RawMessage (what materialization does during
	// concatenation — verifies format compatibility).
	var rawItems []codec.RawMessage
	if err := codec.Unmarshal(data, &rawItems); err != nil {
		t.Fatalf("decoding to []RawMessage: %v", err)
	}
	if len(rawItems) != 4 {
		t.Fatalf("expected 4 raw items, got %d", len(rawItems))
	}

	// Re-encode the raw items (what materialization does after merge).
	merged, err := codec.Marshal(rawItems)
	if err != nil {
		t.Fatalf("re-encoding merged raw items: %v", err)
	}

	// Decode the merged result back to typed messages (what session
	// resume would do).
	var decoded []llm.Message
	if err := codec.Unmarshal(merged, &decoded); err != nil {
		t.Fatalf("decoding merged CBOR to messages: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("expected 4 decoded messages, got %d", len(decoded))
	}

	// Verify message content survived the round-trip.
	if decoded[0].Role != llm.RoleUser {
		t.Errorf("message[0].Role = %q, want %q", decoded[0].Role, llm.RoleUser)
	}
	if len(decoded[0].Content) != 1 || decoded[0].Content[0].Text != "What files are in the directory?" {
		t.Errorf("message[0] text mismatch: %+v", decoded[0].Content)
	}

	// Verify tool use survived.
	if len(decoded[1].Content) != 2 {
		t.Fatalf("message[1] expected 2 content blocks, got %d", len(decoded[1].Content))
	}
	if decoded[1].Content[1].Type != llm.ContentToolUse {
		t.Errorf("message[1].Content[1].Type = %q, want %q", decoded[1].Content[1].Type, llm.ContentToolUse)
	}
	if decoded[1].Content[1].ToolUse == nil {
		t.Fatal("message[1].Content[1].ToolUse is nil")
	}
	if decoded[1].Content[1].ToolUse.Name != "list_files" {
		t.Errorf("tool name = %q, want %q", decoded[1].Content[1].ToolUse.Name, "list_files")
	}

	// Verify tool result survived.
	if len(decoded[2].Content) != 1 || decoded[2].Content[0].Type != llm.ContentToolResult {
		t.Errorf("message[2] expected tool result, got %+v", decoded[2].Content)
	}
	if decoded[2].Content[0].ToolResult == nil {
		t.Fatal("message[2].Content[0].ToolResult is nil")
	}
	if decoded[2].Content[0].ToolResult.Content != "file1.txt\nfile2.txt" {
		t.Errorf("tool result content = %q, want %q",
			decoded[2].Content[0].ToolResult.Content, "file1.txt\nfile2.txt")
	}

	// Verify final text message.
	if decoded[3].Role != llm.RoleAssistant {
		t.Errorf("message[3].Role = %q, want %q", decoded[3].Role, llm.RoleAssistant)
	}
}
