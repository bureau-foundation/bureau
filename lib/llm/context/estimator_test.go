// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"math"
	"testing"

	"github.com/bureau-foundation/bureau/lib/llm"
)

func TestCharEstimator_DefaultRatio(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()

	// 400 chars of text + 20 overhead = 420 chars.
	// At 4.0 chars/token: 420/4.0 = 105 + 1 (round-up) = 106.
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock(string(make([]byte, 400))),
		}},
	}

	tokens := estimator.EstimateTokens(messages)
	expected := int(420.0/defaultCharactersPerToken) + 1
	if tokens != expected {
		t.Errorf("EstimateTokens() = %d, want %d", tokens, expected)
	}
}

func TestCharEstimator_FirstObservationReplacesDefault(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()

	// Two messages: "hello" (5 chars + 20 overhead) + "world" (5 chars + 20 overhead) = 50 chars total.
	messages := []llm.Message{
		llm.UserMessage("hello"),
		llm.AssistantMessage("world"),
	}

	// Simulate the provider reporting 25 input tokens for these messages.
	// Observed ratio: 50 chars / 25 tokens = 2.0 chars/token.
	estimator.RecordUsage(messages, 25)

	// Now estimate again. With 50 chars at 2.0 ratio: 50/2.0 = 25 + 1 = 26.
	tokens := estimator.EstimateTokens(messages)
	if tokens != 26 {
		t.Errorf("after calibration, EstimateTokens() = %d, want 26", tokens)
	}
}

func TestCharEstimator_EMAConvergence(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()

	// Message with 100 chars + 20 overhead = 120 chars.
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock(string(make([]byte, 100))),
		}},
	}

	// Consistently report 40 tokens (true ratio: 120/40 = 3.0).
	for i := 0; i < 20; i++ {
		estimator.RecordUsage(messages, 40)
	}

	// After many observations, the ratio should converge to ~3.0.
	tokens := estimator.EstimateTokens(messages)
	// At ratio 3.0: 120/3.0 = 40 + 1 = 41.
	if tokens < 39 || tokens > 43 {
		t.Errorf("after convergence, EstimateTokens() = %d, want ~41 (ratio ~3.0)", tokens)
	}
}

func TestCharEstimator_ZeroInputTokens(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()
	messages := []llm.Message{llm.UserMessage("hello")}

	// Should not panic or change the ratio.
	estimator.RecordUsage(messages, 0)

	tokens := estimator.EstimateTokens(messages)
	// Should still use default ratio: 25 chars / 4.0 = 6.25 → 7.
	if tokens != 7 {
		t.Errorf("EstimateTokens() = %d, want 7", tokens)
	}
}

func TestCharEstimator_ZeroCharacters(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()
	// Empty message list has zero characters.
	estimator.RecordUsage(nil, 100)

	// Should not have changed the ratio (no observation recorded).
	messages := []llm.Message{llm.UserMessage("hello")}
	tokens := estimator.EstimateTokens(messages)
	// 25 chars / 4.0 = 6.25 → 7.
	if tokens != 7 {
		t.Errorf("EstimateTokens() = %d, want 7", tokens)
	}
}

func TestCharEstimator_AlwaysRoundsUp(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()
	// 21 chars (1 char text + 20 overhead) at ratio 4.0 = 5.25 → should round to 6.
	messages := []llm.Message{llm.UserMessage("a")}
	tokens := estimator.EstimateTokens(messages)

	fractional := 21.0 / defaultCharactersPerToken
	if tokens != int(fractional)+1 {
		t.Errorf("EstimateTokens() = %d, want %d (ceil of %f)", tokens, int(fractional)+1, fractional)
	}
	// Verify it's strictly greater than the fractional value.
	if float64(tokens) <= fractional {
		t.Errorf("EstimateTokens() %d should be > fractional %f", tokens, fractional)
	}
}

func TestCharEstimator_NegativeTokensIgnored(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()
	messages := []llm.Message{llm.UserMessage("hello")}

	// Negative token count should be ignored.
	estimator.RecordUsage(messages, -50)

	tokens := estimator.EstimateTokens(messages)
	// 25 chars / 4.0 = 6.25 → 7.
	if tokens != 7 {
		t.Errorf("EstimateTokens() = %d, want 7", tokens)
	}
}

func TestCharEstimator_SmoothingDoesNotOvershoot(t *testing.T) {
	t.Parallel()

	estimator := NewCharEstimator()
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock(string(make([]byte, 80))),
		}},
	}
	// 80 chars + 20 overhead = 100 chars.

	// First observation: ratio = 100/50 = 2.0.
	estimator.RecordUsage(messages, 50)
	// Second observation with wildly different value: ratio = 100/10 = 10.0.
	estimator.RecordUsage(messages, 10)

	// After EMA: 0.3 * 10.0 + 0.7 * 2.0 = 3.0 + 1.4 = 4.4.
	expectedRatio := 0.3*10.0 + 0.7*2.0
	// EstimateTokens: 100 / 4.4 = 22.7 → 23.
	expectedTokens := int(100.0/expectedRatio) + 1

	tokens := estimator.EstimateTokens(messages)
	if math.Abs(float64(tokens-expectedTokens)) > 1 {
		t.Errorf("EstimateTokens() = %d, want ~%d (ratio ~%.2f)", tokens, expectedTokens, expectedRatio)
	}
}
