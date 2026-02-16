// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Container format constants.
const (
	// containerMagic is the 8-byte header prefix: "BUREAU" + version
	// byte + reserved byte. Version 1 is the initial format.
	containerVersion = 1

	// containerHeaderSize is the fixed header: 8-byte magic + 4-byte
	// chunk count.
	containerHeaderSize = 12

	// chunkIndexEntrySize is the size of each chunk index entry:
	// 32-byte hash + 1-byte compression tag + 3-byte reserved
	// + 4-byte compressed size + 4-byte uncompressed size
	// + 4-byte reserved. The reserved bytes ensure 4-byte alignment
	// for the uint32 fields and an 8-byte stride for the entry.
	chunkIndexEntrySize = 48
)

// containerMagic is the 8-byte container file signature.
var containerMagic = [8]byte{'B', 'U', 'R', 'E', 'A', 'U', containerVersion, 0}

// ChunkIndexEntry describes a single chunk within a container.
type ChunkIndexEntry struct {
	// Hash is the chunk-domain BLAKE3 keyed hash of the uncompressed
	// chunk data.
	Hash Hash

	// Compression is the algorithm used to compress this chunk.
	Compression CompressionTag

	// CompressedSize is the byte length of the compressed chunk data
	// stored in the container.
	CompressedSize uint32

	// UncompressedSize is the original byte length before compression.
	UncompressedSize uint32
}

// ContainerBuilder accumulates compressed chunks and writes them as a
// container. The container format has the chunk index before the data,
// so the builder buffers all chunk data in memory until [Flush] is
// called.
//
// Typical usage:
//
//	builder := NewContainerBuilder()
//	builder.AddChunk(hash, compressedData, tag, uncompressedSize)
//	// ... add more chunks ...
//	containerHash, err := builder.Flush(writer)
type ContainerBuilder struct {
	index []ChunkIndexEntry
	data  [][]byte
}

// NewContainerBuilder creates a builder for a new container.
func NewContainerBuilder() *ContainerBuilder {
	return &ContainerBuilder{}
}

// AddChunk appends a compressed chunk to the container being built.
// The chunkHash must be the chunk-domain BLAKE3 hash of the
// UNCOMPRESSED data. The compressedData is the chunk after
// compression (or the raw data if tag is CompressionNone).
func (b *ContainerBuilder) AddChunk(chunkHash Hash, compressedData []byte, tag CompressionTag, uncompressedSize uint32) {
	b.index = append(b.index, ChunkIndexEntry{
		Hash:             chunkHash,
		Compression:      tag,
		CompressedSize:   uint32(len(compressedData)),
		UncompressedSize: uncompressedSize,
	})
	b.data = append(b.data, compressedData)
}

// ChunkCount returns the number of chunks added so far.
func (b *ContainerBuilder) ChunkCount() int {
	return len(b.index)
}

// DataSize returns the total compressed data size accumulated so far.
func (b *ContainerBuilder) DataSize() int {
	var total int
	for _, d := range b.data {
		total += len(d)
	}
	return total
}

// Flush writes the complete container to w and returns the container
// hash (container-domain BLAKE3 hash of the Merkle root over chunk
// hashes). The builder is reset after flushing.
//
// Returns an error if the builder is empty (no chunks added).
func (b *ContainerBuilder) Flush(w io.Writer) (Hash, error) {
	if len(b.index) == 0 {
		return Hash{}, fmt.Errorf("cannot flush empty container")
	}

	chunkCount := uint32(len(b.index))

	// Write header.
	if _, err := w.Write(containerMagic[:]); err != nil {
		return Hash{}, fmt.Errorf("writing container magic: %w", err)
	}

	var countBytes [4]byte
	binary.LittleEndian.PutUint32(countBytes[:], chunkCount)
	if _, err := w.Write(countBytes[:]); err != nil {
		return Hash{}, fmt.Errorf("writing chunk count: %w", err)
	}

	// Write chunk index.
	chunkHashes := make([]Hash, chunkCount)
	for i, entry := range b.index {
		chunkHashes[i] = entry.Hash

		if _, err := w.Write(entry.Hash[:]); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d hash: %w", i, err)
		}

		if err := writeByte(w, byte(entry.Compression)); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d compression tag: %w", i, err)
		}

		// 3 reserved bytes after compression tag for 4-byte alignment.
		var reserved3 [3]byte
		if _, err := w.Write(reserved3[:]); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d reserved bytes: %w", i, err)
		}

		var sizeBytes [4]byte
		binary.LittleEndian.PutUint32(sizeBytes[:], entry.CompressedSize)
		if _, err := w.Write(sizeBytes[:]); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d compressed size: %w", i, err)
		}

		binary.LittleEndian.PutUint32(sizeBytes[:], entry.UncompressedSize)
		if _, err := w.Write(sizeBytes[:]); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d uncompressed size: %w", i, err)
		}

		// 4 reserved bytes for 8-byte entry stride.
		var reserved4 [4]byte
		if _, err := w.Write(reserved4[:]); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d trailing reserved bytes: %w", i, err)
		}
	}

	// Write chunk data.
	for i, d := range b.data {
		if _, err := w.Write(d); err != nil {
			return Hash{}, fmt.Errorf("writing chunk %d data: %w", i, err)
		}
	}

	// Compute container hash: Merkle root over chunk hashes, then
	// wrap in container domain.
	merkleRoot := MerkleRoot(chunkDomainKey, chunkHashes)
	containerHash := HashContainer(merkleRoot)

	// Reset the builder for reuse.
	b.index = b.index[:0]
	b.data = b.data[:0]

	return containerHash, nil
}

// ContainerReader reads chunks from a container. Create one with
// [ReadContainerIndex] and then extract individual chunks with
// [ReadChunkData].
type ContainerReader struct {
	// Index is the parsed chunk index from the container header.
	Index []ChunkIndexEntry

	// Hash is the container-domain hash (Merkle root over chunk
	// hashes, wrapped in container domain).
	Hash Hash

	// dataOffset is the byte offset where chunk data begins (after
	// header + index).
	dataOffset int64

	// chunkOffsets[i] is the byte offset of chunk i's compressed
	// data relative to dataOffset.
	chunkOffsets []int64
}

// ReadContainerIndex reads and validates the container header and
// chunk index from r. The reader must be positioned at the start of
// the container. After this call, the reader is positioned at the
// start of chunk data.
//
// The returned ContainerReader can extract individual chunks via
// [ContainerReader.ReadChunkData] if r implements [io.ReadSeeker],
// or sequentially via [ContainerReader.ReadAllChunks].
func ReadContainerIndex(r io.Reader) (*ContainerReader, error) {
	// Read and validate magic.
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("reading container magic: %w", err)
	}
	if magic != containerMagic {
		if magic[0] == 'B' && magic[1] == 'U' && magic[2] == 'R' &&
			magic[3] == 'E' && magic[4] == 'A' && magic[5] == 'U' {
			return nil, fmt.Errorf("container version %d is not supported (this code supports version %d)",
				magic[6], containerVersion)
		}
		return nil, fmt.Errorf("not a Bureau container (invalid magic bytes)")
	}

	// Read chunk count.
	var countBytes [4]byte
	if _, err := io.ReadFull(r, countBytes[:]); err != nil {
		return nil, fmt.Errorf("reading chunk count: %w", err)
	}
	chunkCount := binary.LittleEndian.Uint32(countBytes[:])

	if chunkCount == 0 {
		return nil, fmt.Errorf("container has zero chunks")
	}

	// Read chunk index.
	index := make([]ChunkIndexEntry, chunkCount)
	chunkHashes := make([]Hash, chunkCount)

	for i := uint32(0); i < chunkCount; i++ {
		var hash [32]byte
		if _, err := io.ReadFull(r, hash[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d hash: %w", i, err)
		}
		index[i].Hash = hash
		chunkHashes[i] = hash

		var tagByte [1]byte
		if _, err := io.ReadFull(r, tagByte[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d compression tag: %w", i, err)
		}
		tag := CompressionTag(tagByte[0])
		if tag > CompressionBG4LZ4 {
			return nil, fmt.Errorf("chunk %d has unsupported compression tag %d", i, tag)
		}
		index[i].Compression = tag

		// 3 reserved bytes after compression tag (alignment padding).
		var reserved3 [3]byte
		if _, err := io.ReadFull(r, reserved3[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d reserved bytes: %w", i, err)
		}
		if reserved3 != [3]byte{} {
			return nil, fmt.Errorf("chunk %d has non-zero reserved bytes after compression tag: %x", i, reserved3)
		}

		var sizeBytes [4]byte
		if _, err := io.ReadFull(r, sizeBytes[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d compressed size: %w", i, err)
		}
		index[i].CompressedSize = binary.LittleEndian.Uint32(sizeBytes[:])

		if _, err := io.ReadFull(r, sizeBytes[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d uncompressed size: %w", i, err)
		}
		index[i].UncompressedSize = binary.LittleEndian.Uint32(sizeBytes[:])

		// 4 trailing reserved bytes (entry stride padding).
		var reserved4 [4]byte
		if _, err := io.ReadFull(r, reserved4[:]); err != nil {
			return nil, fmt.Errorf("reading chunk %d trailing reserved bytes: %w", i, err)
		}
		if reserved4 != [4]byte{} {
			return nil, fmt.Errorf("chunk %d has non-zero trailing reserved bytes: %x", i, reserved4)
		}
	}

	// Compute chunk data offsets.
	dataOffset := int64(containerHeaderSize) + int64(chunkCount)*int64(chunkIndexEntrySize)
	chunkOffsets := make([]int64, chunkCount)
	var offset int64
	for i := range index {
		chunkOffsets[i] = offset
		offset += int64(index[i].CompressedSize)
	}

	// Compute container hash.
	merkleRoot := MerkleRoot(chunkDomainKey, chunkHashes)
	containerHash := HashContainer(merkleRoot)

	return &ContainerReader{
		Index:        index,
		Hash:         containerHash,
		dataOffset:   dataOffset,
		chunkOffsets: chunkOffsets,
	}, nil
}

// ReadChunkData reads a single chunk's compressed data from a
// seekable reader. The reader must support seeking to the chunk's
// position within the container.
func (cr *ContainerReader) ReadChunkData(rs io.ReadSeeker, chunkIndex int) ([]byte, error) {
	if chunkIndex < 0 || chunkIndex >= len(cr.Index) {
		return nil, fmt.Errorf("chunk index %d out of range [0, %d)", chunkIndex, len(cr.Index))
	}

	entry := cr.Index[chunkIndex]
	offset := cr.dataOffset + cr.chunkOffsets[chunkIndex]

	if _, err := rs.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to chunk %d at offset %d: %w", chunkIndex, offset, err)
	}

	data := make([]byte, entry.CompressedSize)
	if _, err := io.ReadFull(rs, data); err != nil {
		return nil, fmt.Errorf("reading chunk %d data (%d bytes): %w", chunkIndex, entry.CompressedSize, err)
	}

	return data, nil
}

// ReadAllChunks reads all chunk data sequentially from a reader.
// The reader must be positioned at the start of chunk data (i.e.,
// immediately after ReadContainerIndex). Returns compressed chunk
// data for each chunk.
func (cr *ContainerReader) ReadAllChunks(r io.Reader) ([][]byte, error) {
	result := make([][]byte, len(cr.Index))
	for i, entry := range cr.Index {
		data := make([]byte, entry.CompressedSize)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("reading chunk %d data: %w", i, err)
		}
		result[i] = data
	}
	return result, nil
}

// ExtractChunk reads, decompresses, and verifies a single chunk from
// a seekable container. Returns the uncompressed chunk data. Verifies
// the chunk hash matches the index entry.
func (cr *ContainerReader) ExtractChunk(rs io.ReadSeeker, chunkIndex int) ([]byte, error) {
	compressed, err := cr.ReadChunkData(rs, chunkIndex)
	if err != nil {
		return nil, err
	}

	entry := cr.Index[chunkIndex]
	decompressed, err := DecompressChunk(compressed, entry.Compression, int(entry.UncompressedSize))
	if err != nil {
		return nil, fmt.Errorf("decompressing chunk %d: %w", chunkIndex, err)
	}

	// Verify the hash.
	actualHash := HashChunk(decompressed)
	if actualHash != entry.Hash {
		return nil, fmt.Errorf("chunk %d hash mismatch: expected %s, got %s",
			chunkIndex, FormatHash(entry.Hash), FormatHash(actualHash))
	}

	return decompressed, nil
}

// TotalSize returns the total serialized size of the container in bytes
// (header + chunk index + all compressed chunk data). This is the number
// of bytes that were written to produce this container on disk.
func (cr *ContainerReader) TotalSize() int64 {
	headerAndIndex := int64(containerHeaderSize) + int64(len(cr.Index))*int64(chunkIndexEntrySize)
	var dataSize int64
	for _, entry := range cr.Index {
		dataSize += int64(entry.CompressedSize)
	}
	return headerAndIndex + dataSize
}

// writeByte writes a single byte to w.
func writeByte(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}
