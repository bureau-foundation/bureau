// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestDomainKeysAreDistinct(t *testing.T) {
	// Domain separation means the same input produces different hashes
	// in different domains.
	input := []byte("the same input bytes for all three domains")

	chunkHash := HashChunk(input)
	fileHash := keyedHash(fileDomainKey, input)
	containerHash := keyedHash(containerDomainKey, input)

	if chunkHash == fileHash {
		t.Error("chunk and file domain produced the same hash for identical input")
	}
	if chunkHash == containerHash {
		t.Error("chunk and container domain produced the same hash for identical input")
	}
	if fileHash == containerHash {
		t.Error("file and container domain produced the same hash for identical input")
	}
}

func TestDomainKeysAreDeterministic(t *testing.T) {
	input := []byte("deterministic input")

	hash1 := HashChunk(input)
	hash2 := HashChunk(input)
	if hash1 != hash2 {
		t.Error("HashChunk produced different results for the same input")
	}

	hash3 := HashFile(hash1)
	hash4 := HashFile(hash1)
	if hash3 != hash4 {
		t.Error("HashFile produced different results for the same input")
	}

	hash5 := HashContainer(hash1)
	hash6 := HashContainer(hash1)
	if hash5 != hash6 {
		t.Error("HashContainer produced different results for the same input")
	}
}

func TestDomainKeysDoNotOverlap(t *testing.T) {
	// Verify the key constants are correctly zero-padded and don't
	// share the same bytes (a copy-paste error would be catastrophic).
	keys := []struct {
		name string
		key  domainKey
	}{
		{"chunk", chunkDomainKey},
		{"container", containerDomainKey},
		{"file", fileDomainKey},
	}

	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[i].key == keys[j].key {
				t.Errorf("domain keys %s and %s are identical", keys[i].name, keys[j].name)
			}
		}
	}

	// Verify each key contains its domain name as a readable prefix.
	for _, key := range keys {
		prefix := "bureau.artifact."
		keyString := string(key.key[:len(prefix)])
		if keyString != prefix {
			t.Errorf("domain key %s does not start with %q, got %q", key.name, prefix, keyString)
		}
	}
}

func TestHashChunkNonEmpty(t *testing.T) {
	hash := HashChunk([]byte("some chunk data"))
	var zero Hash
	if hash == zero {
		t.Error("HashChunk returned zero hash for non-empty input")
	}
}

func TestHashChunkEmptyInput(t *testing.T) {
	// Empty input should still produce a valid (non-zero) keyed hash.
	hash := HashChunk(nil)
	var zero Hash
	if hash == zero {
		t.Error("HashChunk returned zero hash for nil input")
	}

	hash2 := HashChunk([]byte{})
	if hash2 == zero {
		t.Error("HashChunk returned zero hash for empty slice")
	}

	// nil and empty slice should produce the same hash.
	if hash != hash2 {
		t.Error("HashChunk(nil) != HashChunk([]byte{})")
	}
}

func TestHashFileFromChunkHash(t *testing.T) {
	// Single-chunk artifact: file hash wraps the chunk hash in the
	// file domain. It must NOT be equal to the chunk hash.
	chunkHash := HashChunk([]byte("small file content"))
	fileHash := HashFile(chunkHash)

	if fileHash == chunkHash {
		t.Error("file-domain hash equals chunk-domain hash; domain separation is broken")
	}

	var zero Hash
	if fileHash == zero {
		t.Error("HashFile returned zero hash")
	}
}

func TestHashContainerFromMerkleRoot(t *testing.T) {
	hashes := []Hash{
		HashChunk([]byte("chunk 0")),
		HashChunk([]byte("chunk 1")),
		HashChunk([]byte("chunk 2")),
	}
	root := MerkleRoot(chunkDomainKey, hashes)
	containerHash := HashContainer(root)

	var zero Hash
	if containerHash == zero {
		t.Error("HashContainer returned zero hash")
	}
	if containerHash == root {
		t.Error("container-domain hash equals merkle root; domain separation is broken")
	}
}

func TestMerkleRootSingleHash(t *testing.T) {
	hash := HashChunk([]byte("only chunk"))
	root := MerkleRoot(chunkDomainKey, []Hash{hash})

	// Single-element Merkle tree: root is the element itself.
	if root != hash {
		t.Errorf("MerkleRoot of single hash: got %s, want %s",
			FormatHash(root), FormatHash(hash))
	}
}

func TestMerkleRootTwoHashes(t *testing.T) {
	h0 := HashChunk([]byte("chunk 0"))
	h1 := HashChunk([]byte("chunk 1"))

	root := MerkleRoot(chunkDomainKey, []Hash{h0, h1})

	// Root should be the hash of the concatenation of the two hashes.
	expected := hashPair(chunkDomainKey, h0, h1)
	if root != expected {
		t.Errorf("MerkleRoot of two hashes: got %s, want %s",
			FormatHash(root), FormatHash(expected))
	}
}

func TestMerkleRootOddCount(t *testing.T) {
	h0 := HashChunk([]byte("chunk 0"))
	h1 := HashChunk([]byte("chunk 1"))
	h2 := HashChunk([]byte("chunk 2"))

	root3 := MerkleRoot(chunkDomainKey, []Hash{h0, h1, h2})

	// With 3 hashes: pair(h0,h1) at level 1, h2 promoted.
	// Then pair(pair(h0,h1), h2) at level 0.
	level1Left := hashPair(chunkDomainKey, h0, h1)
	expected := hashPair(chunkDomainKey, level1Left, h2)
	if root3 != expected {
		t.Errorf("MerkleRoot of 3 hashes: got %s, want %s",
			FormatHash(root3), FormatHash(expected))
	}
}

func TestMerkleRootFourHashes(t *testing.T) {
	hashes := make([]Hash, 4)
	for i := range hashes {
		hashes[i] = HashChunk([]byte(fmt.Sprintf("chunk %d", i)))
	}

	root := MerkleRoot(chunkDomainKey, hashes)

	// Full binary tree: pair(pair(h0,h1), pair(h2,h3)).
	left := hashPair(chunkDomainKey, hashes[0], hashes[1])
	right := hashPair(chunkDomainKey, hashes[2], hashes[3])
	expected := hashPair(chunkDomainKey, left, right)
	if root != expected {
		t.Errorf("MerkleRoot of 4 hashes: got %s, want %s",
			FormatHash(root), FormatHash(expected))
	}
}

func TestMerkleRootDeterministic(t *testing.T) {
	hashes := make([]Hash, 17)
	for i := range hashes {
		hashes[i] = HashChunk([]byte(fmt.Sprintf("chunk %d", i)))
	}

	root1 := MerkleRoot(chunkDomainKey, hashes)
	root2 := MerkleRoot(chunkDomainKey, hashes)
	if root1 != root2 {
		t.Error("MerkleRoot is not deterministic")
	}
}

func TestMerkleRootOrderMatters(t *testing.T) {
	h0 := HashChunk([]byte("chunk A"))
	h1 := HashChunk([]byte("chunk B"))

	forward := MerkleRoot(chunkDomainKey, []Hash{h0, h1})
	reverse := MerkleRoot(chunkDomainKey, []Hash{h1, h0})

	if forward == reverse {
		t.Error("MerkleRoot is order-independent; tree structure is broken")
	}
}

func TestMerkleRootDoesNotMutateInput(t *testing.T) {
	hashes := []Hash{
		HashChunk([]byte("a")),
		HashChunk([]byte("b")),
		HashChunk([]byte("c")),
	}

	// Save copies.
	saved := make([]Hash, len(hashes))
	copy(saved, hashes)

	MerkleRoot(chunkDomainKey, hashes)

	for i := range hashes {
		if hashes[i] != saved[i] {
			t.Errorf("MerkleRoot mutated input slice at index %d", i)
		}
	}
}

func TestMerkleRootPanicsOnEmpty(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("MerkleRoot did not panic on empty input")
		}
	}()
	MerkleRoot(chunkDomainKey, nil)
}

func TestMerkleRootDomainSeparation(t *testing.T) {
	hashes := []Hash{
		HashChunk([]byte("chunk 0")),
		HashChunk([]byte("chunk 1")),
	}

	rootChunk := MerkleRoot(chunkDomainKey, hashes)
	rootContainer := MerkleRoot(containerDomainKey, hashes)
	rootFile := MerkleRoot(fileDomainKey, hashes)

	if rootChunk == rootContainer {
		t.Error("Merkle root with chunk key equals container key")
	}
	if rootChunk == rootFile {
		t.Error("Merkle root with chunk key equals file key")
	}
	if rootContainer == rootFile {
		t.Error("Merkle root with container key equals file key")
	}
}

func TestFormatHash(t *testing.T) {
	hash := HashChunk([]byte("test"))
	formatted := FormatHash(hash)

	if len(formatted) != 64 {
		t.Errorf("FormatHash length = %d, want 64", len(formatted))
	}

	// Verify it's valid hex.
	_, err := hex.DecodeString(formatted)
	if err != nil {
		t.Errorf("FormatHash produced invalid hex: %v", err)
	}
}

func TestParseHash(t *testing.T) {
	original := HashChunk([]byte("roundtrip test"))
	formatted := FormatHash(original)

	parsed, err := ParseHash(formatted)
	if err != nil {
		t.Fatalf("ParseHash failed: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseHash roundtrip failed: got %s, want %s",
			FormatHash(parsed), FormatHash(original))
	}
}

func TestParseHashErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too_short", "abcdef"},
		{"too_long", strings.Repeat("ab", 33)},
		{"invalid_hex", strings.Repeat("zz", 32)},
		{"odd_length", strings.Repeat("a", 63)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseHash(tt.input)
			if err == nil {
				t.Errorf("ParseHash(%q) succeeded, want error", tt.input)
			}
		})
	}
}

func TestFormatRef(t *testing.T) {
	hash := HashChunk([]byte("test"))
	fileHash := HashFile(hash)
	ref := FormatRef(fileHash)

	if !strings.HasPrefix(ref, "art-") {
		t.Errorf("FormatRef does not start with art-: %q", ref)
	}

	// "art-" + 12 hex chars = 16 chars total.
	if len(ref) != 16 {
		t.Errorf("FormatRef length = %d, want 16", len(ref))
	}

	// Verify the hex portion matches the hash prefix.
	hexPart := ref[4:]
	hashHex := FormatHash(fileHash)
	if !strings.HasPrefix(hashHex, hexPart) {
		t.Errorf("FormatRef hex %q is not a prefix of full hash %q", hexPart, hashHex)
	}
}

func TestEndToEndSingleChunkArtifact(t *testing.T) {
	// Simulate the full hash chain for a small (single-chunk) artifact.
	content := []byte("a small file that fits in one chunk")

	chunkHash := HashChunk(content)
	merkleRoot := MerkleRoot(chunkDomainKey, []Hash{chunkHash})
	fileHash := HashFile(merkleRoot)
	ref := FormatRef(fileHash)

	// Since MerkleRoot of a single hash is the hash itself, the file
	// hash should equal HashFile(chunkHash).
	directFileHash := HashFile(chunkHash)
	if fileHash != directFileHash {
		t.Errorf("single-chunk: MerkleRoot path differs from direct path")
	}

	// File hash must differ from chunk hash (domain separation).
	if fileHash == chunkHash {
		t.Error("file hash equals chunk hash for single-chunk artifact")
	}

	if !strings.HasPrefix(ref, "art-") {
		t.Errorf("ref does not start with art-: %q", ref)
	}
}

func TestEndToEndMultiChunkArtifact(t *testing.T) {
	// Simulate the full hash chain for a multi-chunk artifact.
	chunks := [][]byte{
		[]byte("first chunk of a larger file"),
		[]byte("second chunk with different content"),
		[]byte("third and final chunk"),
	}

	chunkHashes := make([]Hash, len(chunks))
	for i, chunk := range chunks {
		chunkHashes[i] = HashChunk(chunk)
	}

	merkleRoot := MerkleRoot(chunkDomainKey, chunkHashes)
	fileHash := HashFile(merkleRoot)

	// Container hash for a container holding all three chunks.
	containerMerkle := MerkleRoot(chunkDomainKey, chunkHashes)
	containerHash := HashContainer(containerMerkle)

	// All three hashes (merkle root, file, container) must be distinct.
	if fileHash == merkleRoot {
		t.Error("file hash equals merkle root")
	}
	if containerHash == merkleRoot {
		t.Error("container hash equals merkle root")
	}
	if fileHash == containerHash {
		t.Error("file hash equals container hash")
	}

	var zero Hash
	if fileHash == zero || containerHash == zero || merkleRoot == zero {
		t.Error("one of the hashes is zero")
	}
}

// Benchmarks for the hashing layer. Run with:
//
//	bazel run //lib/artifact:artifact_test -- \
//	    -test.bench=BenchmarkHash -test.benchmem -test.count=10 -test.run='^$'

func BenchmarkHashChunk(b *testing.B) {
	sizes := []int{
		64,               // single BLAKE3 block
		4 * 1024,         // 4KB: L1 cache
		8 * 1024,         // 8KB: minimum chunk size
		64 * 1024,        // 64KB: target chunk size
		128 * 1024,       // 128KB: maximum chunk size
		256 * 1024,       // 256KB: small artifact threshold
		1024 * 1024,      // 1MB
		64 * 1024 * 1024, // 64MB: container scale
	}

	for _, size := range sizes {
		input := make([]byte, size)
		for i := range input {
			input[i] = byte(i)
		}

		b.Run(fmt.Sprintf("size=%s", formatByteSize(size)), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()

			for b.Loop() {
				HashChunk(input)
			}
		})
	}
}

func BenchmarkMerkleRoot(b *testing.B) {
	counts := []int{1, 2, 4, 8, 16, 64, 256, 1024}

	for _, count := range counts {
		hashes := make([]Hash, count)
		for i := range hashes {
			hashes[i] = HashChunk([]byte(fmt.Sprintf("chunk %d", i)))
		}

		b.Run(fmt.Sprintf("chunks=%d", count), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				MerkleRoot(chunkDomainKey, hashes)
			}
		})
	}
}

func BenchmarkHashFile(b *testing.B) {
	input := HashChunk([]byte("a chunk hash to wrap in file domain"))

	b.ReportAllocs()
	for b.Loop() {
		HashFile(input)
	}
}

func formatByteSize(bytes int) string {
	switch {
	case bytes >= 1024*1024:
		return fmt.Sprintf("%dMB", bytes/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%dKB", bytes/1024)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
