// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// metadataDir is the directory name within the store root for
// per-artifact metadata files.
const metadataDir = "metadata"

// Visibility levels for artifacts. Visibility is immutable — set on
// store and cannot be changed afterward. See artifacts.md, "Encryption
// and Visibility" section.
const (
	// VisibilityPrivate means the artifact is encrypted before any
	// transfer outside the Bureau network. Filename, content type,
	// and content are all encrypted. The artifact reference is also
	// obscured in external storage.
	VisibilityPrivate = "private"

	// VisibilityPublic means the artifact is not encrypted for
	// external storage. Used for content from public sources where
	// encryption adds cost without security benefit. Still
	// integrity-verified via hash.
	VisibilityPublic = "public"
)

// ValidateVisibility checks that a visibility value is one of the
// allowed levels. Returns an error for invalid values. Empty string
// is invalid — callers should normalize to VisibilityPrivate before
// validating (see NormalizeVisibility).
func ValidateVisibility(visibility string) error {
	switch visibility {
	case VisibilityPrivate, VisibilityPublic:
		return nil
	default:
		return fmt.Errorf("invalid visibility %q: must be %q or %q", visibility, VisibilityPrivate, VisibilityPublic)
	}
}

// NormalizeVisibility returns the canonical visibility value. Empty
// string defaults to VisibilityPrivate per the security-by-default
// design principle.
func NormalizeVisibility(visibility string) string {
	if visibility == "" {
		return VisibilityPrivate
	}
	return visibility
}

// ArtifactMetadata holds per-artifact metadata that supplements the
// reconstruction record. Written when an artifact is stored, read on
// fetch (to populate FetchResponse.ContentType and Filename) and on
// show (to return the full metadata to the client).
//
// Fields mirror the StoreHeader request fields plus computed fields
// from the StoreResult. The reconstruction record holds the physical
// layout (segments, container references); this struct holds the
// application-level metadata.
type ArtifactMetadata struct {
	FileHash       Hash      `json:"file_hash"`
	Ref            string    `json:"ref"`
	ContentType    string    `json:"content_type"`
	Filename       string    `json:"filename,omitempty"`
	Description    string    `json:"description,omitempty"`
	Labels         []string  `json:"labels,omitempty"`
	CachePolicy    string    `json:"cache_policy,omitempty"`
	Visibility     string    `json:"visibility,omitempty"`
	TTL            string    `json:"ttl,omitempty"`
	Size           int64     `json:"size"`
	ChunkCount     int       `json:"chunk_count"`
	ContainerCount int       `json:"container_count"`
	Compression    string    `json:"compression"`
	StoredAt       time.Time `json:"stored_at"`
}

// MetadataStore persists per-artifact metadata as sharded CBOR files
// on disk. Each metadata file is keyed by the artifact's file hash,
// using the same two-level sharding as reconstruction records:
//
//	<root>/<hex[:2]>/<hex[2:4]>/<hash>.cbor
//
// MetadataStore is safe for concurrent reads. Writes must be
// serialized by the caller (the artifact service holds a write mutex).
type MetadataStore struct {
	root string
}

// NewMetadataStore creates a MetadataStore rooted at the given
// directory. Creates the directory if it does not exist.
func NewMetadataStore(root string) (*MetadataStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating metadata directory %s: %w", root, err)
	}
	return &MetadataStore{root: root}, nil
}

// Write atomically persists metadata to disk. The file is written to
// a temporary location first, then renamed to the final sharded path.
// This ensures readers never see a partially-written file.
func (m *MetadataStore) Write(meta *ArtifactMetadata) error {
	data, err := codec.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshaling artifact metadata: %w", err)
	}

	finalPath := m.path(meta.FileHash)

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("creating metadata shard directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(m.root, "metadata-*.cbor")
	if err != nil {
		return fmt.Errorf("creating temp metadata file: %w", err)
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
		return fmt.Errorf("writing metadata: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp metadata file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("renaming metadata to %s: %w", finalPath, err)
	}

	success = true
	return nil
}

// Read loads metadata for the given file hash. Returns an error
// wrapping os.ErrNotExist if no metadata has been stored for this
// artifact.
func (m *MetadataStore) Read(fileHash Hash) (*ArtifactMetadata, error) {
	data, err := os.ReadFile(m.path(fileHash))
	if err != nil {
		return nil, fmt.Errorf("reading metadata for %s: %w", FormatRef(fileHash), err)
	}

	var meta ArtifactMetadata
	if err := codec.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decoding metadata for %s: %w", FormatRef(fileHash), err)
	}
	return &meta, nil
}

// Delete removes the metadata file for the given file hash. Returns
// nil if the file was removed or did not exist.
func (m *MetadataStore) Delete(fileHash Hash) error {
	path := m.path(fileHash)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing metadata for %s: %w", FormatHash(fileHash), err)
	}
	return nil
}

// ScanRefs walks the metadata directory and returns a mapping from
// short artifact references to their full file hashes. This reads
// only filenames — it does NOT open or parse any CBOR files. The
// hash is extracted from the filename (which is the 64-hex-char hash
// plus a ".cbor" extension), and the ref is computed via FormatRef.
//
// The returned map uses refs as keys and slices of hashes as values
// to handle the (unlikely but possible) case where multiple distinct
// file hashes produce the same 12-hex-char ref prefix.
//
// This is called once at service startup to build the in-memory ref
// index. With 100K artifacts, this is ~100K readdir entries with zero
// file reads — fast enough that startup time is dominated by the
// initial Matrix /sync.
func (m *MetadataStore) ScanRefs() (map[string][]Hash, error) {
	result := make(map[string][]Hash)

	err := filepath.WalkDir(m.root, func(path string, entry os.DirEntry, err error) error {
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

		hexString := strings.TrimSuffix(name, ".cbor")
		hash, err := ParseHash(hexString)
		if err != nil {
			// Skip files that don't parse as hashes (e.g., temp files
			// left by a crash). These are not metadata files.
			return nil
		}

		ref := FormatRef(hash)
		result[ref] = append(result[ref], hash)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning metadata directory: %w", err)
	}

	return result, nil
}

// ScanAll walks the metadata directory, reads every CBOR metadata file,
// and returns all records. Unlike ScanRefs (which only reads filenames),
// this decodes every file. At 100K artifacts this is ~20-50MB of I/O
// — acceptable at startup, where it runs once to populate the in-memory
// artifact index.
func (m *MetadataStore) ScanAll() ([]ArtifactMetadata, error) {
	var results []ArtifactMetadata

	err := filepath.WalkDir(m.root, func(path string, entry os.DirEntry, err error) error {
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

		// Verify this is a proper hash-named file (skip temp files).
		hexString := strings.TrimSuffix(name, ".cbor")
		if _, err := ParseHash(hexString); err != nil {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading metadata %s: %w", path, err)
		}

		var meta ArtifactMetadata
		if err := codec.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("decoding metadata %s: %w", path, err)
		}

		results = append(results, meta)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning metadata directory: %w", err)
	}

	return results, nil
}

// path returns the sharded filesystem path for a metadata file.
func (m *MetadataStore) path(fileHash Hash) string {
	hex := FormatHash(fileHash)
	return filepath.Join(m.root, hex[:2], hex[2:4], hex+".cbor")
}
