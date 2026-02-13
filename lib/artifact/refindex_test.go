// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"strings"
	"sync"
	"testing"
)

func TestRefIndexAddResolve(t *testing.T) {
	idx := NewRefIndex()

	hash := HashFile(HashChunk([]byte("test content")))
	idx.Add(hash)

	ref := FormatRef(hash)
	resolved, err := idx.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != hash {
		t.Errorf("resolved = %s, want %s", FormatHash(resolved), FormatHash(hash))
	}
}

func TestRefIndexNotFound(t *testing.T) {
	idx := NewRefIndex()

	_, err := idx.Resolve("art-000000000000")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want contains 'not found'", err.Error())
	}
}

func TestRefIndexAmbiguous(t *testing.T) {
	idx := NewRefIndex()

	// Create two hashes that have the same first 6 bytes (same ref).
	// We construct these by copying the same hash and changing a byte
	// beyond position 5.
	baseHash := HashFile(HashChunk([]byte("base")))

	hash1 := baseHash
	hash2 := baseHash
	hash2[6] = hash2[6] ^ 0xFF // Flip byte 6 — different full hash, same ref.

	if FormatRef(hash1) != FormatRef(hash2) {
		t.Fatal("test setup: hashes should produce the same ref")
	}

	idx.Add(hash1)
	idx.Add(hash2)

	ref := FormatRef(hash1)
	_, err := idx.Resolve(ref)
	if err == nil {
		t.Fatal("expected error for ambiguous ref")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %q, want contains 'ambiguous'", err.Error())
	}
	// The error should include both full hashes.
	if !strings.Contains(err.Error(), FormatHash(hash1)) {
		t.Errorf("error should include hash1 %s", FormatHash(hash1))
	}
	if !strings.Contains(err.Error(), FormatHash(hash2)) {
		t.Errorf("error should include hash2 %s", FormatHash(hash2))
	}
}

func TestRefIndexDuplicateAdd(t *testing.T) {
	idx := NewRefIndex()

	hash := HashFile(HashChunk([]byte("same content")))
	idx.Add(hash)
	idx.Add(hash) // Duplicate — should be a no-op.

	ref := FormatRef(hash)
	resolved, err := idx.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != hash {
		t.Error("resolved hash mismatch after duplicate add")
	}

	// Len should be 1, not 2.
	if idx.Len() != 1 {
		t.Errorf("Len() = %d, want 1", idx.Len())
	}
}

func TestRefIndexBuild(t *testing.T) {
	idx := NewRefIndex()

	// Create a refMap like MetadataStore.ScanRefs would return.
	hash1 := HashFile(HashChunk([]byte("artifact 1")))
	hash2 := HashFile(HashChunk([]byte("artifact 2")))
	hash3 := HashFile(HashChunk([]byte("artifact 3")))

	refMap := map[string][]Hash{
		FormatRef(hash1): {hash1},
		FormatRef(hash2): {hash2},
		FormatRef(hash3): {hash3},
	}

	idx.Build(refMap)

	// All should resolve.
	for _, hash := range []Hash{hash1, hash2, hash3} {
		ref := FormatRef(hash)
		resolved, err := idx.Resolve(ref)
		if err != nil {
			t.Errorf("resolve %s: %v", ref, err)
			continue
		}
		if resolved != hash {
			t.Errorf("resolved %s: got %s, want %s", ref, FormatHash(resolved), FormatHash(hash))
		}
	}

	if idx.Len() != 3 {
		t.Errorf("Len() = %d, want 3", idx.Len())
	}
}

func TestRefIndexBuildReplaces(t *testing.T) {
	idx := NewRefIndex()

	// Add a hash, then Build with a different set. The old hash
	// should no longer be resolvable.
	oldHash := HashFile(HashChunk([]byte("old")))
	idx.Add(oldHash)

	newHash := HashFile(HashChunk([]byte("new")))
	idx.Build(map[string][]Hash{
		FormatRef(newHash): {newHash},
	})

	_, err := idx.Resolve(FormatRef(oldHash))
	if err == nil {
		t.Error("expected error: old hash should not be resolvable after Build")
	}

	resolved, err := idx.Resolve(FormatRef(newHash))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != newHash {
		t.Error("resolved hash mismatch for new entry")
	}
}

func TestRefIndexLen(t *testing.T) {
	idx := NewRefIndex()
	if idx.Len() != 0 {
		t.Errorf("empty index Len() = %d, want 0", idx.Len())
	}

	hash1 := HashFile(HashChunk([]byte("a")))
	hash2 := HashFile(HashChunk([]byte("b")))
	idx.Add(hash1)
	idx.Add(hash2)

	if idx.Len() != 2 {
		t.Errorf("Len() = %d, want 2", idx.Len())
	}
}

func TestRefIndexConcurrentReadWrite(t *testing.T) {
	idx := NewRefIndex()

	// Pre-populate some entries.
	for i := 0; i < 100; i++ {
		content := []byte{byte(i), byte(i >> 8)}
		hash := HashFile(HashChunk(content))
		idx.Add(hash)
	}

	// Concurrent reads and writes.
	var waitGroup sync.WaitGroup
	waitGroup.Add(3)

	// Writer: add more entries.
	go func() {
		defer waitGroup.Done()
		for i := 100; i < 200; i++ {
			content := []byte{byte(i), byte(i >> 8), 0xFF}
			hash := HashFile(HashChunk(content))
			idx.Add(hash)
		}
	}()

	// Reader 1: resolve existing refs.
	go func() {
		defer waitGroup.Done()
		for i := 0; i < 100; i++ {
			content := []byte{byte(i), byte(i >> 8)}
			hash := HashFile(HashChunk(content))
			ref := FormatRef(hash)
			// Resolve may succeed or fail due to ambiguity, but
			// should never panic.
			idx.Resolve(ref)
		}
	}()

	// Reader 2: check Len.
	go func() {
		defer waitGroup.Done()
		for i := 0; i < 100; i++ {
			length := idx.Len()
			if length < 100 {
				t.Errorf("Len() = %d during concurrent access, expected >= 100", length)
				return
			}
		}
	}()

	waitGroup.Wait()
}
