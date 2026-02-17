// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/llm"
)

// Truncating implements [Manager] by dropping the oldest turn groups
// when the conversation exceeds the token budget. It always preserves
// the first turn group (the initial task prompt) and the most recent
// turn group (the current exchange).
//
// This is the right strategy for Bureau's coordination-heavy agents
// (PMs, triagers, TPMs) where recent context matters most and the
// initial task prompt provides essential grounding. Agents with 200k
// context windows will rarely need truncation; agents on smaller
// models (8k-32k) will truncate frequently.
type Truncating struct {
	messages  []llm.Message
	estimator TokenEstimator
	budget    int

	// protectedGroupCount is the number of turn groups at the start
	// of the conversation that are never evicted. Default is 1
	// (protecting the initial task prompt and its first response).
	protectedGroupCount int

	// lastReturnedMessages is the message slice most recently
	// returned by Messages. Used by RecordUsage to correlate
	// character counts with actual token counts from the provider.
	lastReturnedMessages []llm.Message

	// lastEvictedCount tracks how many turn groups were evicted on
	// the most recent Messages call, for diagnostics.
	lastEvictedCount int
}

// NewTruncating creates a Truncating manager with the given token
// budget and estimator. The first turn group (initial task prompt)
// is always protected from eviction.
func NewTruncating(tokenBudget int, estimator TokenEstimator) *Truncating {
	return &Truncating{
		estimator:           estimator,
		budget:              tokenBudget,
		protectedGroupCount: 1,
	}
}

// Append adds a message to the conversation history.
func (manager *Truncating) Append(message llm.Message) {
	manager.messages = append(manager.messages, message)
}

// Messages returns the conversation history windowed to fit within
// the token budget. If all messages fit, the full history is returned.
// Otherwise, the oldest non-protected turn groups are evicted until
// the estimated token count is within budget.
//
// The first turn group (task prompt) and the last turn group (current
// exchange) are always preserved. Eviction proceeds from oldest to
// newest in the middle of the conversation.
//
// Returns a non-nil error if the conversation cannot fit even after
// maximum eviction. The returned messages are still a best-effort
// result — the caller should log the error and proceed.
func (manager *Truncating) Messages(_ context.Context) ([]llm.Message, error) {
	manager.lastEvictedCount = 0

	if len(manager.messages) == 0 {
		manager.lastReturnedMessages = nil
		return nil, nil
	}

	// Fast path: everything fits.
	estimatedTokens := manager.estimator.EstimateTokens(manager.messages)
	if estimatedTokens <= manager.budget {
		manager.lastReturnedMessages = manager.messages
		return manager.messages, nil
	}

	// Identify turn groups.
	groups := identifyTurnGroups(manager.messages)
	if len(groups) == 0 {
		// No identifiable groups (shouldn't happen with valid conversation).
		manager.lastReturnedMessages = manager.messages
		return manager.messages, nil
	}

	// Compute per-group token estimates.
	groupTokens := make([]int, len(groups))
	for i, group := range groups {
		groupTokens[i] = manager.estimator.EstimateTokens(
			manager.messages[group.startIndex:group.endIndex],
		)
	}

	// Determine the evictable range: everything between the protected
	// groups and the most recent group.
	//   protected: groups[0:protectedGroupCount]
	//   evictable: groups[protectedGroupCount:len(groups)-1]
	//   current:   groups[len(groups)-1]
	evictableStart := manager.protectedGroupCount
	if evictableStart >= len(groups) {
		evictableStart = len(groups)
	}
	evictableEnd := len(groups) - 1 // exclusive; last group is always kept

	if evictableStart >= evictableEnd {
		// Nothing evictable: only protected groups + current group.
		manager.lastReturnedMessages = manager.messages
		return manager.messages, fmt.Errorf(
			"context budget exceeded: estimated %d tokens, budget %d tokens, "+
				"but only %d turn groups remain (all protected or current)",
			estimatedTokens, manager.budget, len(groups))
	}

	// Evict oldest groups first until under budget.
	tokensToFree := estimatedTokens - manager.budget
	freedTokens := 0
	evictCount := 0
	for i := evictableStart; i < evictableEnd && freedTokens < tokensToFree; i++ {
		freedTokens += groupTokens[i]
		evictCount++
	}

	manager.lastEvictedCount = evictCount

	// Rebuild the message slice: protected + surviving + current.
	var result []llm.Message

	// Protected groups.
	for i := 0; i < evictableStart && i < len(groups); i++ {
		result = append(result, manager.messages[groups[i].startIndex:groups[i].endIndex]...)
	}

	// Surviving evictable groups (those not evicted).
	for i := evictableStart + evictCount; i < evictableEnd; i++ {
		result = append(result, manager.messages[groups[i].startIndex:groups[i].endIndex]...)
	}

	// Current group (always kept).
	lastGroup := groups[len(groups)-1]
	result = append(result, manager.messages[lastGroup.startIndex:lastGroup.endIndex]...)

	manager.lastReturnedMessages = result

	// Check if still over budget after maximum eviction.
	remainingEstimate := estimatedTokens - freedTokens
	if remainingEstimate > manager.budget {
		return result, fmt.Errorf(
			"context budget exceeded after evicting %d turn groups (freed ~%d tokens): "+
				"estimated %d tokens remaining, budget %d tokens",
			evictCount, freedTokens, remainingEstimate, manager.budget)
	}

	return result, nil
}

// RecordUsage feeds back actual token counts from the provider to
// calibrate the token estimator. Uses the messages last returned by
// Messages (which is what was actually sent to the provider).
func (manager *Truncating) RecordUsage(usage llm.Usage) {
	if manager.lastReturnedMessages != nil {
		manager.estimator.RecordUsage(manager.lastReturnedMessages, usage.InputTokens)
	}
}

// EvictedTurnGroups returns the number of turn groups evicted on the
// most recent Messages call. Zero means no truncation occurred.
func (manager *Truncating) EvictedTurnGroups() int {
	return manager.lastEvictedCount
}
