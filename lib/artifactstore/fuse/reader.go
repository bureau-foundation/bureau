// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
)

// chunkEntry describes one chunk in reconstruction order, mapping a
// byte offset range to a specific location within a container.
type chunkEntry struct {
	// offset is the cumulative byte offset where this chunk starts
	// within the reconstructed artifact.
	offset int64

	// size is the uncompressed size of this chunk in bytes.
	size uint32

	// container is the container-domain hash identifying which
	// container file holds this chunk's compressed data.
	container artifactstore.Hash

	// indexInContainer is the zero-based position of this chunk
	// within the container's chunk index.
	indexInContainer int
}

// containerProvider abstracts how container bytes are obtained,
// enabling testing without a real Store or Cache.
type containerProvider interface {
	// containerBytes returns the raw bytes of a container.
	containerBytes(hash artifactstore.Hash) ([]byte, error)
}

// storeProvider implements containerProvider using a Store and
// optional Cache.
type storeProvider struct {
	store *artifactstore.Store
	cache *artifactstore.Cache
}

func (p *storeProvider) containerBytes(hash artifactstore.Hash) ([]byte, error) {
	// Check cache first.
	if p.cache != nil {
		data, err := p.cache.Get(hash)
		if err == nil {
			return data, nil
		}
	}

	// Cache miss: read from disk.
	path := p.store.ContainerPath(hash)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading container %s: %w", artifactstore.FormatHash(hash), err)
	}

	// Populate cache for future reads.
	if p.cache != nil {
		// Ignore cache write errors — the read still succeeds.
		_ = p.cache.Put(hash, data)
	}

	return data, nil
}

// buildChunkTable constructs a sorted chunk table from a
// reconstruction record by reading all referenced container indexes.
// This is the expensive part of open() — it reads container headers
// (a few KB each) but not chunk data.
func buildChunkTable(record *artifactstore.ReconstructionRecord, provider containerProvider) ([]chunkEntry, error) {
	// Pre-allocate for the total number of chunks.
	chunks := make([]chunkEntry, 0, record.ChunkCount)
	var cumulativeOffset int64

	for segmentIndex, segment := range record.Segments {
		containerData, err := provider.containerBytes(segment.Container)
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", segmentIndex, err)
		}

		cr, err := artifactstore.ReadContainerIndex(bytes.NewReader(containerData))
		if err != nil {
			return nil, fmt.Errorf("segment %d: reading container index for %s: %w",
				segmentIndex, artifactstore.FormatHash(segment.Container), err)
		}

		for i := 0; i < segment.ChunkCount; i++ {
			chunkIndex := segment.StartIndex + i
			if chunkIndex >= len(cr.Index) {
				return nil, fmt.Errorf("segment %d: chunk index %d out of range (container has %d chunks)",
					segmentIndex, chunkIndex, len(cr.Index))
			}

			entry := cr.Index[chunkIndex]
			chunks = append(chunks, chunkEntry{
				offset:           cumulativeOffset,
				size:             entry.UncompressedSize,
				container:        segment.Container,
				indexInContainer: chunkIndex,
			})
			cumulativeOffset += int64(entry.UncompressedSize)
		}
	}

	// Verify the total size matches the reconstruction record.
	if cumulativeOffset != record.Size {
		return nil, fmt.Errorf("chunk sizes sum to %d bytes, but reconstruction record says %d",
			cumulativeOffset, record.Size)
	}

	return chunks, nil
}

// findChunk returns the index of the chunk that contains the given
// byte offset. Returns -1 if the offset is out of range.
func findChunk(chunks []chunkEntry, offset int64) int {
	if len(chunks) == 0 || offset < 0 {
		return -1
	}

	// Binary search for the last chunk whose offset <= the target.
	index := sort.Search(len(chunks), func(i int) bool {
		return chunks[i].offset > offset
	}) - 1

	if index < 0 || index >= len(chunks) {
		return -1
	}

	// Verify the offset falls within this chunk's range.
	chunk := chunks[index]
	if offset >= chunk.offset+int64(chunk.size) {
		return -1
	}

	return index
}

// readAt reads up to len(dest) bytes starting at the given offset
// within the reconstructed artifact. Returns the number of bytes
// read. The read may span multiple chunks.
func readAt(chunks []chunkEntry, provider containerProvider, dest []byte, offset int64, totalSize int64) (int, error) {
	if offset >= totalSize {
		return 0, io.EOF
	}

	// Clamp the read to the file size.
	remaining := totalSize - offset
	if int64(len(dest)) > remaining {
		dest = dest[:remaining]
	}

	var totalRead int
	for totalRead < len(dest) {
		currentOffset := offset + int64(totalRead)
		chunkIndex := findChunk(chunks, currentOffset)
		if chunkIndex < 0 {
			if totalRead > 0 {
				return totalRead, nil
			}
			return 0, io.EOF
		}

		chunk := chunks[chunkIndex]

		// Decompress the chunk.
		decompressed, err := extractChunkData(provider, chunk)
		if err != nil {
			return totalRead, fmt.Errorf("reading chunk at offset %d: %w", currentOffset, err)
		}

		// Calculate the position within this chunk.
		offsetInChunk := int(currentOffset - chunk.offset)
		available := len(decompressed) - offsetInChunk
		if available <= 0 {
			break
		}

		// Copy the relevant portion to dest.
		toCopy := len(dest) - totalRead
		if toCopy > available {
			toCopy = available
		}
		copy(dest[totalRead:totalRead+toCopy], decompressed[offsetInChunk:offsetInChunk+toCopy])
		totalRead += toCopy
	}

	return totalRead, nil
}

// extractChunkData loads a container (from cache or disk), parses its
// index, and decompresses a specific chunk.
func extractChunkData(provider containerProvider, chunk chunkEntry) ([]byte, error) {
	containerData, err := provider.containerBytes(chunk.container)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(containerData)
	cr, err := artifactstore.ReadContainerIndex(reader)
	if err != nil {
		return nil, fmt.Errorf("parsing container %s: %w",
			artifactstore.FormatHash(chunk.container), err)
	}

	decompressed, err := cr.ExtractChunk(reader, chunk.indexInContainer)
	if err != nil {
		return nil, fmt.Errorf("extracting chunk %d from container %s: %w",
			chunk.indexInContainer, artifactstore.FormatHash(chunk.container), err)
	}

	return decompressed, nil
}
