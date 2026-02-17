// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/llm"
)

func TestIdentifyTurnGroups_TextOnly(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		llm.UserMessage("first"),
		llm.AssistantMessage("response 1"),
		llm.UserMessage("second"),
		llm.AssistantMessage("response 2"),
		llm.UserMessage("third"),
		llm.AssistantMessage("response 3"),
	}

	groups := identifyTurnGroups(messages)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}

	// Group 0: messages[0:2]
	if groups[0].startIndex != 0 || groups[0].endIndex != 2 {
		t.Errorf("group[0] = [%d:%d], want [0:2]", groups[0].startIndex, groups[0].endIndex)
	}
	// Group 1: messages[2:4]
	if groups[1].startIndex != 2 || groups[1].endIndex != 4 {
		t.Errorf("group[1] = [%d:%d], want [2:4]", groups[1].startIndex, groups[1].endIndex)
	}
	// Group 2: messages[4:6]
	if groups[2].startIndex != 4 || groups[2].endIndex != 6 {
		t.Errorf("group[2] = [%d:%d], want [4:6]", groups[2].startIndex, groups[2].endIndex)
	}
}

func TestIdentifyTurnGroups_WithToolCycle(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		// Group 0: user prompt → assistant tool call → tool result → assistant text
		llm.UserMessage("do something"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock("tc_01", "my_tool", json.RawMessage(`{}`)),
		}},
		llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_01", Content: "done"}),
		llm.AssistantMessage("tool returned: done"),
		// Group 1: next user message
		llm.UserMessage("next task"),
		llm.AssistantMessage("on it"),
	}

	groups := identifyTurnGroups(messages)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	// Group 0 spans the entire tool cycle: messages[0:4]
	if groups[0].startIndex != 0 || groups[0].endIndex != 4 {
		t.Errorf("group[0] = [%d:%d], want [0:4]", groups[0].startIndex, groups[0].endIndex)
	}
	// Group 1: messages[4:6]
	if groups[1].startIndex != 4 || groups[1].endIndex != 6 {
		t.Errorf("group[1] = [%d:%d], want [4:6]", groups[1].startIndex, groups[1].endIndex)
	}
}

func TestIdentifyTurnGroups_MultipleToolCycles(t *testing.T) {
	t.Parallel()

	// A single turn group with three tool call/result rounds.
	messages := []llm.Message{
		llm.UserMessage("complex task"),
		// Tool cycle 1.
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock("tc_01", "tool_a", json.RawMessage(`{}`)),
		}},
		llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_01", Content: "result_a"}),
		// Tool cycle 2.
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock("tc_02", "tool_b", json.RawMessage(`{}`)),
		}},
		llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_02", Content: "result_b"}),
		// Tool cycle 3.
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock("tc_03", "tool_c", json.RawMessage(`{}`)),
		}},
		llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_03", Content: "result_c"}),
		// Final text response.
		llm.AssistantMessage("all done"),
	}

	groups := identifyTurnGroups(messages)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1 (all tool cycles are one group)", len(groups))
	}
	if groups[0].startIndex != 0 || groups[0].endIndex != 8 {
		t.Errorf("group[0] = [%d:%d], want [0:8]", groups[0].startIndex, groups[0].endIndex)
	}
}

func TestIdentifyTurnGroups_Empty(t *testing.T) {
	t.Parallel()

	groups := identifyTurnGroups(nil)
	if groups != nil {
		t.Errorf("got %v, want nil", groups)
	}
}

func TestIdentifyTurnGroups_SingleMessage(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{llm.UserMessage("hello")}
	groups := identifyTurnGroups(messages)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].startIndex != 0 || groups[0].endIndex != 1 {
		t.Errorf("group[0] = [%d:%d], want [0:1]", groups[0].startIndex, groups[0].endIndex)
	}
}

func TestMessageHasTextContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		message  llm.Message
		expected bool
	}{
		{
			name:     "text message",
			message:  llm.UserMessage("hello"),
			expected: true,
		},
		{
			name:     "tool result only",
			message:  llm.ToolResultMessage(llm.ToolResult{ToolUseID: "tc_01", Content: "done"}),
			expected: false,
		},
		{
			name:     "assistant with text",
			message:  llm.AssistantMessage("response"),
			expected: true,
		},
		{
			name:     "assistant with tool use only",
			message:  llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock("tc_01", "my_tool", json.RawMessage(`{}`))}},
			expected: false,
		},
		{
			name:     "empty content",
			message:  llm.Message{Role: llm.RoleUser},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := messageHasTextContent(test.message)
			if result != test.expected {
				t.Errorf("messageHasTextContent() = %v, want %v", result, test.expected)
			}
		})
	}
}

func TestMessageCharCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		message  llm.Message
		expected int
	}{
		{
			name:     "text message",
			message:  llm.UserMessage("hello"),
			expected: 5 + 20, // "hello" + overhead
		},
		{
			name: "tool result",
			message: llm.ToolResultMessage(llm.ToolResult{
				ToolUseID: "tc_01",
				Content:   "output data",
			}),
			expected: len("tc_01") + len("output data") + 20,
		},
		{
			name: "tool use",
			message: llm.Message{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					llm.ToolUseBlock("tc_01", "my_tool", json.RawMessage(`{"key":"value"}`)),
				},
			},
			expected: len("my_tool") + len(`{"key":"value"}`) + 20,
		},
		{
			name:     "empty message",
			message:  llm.Message{Role: llm.RoleUser},
			expected: 20, // just the overhead
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := messageCharCount(test.message)
			if result != test.expected {
				t.Errorf("messageCharCount() = %d, want %d", result, test.expected)
			}
		})
	}
}

func TestMessagesCharCount(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		llm.UserMessage("hello"),
		llm.AssistantMessage("world"),
	}

	expected := (5 + 20) + (5 + 20) // "hello"+overhead + "world"+overhead
	result := messagesCharCount(messages)
	if result != expected {
		t.Errorf("messagesCharCount() = %d, want %d", result, expected)
	}
}
