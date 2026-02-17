// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"testing"

	"github.com/bureau-foundation/bureau/lib/llm"
)

func TestBudget_MessageTokenBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		budget   Budget
		expected int
	}{
		{
			name: "standard agent with defaults",
			budget: Budget{
				ContextWindow:   200000,
				MaxOutputTokens: 8192,
			},
			expected: 200000 - 8192 - defaultOverheadTokens,
		},
		{
			name: "explicit overhead",
			budget: Budget{
				ContextWindow:   128000,
				MaxOutputTokens: 4096,
				OverheadTokens:  10000,
			},
			expected: 128000 - 4096 - 10000,
		},
		{
			name: "tiny context window clamps to zero",
			budget: Budget{
				ContextWindow:   4096,
				MaxOutputTokens: 4096,
			},
			expected: 0,
		},
		{
			name: "output tokens exceed window clamps to zero",
			budget: Budget{
				ContextWindow:   8192,
				MaxOutputTokens: 16384,
			},
			expected: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := test.budget.MessageTokenBudget()
			if result != test.expected {
				t.Errorf("MessageTokenBudget() = %d, want %d", result, test.expected)
			}
		})
	}
}

func TestUnbounded_ReturnsAllMessages(t *testing.T) {
	t.Parallel()

	manager := &Unbounded{}
	manager.Append(llm.UserMessage("hello"))
	manager.Append(llm.AssistantMessage("hi"))
	manager.Append(llm.UserMessage("how are you"))

	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("Messages() returned %d messages, want 3", len(messages))
	}
	if messages[0].Content[0].Text != "hello" {
		t.Errorf("messages[0] text = %q, want %q", messages[0].Content[0].Text, "hello")
	}
	if messages[2].Content[0].Text != "how are you" {
		t.Errorf("messages[2] text = %q, want %q", messages[2].Content[0].Text, "how are you")
	}
}

func TestUnbounded_EmptyReturnsNil(t *testing.T) {
	t.Parallel()

	manager := &Unbounded{}
	messages, err := manager.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}
	if messages != nil {
		t.Errorf("Messages() = %v, want nil", messages)
	}
}

func TestUnbounded_RecordUsageIsNoOp(t *testing.T) {
	t.Parallel()

	manager := &Unbounded{}
	// Should not panic.
	manager.RecordUsage(llm.Usage{InputTokens: 1000})
}
