// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
)

// testProvider is a containerProvider backed by an in-memory map.
type testProvider struct {
	containers map[artifactstore.Hash][]byte
}

func (p *testProvider) containerBytes(hash artifactstore.Hash) ([]byte, error) {
	data, ok := p.containers[hash]
	if !ok {
		return nil, &containerNotFoundError{hash: hash}
	}
	return data, nil
}

type containerNotFoundError struct {
	hash artifactstore.Hash
}

func (e *containerNotFoundError) Error() string {
	return "container not found: " + artifactstore.FormatHash(e.hash)
}

// storeAndBuildProvider stores content in a real Store and wraps it
// in a storeProvider (no cache).
func storeAndBuildProvider(t *testing.T, content []byte, contentType string) (*artifactstore.Store, *artifactstore.StoreResult, containerProvider) {
	t.Helper()
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifact"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	result, err := store.WriteContent(content, contentType)
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	return store, result, &storeProvider{store: store, cache: nil}
}

func TestFindChunkEmpty(t *testing.T) {
	if findChunk(nil, 0) != -1 {
		t.Error("findChunk on nil should return -1")
	}
	if findChunk([]chunkEntry{}, 0) != -1 {
		t.Error("findChunk on empty should return -1")
	}
}

func TestFindChunkSingleChunk(t *testing.T) {
	chunks := []chunkEntry{
		{offset: 0, size: 100},
	}

	tests := []struct {
		offset int64
		want   int
	}{
		{0, 0},
		{50, 0},
		{99, 0},
		{100, -1},  // past end
		{-1, -1},   // negative
		{1000, -1}, // way past end
	}

	for _, test := range tests {
		got := findChunk(chunks, test.offset)
		if got != test.want {
			t.Errorf("findChunk(offset=%d) = %d, want %d", test.offset, got, test.want)
		}
	}
}

func TestFindChunkMultipleChunks(t *testing.T) {
	chunks := []chunkEntry{
		{offset: 0, size: 100},
		{offset: 100, size: 200},
		{offset: 300, size: 50},
	}

	tests := []struct {
		offset int64
		want   int
	}{
		{0, 0},     // start of chunk 0
		{99, 0},    // end of chunk 0
		{100, 1},   // start of chunk 1
		{299, 1},   // end of chunk 1
		{300, 2},   // start of chunk 2
		{349, 2},   // end of chunk 2
		{350, -1},  // past all chunks
		{1000, -1}, // way past
	}

	for _, test := range tests {
		got := findChunk(chunks, test.offset)
		if got != test.want {
			t.Errorf("findChunk(offset=%d) = %d, want %d", test.offset, got, test.want)
		}
	}
}

func TestBuildChunkTableSmallArtifact(t *testing.T) {
	store, result, provider := storeAndBuildProvider(t, []byte("hello world"), "text/plain")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}

	if chunks[0].offset != 0 {
		t.Errorf("chunk 0 offset = %d, want 0", chunks[0].offset)
	}
	if chunks[0].size != uint32(len("hello world")) {
		t.Errorf("chunk 0 size = %d, want %d", chunks[0].size, len("hello world"))
	}
}

func TestBuildChunkTableLargeArtifact(t *testing.T) {
	// Create content large enough to span multiple chunks.
	// 512 KiB should produce several CDC chunks.
	content := make([]byte, 512*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	store, result, provider := storeAndBuildProvider(t, content, "application/octet-stream")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	// Verify cumulative offsets are monotonically increasing.
	for i := 1; i < len(chunks); i++ {
		expected := chunks[i-1].offset + int64(chunks[i-1].size)
		if chunks[i].offset != expected {
			t.Errorf("chunk %d: offset = %d, want %d (cumulative from previous)",
				i, chunks[i].offset, expected)
		}
	}

	// Verify total size matches.
	lastChunk := chunks[len(chunks)-1]
	totalSize := lastChunk.offset + int64(lastChunk.size)
	if totalSize != int64(len(content)) {
		t.Errorf("total size = %d, want %d", totalSize, len(content))
	}
}

func TestReadAtSmallArtifact(t *testing.T) {
	content := []byte("hello world, this is a test artifact")
	store, result, provider := storeAndBuildProvider(t, content, "text/plain")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	// Read the entire file.
	dest := make([]byte, len(content))
	bytesRead, err := readAt(chunks, provider, dest, 0, record.Size)
	if err != nil {
		t.Fatalf("readAt: %v", err)
	}
	if bytesRead != len(content) {
		t.Fatalf("read %d bytes, want %d", bytesRead, len(content))
	}
	if !bytes.Equal(dest, content) {
		t.Error("content mismatch")
	}
}

func TestReadAtPartialRead(t *testing.T) {
	content := []byte("0123456789abcdef")
	store, result, provider := storeAndBuildProvider(t, content, "text/plain")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	// Read 4 bytes from offset 5.
	dest := make([]byte, 4)
	bytesRead, err := readAt(chunks, provider, dest, 5, record.Size)
	if err != nil {
		t.Fatalf("readAt: %v", err)
	}
	if bytesRead != 4 {
		t.Fatalf("read %d bytes, want 4", bytesRead)
	}
	if string(dest[:bytesRead]) != "5678" {
		t.Errorf("got %q, want %q", string(dest[:bytesRead]), "5678")
	}
}

func TestReadAtPastEnd(t *testing.T) {
	content := []byte("short")
	store, result, provider := storeAndBuildProvider(t, content, "text/plain")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	dest := make([]byte, 100)
	bytesRead, err := readAt(chunks, provider, dest, int64(len(content)), record.Size)
	if err == nil {
		t.Fatalf("expected EOF, got %d bytes", bytesRead)
	}
}

func TestReadAtClampedAtEnd(t *testing.T) {
	content := []byte("hello world")
	store, result, provider := storeAndBuildProvider(t, content, "text/plain")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	// Request more bytes than available from offset 8.
	dest := make([]byte, 100)
	bytesRead, err := readAt(chunks, provider, dest, 8, record.Size)
	if err != nil {
		t.Fatalf("readAt: %v", err)
	}
	if bytesRead != 3 {
		t.Fatalf("read %d bytes, want 3", bytesRead)
	}
	if string(dest[:bytesRead]) != "rld" {
		t.Errorf("got %q, want %q", string(dest[:bytesRead]), "rld")
	}
}

func TestReadAtLargeArtifactSpansChunks(t *testing.T) {
	// 512 KiB of random data spans multiple chunks.
	content := make([]byte, 512*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	store, result, provider := storeAndBuildProvider(t, content, "application/octet-stream")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	if len(chunks) < 2 {
		t.Skipf("need multi-chunk artifact, got %d chunks", len(chunks))
	}

	// Read the entire file in one call.
	fullRead := make([]byte, len(content))
	bytesRead, err := readAt(chunks, provider, fullRead, 0, record.Size)
	if err != nil {
		t.Fatalf("full readAt: %v", err)
	}
	if bytesRead != len(content) {
		t.Fatalf("read %d bytes, want %d", bytesRead, len(content))
	}
	if !bytes.Equal(fullRead, content) {
		t.Error("full read content mismatch")
	}

	// Read a range that spans a chunk boundary.
	boundary := chunks[1].offset
	start := boundary - 10
	length := 20
	dest := make([]byte, length)
	bytesRead, err = readAt(chunks, provider, dest, start, record.Size)
	if err != nil {
		t.Fatalf("cross-boundary readAt: %v", err)
	}
	if bytesRead != length {
		t.Fatalf("cross-boundary read %d bytes, want %d", bytesRead, length)
	}
	if !bytes.Equal(dest, content[start:start+int64(length)]) {
		t.Error("cross-boundary content mismatch")
	}
}

func TestReadAtSequentialSmallReads(t *testing.T) {
	// Simulate a typical sequential read pattern: 4 KiB reads.
	content := make([]byte, 256*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	store, result, provider := storeAndBuildProvider(t, content, "application/octet-stream")

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	chunks, err := buildChunkTable(record, provider)
	if err != nil {
		t.Fatalf("buildChunkTable: %v", err)
	}

	// Read the entire file in 4 KiB chunks.
	readSize := 4096
	var assembled []byte
	offset := int64(0)
	for offset < int64(len(content)) {
		dest := make([]byte, readSize)
		bytesRead, err := readAt(chunks, provider, dest, offset, record.Size)
		if err != nil {
			t.Fatalf("readAt at offset %d: %v", offset, err)
		}
		if bytesRead == 0 {
			break
		}
		assembled = append(assembled, dest[:bytesRead]...)
		offset += int64(bytesRead)
	}

	if !bytes.Equal(assembled, content) {
		t.Error("sequential 4K reads produced different content than original")
	}
}
