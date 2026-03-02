// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// defaultContinuationTTL is how long a continuation is retained after
// its last use. One hour is long enough for a multi-turn analysis
// session but short enough that abandoned continuations don't
// accumulate unbounded memory.
const defaultContinuationTTL = 1 * time.Hour

// continuationKey uniquely identifies a conversation across all agents.
// The agent identity (Subject from the service token) is part of the
// key so that different agents cannot read each other's continuations,
// even if they happen to use the same continuation_id string.
type continuationKey struct {
	agent          string // full Matrix user ID from token.Subject
	continuationID string // client-provided continuation identifier
}

// continuationEntry is a stored conversation. Messages contains the
// full message history: system messages, user turns, and assistant
// responses from all previous requests in this continuation.
type continuationEntry struct {
	messages []model.Message
	lastUsed time.Time
}

// continuationStore is a TTL-expiring in-memory store for conversation
// history. Each entry is a complete message sequence for a (agent,
// continuation_id) pair. The store is safe for concurrent use.
//
// The store does not perform token budgeting (truncating old messages
// when history exceeds a model's context window). That requires
// per-model context length metadata that the registry doesn't yet
// track. For now, if an agent accumulates more history than the model
// can handle, the provider returns an error and the agent can start a
// new continuation. Token budgeting is a natural extension once model
// metadata includes context window sizes.
type continuationStore struct {
	mu      sync.Mutex
	entries map[continuationKey]*continuationEntry
	ttl     time.Duration
	clock   clock.Clock
}

// newContinuationStore creates an empty store with the given TTL and
// clock. Call runExpiry to start background eviction.
func newContinuationStore(ttl time.Duration, clock clock.Clock) *continuationStore {
	return &continuationStore{
		entries: make(map[continuationKey]*continuationEntry),
		ttl:     ttl,
		clock:   clock,
	}
}

// Load returns the stored message history for the given key, or nil
// if no continuation exists. The returned slice is a copy — callers
// may append to it without affecting the store.
func (cs *continuationStore) Load(key continuationKey) []model.Message {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry, ok := cs.entries[key]
	if !ok {
		return nil
	}

	entry.lastUsed = cs.clock.Now()

	// Return a copy so callers can safely append.
	copied := make([]model.Message, len(entry.messages))
	copy(copied, entry.messages)
	return copied
}

// Store saves the full conversation for the given key, replacing any
// previous entry. The messages slice is copied — the caller retains
// ownership of the original.
func (cs *continuationStore) Store(key continuationKey, messages []model.Message) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	copied := make([]model.Message, len(messages))
	copy(copied, messages)

	cs.entries[key] = &continuationEntry{
		messages: copied,
		lastUsed: cs.clock.Now(),
	}
}

// Len returns the number of active continuations. Used by tests and
// diagnostics.
func (cs *continuationStore) Len() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.entries)
}

// evictExpired removes all entries whose lastUsed is older than the
// TTL. Called periodically by runExpiry.
func (cs *continuationStore) evictExpired() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cutoff := cs.clock.Now().Add(-cs.ttl)
	evicted := 0
	for key, entry := range cs.entries {
		if entry.lastUsed.Before(cutoff) {
			delete(cs.entries, key)
			evicted++
		}
	}
	return evicted
}

// runExpiry periodically evicts expired continuations until the
// context is cancelled. The check interval is TTL/4, so entries
// persist at most 25% longer than their nominal TTL.
func (cs *continuationStore) runExpiry(ctx context.Context) {
	interval := cs.ttl / 4
	if interval < 1*time.Second {
		interval = 1 * time.Second
	}

	ticker := cs.clock.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cs.evictExpired()
		}
	}
}
