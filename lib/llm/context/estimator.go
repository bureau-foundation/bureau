// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import "github.com/bureau-foundation/bureau/lib/llm"

// defaultCharactersPerToken is the initial ratio before calibration.
// 4.0 is conservative for English text with code — BPE tokenizers
// typically average 3.5-4.5 characters per token. Conservative means
// we overestimate token counts, which triggers truncation slightly
// early rather than risking context overflow from the provider.
const defaultCharactersPerToken = 4.0

// defaultSmoothingFactor controls how quickly the ratio adapts to
// new observations. 0.3 means 30% weight on the new observation,
// 70% on the running average.
const defaultSmoothingFactor = 0.3

// CharEstimator estimates token counts from character counts using an
// adaptive ratio that calibrates over time from actual provider usage.
//
// The initial ratio of 4.0 characters per token is conservative for
// English text with code. After each LLM call, [RecordUsage] adjusts
// the ratio via exponential moving average (EMA), so the estimate
// converges toward the actual tokenizer's behavior for the specific
// content this agent processes.
//
// The ratio intentionally absorbs the fixed overhead of system
// prompts and tool definitions. This makes early estimates slightly
// conservative (overestimates tokens), which is the safe direction —
// the manager truncates a bit more aggressively rather than risking
// a context overflow error from the provider. As the conversation
// grows and message content dominates the overhead, the ratio
// converges toward the true tokenizer ratio.
type CharEstimator struct {
	charactersPerToken float64
	smoothingFactor    float64
	observationCount   int
}

// NewCharEstimator creates a CharEstimator with the default initial
// ratio of 4.0 characters per token and a smoothing factor of 0.3.
func NewCharEstimator() *CharEstimator {
	return &CharEstimator{
		charactersPerToken: defaultCharactersPerToken,
		smoothingFactor:    defaultSmoothingFactor,
	}
}

// EstimateTokens returns the estimated token count for the given
// messages based on the current character-to-token ratio. Always
// rounds up — it is better to overestimate than underestimate.
func (estimator *CharEstimator) EstimateTokens(messages []llm.Message) int {
	characters := messagesCharCount(messages)
	tokens := float64(characters) / estimator.charactersPerToken
	return int(tokens) + 1
}

// RecordUsage updates the estimator's calibration using the actual
// token count from a provider response.
//
// On the first observation, the default ratio is replaced entirely
// by the observed ratio — a single real data point is far more
// informative than any default. Subsequent observations blend via
// EMA to smooth out variation between turns with different content
// profiles (text-heavy vs JSON-heavy tool outputs).
func (estimator *CharEstimator) RecordUsage(messages []llm.Message, actualInputTokens int64) {
	if actualInputTokens <= 0 {
		return
	}
	characters := messagesCharCount(messages)
	if characters == 0 {
		return
	}

	observedRatio := float64(characters) / float64(actualInputTokens)

	estimator.observationCount++
	if estimator.observationCount == 1 {
		estimator.charactersPerToken = observedRatio
		return
	}

	// EMA update: blend new observation with running average.
	estimator.charactersPerToken = estimator.smoothingFactor*observedRatio +
		(1.0-estimator.smoothingFactor)*estimator.charactersPerToken
}
