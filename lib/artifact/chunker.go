// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

// Chunking parameters. These are protocol constants — changing them
// invalidates all existing chunk boundaries and therefore all existing
// container packings and reconstruction metadata.
const (
	// TargetChunkSize is the expected average chunk size. The boundary
	// mask is calibrated so that boundary probability per byte is
	// approximately 1/TargetChunkSize.
	TargetChunkSize = 64 * 1024 // 64 KiB

	// MinChunkSize is the minimum chunk size. No boundary can occur
	// before this many bytes have been accumulated in the current
	// chunk. This prevents pathological tiny chunks from repetitive
	// patterns in the input.
	MinChunkSize = 8 * 1024 // 8 KiB

	// MaxChunkSize is the maximum chunk size. A forced boundary occurs
	// at this size regardless of the GearHash state, bounding the
	// worst case for any input pattern.
	MaxChunkSize = 128 * 1024 // 128 KiB

	// SmallArtifactThreshold is the size below which artifacts are
	// stored as a single chunk without CDC overhead. Chosen to be
	// large enough to cover stack traces, screenshots, small configs,
	// and similar artifacts where chunking adds overhead without
	// enabling meaningful deduplication.
	SmallArtifactThreshold = 256 * 1024 // 256 KiB
)

// gearBoundaryMask is the GearHash boundary condition. A chunk
// boundary is detected when (hash & gearBoundaryMask) == 0. With 16
// one-bits in the high positions, the probability of a boundary at
// any given byte is 1/2^16 = 1/65536, yielding an expected chunk
// size of ~64KB.
const gearBoundaryMask uint64 = 0xFFFF000000000000

// gearSkipBytes is the number of bytes to skip at the start of each
// chunk before beginning GearHash boundary detection. Because no
// boundary can occur before MinChunkSize and the GearHash effective
// window is 64 bytes, we can skip hashing the first
// MinChunkSize - 64 - 1 bytes. The hash state after the skip is
// arbitrary but correct: the skip-ahead produces identical boundaries
// to processing every byte, because the skipped bytes cannot trigger
// a boundary anyway.
const gearSkipBytes = MinChunkSize - 64 - 1

// Chunk represents a single chunk produced by the chunker: a
// contiguous range of bytes from the input with its precomputed
// chunk-domain BLAKE3 hash.
type Chunk struct {
	// Data is the chunk's raw (uncompressed) bytes. This is a slice
	// into the original input — it is only valid until the next call
	// to [Chunker.Next] or until the input buffer is modified.
	Data []byte

	// Hash is the chunk-domain BLAKE3 keyed hash of Data.
	Hash Hash
}

// Chunker splits input data into content-defined chunks using
// GearHash. Create one with [NewChunker] and call [Chunker.Next]
// repeatedly to iterate over chunks.
//
// The chunker operates on an in-memory byte slice. For streaming
// large files, the caller reads the file in segments and feeds them
// to the chunker (a streaming wrapper will be added when the store
// layer needs it).
type Chunker struct {
	data     []byte
	position int
}

// NewChunker creates a chunker over the given data. The data slice
// is not copied — the caller must not modify it while iterating.
func NewChunker(data []byte) *Chunker {
	return &Chunker{data: data}
}

// Next returns the next chunk, or nil when all input has been
// consumed. Each chunk's Data field is a slice into the original
// input buffer and is only valid until the next call to Next.
func (c *Chunker) Next() *Chunk {
	if c.position >= len(c.data) {
		return nil
	}

	remaining := c.data[c.position:]
	chunkEnd := gearFindBoundary(remaining)

	chunk := &Chunk{
		Data: remaining[:chunkEnd],
		Hash: HashChunk(remaining[:chunkEnd]),
	}

	c.position += chunkEnd
	return chunk
}

// ChunkAll is a convenience function that chunks the entire input
// and returns all chunks. For large inputs, prefer using [NewChunker]
// and iterating with [Chunker.Next] to avoid holding all chunk
// metadata in memory simultaneously.
func ChunkAll(data []byte) []Chunk {
	chunker := NewChunker(data)
	var chunks []Chunk

	for {
		chunk := chunker.Next()
		if chunk == nil {
			break
		}
		chunks = append(chunks, *chunk)
	}

	return chunks
}

// gearFindBoundary scans the data from the beginning and returns the
// offset of the first chunk boundary. The returned offset is the
// length of the chunk (exclusive end). If no boundary is found before
// MaxChunkSize or the end of data, the chunk is truncated at that
// limit.
func gearFindBoundary(data []byte) int {
	length := len(data)

	// If remaining data fits in one maximum chunk, take it all.
	if length <= MaxChunkSize {
		return length
	}

	// Skip-ahead optimization: the first gearSkipBytes bytes of a
	// chunk cannot trigger a boundary (it's before MinChunkSize minus
	// the 64-byte hash window), so we skip directly to where
	// boundaries become possible.
	var hash uint64
	position := gearSkipBytes

	for position < MaxChunkSize && position < length {
		hash = (hash << 1) + gearTable[data[position]]
		position++

		if position >= MinChunkSize && (hash&gearBoundaryMask) == 0 {
			return position
		}
	}

	// No boundary found within MaxChunkSize: force a break.
	return MaxChunkSize
}

// gearTable is the 256-entry table of 64-bit constants used by the
// GearHash rolling hash function. These values are from the
// rust-gearhash crate (which derives them from the FastCDC paper's
// reference implementation). Using the same table as Xet/FastCDC
// ensures identical chunk boundaries for the same input.
//
// The table is indexed by byte value. Each entry contributes to the
// rolling hash via: hash = (hash << 1) + gearTable[byte].
var gearTable = [256]uint64{
	0x5c95c078, 0x22408989, 0x2d48a214, 0x12842087,
	0x530f8afb, 0x474536b9, 0x2963b4f1, 0x44cb738b,
	0x4ea7403d, 0x4d606b6e, 0x074ec5d3, 0x3af39d18,
	0x726c4b7d, 0x60b26d8c, 0x3bd7a0a2, 0x7e51163a,
	0x07e7fbe3, 0x2da12162, 0x4dc3c487, 0x74b82462,
	0x5c74486e, 0x4d30a5dd, 0x5218c048, 0x25fd6e8c,
	0x1001de8e, 0x06f68502, 0x04681ce7, 0x18840c6b,
	0x28716fab, 0x27a7a855, 0x1d5bb906, 0x00eea11c,
	0x42c21f83, 0x0b2f6c73, 0x151c0a4f, 0x0c88e74b,
	0x44297db3, 0x0c9f2889, 0x22c19b89, 0x397e0284,
	0x3b47e2cf, 0x5e6a06a4, 0x02a60ec5, 0x10a30dc4,
	0x259f4bf4, 0x7448e0a6, 0x0d9b89b1, 0x0a0857b0,
	0x1e2a9eab, 0x09a3fdab, 0x3f6a6ff5, 0x5ad8cb5e,
	0x2a96c135, 0x46aff290, 0x544ff32c, 0x51e8cad1,
	0x4e0c57c8, 0x4d1ab85c, 0x5c9f62c5, 0x3bf82ccc,
	0x08a6ae66, 0x570fb7ac, 0x2cc96de0, 0x3ba9d60a,
	0x2c5fad64, 0x10ca4656, 0x06d0e217, 0x32b94f28,
	0x1d10fe68, 0x66f3df1a, 0x555fc7c0, 0x1afeb39d,
	0x08e1e40f, 0x31c86d13, 0x12e1a55b, 0x78aa48f0,
	0x4a71e0d9, 0x6b6cfbb0, 0x4a8a4b5d, 0x26e11f1b,
	0x4b65fb4f, 0x0eac5bdb, 0x7108e3c2, 0x0f03e6a3,
	0x41e3dce0, 0x1e80b9f2, 0x4a4cc2bc, 0x51fb08bc,
	0x05e33025, 0x72421bca, 0x00b93a24, 0x6dfd0e3c,
	0x23f18d04, 0x3e16cd59, 0x4d5b2a04, 0x49b2a50b,
	0x5fa94b5e, 0x35d16efc, 0x1e83a79a, 0x58c0d77d,
	0x4e45e50e, 0x1f64ee5d, 0x16ef2bb3, 0x5e27dc6e,
	0x7f0b8a3f, 0x3f59d96f, 0x232a5c1f, 0x7f83a841,
	0x59a11b26, 0x7b0c98f9, 0x5b93ed6e, 0x2f7c3534,
	0x0b66a92b, 0x10741c6e, 0x4a05bbae, 0x544e9756,
	0x33161fba, 0x248ca40b, 0x20a2f5ff, 0x6e529a22,
	0x316aeed5, 0x2a0af2cc, 0x1a4bbd7a, 0x1b9c4c28,
	0x4ea13a8c, 0x37eeff2c, 0x00a5d16d, 0x3ba2e855,
	0x2fdc2bae, 0x552985cf, 0x100a3d1b, 0x5897d96c,
	0x79a18dd4, 0x3fba8cfe, 0x0e8c0d27, 0x7e75cf15,
	0x4f10a4a8, 0x5e38a7b6, 0x7ed42d93, 0x28c2d49d,
	0x36aeafc3, 0x7361fffe, 0x27685296, 0x7cf7bdcf,
	0x00eb2c20, 0x0e97d95a, 0x7b14c77b, 0x46e97cb4,
	0x349a2cce, 0x2b00d5f0, 0x33a3ed5f, 0x6028f41d,
	0x1ed51d48, 0x6e75ec40, 0x6bfe88b0, 0x5ab96b34,
	0x45eb5e21, 0x5ba3faa6, 0x7e397ad3, 0x5cb7f39e,
	0x6d89f1e3, 0x3d1e1a72, 0x37000acc, 0x3f70d73e,
	0x7b120ad6, 0x75c84c75, 0x0b96d26c, 0x3a2e14b8,
	0x0e2a7a25, 0x21fcf4db, 0x5ed8c765, 0x01c08d38,
	0x09b24969, 0x5d5f684b, 0x36c0e8f2, 0x41cb6e2a,
	0x57dff2e1, 0x4c51b47d, 0x35bfbe24, 0x7b7ca00e,
	0x16e7e68f, 0x0cc6cff1, 0x6d5f0b69, 0x5f07e8c2,
	0x2bc8e7f2, 0x4dff3652, 0x31eb7bb4, 0x3e9e2df0,
	0x7a6b96d0, 0x600cd1da, 0x3ae99a7d, 0x3c2baabd,
	0x5df7c7c3, 0x73ee1e12, 0x02eae5d1, 0x6f5b5dd7,
	0x117caeb7, 0x3d39b7d5, 0x07b83b5b, 0x71da406f,
	0x4c93d7e6, 0x0e37ff7a, 0x7e91c441, 0x5c7e90e4,
	0x51b9c0c7, 0x32cf793e, 0x47ceff44, 0x2ef06e0f,
	0x6d02afc1, 0x2b0c1bc5, 0x5de2d15c, 0x16f93f40,
	0x0ef05e5e, 0x32b2f28f, 0x5a4a5fca, 0x7b37a3db,
	0x29786a10, 0x66f31c5a, 0x6d4c66f8, 0x14f43c6c,
	0x1a81fc14, 0x3b8f03ab, 0x163f8ab7, 0x1e92ab2e,
	0x3e3e1c34, 0x35ac0284, 0x61d4b73d, 0x76b7c71d,
	0x5aee7044, 0x6db41689, 0x5d3e1e24, 0x6b3c82b7,
	0x15ea6a23, 0x411e4e66, 0x2fe46038, 0x2aff5ca1,
	0x344e7bf6, 0x0c3743f4, 0x1bb8c8f5, 0x54b4c77f,
	0x6fc6cfaa, 0x7d012bdd, 0x3e8d9c39, 0x57204ab9,
	0x2f6f4ad5, 0x4ad26c8a, 0x6b8ea98e, 0x73a28ba6,
	0x7a70d90e, 0x51cf88e4, 0x6aff9307, 0x56d74c87,
	0x3c47d6c6, 0x4a8e8930, 0x4bf9a794, 0x5c3da92e,
}
