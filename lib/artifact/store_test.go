// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "artifact"))
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return store
}

func TestStoreDirectoryStructure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifact")
	_, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	for _, dir := range []string{containerDir, reconstructionDir, tmpDir} {
		path := filepath.Join(root, dir)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("directory %s does not exist: %v", dir, err)
		} else if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
}

func TestStoreNewStoreIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifact")

	// Creating twice should not error.
	_, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewStore(root)
	if err != nil {
		t.Fatalf("second NewStore failed: %v", err)
	}
}

func TestStoreSmallArtifactRoundtrip(t *testing.T) {
	store := newTestStore(t)

	// Small artifact: below SmallArtifactThreshold.
	content := []byte("a small file — a stack trace, a config, a screenshot description")

	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent failed: %v", err)
	}

	if result.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", result.Size, len(content))
	}
	if result.ChunkCount != 1 {
		t.Errorf("ChunkCount = %d, want 1", result.ChunkCount)
	}
	if result.ContainerCount != 1 {
		t.Errorf("ContainerCount = %d, want 1", result.ContainerCount)
	}

	var zeroHash Hash
	if result.FileHash == zeroHash {
		t.Error("FileHash is zero")
	}
	if result.Ref == "" {
		t.Error("Ref is empty")
	}

	// Read it back.
	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Errorf("read-back content does not match original (got %d bytes, want %d)",
			len(readBack), len(content))
	}
}

func TestStoreLargeArtifactRoundtrip(t *testing.T) {
	store := newTestStore(t)

	// Large artifact: above SmallArtifactThreshold, multiple chunks.
	content := make([]byte, 512*1024)
	for i := range content {
		content[i] = byte(i * 37)
	}

	result, err := store.WriteContent(content, "application/octet-stream")
	if err != nil {
		t.Fatalf("WriteContent failed: %v", err)
	}

	if result.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", result.Size, len(content))
	}
	if result.ChunkCount < 2 {
		t.Errorf("ChunkCount = %d, expected > 1 for 512KB", result.ChunkCount)
	}
	if result.ContainerCount < 1 {
		t.Errorf("ContainerCount = %d, expected >= 1", result.ContainerCount)
	}

	// Read it back.
	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back content does not match original")
	}
}

func TestStoreMultiMBArtifact(t *testing.T) {
	store := newTestStore(t)

	// 2MB artifact: spans multiple chunks, likely 1 container.
	content := make([]byte, 2*1024*1024)
	for i := range content {
		content[i] = byte(i % 251) // prime modulus for variety
	}

	result, err := store.WriteContent(content, "")
	if err != nil {
		t.Fatalf("WriteContent failed: %v", err)
	}

	t.Logf("2MB artifact: %d chunks, %d containers, %d compressed bytes (%.1f%% of original)",
		result.ChunkCount, result.ContainerCount, result.CompressedSize,
		100.0*float64(result.CompressedSize)/float64(result.Size))

	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back content does not match original")
	}
}

func TestStoreCompressibleContent(t *testing.T) {
	store := newTestStore(t)

	// Highly compressible content: repeated JSON.
	line := []byte(`{"event_type":"m.bureau.artifact","hash":"abcdef0123456789"}` + "\n")
	content := bytes.Repeat(line, 5000) // ~300KB

	result, err := store.WriteContent(content, "application/x-ndjson")
	if err != nil {
		t.Fatalf("WriteContent failed: %v", err)
	}

	// Should have compressed well.
	if result.Compression != CompressionZstd {
		t.Errorf("Compression = %s, want zstd for NDJSON", result.Compression)
	}
	if result.CompressedSize >= result.Size {
		t.Errorf("CompressedSize %d >= Size %d — compression should help for repetitive JSON",
			result.CompressedSize, result.Size)
	}

	t.Logf("NDJSON: %d bytes → %d compressed (%.1fx ratio)",
		result.Size, result.CompressedSize,
		float64(result.Size)/float64(result.CompressedSize))

	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back content does not match original")
	}
}

func TestStoreIncompressibleContent(t *testing.T) {
	store := newTestStore(t)

	// Random data: incompressible.
	content := make([]byte, 300*1024)
	rand.Read(content)

	result, err := store.WriteContent(content, "")
	if err != nil {
		t.Fatalf("WriteContent failed: %v", err)
	}

	// Auto-selection should choose none for random data.
	if result.Compression != CompressionNone {
		t.Errorf("Compression = %s, expected none for random data", result.Compression)
	}

	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back content does not match original")
	}
}

func TestStoreCompressionOverride(t *testing.T) {
	store := newTestStore(t)

	content := bytes.Repeat([]byte("compressible text data "), 5000) // ~110KB

	none := CompressionNone
	result, err := store.Write(bytes.NewReader(content), "", &none)
	if err != nil {
		t.Fatalf("Write with none override failed: %v", err)
	}

	// With none override, compressed size should equal uncompressed.
	if result.Compression != CompressionNone {
		t.Errorf("Compression = %s, want none", result.Compression)
	}

	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatalf("ReadContent failed: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back content does not match original")
	}
}

func TestStoreDeterministic(t *testing.T) {
	// Same content stored twice should produce the same file hash.
	content := []byte("deterministic content for dedup testing")

	store1 := newTestStore(t)
	result1, err := store1.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	store2 := newTestStore(t)
	result2, err := store2.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	if result1.FileHash != result2.FileHash {
		t.Errorf("same content produced different file hashes:\n  %s\n  %s",
			FormatHash(result1.FileHash), FormatHash(result2.FileHash))
	}
	if result1.Ref != result2.Ref {
		t.Errorf("same content produced different refs: %s vs %s", result1.Ref, result2.Ref)
	}
}

func TestStoreDedup(t *testing.T) {
	store := newTestStore(t)

	content := bytes.Repeat([]byte("dedup me "), 1000)

	result1, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// Store the same content again. The container already exists on
	// disk, so the second write should succeed without creating a
	// duplicate file.
	result2, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	if result1.FileHash != result2.FileHash {
		t.Error("same content produced different hashes on second store")
	}

	// Both should be readable.
	readBack, err := store.ReadContent(result1.FileHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("read-back after dedup does not match")
	}
}

func TestStoreExists(t *testing.T) {
	store := newTestStore(t)

	content := []byte("existence test")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	if !store.Exists(result.FileHash) {
		t.Error("Exists returns false for stored artifact")
	}

	var unknownHash Hash
	unknownHash[0] = 0xFF
	if store.Exists(unknownHash) {
		t.Error("Exists returns true for unknown artifact")
	}
}

func TestStoreStat(t *testing.T) {
	store := newTestStore(t)

	content := make([]byte, 300*1024)
	for i := range content {
		content[i] = byte(i)
	}

	result, err := store.WriteContent(content, "")
	if err != nil {
		t.Fatal(err)
	}

	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	if record.FileHash != result.FileHash {
		t.Error("Stat file hash mismatch")
	}
	if record.Size != result.Size {
		t.Errorf("Stat size = %d, want %d", record.Size, result.Size)
	}
	if record.ChunkCount != result.ChunkCount {
		t.Errorf("Stat chunk count = %d, want %d", record.ChunkCount, result.ChunkCount)
	}
}

func TestStoreStatNotFound(t *testing.T) {
	store := newTestStore(t)

	var unknownHash Hash
	unknownHash[0] = 0xFF
	_, err := store.Stat(unknownHash)
	if err == nil {
		t.Error("Stat should fail for unknown artifact")
	}
}

func TestStoreReadNotFound(t *testing.T) {
	store := newTestStore(t)

	var unknownHash Hash
	unknownHash[0] = 0xFF
	_, err := store.ReadContent(unknownHash)
	if err == nil {
		t.Error("ReadContent should fail for unknown artifact")
	}
}

func TestStoreEmptyContent(t *testing.T) {
	store := newTestStore(t)

	_, err := store.WriteContent(nil, "")
	if err == nil {
		t.Error("WriteContent(nil) should fail")
	}

	_, err = store.WriteContent([]byte{}, "")
	if err == nil {
		t.Error("WriteContent(empty) should fail")
	}
}

func TestStoreShardedPaths(t *testing.T) {
	store := newTestStore(t)

	content := []byte("sharding test content")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// Verify container files exist in the sharded directory structure.
	record, err := store.Stat(result.FileHash)
	if err != nil {
		t.Fatal(err)
	}

	for _, segment := range record.Segments {
		containerPath := store.ContainerPath(segment.Container)
		if _, err := os.Stat(containerPath); err != nil {
			t.Errorf("container file does not exist at %s: %v", containerPath, err)
		}

		// Verify the path is sharded: root/containers/XX/YY/fullhash
		dir := filepath.Dir(containerPath)
		parent := filepath.Dir(dir)
		if filepath.Base(parent) != FormatHash(segment.Container)[:2] {
			t.Errorf("container shard structure incorrect: %s", containerPath)
		}
	}

	// Verify reconstruction file exists in the sharded structure.
	reconPath := store.reconstructionPath(result.FileHash)
	if _, err := os.Stat(reconPath); err != nil {
		t.Errorf("reconstruction file does not exist at %s: %v", reconPath, err)
	}
}

func TestStoreDelete(t *testing.T) {
	store := newTestStore(t)

	content := []byte("deletable artifact")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// Verify the artifact exists before deletion.
	if !store.Exists(result.FileHash) {
		t.Fatal("artifact should exist before delete")
	}

	// Delete with an empty live set (no shared containers to protect).
	deletedContainers, err := store.Delete(result.FileHash, nil)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(deletedContainers) != result.ContainerCount {
		t.Errorf("deleted %d containers, want %d", len(deletedContainers), result.ContainerCount)
	}

	// The artifact should no longer exist.
	if store.Exists(result.FileHash) {
		t.Error("artifact should not exist after delete")
	}

	// ReadContent should fail.
	if _, err := store.ReadContent(result.FileHash); err == nil {
		t.Error("ReadContent should fail after delete")
	}
}

func TestStoreDeleteSharedContainers(t *testing.T) {
	store := newTestStore(t)

	// Store two identical artifacts. They share containers because
	// same content produces the same container hashes.
	content := []byte("shared container content for dedup")
	result1, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// Get the container hashes from the first artifact.
	record, err := store.Stat(result1.FileHash)
	if err != nil {
		t.Fatal(err)
	}

	// Build a live set that includes all containers (simulating
	// another artifact sharing the same containers).
	liveContainers := make(map[Hash]struct{})
	for _, segment := range record.Segments {
		liveContainers[segment.Container] = struct{}{}
	}

	// Delete with the live set. Containers should be preserved.
	deletedContainers, err := store.Delete(result1.FileHash, liveContainers)
	if err != nil {
		t.Fatalf("Delete with live set: %v", err)
	}

	if len(deletedContainers) != 0 {
		t.Errorf("deleted %d containers, want 0 (all in live set)", len(deletedContainers))
	}

	// Container files should still exist on disk.
	for _, segment := range record.Segments {
		containerPath := store.ContainerPath(segment.Container)
		if _, err := os.Stat(containerPath); err != nil {
			t.Errorf("shared container should survive delete: %v", err)
		}
	}
}

func TestStoreDeleteNotFound(t *testing.T) {
	store := newTestStore(t)

	var unknownHash Hash
	unknownHash[0] = 0xFF

	_, err := store.Delete(unknownHash, nil)
	if err == nil {
		t.Error("Delete should fail for unknown artifact")
	}
}

func TestStoreAtSmallArtifactBoundary(t *testing.T) {
	store := newTestStore(t)

	// Exactly at the threshold: should take the small path.
	content := make([]byte, SmallArtifactThreshold)
	for i := range content {
		content[i] = byte(i)
	}

	result, err := store.WriteContent(content, "")
	if err != nil {
		t.Fatal(err)
	}

	if result.ChunkCount != 1 {
		t.Errorf("at threshold: ChunkCount = %d, want 1", result.ChunkCount)
	}

	readBack, err := store.ReadContent(result.FileHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("roundtrip failed at small artifact boundary")
	}

	// One byte above threshold: should take the large path.
	contentPlus := make([]byte, SmallArtifactThreshold+1)
	for i := range contentPlus {
		contentPlus[i] = byte(i)
	}

	resultPlus, err := store.WriteContent(contentPlus, "")
	if err != nil {
		t.Fatal(err)
	}

	// Above threshold, CDC might still produce only 1-2 chunks depending
	// on boundary detection, so we just verify the roundtrip works.
	readBackPlus, err := store.ReadContent(resultPlus.FileHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBackPlus, contentPlus) {
		t.Error("roundtrip failed just above small artifact boundary")
	}
}

// Benchmarks for the store. Run with:
//
//	bazel run //lib/artifact:artifact_test -- \
//	    -test.bench=BenchmarkStore -test.benchmem -test.count=5 -test.run='^$'

func BenchmarkStoreWriteSmall(b *testing.B) {
	store := newBenchStore(b)
	content := bytes.Repeat([]byte("small artifact content "), 40) // ~920 bytes

	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	for b.Loop() {
		store.WriteContent(content, "text/plain")
	}
}

func BenchmarkStoreWriteLarge(b *testing.B) {
	sizes := []int{
		512 * 1024,       // 512KB
		2 * 1024 * 1024,  // 2MB
		16 * 1024 * 1024, // 16MB
	}

	for _, size := range sizes {
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i % 251)
		}

		b.Run(fmt.Sprintf("size=%s", formatByteSize(size)), func(b *testing.B) {
			store := newBenchStore(b)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				store.WriteContent(content, "")
			}
		})
	}
}

func BenchmarkStoreReadSmall(b *testing.B) {
	store := newBenchStore(b)
	content := bytes.Repeat([]byte("read benchmark small "), 40)

	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	for b.Loop() {
		store.ReadContent(result.FileHash)
	}
}

func BenchmarkStoreReadLarge(b *testing.B) {
	sizes := []int{
		512 * 1024,      // 512KB
		2 * 1024 * 1024, // 2MB
	}

	for _, size := range sizes {
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i % 251)
		}

		b.Run(fmt.Sprintf("size=%s", formatByteSize(size)), func(b *testing.B) {
			store := newBenchStore(b)
			result, err := store.WriteContent(content, "")
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				store.ReadContent(result.FileHash)
			}
		})
	}
}

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	store, err := NewStore(filepath.Join(b.TempDir(), "artifact"))
	if err != nil {
		b.Fatal(err)
	}
	return store
}
