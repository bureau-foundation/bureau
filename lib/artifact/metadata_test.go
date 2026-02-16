// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataStoreWriteRead(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	hash := HashChunk([]byte("test content"))
	fileHash := HashFile(hash)
	storedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	original := &ArtifactMetadata{
		FileHash:       fileHash,
		Ref:            FormatRef(fileHash),
		ContentType:    "text/plain",
		Filename:       "notes.txt",
		Description:    "test artifact",
		Labels:         []string{"test", "unit"},
		CachePolicy:    "default",
		Visibility:     "private",
		TTL:            "24h",
		Size:           1024,
		ChunkCount:     1,
		ContainerCount: 1,
		Compression:    "lz4",
		StoredAt:       storedAt,
	}

	if err := store.Write(original); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Read(fileHash)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.FileHash != original.FileHash {
		t.Errorf("FileHash mismatch")
	}
	if loaded.Ref != original.Ref {
		t.Errorf("Ref = %q, want %q", loaded.Ref, original.Ref)
	}
	if loaded.ContentType != original.ContentType {
		t.Errorf("ContentType = %q, want %q", loaded.ContentType, original.ContentType)
	}
	if loaded.Filename != original.Filename {
		t.Errorf("Filename = %q, want %q", loaded.Filename, original.Filename)
	}
	if loaded.Description != original.Description {
		t.Errorf("Description = %q, want %q", loaded.Description, original.Description)
	}
	if len(loaded.Labels) != len(original.Labels) {
		t.Errorf("Labels length = %d, want %d", len(loaded.Labels), len(original.Labels))
	}
	for i, label := range loaded.Labels {
		if label != original.Labels[i] {
			t.Errorf("Labels[%d] = %q, want %q", i, label, original.Labels[i])
		}
	}
	if loaded.Size != original.Size {
		t.Errorf("Size = %d, want %d", loaded.Size, original.Size)
	}
	if loaded.ChunkCount != original.ChunkCount {
		t.Errorf("ChunkCount = %d, want %d", loaded.ChunkCount, original.ChunkCount)
	}
	if loaded.ContainerCount != original.ContainerCount {
		t.Errorf("ContainerCount = %d, want %d", loaded.ContainerCount, original.ContainerCount)
	}
	if loaded.Compression != original.Compression {
		t.Errorf("Compression = %q, want %q", loaded.Compression, original.Compression)
	}
	if !loaded.StoredAt.Equal(original.StoredAt) {
		t.Errorf("StoredAt = %v, want %v", loaded.StoredAt, original.StoredAt)
	}
}

func TestValidateVisibility(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{VisibilityPrivate, false},
		{VisibilityPublic, false},
		{"", true},
		{"banana", true},
		{"PRIVATE", true}, // case-sensitive
		{"Protected", true},
	}

	for _, test := range tests {
		err := ValidateVisibility(test.input)
		if (err != nil) != test.wantErr {
			t.Errorf("ValidateVisibility(%q): err=%v, wantErr=%v", test.input, err, test.wantErr)
		}
	}
}

func TestNormalizeVisibility(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", VisibilityPrivate},
		{VisibilityPrivate, VisibilityPrivate},
		{VisibilityPublic, VisibilityPublic},
		{"banana", "banana"}, // NormalizeVisibility doesn't validate — that's ValidateVisibility's job
	}

	for _, test := range tests {
		got := NormalizeVisibility(test.input)
		if got != test.want {
			t.Errorf("NormalizeVisibility(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestMetadataStoreReadNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	hash := HashChunk([]byte("nonexistent"))
	fileHash := HashFile(hash)

	_, err = store.Read(fileHash)
	if err == nil {
		t.Fatal("expected error for missing metadata")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

func TestMetadataStoreScanRefs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Store several artifacts.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var hashes []Hash
	for i := 0; i < 5; i++ {
		content := []byte{byte(i), byte(i + 1), byte(i + 2)}
		chunkHash := HashChunk(content)
		fileHash := HashFile(chunkHash)
		hashes = append(hashes, fileHash)

		meta := &ArtifactMetadata{
			FileHash:    fileHash,
			Ref:         FormatRef(fileHash),
			ContentType: "application/octet-stream",
			Size:        int64(len(content)),
			ChunkCount:  1,
			StoredAt:    baseTime.Add(time.Duration(i) * time.Minute),
		}
		if err := store.Write(meta); err != nil {
			t.Fatalf("writing metadata %d: %v", i, err)
		}
	}

	// Scan and verify all refs are found.
	refMap, err := store.ScanRefs()
	if err != nil {
		t.Fatal(err)
	}

	for _, fileHash := range hashes {
		ref := FormatRef(fileHash)
		hashList, exists := refMap[ref]
		if !exists {
			t.Errorf("ref %s not found in scan results", ref)
			continue
		}
		found := false
		for _, scannedHash := range hashList {
			if scannedHash == fileHash {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("hash %s not found under ref %s", FormatHash(fileHash), ref)
		}
	}
}

func TestMetadataStoreScanRefsEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	refMap, err := store.ScanRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refMap) != 0 {
		t.Errorf("expected empty map, got %d entries", len(refMap))
	}
}

func TestMetadataStoreScanRefsIgnoresNonHashFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write a real metadata file.
	chunkHash := HashChunk([]byte("real"))
	fileHash := HashFile(chunkHash)
	meta := &ArtifactMetadata{
		FileHash:    fileHash,
		Ref:         FormatRef(fileHash),
		ContentType: "text/plain",
		Size:        4,
		ChunkCount:  1,
		StoredAt:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Write(meta); err != nil {
		t.Fatal(err)
	}

	// Create a non-hash file that should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "not-a-hash.cbor"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	refMap, err := store.ScanRefs()
	if err != nil {
		t.Fatal(err)
	}

	// Should find exactly 1 ref.
	totalHashes := 0
	for _, hashList := range refMap {
		totalHashes += len(hashList)
	}
	if totalHashes != 1 {
		t.Errorf("expected 1 hash, got %d", totalHashes)
	}
}

func TestMetadataStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	chunkHash := HashChunk([]byte("deletable metadata"))
	fileHash := HashFile(chunkHash)

	meta := &ArtifactMetadata{
		FileHash:    fileHash,
		Ref:         FormatRef(fileHash),
		ContentType: "text/plain",
		Size:        18,
		ChunkCount:  1,
		StoredAt:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Write(meta); err != nil {
		t.Fatal(err)
	}

	// Delete the metadata.
	if err := store.Delete(fileHash); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Read should fail.
	_, err = store.Read(fileHash)
	if err == nil {
		t.Error("expected error reading deleted metadata")
	}
}

func TestMetadataStoreDeleteNonExistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	var unknownHash Hash
	unknownHash[0] = 0xFF

	// Deleting a non-existent file should not error.
	if err := store.Delete(unknownHash); err != nil {
		t.Errorf("Delete non-existent should succeed, got: %v", err)
	}
}

func TestMetadataStoreOverwrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMetadataStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	chunkHash := HashChunk([]byte("content"))
	fileHash := HashFile(chunkHash)

	// Write initial metadata.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	meta1 := &ArtifactMetadata{
		FileHash:    fileHash,
		Ref:         FormatRef(fileHash),
		ContentType: "text/plain",
		Size:        7,
		ChunkCount:  1,
		StoredAt:    baseTime,
	}
	if err := store.Write(meta1); err != nil {
		t.Fatal(err)
	}

	// Overwrite with different metadata (same hash, different fields).
	meta2 := &ArtifactMetadata{
		FileHash:    fileHash,
		Ref:         FormatRef(fileHash),
		ContentType: "text/plain; charset=utf-8",
		Description: "updated description",
		Size:        7,
		ChunkCount:  1,
		StoredAt:    baseTime.Add(time.Minute),
	}
	if err := store.Write(meta2); err != nil {
		t.Fatal(err)
	}

	// Read back and verify the overwrite took effect.
	loaded, err := store.Read(fileHash)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContentType != meta2.ContentType {
		t.Errorf("ContentType = %q, want %q", loaded.ContentType, meta2.ContentType)
	}
	if loaded.Description != meta2.Description {
		t.Errorf("Description = %q, want %q", loaded.Description, meta2.Description)
	}
}
