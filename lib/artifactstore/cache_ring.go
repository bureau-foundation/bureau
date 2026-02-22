// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package artifactstore

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// DefaultBlockSize is the default size of each block in the cache
// ring (256 MiB). Configurable at cache creation time.
const DefaultBlockSize int64 = 256 * 1024 * 1024

// MinBlockCount is the minimum number of blocks in a cache ring.
// The ring needs: one block for writing, one for recent reads, and
// at least two for the eviction buffer.
const MinBlockCount = 4

// CacheLocation identifies a container's position within the cache.
type CacheLocation struct {
	Block      int
	Generation uint64
	Offset     int64 // byte offset within the block
	Length     int64 // container size in bytes
}

// blockState tracks per-block metadata. The generation counter
// increments each time the block is reclaimed (evicted). The readers
// counter tracks in-flight reads, preventing reclamation while reads
// are active.
type blockState struct {
	generation atomic.Uint64
	readers    atomic.Int32
}

// BlockRing manages a circular buffer of fixed-size blocks on a
// CacheDevice. Containers are written sequentially within blocks.
// When the current block fills, the write cursor advances to the
// next block, reclaiming it (incrementing its generation so all
// index entries pointing to the old generation become stale).
//
// Reads are lock-free: they use atomic generation checks and reader
// reference counts. Writes are serialized by the caller.
type BlockRing struct {
	device     *CacheDevice
	blocks     []blockState
	blockSize  int64
	blockCount int

	// Writer state, protected by mu.
	mu          sync.Mutex
	writeBlock  int
	writeOffset int64
}

// NewBlockRing creates a block ring over the given device. The device
// is divided into blockCount blocks of blockSize bytes each.
func NewBlockRing(device *CacheDevice, blockSize int64, blockCount int) (*BlockRing, error) {
	if blockSize <= 0 {
		return nil, fmt.Errorf("block size must be positive, got %d", blockSize)
	}
	if blockCount < MinBlockCount {
		return nil, fmt.Errorf("block count must be >= %d, got %d", MinBlockCount, blockCount)
	}
	requiredSize := blockSize * int64(blockCount)
	if requiredSize > device.Size() {
		return nil, fmt.Errorf("block size %d * count %d = %d exceeds device size %d",
			blockSize, blockCount, requiredSize, device.Size())
	}

	return &BlockRing{
		device:     device,
		blocks:     make([]blockState, blockCount),
		blockSize:  blockSize,
		blockCount: blockCount,
	}, nil
}

// WriteResult is returned by [BlockRing.Write] with the stored
// location and eviction information.
type WriteResult struct {
	// Location is where the data was stored.
	Location CacheLocation

	// EvictedBlock is the block index that was reclaimed, or -1 if
	// no eviction occurred (first use of the next block).
	EvictedBlock int

	// EvictedGeneration is the generation of the evicted block
	// before reclamation. Only meaningful when EvictedBlock >= 0.
	EvictedGeneration uint64
}

// Write stores data in the current block. If the current block lacks
// space, advances to the next block (reclaiming it if necessary).
//
// Must be called from a single goroutine (the cache facade serializes
// writes).
func (r *BlockRing) Write(data []byte) (WriteResult, error) {
	dataLength := int64(len(data))
	if dataLength > r.blockSize {
		return WriteResult{}, fmt.Errorf("data size %d exceeds block size %d", dataLength, r.blockSize)
	}
	if dataLength == 0 {
		return WriteResult{}, fmt.Errorf("cannot write empty data")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Ensure the write block is active (generation > 0).
	if r.blocks[r.writeBlock].generation.Load() == 0 {
		r.blocks[r.writeBlock].generation.Store(1)
	}

	var result WriteResult
	result.EvictedBlock = -1

	// Advance to next block if current one lacks space.
	if r.writeOffset+dataLength > r.blockSize {
		evictedBlock, evictedGeneration, err := r.advanceWriteBlock()
		if err != nil {
			return WriteResult{}, err
		}
		result.EvictedBlock = evictedBlock
		result.EvictedGeneration = evictedGeneration
	}

	// Write the data to the device.
	deviceOffset := int64(r.writeBlock)*r.blockSize + r.writeOffset
	if _, err := r.device.WriteAt(data, deviceOffset); err != nil {
		return WriteResult{}, fmt.Errorf("writing %d bytes at device offset %d: %w",
			dataLength, deviceOffset, err)
	}

	result.Location = CacheLocation{
		Block:      r.writeBlock,
		Generation: r.blocks[r.writeBlock].generation.Load(),
		Offset:     r.writeOffset,
		Length:     dataLength,
	}

	r.writeOffset += dataLength
	return result, nil
}

// advanceWriteBlock moves the write cursor to the next block,
// reclaiming it if necessary. Returns the evicted block index and
// its old generation, or (-1, 0) if no eviction occurred (unused
// block). Must be called with r.mu held.
func (r *BlockRing) advanceWriteBlock() (int, uint64, error) {
	nextBlock := (r.writeBlock + 1) % r.blockCount

	// Wait for any readers on the next block to finish. Readers
	// hold the count for microseconds (mmap read duration), so
	// this spin is brief in practice.
	for r.blocks[nextBlock].readers.Load() > 0 {
		runtime.Gosched()
	}

	oldGeneration := r.blocks[nextBlock].generation.Load()
	r.blocks[nextBlock].generation.Store(oldGeneration + 1)

	r.writeBlock = nextBlock
	r.writeOffset = 0

	if oldGeneration > 0 {
		return nextBlock, oldGeneration, nil
	}
	return -1, 0, nil
}

// Read reads container data from the given location into p. Returns
// the number of bytes read. The location must be valid (generation
// must match the block's current generation).
//
// Read is safe for concurrent use. It increments the block's reader
// count to prevent reclamation during the read, and decrements it
// on return.
func (r *BlockRing) Read(location CacheLocation, p []byte) (int, error) {
	if location.Block < 0 || location.Block >= r.blockCount {
		return 0, fmt.Errorf("block index %d out of range [0, %d)", location.Block, r.blockCount)
	}

	block := &r.blocks[location.Block]

	// Increment reader count BEFORE checking generation. This
	// prevents a TOCTOU race: advanceWriteBlock waits for readers
	// to drain before reclaiming.
	block.readers.Add(1)
	defer block.readers.Add(-1)

	if block.generation.Load() != location.Generation {
		return 0, fmt.Errorf("generation mismatch for block %d: expected %d, current %d",
			location.Block, location.Generation, block.generation.Load())
	}

	deviceOffset := int64(location.Block)*r.blockSize + location.Offset
	readCount, err := r.device.ReadAt(p[:location.Length], deviceOffset)
	if err != nil {
		return readCount, fmt.Errorf("reading %d bytes at device offset %d: %w",
			location.Length, deviceOffset, err)
	}

	// Verify generation hasn't changed during the read. The reader
	// count should prevent this, but belt-and-suspenders.
	if block.generation.Load() != location.Generation {
		return 0, fmt.Errorf("block %d generation changed during read (torn read)", location.Block)
	}

	return readCount, nil
}

// IsValid checks whether a cache location's generation still matches
// its block. This is a quick check for index validation — it does
// not increment the reader count.
func (r *BlockRing) IsValid(location CacheLocation) bool {
	if location.Block < 0 || location.Block >= r.blockCount {
		return false
	}
	return r.blocks[location.Block].generation.Load() == location.Generation
}

// BlockCount returns the number of blocks in the ring.
func (r *BlockRing) BlockCount() int {
	return r.blockCount
}

// BlockSize returns the size of each block in bytes.
func (r *BlockRing) BlockSize() int64 {
	return r.blockSize
}

// Generation returns the current generation of a block.
func (r *BlockRing) Generation(block int) uint64 {
	if block < 0 || block >= r.blockCount {
		return 0
	}
	return r.blocks[block].generation.Load()
}

// WriteState returns the current write cursor position.
func (r *BlockRing) WriteState() (writeBlock int, writeOffset int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeBlock, r.writeOffset
}

// SetState restores ring state from persisted metadata. Must be
// called before any reads or writes.
func (r *BlockRing) SetState(writeBlock int, writeOffset int64, generations []uint64) error {
	if writeBlock < 0 || writeBlock >= r.blockCount {
		return fmt.Errorf("write block %d out of range [0, %d)", writeBlock, r.blockCount)
	}
	if len(generations) != r.blockCount {
		return fmt.Errorf("generations length %d does not match block count %d",
			len(generations), r.blockCount)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.writeBlock = writeBlock
	r.writeOffset = writeOffset
	for i, generation := range generations {
		r.blocks[i].generation.Store(generation)
	}
	return nil
}
