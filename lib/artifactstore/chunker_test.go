// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"crypto/rand"
	"fmt"
	"testing"
)

func TestChunkerEmpty(t *testing.T) {
	chunker := NewChunker(nil)
	if chunk := chunker.Next(); chunk != nil {
		t.Errorf("expected nil for empty input, got chunk of %d bytes", len(chunk.Data))
	}

	chunker2 := NewChunker([]byte{})
	if chunk := chunker2.Next(); chunk != nil {
		t.Errorf("expected nil for zero-length input, got chunk of %d bytes", len(chunk.Data))
	}
}

func TestChunkerSmallInput(t *testing.T) {
	// Input smaller than MinChunkSize: should produce exactly one chunk.
	input := make([]byte, 1024)
	for i := range input {
		input[i] = byte(i)
	}

	chunker := NewChunker(input)
	chunk := chunker.Next()
	if chunk == nil {
		t.Fatal("expected a chunk, got nil")
	}
	if len(chunk.Data) != 1024 {
		t.Errorf("chunk size = %d, want 1024", len(chunk.Data))
	}
	if chunk.Hash != HashChunk(input) {
		t.Error("chunk hash does not match HashChunk(input)")
	}

	if next := chunker.Next(); next != nil {
		t.Errorf("expected nil after single small chunk, got chunk of %d bytes", len(next.Data))
	}
}

func TestChunkerMinChunkSize(t *testing.T) {
	// Input exactly at MinChunkSize: should produce exactly one chunk
	// (boundary detection starts at MinChunkSize, so the boundary can
	// only occur AT MinChunkSize or later).
	input := make([]byte, MinChunkSize)
	for i := range input {
		input[i] = byte(i)
	}

	chunks := ChunkAll(input)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for MinChunkSize input, got %d", len(chunks))
	}
}

func TestChunkerMaxChunkSize(t *testing.T) {
	// All-zero input: the GearHash rolling hash of zeros with these
	// table entries may or may not trigger boundaries. Regardless of
	// content, no chunk should exceed MaxChunkSize.
	input := make([]byte, MaxChunkSize*3)

	chunks := ChunkAll(input)
	for i, chunk := range chunks {
		if len(chunk.Data) > MaxChunkSize {
			t.Errorf("chunk %d: size %d exceeds MaxChunkSize %d", i, len(chunk.Data), MaxChunkSize)
		}
	}
}

func TestChunkerReassembly(t *testing.T) {
	// Concatenating all chunks must reproduce the original input.
	input := make([]byte, 512*1024) // 512KB
	for i := range input {
		input[i] = byte(i * 37) // non-trivial pattern
	}

	chunks := ChunkAll(input)
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}

	var reassembled []byte
	for _, chunk := range chunks {
		reassembled = append(reassembled, chunk.Data...)
	}

	if len(reassembled) != len(input) {
		t.Fatalf("reassembled length %d != input length %d", len(reassembled), len(input))
	}
	for i := range input {
		if reassembled[i] != input[i] {
			t.Fatalf("reassembled differs at byte %d: got %d, want %d", i, reassembled[i], input[i])
		}
	}
}

func TestChunkerDeterministic(t *testing.T) {
	input := make([]byte, 256*1024)
	for i := range input {
		input[i] = byte(i ^ 0xAB)
	}

	chunks1 := ChunkAll(input)
	chunks2 := ChunkAll(input)

	if len(chunks1) != len(chunks2) {
		t.Fatalf("chunk count differs: %d vs %d", len(chunks1), len(chunks2))
	}

	for i := range chunks1 {
		if len(chunks1[i].Data) != len(chunks2[i].Data) {
			t.Errorf("chunk %d: size %d vs %d", i, len(chunks1[i].Data), len(chunks2[i].Data))
		}
		if chunks1[i].Hash != chunks2[i].Hash {
			t.Errorf("chunk %d: hash mismatch", i)
		}
	}
}

func TestChunkerChunkSizeBounds(t *testing.T) {
	// With random data (high entropy), we should see a reasonable
	// distribution of chunk sizes between Min and Max.
	input := make([]byte, 4*1024*1024) // 4MB
	rand.Read(input)

	chunks := ChunkAll(input)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for 4MB random input, got %d", len(chunks))
	}

	var totalSize int
	for i, chunk := range chunks {
		size := len(chunk.Data)
		totalSize += size

		// Last chunk can be smaller than MinChunkSize.
		if i < len(chunks)-1 {
			if size < MinChunkSize {
				t.Errorf("chunk %d: size %d is below MinChunkSize %d (not the last chunk)", i, size, MinChunkSize)
			}
		}

		if size > MaxChunkSize {
			t.Errorf("chunk %d: size %d exceeds MaxChunkSize %d", i, size, MaxChunkSize)
		}
	}

	if totalSize != len(input) {
		t.Errorf("total chunk bytes %d != input length %d", totalSize, len(input))
	}

	// With random data and a 64KB target, we expect roughly 4MB/64KB = 64 chunks.
	// Allow a wide range since it's random, but flag extreme outliers.
	expectedChunks := len(input) / TargetChunkSize
	if len(chunks) < expectedChunks/4 || len(chunks) > expectedChunks*4 {
		t.Errorf("chunk count %d is far from expected ~%d for %d bytes with %d target",
			len(chunks), expectedChunks, len(input), TargetChunkSize)
	}
}

func TestChunkerInsertionLocality(t *testing.T) {
	// The key property of CDC: inserting bytes at the beginning of the
	// input should only affect the first chunk or two. Later chunks
	// should have the same boundaries (and thus the same hashes).
	//
	// Use deterministic pseudo-random data (LCG) so chunk boundaries
	// are reproducible across runs. 2MB gives ~32 chunks at 64KB
	// target, making the locality assertion robust.
	base := make([]byte, 2*1024*1024) // 2MB
	lcg := uint64(0xDEADBEEF)
	for i := range base {
		lcg = lcg*6364136223846793005 + 1442695040888963407
		base[i] = byte(lcg >> 56)
	}

	// Modified version: 16 bytes inserted at the front.
	modified := make([]byte, len(base)+16)
	for i := range modified[:16] {
		modified[i] = byte(i + 0xFF)
	}
	copy(modified[16:], base)

	baseChunks := ChunkAll(base)
	modifiedChunks := ChunkAll(modified)

	// Count how many chunks in modified have the same hash as some
	// chunk in base. With CDC, most chunks should be shared because
	// the insertion only affects boundaries near the insertion point.
	baseHashes := make(map[Hash]bool, len(baseChunks))
	for _, chunk := range baseChunks {
		baseHashes[chunk.Hash] = true
	}

	var shared int
	for _, chunk := range modifiedChunks {
		if baseHashes[chunk.Hash] {
			shared++
		}
	}

	// We expect the vast majority of chunks to be shared. With 512KB
	// input and 64KB target chunks, that's ~8 chunks. A 16-byte
	// insertion should only affect 1-2 chunks.
	minExpectedShared := len(baseChunks) - 3
	if minExpectedShared < 0 {
		minExpectedShared = 0
	}
	if shared < minExpectedShared {
		t.Errorf("only %d/%d base chunks found in modified output (expected >= %d); CDC locality is poor",
			shared, len(baseChunks), minExpectedShared)
	}
}

func TestChunkerHashesMatchStandalone(t *testing.T) {
	// Each chunk's Hash field must match what HashChunk produces on
	// the same bytes.
	input := make([]byte, 200*1024)
	for i := range input {
		input[i] = byte(i)
	}

	chunks := ChunkAll(input)
	for i, chunk := range chunks {
		expected := HashChunk(chunk.Data)
		if chunk.Hash != expected {
			t.Errorf("chunk %d: hash mismatch between chunker and HashChunk", i)
		}
	}
}

func TestChunkAllEmptyInput(t *testing.T) {
	chunks := ChunkAll(nil)
	if len(chunks) != 0 {
		t.Errorf("expected no chunks for nil input, got %d", len(chunks))
	}
}

func TestGearFindBoundaryShort(t *testing.T) {
	// Data shorter than MaxChunkSize: gearFindBoundary should return
	// the full length (take everything).
	data := make([]byte, 1000)
	boundary := gearFindBoundary(data)
	if boundary != 1000 {
		t.Errorf("gearFindBoundary(1000 bytes) = %d, want 1000", boundary)
	}
}

func TestGearFindBoundaryMaxChunk(t *testing.T) {
	// With data larger than MaxChunkSize, the boundary must be at
	// most MaxChunkSize.
	data := make([]byte, MaxChunkSize*2)
	boundary := gearFindBoundary(data)
	if boundary > MaxChunkSize {
		t.Errorf("gearFindBoundary exceeded MaxChunkSize: got %d", boundary)
	}
	if boundary < MinChunkSize {
		t.Errorf("gearFindBoundary below MinChunkSize: got %d", boundary)
	}
}

func TestGearTableLength(t *testing.T) {
	if len(gearTable) != 256 {
		t.Errorf("gearTable length = %d, want 256", len(gearTable))
	}
}

func TestGearTableNonZero(t *testing.T) {
	// At least some entries should be non-zero. An all-zero table
	// would make the hash degenerate.
	var nonZero int
	for _, entry := range gearTable {
		if entry != 0 {
			nonZero++
		}
	}
	if nonZero < 200 {
		t.Errorf("only %d/256 non-zero gear table entries; table may be corrupt", nonZero)
	}
}

// Benchmarks for the chunker. Run with:
//
//	bazel run //lib/artifact:artifact_test -- \
//	    -test.bench=BenchmarkChunk -test.benchmem -test.count=10 -test.run='^$'

func BenchmarkChunker(b *testing.B) {
	sizes := []int{
		64 * 1024,        // 64KB: one target-sized chunk
		256 * 1024,       // 256KB: small artifact threshold
		1024 * 1024,      // 1MB
		4 * 1024 * 1024,  // 4MB
		64 * 1024 * 1024, // 64MB: container scale
	}

	for _, size := range sizes {
		// Use pseudorandom data for realistic chunk boundary distribution.
		input := make([]byte, size)
		rand.Read(input)

		b.Run(fmt.Sprintf("size=%s", formatByteSize(size)), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()

			var chunkCount int64
			for b.Loop() {
				chunkCount = 0
				chunker := NewChunker(input)
				for chunker.Next() != nil {
					chunkCount++
				}
			}
			b.ReportMetric(float64(chunkCount), "chunks/op")
		})
	}
}

func BenchmarkGearFindBoundary(b *testing.B) {
	// Benchmark the raw boundary detection without hashing.
	input := make([]byte, MaxChunkSize*2)
	rand.Read(input)

	b.SetBytes(int64(MaxChunkSize))
	b.ReportAllocs()
	for b.Loop() {
		gearFindBoundary(input)
	}
}
