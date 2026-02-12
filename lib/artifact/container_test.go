// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"testing"
)

func TestContainerRoundtrip(t *testing.T) {
	// Build a container with a few chunks, write it, read it back,
	// and verify every chunk decompresses to the original data.
	chunks := [][]byte{
		bytes.Repeat([]byte("chunk zero data "), 512),  // 8KB, compressible
		bytes.Repeat([]byte("chunk one data! "), 1024), // 16KB, compressible
		bytes.Repeat([]byte("chunk two bytes "), 2048), // 32KB, compressible
	}

	builder := NewContainerBuilder()
	for _, chunk := range chunks {
		chunkHash := HashChunk(chunk)
		compressed, err := CompressChunk(chunk, CompressionLZ4)
		if err != nil {
			if IsIncompressible(err) {
				builder.AddChunk(chunkHash, chunk, CompressionNone, uint32(len(chunk)))
				continue
			}
			t.Fatalf("CompressChunk failed: %v", err)
		}
		builder.AddChunk(chunkHash, compressed, CompressionLZ4, uint32(len(chunk)))
	}

	if builder.ChunkCount() != 3 {
		t.Fatalf("ChunkCount = %d, want 3", builder.ChunkCount())
	}

	// Write container to buffer.
	var buffer bytes.Buffer
	containerHash, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	var zeroHash Hash
	if containerHash == zeroHash {
		t.Error("container hash is zero")
	}

	// Read it back.
	reader := bytes.NewReader(buffer.Bytes())
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		t.Fatalf("ReadContainerIndex failed: %v", err)
	}

	if len(cr.Index) != 3 {
		t.Fatalf("index length = %d, want 3", len(cr.Index))
	}

	if cr.Hash != containerHash {
		t.Errorf("container hash mismatch: wrote %s, read %s",
			FormatHash(containerHash), FormatHash(cr.Hash))
	}

	// Extract each chunk and verify.
	for i, original := range chunks {
		decompressed, err := cr.ExtractChunk(reader, i)
		if err != nil {
			t.Fatalf("ExtractChunk(%d) failed: %v", i, err)
		}

		if !bytes.Equal(decompressed, original) {
			t.Errorf("chunk %d: decompressed data does not match original", i)
		}
	}
}

func TestContainerUncompressedChunks(t *testing.T) {
	// Container with CompressionNone chunks.
	data := make([]byte, 1024)
	rand.Read(data)

	builder := NewContainerBuilder()
	builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	_, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	reader := bytes.NewReader(buffer.Bytes())
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		t.Fatalf("ReadContainerIndex failed: %v", err)
	}

	extracted, err := cr.ExtractChunk(reader, 0)
	if err != nil {
		t.Fatalf("ExtractChunk failed: %v", err)
	}

	if !bytes.Equal(extracted, data) {
		t.Error("extracted data does not match original")
	}
}

func TestContainerMixedCompression(t *testing.T) {
	// Container with chunks using different compression algorithms.
	textData := bytes.Repeat([]byte("compressible text data for zstd "), 256)
	binaryData := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 2048)
	randomData := make([]byte, 4096)
	rand.Read(randomData)

	type chunkSpec struct {
		data []byte
		tag  CompressionTag
	}

	specs := []chunkSpec{
		{textData, CompressionZstd},
		{binaryData, CompressionLZ4},
		{randomData, CompressionNone},
	}

	builder := NewContainerBuilder()
	for _, spec := range specs {
		chunkHash := HashChunk(spec.data)
		if spec.tag == CompressionNone {
			builder.AddChunk(chunkHash, spec.data, CompressionNone, uint32(len(spec.data)))
		} else {
			compressed, err := CompressChunk(spec.data, spec.tag)
			if err != nil {
				if IsIncompressible(err) {
					builder.AddChunk(chunkHash, spec.data, CompressionNone, uint32(len(spec.data)))
					continue
				}
				t.Fatalf("CompressChunk(%s) failed: %v", spec.tag, err)
			}
			builder.AddChunk(chunkHash, compressed, spec.tag, uint32(len(spec.data)))
		}
	}

	var buffer bytes.Buffer
	containerHash, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	reader := bytes.NewReader(buffer.Bytes())
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		t.Fatalf("ReadContainerIndex failed: %v", err)
	}

	if cr.Hash != containerHash {
		t.Error("container hash mismatch")
	}

	for i, spec := range specs {
		extracted, err := cr.ExtractChunk(reader, i)
		if err != nil {
			t.Fatalf("ExtractChunk(%d) failed: %v", i, err)
		}
		if !bytes.Equal(extracted, spec.data) {
			t.Errorf("chunk %d data mismatch", i)
		}
	}
}

func TestContainerHashDeterministic(t *testing.T) {
	data := bytes.Repeat([]byte("deterministic"), 100)

	build := func() Hash {
		builder := NewContainerBuilder()
		builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))
		var buffer bytes.Buffer
		hash, err := builder.Flush(&buffer)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}

	hash1 := build()
	hash2 := build()
	if hash1 != hash2 {
		t.Error("container hash is not deterministic")
	}
}

func TestContainerHashDependsOnChunkOrder(t *testing.T) {
	chunk0 := []byte("first chunk")
	chunk1 := []byte("second chunk")

	buildContainer := func(first, second []byte) Hash {
		builder := NewContainerBuilder()
		builder.AddChunk(HashChunk(first), first, CompressionNone, uint32(len(first)))
		builder.AddChunk(HashChunk(second), second, CompressionNone, uint32(len(second)))
		var buffer bytes.Buffer
		hash, _ := builder.Flush(&buffer)
		return hash
	}

	forward := buildContainer(chunk0, chunk1)
	reverse := buildContainer(chunk1, chunk0)

	if forward == reverse {
		t.Error("container hash is order-independent; should depend on chunk order")
	}
}

func TestContainerFlushResets(t *testing.T) {
	builder := NewContainerBuilder()
	data := []byte("chunk data")
	builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	_, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	if builder.ChunkCount() != 0 {
		t.Errorf("after Flush, ChunkCount = %d, want 0", builder.ChunkCount())
	}
	if builder.DataSize() != 0 {
		t.Errorf("after Flush, DataSize = %d, want 0", builder.DataSize())
	}
}

func TestContainerFlushEmpty(t *testing.T) {
	builder := NewContainerBuilder()
	var buffer bytes.Buffer
	_, err := builder.Flush(&buffer)
	if err == nil {
		t.Error("Flush on empty builder should fail")
	}
}

func TestContainerInvalidMagic(t *testing.T) {
	data := bytes.Repeat([]byte{0}, 100)
	_, err := ReadContainerIndex(bytes.NewReader(data))
	if err == nil {
		t.Error("ReadContainerIndex should fail on invalid magic")
	}
}

func TestContainerWrongVersion(t *testing.T) {
	// Valid magic prefix but wrong version byte.
	header := []byte{'B', 'U', 'R', 'E', 'A', 'U', 99, 0}
	header = append(header, 0, 0, 0, 0) // chunk count
	_, err := ReadContainerIndex(bytes.NewReader(header))
	if err == nil {
		t.Error("ReadContainerIndex should fail on wrong version")
	}
}

func TestContainerZeroChunks(t *testing.T) {
	var buffer bytes.Buffer
	buffer.Write(containerMagic[:])
	buffer.Write([]byte{0, 0, 0, 0}) // chunk count = 0

	_, err := ReadContainerIndex(&buffer)
	if err == nil {
		t.Error("ReadContainerIndex should fail on zero chunks")
	}
}

func TestContainerTruncatedHeader(t *testing.T) {
	// Only the magic, no chunk count.
	_, err := ReadContainerIndex(bytes.NewReader(containerMagic[:]))
	if err == nil {
		t.Error("ReadContainerIndex should fail on truncated header")
	}
}

func TestContainerTruncatedIndex(t *testing.T) {
	// Valid header claiming 10 chunks, but index data is truncated.
	var buffer bytes.Buffer
	buffer.Write(containerMagic[:])
	buffer.Write([]byte{10, 0, 0, 0})         // chunk count = 10
	buffer.Write(bytes.Repeat([]byte{0}, 20)) // partial index

	_, err := ReadContainerIndex(&buffer)
	if err == nil {
		t.Error("ReadContainerIndex should fail on truncated index")
	}
}

func TestContainerReadAllChunks(t *testing.T) {
	chunks := [][]byte{
		bytes.Repeat([]byte("A"), 100),
		bytes.Repeat([]byte("B"), 200),
		bytes.Repeat([]byte("C"), 300),
	}

	builder := NewContainerBuilder()
	for _, chunk := range chunks {
		builder.AddChunk(HashChunk(chunk), chunk, CompressionNone, uint32(len(chunk)))
	}

	var buffer bytes.Buffer
	_, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	// Read index, then read all chunks sequentially.
	cr, err := ReadContainerIndex(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	allChunks, err := cr.ReadAllChunks(&buffer)
	if err != nil {
		t.Fatalf("ReadAllChunks failed: %v", err)
	}

	if len(allChunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(allChunks))
	}

	for i, chunk := range allChunks {
		if !bytes.Equal(chunk, chunks[i]) {
			t.Errorf("chunk %d data mismatch", i)
		}
	}
}

func TestContainerExtractChunkVerifiesHash(t *testing.T) {
	data := bytes.Repeat([]byte("valid data"), 100)

	builder := NewContainerBuilder()
	builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	_, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt a byte in the chunk data region.
	raw := buffer.Bytes()
	dataStart := containerHeaderSize + chunkIndexEntrySize
	if dataStart < len(raw) {
		raw[dataStart] ^= 0xFF // flip bits
	}

	reader := bytes.NewReader(raw)
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		t.Fatal(err)
	}

	_, err = cr.ExtractChunk(reader, 0)
	if err == nil {
		t.Error("ExtractChunk should fail when chunk data is corrupted")
	}
}

func TestContainerReadChunkDataOutOfRange(t *testing.T) {
	data := []byte("chunk")
	builder := NewContainerBuilder()
	builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	builder.Flush(&buffer)

	reader := bytes.NewReader(buffer.Bytes())
	cr, _ := ReadContainerIndex(reader)

	_, err := cr.ReadChunkData(reader, -1)
	if err == nil {
		t.Error("ReadChunkData(-1) should fail")
	}

	_, err = cr.ReadChunkData(reader, 1)
	if err == nil {
		t.Error("ReadChunkData(1) should fail for single-chunk container")
	}
}

func TestContainerIndexEntryCompression(t *testing.T) {
	// Verify index entries record the correct compression tags.
	data := bytes.Repeat([]byte("compress me "), 1024) // 12KB, compressible

	compressed, err := CompressChunk(data, CompressionZstd)
	if err != nil {
		t.Fatalf("CompressChunk failed: %v", err)
	}

	builder := NewContainerBuilder()
	builder.AddChunk(HashChunk(data), compressed, CompressionZstd, uint32(len(data)))

	var buffer bytes.Buffer
	builder.Flush(&buffer)

	reader := bytes.NewReader(buffer.Bytes())
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		t.Fatal(err)
	}

	entry := cr.Index[0]
	if entry.Compression != CompressionZstd {
		t.Errorf("compression tag = %s, want zstd", entry.Compression)
	}
	if entry.UncompressedSize != uint32(len(data)) {
		t.Errorf("uncompressed size = %d, want %d", entry.UncompressedSize, len(data))
	}
	if entry.CompressedSize != uint32(len(compressed)) {
		t.Errorf("compressed size = %d, want %d", entry.CompressedSize, len(compressed))
	}
}

func TestContainerUnsupportedCompressionTag(t *testing.T) {
	// Build a valid container, then patch a chunk's compression tag
	// to an unsupported value.
	data := []byte("some data for the chunk")
	builder := NewContainerBuilder()
	builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	builder.Flush(&buffer)

	// The compression tag byte is at offset: header(12) + hash(32) = 44.
	raw := buffer.Bytes()
	raw[44] = 99 // unsupported tag

	_, err := ReadContainerIndex(bytes.NewReader(raw))
	if err == nil {
		t.Error("ReadContainerIndex should reject unsupported compression tag")
	}
}

// Benchmarks for container I/O. Run with:
//
//	bazel run //lib/artifact:artifact_test -- \
//	    -test.bench=BenchmarkContainer -test.benchmem -test.count=10 -test.run='^$'

func BenchmarkContainerWrite(b *testing.B) {
	// Simulate writing a container with 1024 chunks of ~64KB each.
	chunkCount := 1024
	chunkSize := 64 * 1024

	// Pre-generate chunks and compress them.
	type preparedChunk struct {
		hash           Hash
		compressedData []byte
		tag            CompressionTag
		uncompressed   uint32
	}

	prepared := make([]preparedChunk, chunkCount)
	for i := range prepared {
		data := make([]byte, chunkSize)
		for j := range data {
			data[j] = byte((i + j) % 17) // compressible pattern
		}
		hash := HashChunk(data)
		compressed, err := CompressChunk(data, CompressionLZ4)
		if err != nil {
			b.Fatal(err)
		}
		prepared[i] = preparedChunk{
			hash:           hash,
			compressedData: compressed,
			tag:            CompressionLZ4,
			uncompressed:   uint32(chunkSize),
		}
	}

	builder := NewContainerBuilder()
	b.SetBytes(int64(chunkCount * chunkSize))
	b.ReportAllocs()

	for b.Loop() {
		for _, pc := range prepared {
			builder.AddChunk(pc.hash, pc.compressedData, pc.tag, pc.uncompressed)
		}
		builder.Flush(io.Discard)
	}
}

func BenchmarkContainerRead(b *testing.B) {
	// Build a container, then benchmark reading it.
	chunkCount := 1024
	chunkSize := 64 * 1024

	builder := NewContainerBuilder()
	for i := range chunkCount {
		data := make([]byte, chunkSize)
		for j := range data {
			data[j] = byte((i + j) % 17)
		}
		builder.AddChunk(HashChunk(data), data, CompressionNone, uint32(chunkSize))
	}

	var buffer bytes.Buffer
	builder.Flush(&buffer)
	containerBytes := buffer.Bytes()

	b.SetBytes(int64(len(containerBytes)))
	b.ReportAllocs()

	for b.Loop() {
		reader := bytes.NewReader(containerBytes)
		cr, err := ReadContainerIndex(reader)
		if err != nil {
			b.Fatal(err)
		}
		_ = cr
	}
}

func BenchmarkContainerExtractChunk(b *testing.B) {
	// Build a container and benchmark extracting a single chunk.
	data := bytes.Repeat([]byte("benchmark chunk data "), 3277) // ~64KB
	compressed, err := CompressChunk(data, CompressionLZ4)
	if err != nil {
		b.Fatal(err)
	}

	builder := NewContainerBuilder()
	for range 100 {
		builder.AddChunk(HashChunk(data), compressed, CompressionLZ4, uint32(len(data)))
	}

	var buffer bytes.Buffer
	builder.Flush(&buffer)
	containerBytes := buffer.Bytes()

	reader := bytes.NewReader(containerBytes)
	cr, err := ReadContainerIndex(reader)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.Run(fmt.Sprintf("chunks=%d", len(cr.Index)), func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			// Extract chunk from the middle.
			_, err := cr.ExtractChunk(reader, 50)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
