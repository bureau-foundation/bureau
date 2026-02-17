// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package context manages conversation history for LLM agent loops.
//
// The central abstraction is [Manager], an interface that controls how
// messages are stored, windowed, and prepared for LLM requests. The
// agent loop appends every message to the manager and calls [Manager.Messages]
// before each LLM call to get a history that fits within the model's
// context window.
//
// Implementations range from trivial ([Unbounded], which returns
// everything) to budget-aware ([Truncating], which drops oldest turn
// groups to stay within a token limit). Future strategies include
// summarization (compress evicted turns into a condensed block) and
// retrieval (store old turns externally, retrieve relevant ones
// per-request).
//
// Token estimation is handled by the [TokenEstimator] interface.
// [CharEstimator] provides a calibrating heuristic that starts with a
// character-based ratio and refines it from actual provider usage data.
//
// Turn groups are the atomic unit of eviction. A turn group starts
// with a user message containing text content and includes all
// subsequent messages (assistant responses, tool results, follow-up
// responses) until the next such user message. Evicting part of a
// turn group would break message alternation invariants or orphan
// tool results, so groups are always evicted whole.
package context
