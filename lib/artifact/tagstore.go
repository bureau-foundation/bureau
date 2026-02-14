// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// tagNameDomainKey is the BLAKE3 domain key for hashing tag names to
// filesystem-safe paths. Using a dedicated domain prevents collisions
// with artifact hashes (which use chunk, container, and file domains).
var tagNameDomainKey = domainKey{
	'b', 'u', 'r', 'e', 'a', 'u', '.', 'a', 'r', 't', 'i', 'f', 'a', 'c', 't', '.',
	't', 'a', 'g', '.', 'n', 'a', 'm', 'e', 0, 0, 0, 0, 0, 0, 0, 0,
}

// MaxTagNameLength is the maximum byte length of a tag name. Tag names
// are hierarchical (e.g., "pipeline/build/2024-02-13/latest") and this
// limit is generous enough for real use while preventing abuse.
const MaxTagNameLength = 512

// TagRecord holds the on-disk and in-memory representation of a single
// tag. Each tag file on disk contains a CBOR-encoded TagRecord.
type TagRecord struct {
	Name      string    `cbor:"name"`
	Target    Hash      `cbor:"target"`
	CreatedAt time.Time `cbor:"created_at"`
	UpdatedAt time.Time `cbor:"updated_at"`
}

// TagStore manages mutable name-to-hash tag mappings with an in-memory
// index backed by per-tag CBOR files on disk. Tag names can contain
// slashes (hierarchical names like "model/production/latest"), so the
// store hashes each name to produce a filesystem-safe path.
//
// On-disk layout:
//
//	<root>/<hash[:2]>/<hash[2:4]>/<hash>.cbor
//
// where hash is the BLAKE3 keyed hash (tagNameDomainKey) of the tag
// name. Each CBOR file contains the original tag name, enabling
// reconstruction of the in-memory map from a directory scan.
//
// TagStore is safe for concurrent reads with a single writer. The
// artifact service serializes all writes through its write mutex.
type TagStore struct {
	root    string
	mu      sync.RWMutex
	entries map[string]TagRecord // tag name → record
}

// NewTagStore creates a TagStore rooted at the given directory. If the
// directory contains tag files from a previous run, they are loaded
// into the in-memory index.
func NewTagStore(root string) (*TagStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating tag directory %s: %w", root, err)
	}

	store := &TagStore{
		root:    root,
		entries: make(map[string]TagRecord),
	}

	if err := store.scanAll(); err != nil {
		return nil, fmt.Errorf("scanning existing tags: %w", err)
	}

	return store, nil
}

// Get returns the tag record for the given name, or false if no tag
// with that name exists.
func (ts *TagStore) Get(name string) (TagRecord, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	record, exists := ts.entries[name]
	return record, exists
}

// Set creates or updates a tag. When optimistic is false (the default),
// this is a compare-and-swap operation: if the tag already exists and
// expectedPrevious does not match the current target hash, the
// operation fails with an error that includes the current hash. When
// optimistic is true, the write is unconditional (last writer wins).
//
// expectedPrevious is ignored for new tags and when optimistic is true.
func (ts *TagStore) Set(name string, target Hash, expectedPrevious *Hash, optimistic bool, now time.Time) error {
	if name == "" {
		return fmt.Errorf("tag name is required")
	}
	if len(name) > MaxTagNameLength {
		return fmt.Errorf("tag name is %d bytes, maximum is %d", len(name), MaxTagNameLength)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	existing, exists := ts.entries[name]

	// Compare-and-swap check.
	if !optimistic && exists && expectedPrevious != nil {
		if existing.Target != *expectedPrevious {
			return fmt.Errorf(
				"tag conflict: %q currently points to %s, expected %s",
				name, FormatRef(existing.Target), FormatRef(*expectedPrevious),
			)
		}
	}

	record := TagRecord{
		Name:      name,
		Target:    target,
		UpdatedAt: now,
	}
	if exists {
		record.CreatedAt = existing.CreatedAt
	} else {
		record.CreatedAt = now
	}

	if err := ts.writeFile(record); err != nil {
		return err
	}

	ts.entries[name] = record
	return nil
}

// Delete removes a tag by name. Returns an error if the tag does not
// exist.
func (ts *TagStore) Delete(name string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if _, exists := ts.entries[name]; !exists {
		return fmt.Errorf("tag %q not found", name)
	}

	path := ts.tagPath(name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing tag file for %q: %w", name, err)
	}

	delete(ts.entries, name)
	return nil
}

// List returns all tags whose names start with prefix. An empty prefix
// returns all tags. Results are not sorted — the caller should sort if
// a specific order is needed.
func (ts *TagStore) List(prefix string) []TagRecord {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	var results []TagRecord
	for _, record := range ts.entries {
		if prefix == "" || strings.HasPrefix(record.Name, prefix) {
			results = append(results, record)
		}
	}
	return results
}

// Names returns all tag names. This is used by GC to determine which
// artifact hashes are protected by tags.
func (ts *TagStore) Names() map[Hash]string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make(map[Hash]string, len(ts.entries))
	for _, record := range ts.entries {
		result[record.Target] = record.Name
	}
	return result
}

// Len returns the number of tags in the store.
func (ts *TagStore) Len() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	return len(ts.entries)
}

// scanAll walks the tag directory and loads all tag files into the
// in-memory index. Called once at startup.
func (ts *TagStore) scanAll() error {
	return filepath.WalkDir(ts.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		name := entry.Name()
		if !strings.HasSuffix(name, ".cbor") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading tag file %s: %w", path, err)
		}

		var record TagRecord
		if err := codec.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decoding tag file %s: %w", path, err)
		}

		if record.Name == "" {
			// Skip corrupt or incomplete tag files.
			return nil
		}

		ts.entries[record.Name] = record
		return nil
	})
}

// writeFile atomically writes a tag record to disk.
func (ts *TagStore) writeFile(record TagRecord) error {
	data, err := codec.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding tag %q: %w", record.Name, err)
	}

	finalPath := ts.tagPath(record.Name)

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("creating tag shard directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(ts.root, "tag-*.cbor")
	if err != nil {
		return fmt.Errorf("creating temp tag file: %w", err)
	}
	tmpPath := tmpFile.Name()

	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing tag data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp tag file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("renaming tag file to %s: %w", finalPath, err)
	}

	success = true
	return nil
}

// tagPath returns the sharded filesystem path for a tag file. The tag
// name is hashed with BLAKE3 (tagNameDomainKey) to produce a
// filesystem-safe name, using the same two-level sharding as metadata
// and reconstruction records.
func (ts *TagStore) tagPath(name string) string {
	nameHash := keyedHash(tagNameDomainKey, []byte(name))
	hexString := FormatHash(nameHash)
	return filepath.Join(ts.root, hexString[:2], hexString[2:4], hexString+".cbor")
}
