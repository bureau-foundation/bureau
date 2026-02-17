// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/llm"
)

// Manager manages the conversation message history for an LLM agent
// loop. Implementations control how messages are stored, windowed,
// and evicted as the conversation grows.
//
// The expected call sequence per turn is:
//   - Append each new message (user prompt, assistant response, tool results)
//   - Messages before each LLM call to get the windowed view
//   - RecordUsage after the LLM response to feed back actual token counts
//
// Manager implementations are not required to be safe for concurrent
// use. The agent loop is single-threaded: one goroutine calls Append,
// Messages, and RecordUsage in sequence.
type Manager interface {
	// Append adds a message to the conversation history.
	Append(message llm.Message)

	// Messages returns the messages to send to the LLM on the next
	// call. Implementations may return a subset of the full history
	// (e.g., truncating old turn groups to fit within a token budget).
	//
	// The returned slice must maintain correct conversation structure:
	// alternating user/assistant roles, tool results paired with their
	// tool uses, and the conversation starting with a user message.
	//
	// A non-nil error indicates the conversation cannot fit within the
	// configured budget even after maximum eviction. The returned
	// messages are still a best-effort result (protected groups + most
	// recent group) — the caller should log the error and proceed.
	Messages(ctx context.Context) ([]llm.Message, error)

	// RecordUsage feeds back the actual token consumption from the
	// provider's response. Implementations use this to calibrate
	// token estimation for future calls.
	RecordUsage(usage llm.Usage)
}

// TokenEstimator estimates the token count of a message slice without
// calling a tokenizer. Implementations are calibrated over time via
// RecordUsage feedback from actual provider responses.
type TokenEstimator interface {
	// EstimateTokens returns the estimated token count for the given
	// messages. This covers only the messages themselves — not the
	// system prompt, tool definitions, or protocol framing overhead.
	EstimateTokens(messages []llm.Message) int

	// RecordUsage updates the estimator's internal calibration using
	// the actual token count from a provider response. The messages
	// parameter is the exact slice that was sent to the provider;
	// actualInputTokens is Usage.InputTokens from the response.
	//
	// Note: actualInputTokens includes system prompt, tool
	// definitions, and protocol overhead. The estimator absorbs this
	// into its ratio (see [CharEstimator] for details).
	RecordUsage(messages []llm.Message, actualInputTokens int64)
}

// Budget configures the token limits for a Manager.
type Budget struct {
	// ContextWindow is the model's total context window in tokens
	// (e.g., 200000 for Claude Sonnet, 128000 for GPT-4o).
	ContextWindow int

	// MaxOutputTokens is the maximum output tokens reserved for each
	// LLM response. Subtracted from the context window to determine
	// the input token budget.
	MaxOutputTokens int

	// OverheadTokens is an estimate of the fixed per-request token
	// overhead: system prompt, tool definitions, and protocol framing.
	// Subtracted from the context window alongside MaxOutputTokens.
	// If zero, a default of 4096 is used (conservative for agents
	// with ~20 tools and a medium system prompt).
	OverheadTokens int
}

// defaultOverheadTokens is used when Budget.OverheadTokens is zero.
const defaultOverheadTokens = 4096

// MessageTokenBudget returns the number of tokens available for
// conversation messages after subtracting output reservation and
// overhead.
func (budget Budget) MessageTokenBudget() int {
	overhead := budget.OverheadTokens
	if overhead == 0 {
		overhead = defaultOverheadTokens
	}
	available := budget.ContextWindow - budget.MaxOutputTokens - overhead
	if available < 0 {
		return 0
	}
	return available
}

// Unbounded implements Manager with no truncation. All appended
// messages are always returned. This is the equivalent of the
// pre-Manager behavior where messages grew unboundedly. Useful for
// testing and for operators who manage context externally.
type Unbounded struct {
	messages []llm.Message
}

// Append adds a message to the history.
func (manager *Unbounded) Append(message llm.Message) {
	manager.messages = append(manager.messages, message)
}

// Messages returns the full, untruncated history.
func (manager *Unbounded) Messages(_ context.Context) ([]llm.Message, error) {
	return manager.messages, nil
}

// RecordUsage is a no-op for the unbounded manager.
func (manager *Unbounded) RecordUsage(_ llm.Usage) {}
