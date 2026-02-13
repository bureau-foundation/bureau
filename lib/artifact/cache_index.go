// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package artifact

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Index file format constants.
const (
	indexMagic   = "BACI" // Bureau Artifact Cache Index
	indexVersion = 1

	// Fixed header fields before the variable-length generations array:
	// magic(4) + version(4) + blockCount(4) + blockSize(8) +
	// writeBlock(4) + writeOffset(8) = 32 bytes.
	indexFixedHeaderSize = 32

	// Each index record: hash(32) + block(4) + generation(8) +
	// offset(8) + length(8) + crc(4) = 64 bytes.
	indexRecordSize = 64
)

// crc32cTable is the CRC32C (Castagnoli) table for index checksums.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// CacheIndex is an in-memory hash map of container locations backed
// by an append-only log file. The log file is compacted when it grows
// significantly larger than the live entry set.
//
// Records are 64-byte fixed-size entries appended to the log. There
// are no explicit delete records — entries for evicted blocks become
// stale and are pruned during compaction. Compaction writes a new
// file with a header (containing ring state) and only the live
// entries, then atomically renames it over the old file.
//
// On startup, the log is replayed to rebuild the in-memory map. Each
// record's generation is validated against the ring to skip stale
// entries. If the log is corrupt or missing, the cache can rebuild
// from scanning the device (slower, but correct).
type CacheIndex struct {
	mu      sync.RWMutex
	entries map[Hash]CacheLocation

	logFile      *os.File
	logPath      string
	totalRecords int // total records in the log file (including stale)
}

// NewCacheIndex creates a new empty index with a log file at path.
func NewCacheIndex(path string) (*CacheIndex, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating index directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("creating index file %s: %w", path, err)
	}

	return &CacheIndex{
		entries: make(map[Hash]CacheLocation),
		logFile: file,
		logPath: path,
	}, nil
}

// OpenCacheIndex loads an existing index log, replaying records and
// validating them against the ring. Returns the ring state (write
// block, write offset, generations) from the log header, which the
// caller should use to restore the ring via SetState.
//
// If the log file does not exist or is corrupt, returns an error.
// The caller should fall back to NewCacheIndex + RebuildFromDevice.
func OpenCacheIndex(path string, ring *BlockRing) (*CacheIndex, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening index file %s: %w", path, err)
	}

	index := &CacheIndex{
		entries: make(map[Hash]CacheLocation),
		logFile: file,
		logPath: path,
	}

	if err := index.loadAndReplay(ring); err != nil {
		file.Close()
		return nil, err
	}

	return index, nil
}

// loadAndReplay reads the header from the log file, restores the
// ring state, and replays all records to rebuild the in-memory map.
func (idx *CacheIndex) loadAndReplay(ring *BlockRing) error {
	// Read the fixed header.
	var fixedHeader [indexFixedHeaderSize]byte
	if _, err := io.ReadFull(idx.logFile, fixedHeader[:]); err != nil {
		return fmt.Errorf("reading index header: %w", err)
	}

	magic := string(fixedHeader[0:4])
	if magic != indexMagic {
		return fmt.Errorf("invalid index magic: got %q, want %q", magic, indexMagic)
	}

	version := binary.LittleEndian.Uint32(fixedHeader[4:8])
	if version != indexVersion {
		return fmt.Errorf("unsupported index version %d (this code supports %d)", version, indexVersion)
	}

	blockCount := binary.LittleEndian.Uint32(fixedHeader[8:12])
	blockSize := binary.LittleEndian.Uint64(fixedHeader[12:20])
	writeBlock := binary.LittleEndian.Uint32(fixedHeader[20:24])
	writeOffset := binary.LittleEndian.Uint64(fixedHeader[24:32])

	// Validate against the ring's configuration.
	if int(blockCount) != ring.BlockCount() {
		return fmt.Errorf("index block count %d does not match ring block count %d",
			blockCount, ring.BlockCount())
	}
	if int64(blockSize) != ring.BlockSize() {
		return fmt.Errorf("index block size %d does not match ring block size %d",
			blockSize, ring.BlockSize())
	}

	// Read per-block generations.
	generationsBytes := make([]byte, blockCount*8)
	if _, err := io.ReadFull(idx.logFile, generationsBytes); err != nil {
		return fmt.Errorf("reading index generations: %w", err)
	}

	generations := make([]uint64, blockCount)
	for i := range generations {
		generations[i] = binary.LittleEndian.Uint64(generationsBytes[i*8 : i*8+8])
	}

	// Read and validate the header CRC.
	var crcBytes [4]byte
	if _, err := io.ReadFull(idx.logFile, crcBytes[:]); err != nil {
		return fmt.Errorf("reading header CRC: %w", err)
	}
	expectedCRC := binary.LittleEndian.Uint32(crcBytes[:])

	headerCRC := crc32.New(crc32cTable)
	headerCRC.Write(fixedHeader[:])
	headerCRC.Write(generationsBytes)
	if headerCRC.Sum32() != expectedCRC {
		return fmt.Errorf("header CRC mismatch: expected %08x, got %08x",
			expectedCRC, headerCRC.Sum32())
	}

	// Restore ring state.
	if err := ring.SetState(int(writeBlock), int64(writeOffset), generations); err != nil {
		return fmt.Errorf("restoring ring state from index: %w", err)
	}

	// Replay records. First pass: determine the maximum generation
	// per block across all records (some records may reflect
	// generations newer than the header if blocks were evicted
	// between the last compaction and shutdown).
	maxGenerations := make([]uint64, blockCount)
	copy(maxGenerations, generations)

	var records []indexRecord
	var recordBuffer [indexRecordSize]byte

	for {
		_, err := io.ReadFull(idx.logFile, recordBuffer[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading index record: %w", err)
		}

		record, err := decodeIndexRecord(recordBuffer[:])
		if err != nil {
			// Truncated or corrupt record at end of file — stop
			// replaying. Everything before this point was valid.
			break
		}

		records = append(records, record)

		if record.location.Block >= 0 && record.location.Block < int(blockCount) {
			if record.location.Generation > maxGenerations[record.location.Block] {
				maxGenerations[record.location.Block] = record.location.Generation
			}
		}
	}

	// Update ring generations if any records had newer generations.
	generationsChanged := false
	for i, maxGeneration := range maxGenerations {
		if maxGeneration > generations[i] {
			generations[i] = maxGeneration
			generationsChanged = true
		}
	}
	if generationsChanged {
		if err := ring.SetState(int(writeBlock), int64(writeOffset), generations); err != nil {
			return fmt.Errorf("updating ring with replayed generations: %w", err)
		}
	}

	// Second pass: add records that are still live (their generation
	// matches the max for their block).
	for _, record := range records {
		block := record.location.Block
		if block >= 0 && block < int(blockCount) &&
			record.location.Generation == maxGenerations[block] {
			idx.entries[record.hash] = record.location
		}
	}

	// Update the ring write cursor to account for records that were
	// appended after the last compaction. Find the highest offset
	// in the current write block.
	for _, record := range records {
		if record.location.Block == int(writeBlock) &&
			record.location.Generation == maxGenerations[writeBlock] {
			endOffset := record.location.Offset + record.location.Length
			if endOffset > int64(writeOffset) {
				writeOffset = uint64(endOffset)
			}
		}
	}
	if err := ring.SetState(int(writeBlock), int64(writeOffset), generations); err != nil {
		return fmt.Errorf("updating ring write offset: %w", err)
	}

	idx.totalRecords = len(records)

	// Seek to end of file for future appends.
	if _, err := idx.logFile.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seeking to end of index file: %w", err)
	}

	return nil
}

// indexRecord is a decoded index log entry.
type indexRecord struct {
	hash     Hash
	location CacheLocation
}

// encodeIndexRecord serializes a record to a 64-byte buffer.
func encodeIndexRecord(hash Hash, location CacheLocation) [indexRecordSize]byte {
	var buffer [indexRecordSize]byte
	copy(buffer[0:32], hash[:])
	binary.LittleEndian.PutUint32(buffer[32:36], uint32(location.Block))
	binary.LittleEndian.PutUint64(buffer[36:44], location.Generation)
	binary.LittleEndian.PutUint64(buffer[44:52], uint64(location.Offset))
	binary.LittleEndian.PutUint64(buffer[52:60], uint64(location.Length))

	checksum := crc32.Checksum(buffer[:60], crc32cTable)
	binary.LittleEndian.PutUint32(buffer[60:64], checksum)

	return buffer
}

// decodeIndexRecord deserializes a record from a 64-byte buffer.
// Returns an error if the CRC does not match.
func decodeIndexRecord(buffer []byte) (indexRecord, error) {
	if len(buffer) < indexRecordSize {
		return indexRecord{}, fmt.Errorf("record too short: %d bytes", len(buffer))
	}

	expectedCRC := binary.LittleEndian.Uint32(buffer[60:64])
	actualCRC := crc32.Checksum(buffer[:60], crc32cTable)
	if expectedCRC != actualCRC {
		return indexRecord{}, fmt.Errorf("record CRC mismatch: expected %08x, got %08x",
			expectedCRC, actualCRC)
	}

	var record indexRecord
	copy(record.hash[:], buffer[0:32])
	record.location.Block = int(binary.LittleEndian.Uint32(buffer[32:36]))
	record.location.Generation = binary.LittleEndian.Uint64(buffer[36:44])
	record.location.Offset = int64(binary.LittleEndian.Uint64(buffer[44:52]))
	record.location.Length = int64(binary.LittleEndian.Uint64(buffer[52:60]))

	return record, nil
}

// Get looks up a container's location by hash. Returns the location
// and true if found, or a zero location and false if not.
// Thread-safe for concurrent reads.
func (idx *CacheIndex) Get(hash Hash) (CacheLocation, bool) {
	idx.mu.RLock()
	location, found := idx.entries[hash]
	idx.mu.RUnlock()
	return location, found
}

// Put adds or updates a container location in the index and appends
// a record to the log file.
func (idx *CacheIndex) Put(hash Hash, location CacheLocation) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries[hash] = location
	idx.totalRecords++

	record := encodeIndexRecord(hash, location)
	if _, err := idx.logFile.Write(record[:]); err != nil {
		return fmt.Errorf("appending index record: %w", err)
	}

	return nil
}

// Remove deletes a container from the in-memory index. No log record
// is written — the stale entry is pruned during the next compaction.
func (idx *CacheIndex) Remove(hash Hash) {
	idx.mu.Lock()
	delete(idx.entries, hash)
	idx.mu.Unlock()
}

// RemoveBlock removes all index entries pointing to the given block
// with a generation less than or equal to oldGeneration. Called when
// the ring reclaims a block.
func (idx *CacheIndex) RemoveBlock(block int, oldGeneration uint64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for hash, location := range idx.entries {
		if location.Block == block && location.Generation <= oldGeneration {
			delete(idx.entries, hash)
		}
	}
}

// Len returns the number of live entries in the index.
func (idx *CacheIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// NeedsCompaction returns true if the log file has grown much larger
// than the live entry set (more than 2x the live entries).
func (idx *CacheIndex) NeedsCompaction() bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.totalRecords > 2*len(idx.entries) && len(idx.entries) > 0
}

// Compact writes a fresh index file containing only the current ring
// state and live entries, then atomically replaces the old file.
func (idx *CacheIndex) Compact(ring *BlockRing) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.compactLocked(ring)
}

// compactLocked performs compaction. Must be called with idx.mu held.
func (idx *CacheIndex) compactLocked(ring *BlockRing) error {
	tmpPath := idx.logPath + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating temp index file: %w", err)
	}

	success := false
	defer func() {
		if !success {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	// Write header.
	writeBlock, writeOffset := ring.WriteState()
	blockCount := ring.BlockCount()

	var fixedHeader [indexFixedHeaderSize]byte
	copy(fixedHeader[0:4], indexMagic)
	binary.LittleEndian.PutUint32(fixedHeader[4:8], indexVersion)
	binary.LittleEndian.PutUint32(fixedHeader[8:12], uint32(blockCount))
	binary.LittleEndian.PutUint64(fixedHeader[12:20], uint64(ring.BlockSize()))
	binary.LittleEndian.PutUint32(fixedHeader[20:24], uint32(writeBlock))
	binary.LittleEndian.PutUint64(fixedHeader[24:32], uint64(writeOffset))

	if _, err := tmpFile.Write(fixedHeader[:]); err != nil {
		return fmt.Errorf("writing index header: %w", err)
	}

	// Write per-block generations.
	headerCRC := crc32.New(crc32cTable)
	headerCRC.Write(fixedHeader[:])

	generationsBytes := make([]byte, blockCount*8)
	for i := 0; i < blockCount; i++ {
		binary.LittleEndian.PutUint64(generationsBytes[i*8:i*8+8], ring.Generation(i))
	}
	if _, err := tmpFile.Write(generationsBytes); err != nil {
		return fmt.Errorf("writing index generations: %w", err)
	}
	headerCRC.Write(generationsBytes)

	// Write header CRC.
	var crcBytes [4]byte
	binary.LittleEndian.PutUint32(crcBytes[:], headerCRC.Sum32())
	if _, err := tmpFile.Write(crcBytes[:]); err != nil {
		return fmt.Errorf("writing header CRC: %w", err)
	}

	// Write live records.
	for hash, location := range idx.entries {
		record := encodeIndexRecord(hash, location)
		if _, err := tmpFile.Write(record[:]); err != nil {
			return fmt.Errorf("writing compacted record: %w", err)
		}
	}

	// Sync and close the temp file.
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("syncing temp index: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp index: %w", err)
	}

	// Atomic rename.
	if err := os.Rename(tmpPath, idx.logPath); err != nil {
		return fmt.Errorf("renaming temp index to %s: %w", idx.logPath, err)
	}

	// Reopen the new file for future appends.
	newFile, err := os.OpenFile(idx.logPath, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("reopening compacted index: %w", err)
	}

	idx.logFile.Close()
	idx.logFile = newFile
	idx.totalRecords = len(idx.entries)

	success = true
	return nil
}

// RebuildFromDevice scans all blocks in the cache device to discover
// containers and rebuild the index. This is the recovery path when
// the index file is corrupt or missing. It is O(device size).
func (idx *CacheIndex) RebuildFromDevice(device *CacheDevice, ring *BlockRing) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Clear existing entries.
	idx.entries = make(map[Hash]CacheLocation)

	for block := 0; block < ring.BlockCount(); block++ {
		generation := ring.Generation(block)
		if generation == 0 {
			continue // unused block
		}

		blockStart := int64(block) * ring.BlockSize()
		offset := int64(0)

		for offset+int64(containerHeaderSize) <= ring.BlockSize() {
			// Try to read a container magic at this offset.
			var magic [8]byte
			if _, err := device.ReadAt(magic[:], blockStart+offset); err != nil {
				break
			}
			if magic != containerMagic {
				break // no more containers in this block
			}

			// Read chunk count.
			var countBytes [4]byte
			if _, err := device.ReadAt(countBytes[:], blockStart+offset+8); err != nil {
				break
			}
			chunkCount := binary.LittleEndian.Uint32(countBytes[:])
			if chunkCount == 0 {
				break
			}

			// Compute total container size without reading the
			// full index. We need the compressed sizes from each
			// index entry.
			indexSize := int64(chunkCount) * int64(chunkIndexEntrySize)
			totalHeaderAndIndex := int64(containerHeaderSize) + indexSize

			if offset+totalHeaderAndIndex > ring.BlockSize() {
				break // index extends past block boundary
			}

			// Read compressed sizes from the index to compute data size.
			var dataSize int64
			for i := uint32(0); i < chunkCount; i++ {
				entryOffset := blockStart + offset + int64(containerHeaderSize) + int64(i)*int64(chunkIndexEntrySize)
				// CompressedSize is at offset 33 within each entry (after 32-byte hash + 1-byte tag).
				var sizeBytes [4]byte
				if _, err := device.ReadAt(sizeBytes[:], entryOffset+33); err != nil {
					break
				}
				dataSize += int64(binary.LittleEndian.Uint32(sizeBytes[:]))
			}

			containerSize := totalHeaderAndIndex + dataSize
			if offset+containerSize > ring.BlockSize() {
				break // container extends past block boundary
			}

			// Parse the container index to get the hash. The
			// SectionReader reads directly from the mmap'd device.
			reader := io.NewSectionReader(
				readerAtAdapter{device},
				blockStart+offset,
				containerSize,
			)
			containerReader, err := ReadContainerIndex(reader)
			if err != nil {
				break // corrupt container, stop scanning this block
			}

			idx.entries[containerReader.Hash] = CacheLocation{
				Block:      block,
				Generation: generation,
				Offset:     offset,
				Length:     containerSize,
			}

			offset += containerSize
		}
	}

	return nil
}

// readerAtAdapter wraps a *CacheDevice to implement io.ReaderAt for
// use with io.NewSectionReader.
type readerAtAdapter struct {
	device *CacheDevice
}

func (r readerAtAdapter) ReadAt(p []byte, off int64) (int, error) {
	return r.device.ReadAt(p, off)
}

// Close compacts the index (if a ring is available) and closes the
// log file.
func (idx *CacheIndex) Close(ring *BlockRing) error {
	if ring != nil {
		if err := idx.Compact(ring); err != nil {
			return fmt.Errorf("compacting index on close: %w", err)
		}
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if idx.logFile != nil {
		if err := idx.logFile.Close(); err != nil {
			return fmt.Errorf("closing index file: %w", err)
		}
		idx.logFile = nil
	}
	return nil
}
