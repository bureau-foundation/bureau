// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func TestContinuationStore_LoadMissing(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	key := continuationKey{agent: "@agent:server", continuationID: "missing"}
	messages := store.Load(key)
	if messages != nil {
		t.Fatalf("expected nil for missing key, got %v", messages)
	}
}

func TestContinuationStore_StoreAndLoad(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	key := continuationKey{agent: "@agent:server", continuationID: "conv-1"}
	original := []model.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	}

	store.Store(key, original)

	loaded := store.Load(key)
	if len(loaded) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(loaded))
	}
	if loaded[0].Role != "system" || loaded[0].Content != "You are helpful." {
		t.Errorf("message 0: got %q %q", loaded[0].Role, loaded[0].Content)
	}
	if loaded[1].Role != "user" || loaded[1].Content != "Hello" {
		t.Errorf("message 1: got %q %q", loaded[1].Role, loaded[1].Content)
	}
	if loaded[2].Role != "assistant" || loaded[2].Content != "Hi there!" {
		t.Errorf("message 2: got %q %q", loaded[2].Role, loaded[2].Content)
	}
}

func TestContinuationStore_LoadReturnsCopy(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	key := continuationKey{agent: "@agent:server", continuationID: "conv-1"}
	store.Store(key, []model.Message{
		{Role: "user", Content: "Hello"},
	})

	// Load and append to the returned slice — this should not
	// affect the stored data.
	loaded := store.Load(key)
	loaded = append(loaded, model.Message{Role: "assistant", Content: "injected"})

	reloaded := store.Load(key)
	if len(reloaded) != 1 {
		t.Fatalf("store was mutated by append to loaded slice: got %d messages, want 1", len(reloaded))
	}
}

func TestContinuationStore_StoreReplacesPrevious(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	key := continuationKey{agent: "@agent:server", continuationID: "conv-1"}

	store.Store(key, []model.Message{
		{Role: "user", Content: "First turn"},
	})
	store.Store(key, []model.Message{
		{Role: "user", Content: "First turn"},
		{Role: "assistant", Content: "Response"},
		{Role: "user", Content: "Second turn"},
	})

	loaded := store.Load(key)
	if len(loaded) != 3 {
		t.Fatalf("expected 3 messages after replace, got %d", len(loaded))
	}
	if loaded[2].Content != "Second turn" {
		t.Errorf("expected last message to be 'Second turn', got %q", loaded[2].Content)
	}
}

func TestContinuationStore_AgentIsolation(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	// Two agents using the same continuation_id should not see each
	// other's conversations.
	keyA := continuationKey{agent: "@alice:server", continuationID: "shared-id"}
	keyB := continuationKey{agent: "@bob:server", continuationID: "shared-id"}

	store.Store(keyA, []model.Message{{Role: "user", Content: "Alice's message"}})
	store.Store(keyB, []model.Message{{Role: "user", Content: "Bob's message"}})

	loadedA := store.Load(keyA)
	loadedB := store.Load(keyB)

	if len(loadedA) != 1 || loadedA[0].Content != "Alice's message" {
		t.Errorf("Alice's data corrupted: %v", loadedA)
	}
	if len(loadedB) != 1 || loadedB[0].Content != "Bob's message" {
		t.Errorf("Bob's data corrupted: %v", loadedB)
	}
}

func TestContinuationStore_EvictExpired(t *testing.T) {
	fake := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ttl := 1 * time.Hour
	store := newContinuationStore(ttl, fake)

	key1 := continuationKey{agent: "@agent:server", continuationID: "old"}
	key2 := continuationKey{agent: "@agent:server", continuationID: "fresh"}

	// Store both at t=0.
	store.Store(key1, []model.Message{{Role: "user", Content: "old"}})
	store.Store(key2, []model.Message{{Role: "user", Content: "fresh"}})

	// Advance past TTL.
	fake.Advance(ttl + 1*time.Minute)

	// Touch key2 (Load updates lastUsed).
	store.Load(key2)

	// Evict. key1 should be removed; key2 should survive.
	evicted := store.evictExpired()
	if evicted != 1 {
		t.Fatalf("expected 1 eviction, got %d", evicted)
	}
	if store.Load(key1) != nil {
		t.Error("key1 should have been evicted")
	}
	if store.Load(key2) == nil {
		t.Error("key2 should have survived (recently accessed)")
	}
}

func TestContinuationStore_Len(t *testing.T) {
	fake := clock.Fake(time.Unix(1735689600, 0))
	store := newContinuationStore(1*time.Hour, fake)

	if store.Len() != 0 {
		t.Fatalf("expected 0, got %d", store.Len())
	}

	store.Store(
		continuationKey{agent: "@a:s", continuationID: "1"},
		[]model.Message{{Role: "user", Content: "msg"}},
	)
	store.Store(
		continuationKey{agent: "@a:s", continuationID: "2"},
		[]model.Message{{Role: "user", Content: "msg"}},
	)

	if store.Len() != 2 {
		t.Fatalf("expected 2, got %d", store.Len())
	}
}
