// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/llm"
)

// mockTokenEstimator returns a deterministic token count based on
// message count, making truncation tests predictable.
type mockTokenEstimator struct {
	tokensPerMessage int
	recordedCalls    int
}

func (estimator *mockTokenEstimator) EstimateTokens(messages []llm.Message) int {
	return len(messages) * estimator.tokensPerMessage
}

func (estimator *mockTokenEstimator) RecordUsage(_ []llm.Message, _ int64) {
	estimator.recordedCalls++
}

func TestTruncating_NoTruncation(t *testing.T) {
	t.Parallel()

	// 100 tokens per message, budget of 1000. 4 messages = 400 tokens.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(1000, estimator)

	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))
	manager.Append(llm.UserMessage("second"))
	manager.Append(llm.AssistantMessage("response 2"))

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
	if manager.EvictedTurnGroups() != 0 {
		t.Errorf("EvictedTurnGroups() = %d, want 0", manager.EvictedTurnGroups())
	}
}

func TestTruncating_EvictsOldestGroup(t *testing.T) {
	t.Parallel()

	// 100 tokens per message, budget of 500.
	// 3 groups × 2 messages = 6 messages = 600 tokens (over budget).
	// Evict group 1 (middle) → 4 messages = 400 tokens (under budget).
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(500, estimator)

	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))
	manager.Append(llm.UserMessage("second"))
	manager.Append(llm.AssistantMessage("response 2"))
	manager.Append(llm.UserMessage("third"))
	manager.Append(llm.AssistantMessage("response 3"))

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
	if manager.EvictedTurnGroups() != 1 {
		t.Errorf("EvictedTurnGroups() = %d, want 1", manager.EvictedTurnGroups())
	}

	// First group preserved (task prompt).
	if messages[0].Content[0].Text != "first" {
		t.Errorf("messages[0] = %q, want %q", messages[0].Content[0].Text, "first")
	}
	// Last group preserved (current exchange).
	if messages[2].Content[0].Text != "third" {
		t.Errorf("messages[2] = %q, want %q", messages[2].Content[0].Text, "third")
	}
}

func TestTruncating_ProtectsFirstGroup(t *testing.T) {
	t.Parallel()

	// Budget fits only 2 messages (200 tokens). 3 groups × 2 = 6 messages.
	// After evicting groups 1 and 2 (middle), we keep group 0 + group 2
	// (4 messages = 400 tokens, still over budget). But group 0 is
	// protected and group 2 is current, so we return them with an error.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(200, estimator)

	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))
	manager.Append(llm.UserMessage("second"))
	manager.Append(llm.AssistantMessage("response 2"))
	manager.Append(llm.UserMessage("third"))
	manager.Append(llm.AssistantMessage("response 3"))

	messages, err := manager.Messages(context.Background())
	// Should return an error since we're still over budget.
	if err == nil {
		t.Fatal("Messages() should return error when over budget after max eviction")
	}
	// But still returns best-effort: first group + last group.
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
	// Verify first group is preserved.
	if messages[0].Content[0].Text != "first" {
		t.Errorf("messages[0] = %q, want %q", messages[0].Content[0].Text, "first")
	}
}

func TestTruncating_MultiGroupEviction(t *testing.T) {
	t.Parallel()

	// 100 tokens per message, budget of 500.
	// 5 groups × 2 messages = 10 messages = 1000 tokens.
	// Need to free 500 tokens = evict 2-3 groups.
	// Evict groups 1,2 (4 messages = 400 tokens). Remaining: 600, still over.
	// Evict group 3 too (2 more = 200 tokens). Freed = 600. Remaining: 400.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(500, estimator)

	for i := 0; i < 5; i++ {
		manager.Append(llm.UserMessage("user " + string(rune('A'+i))))
		manager.Append(llm.AssistantMessage("response " + string(rune('A'+i))))
	}

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	// Should keep group 0 (protected) + group 4 (current) = 4 messages.
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
	if manager.EvictedTurnGroups() != 3 {
		t.Errorf("EvictedTurnGroups() = %d, want 3", manager.EvictedTurnGroups())
	}
}

func TestTruncating_ToolCyclePreservedAsUnit(t *testing.T) {
	t.Parallel()

	// Group 0: user → assistant(tool) → user(tool_result) → assistant(text) = 4 messages.
	// Group 1: user → assistant(text) = 2 messages.
	// Group 2: user → assistant(text) = 2 messages.
	// Total: 8 messages = 800 tokens. Budget: 700.
	// Evict group 1 (2 messages, 200 tokens). Remaining: 600.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(700, estimator)

	// Group 0: tool cycle.
	manager.Append(llm.UserMessage("task with tools"))
	manager.Append(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolUseBlock("tc_01", "my_tool", json.RawMessage(`{}`)),
	}})
	manager.Append(llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_01", Content: "result"}))
	manager.Append(llm.AssistantMessage("tool returned: result"))

	// Group 1.
	manager.Append(llm.UserMessage("simple question"))
	manager.Append(llm.AssistantMessage("simple answer"))

	// Group 2.
	manager.Append(llm.UserMessage("final question"))
	manager.Append(llm.AssistantMessage("final answer"))

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	// Group 0 (4 msgs) + Group 2 (2 msgs) = 6 messages.
	if len(messages) != 6 {
		t.Fatalf("Messages() returned %d messages, want 6", len(messages))
	}
	if manager.EvictedTurnGroups() != 1 {
		t.Errorf("EvictedTurnGroups() = %d, want 1", manager.EvictedTurnGroups())
	}

	// Verify the tool cycle is intact: group 0's 4 messages are first.
	if messages[0].Content[0].Text != "task with tools" {
		t.Errorf("messages[0] text = %q, want %q", messages[0].Content[0].Text, "task with tools")
	}
	if messages[1].Content[0].Type != llm.ContentToolUse {
		t.Errorf("messages[1] type = %v, want tool_use", messages[1].Content[0].Type)
	}
	if messages[2].Content[0].Type != llm.ContentToolResult {
		t.Errorf("messages[2] type = %v, want tool_result", messages[2].Content[0].Type)
	}
	if messages[3].Content[0].Text != "tool returned: result" {
		t.Errorf("messages[3] text = %q, want %q", messages[3].Content[0].Text, "tool returned: result")
	}
}

func TestTruncating_SingleGroupOverBudget(t *testing.T) {
	t.Parallel()

	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(50, estimator)

	manager.Append(llm.UserMessage("hello"))
	manager.Append(llm.AssistantMessage("world"))

	messages, err := manager.Messages(context.Background())
	// Single group: nothing to evict, should return error.
	if err == nil {
		t.Fatal("Messages() should return error for single group over budget")
	}
	// But still returns the messages (best-effort).
	if len(messages) != 2 {
		t.Fatalf("Messages() returned %d messages, want 2", len(messages))
	}
}

func TestTruncating_TwoGroupsNothingEvictable(t *testing.T) {
	t.Parallel()

	// Two groups: group 0 is protected, group 1 is current.
	// Nothing in between to evict.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(200, estimator)

	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))
	manager.Append(llm.UserMessage("second"))
	manager.Append(llm.AssistantMessage("response 2"))

	messages, err := manager.Messages(context.Background())
	// 400 tokens, budget 200: over budget but nothing evictable.
	if err == nil {
		t.Fatal("Messages() should return error when nothing evictable")
	}
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
}

func TestTruncating_ExactBudget(t *testing.T) {
	t.Parallel()

	// 4 messages × 100 tokens = 400. Budget = 400. Exact fit.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(400, estimator)

	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))
	manager.Append(llm.UserMessage("second"))
	manager.Append(llm.AssistantMessage("response 2"))

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("Messages() returned %d messages, want 4", len(messages))
	}
	if manager.EvictedTurnGroups() != 0 {
		t.Errorf("EvictedTurnGroups() = %d, want 0", manager.EvictedTurnGroups())
	}
}

func TestTruncating_EmptyMessages(t *testing.T) {
	t.Parallel()

	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(1000, estimator)

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if messages != nil {
		t.Errorf("Messages() = %v, want nil", messages)
	}
}

func TestTruncating_RecordUsageCalibratesEstimator(t *testing.T) {
	t.Parallel()

	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(1000, estimator)

	manager.Append(llm.UserMessage("hello"))
	manager.Append(llm.AssistantMessage("world"))

	// Call Messages to populate lastReturnedMessages.
	_, _ = manager.Messages(context.Background())

	manager.RecordUsage(llm.Usage{InputTokens: 42})

	if estimator.recordedCalls != 1 {
		t.Errorf("estimator.recordedCalls = %d, want 1", estimator.recordedCalls)
	}
}

func TestTruncating_RecordUsageBeforeMessagesIsNoOp(t *testing.T) {
	t.Parallel()

	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(1000, estimator)

	// RecordUsage before any Messages call should not panic.
	manager.RecordUsage(llm.Usage{InputTokens: 42})

	if estimator.recordedCalls != 0 {
		t.Errorf("estimator.recordedCalls = %d, want 0", estimator.recordedCalls)
	}
}

func TestTruncating_ConversationStructureInvariant(t *testing.T) {
	t.Parallel()

	// Build a conversation with 5 groups, some with tool cycles.
	estimator := &mockTokenEstimator{tokensPerMessage: 100}
	manager := NewTruncating(800, estimator)

	// Group 0: simple.
	manager.Append(llm.UserMessage("first"))
	manager.Append(llm.AssistantMessage("response 1"))

	// Group 1: with tool cycle.
	manager.Append(llm.UserMessage("use tools"))
	manager.Append(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolUseBlock("tc_01", "tool", json.RawMessage(`{}`)),
	}})
	manager.Append(llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_01", Content: "ok"}))
	manager.Append(llm.AssistantMessage("tool done"))

	// Group 2: simple.
	manager.Append(llm.UserMessage("third"))
	manager.Append(llm.AssistantMessage("response 3"))

	// Group 3: simple.
	manager.Append(llm.UserMessage("fourth"))
	manager.Append(llm.AssistantMessage("response 4"))

	// Group 4: simple.
	manager.Append(llm.UserMessage("fifth"))
	manager.Append(llm.AssistantMessage("response 5"))

	// 12 messages = 1200 tokens, budget 800. Need to evict 400 tokens.
	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}

	// Verify structural invariants.

	// Must start with a user message.
	if len(messages) == 0 {
		t.Fatal("Messages() returned empty slice")
	}
	if messages[0].Role != llm.RoleUser {
		t.Errorf("first message role = %q, want %q", messages[0].Role, llm.RoleUser)
	}

	// Roles must follow valid patterns: user followed by assistant,
	// tool results (user role) followed by assistant.
	for i := 1; i < len(messages); i++ {
		previous := messages[i-1]
		current := messages[i]

		if previous.Role == llm.RoleUser && current.Role == llm.RoleUser {
			t.Errorf("messages[%d] and [%d] are both user role (invalid)", i-1, i)
		}
		if previous.Role == llm.RoleAssistant && current.Role == llm.RoleAssistant {
			t.Errorf("messages[%d] and [%d] are both assistant role (invalid)", i-1, i)
		}
	}

	// Every tool_use must have a matching tool_result.
	toolUseIDs := map[string]bool{}
	toolResultIDs := map[string]bool{}
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == llm.ContentToolUse && block.ToolUse != nil {
				toolUseIDs[block.ToolUse.ID] = true
			}
			if block.Type == llm.ContentToolResult && block.ToolResult != nil {
				toolResultIDs[block.ToolResult.ToolUseID] = true
			}
		}
	}
	for id := range toolUseIDs {
		if !toolResultIDs[id] {
			t.Errorf("tool_use %q has no matching tool_result", id)
		}
	}
	for id := range toolResultIDs {
		if !toolUseIDs[id] {
			t.Errorf("tool_result %q has no matching tool_use", id)
		}
	}
}
