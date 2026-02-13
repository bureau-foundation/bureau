// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"fmt"
	"strings"
	"sync"
)

// RefIndex maps short artifact references (art-<12 hex chars>) to
// their full 32-byte file hashes. Because FormatRef is lossy (it
// uses only the first 6 bytes of the hash), multiple distinct hashes
// can theoretically map to the same ref. The index tracks these
// collisions and reports them as errors when a client tries to
// resolve an ambiguous ref.
//
// RefIndex is safe for concurrent reads with a single writer. The
// caller (artifact service) serializes all writes through a mutex.
type RefIndex struct {
	mu      sync.RWMutex
	entries map[string][]Hash // ref → list of full hashes
}

// NewRefIndex creates an empty ref index.
func NewRefIndex() *RefIndex {
	return &RefIndex{
		entries: make(map[string][]Hash),
	}
}

// Build populates the index from a map of refs to hash lists, as
// returned by MetadataStore.ScanRefs. This replaces any existing
// entries. Called once at service startup.
func (idx *RefIndex) Build(refMap map[string][]Hash) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries = make(map[string][]Hash, len(refMap))
	for ref, hashes := range refMap {
		// Copy the slice to avoid aliasing the caller's map.
		copied := make([]Hash, len(hashes))
		copy(copied, hashes)
		idx.entries[ref] = copied
	}
}

// Add registers a new file hash in the index. The ref is computed
// from the hash via FormatRef. If the hash is already present (same
// content stored twice), this is a no-op. Called after each
// successful store operation.
func (idx *RefIndex) Add(fileHash Hash) {
	ref := FormatRef(fileHash)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Check for duplicates (same artifact stored again).
	for _, existing := range idx.entries[ref] {
		if existing == fileHash {
			return
		}
	}

	idx.entries[ref] = append(idx.entries[ref], fileHash)
}

// Resolve looks up a short ref and returns the full file hash.
// Returns an error if the ref is not found or if it is ambiguous
// (multiple distinct hashes share the same 12-hex-char prefix). The
// error for ambiguous refs includes all colliding full hashes so the
// client can use the full hash instead.
func (idx *RefIndex) Resolve(ref string) (Hash, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	hashes, exists := idx.entries[ref]
	if !exists || len(hashes) == 0 {
		return Hash{}, fmt.Errorf("artifact %s not found", ref)
	}

	if len(hashes) > 1 {
		var hashStrings []string
		for _, hash := range hashes {
			hashStrings = append(hashStrings, FormatHash(hash))
		}
		return Hash{}, fmt.Errorf(
			"ambiguous ref %s matches %d artifacts, use full hash: %s",
			ref, len(hashes), strings.Join(hashStrings, ", "),
		)
	}

	return hashes[0], nil
}

// Len returns the total number of unique file hashes in the index.
// Multiple hashes under the same ref are counted individually.
func (idx *RefIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	total := 0
	for _, hashes := range idx.entries {
		total += len(hashes)
	}
	return total
}
