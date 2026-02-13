// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package artifact

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// --- CacheDevice tests ---

func TestCacheDeviceCreateAndReadWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.data")
	device, err := NewCacheDevice(path, 4096)
	if err != nil {
		t.Fatalf("NewCacheDevice: %v", err)
	}
	defer device.Close()

	if device.Size() != 4096 {
		t.Errorf("Size = %d, want 4096", device.Size())
	}

	// Write some data.
	data := []byte("hello, mmap world!")
	if _, err := device.WriteAt(data, 100); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Read it back via the memory map.
	readBuffer := make([]byte, len(data))
	if _, err := device.ReadAt(readBuffer, 100); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(readBuffer, data) {
		t.Errorf("read-back = %q, want %q", readBuffer, data)
	}
}

func TestCacheDeviceReopenExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.data")

	// Create and write.
	device, err := NewCacheDevice(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("persistent data")
	device.WriteAt(data, 0)
	device.Sync()
	device.Close()

	// Reopen at the same size.
	device, err = NewCacheDevice(path, 4096)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer device.Close()

	readBuffer := make([]byte, len(data))
	device.ReadAt(readBuffer, 0)
	if !bytes.Equal(readBuffer, data) {
		t.Errorf("after reopen: got %q, want %q", readBuffer, data)
	}
}

func TestCacheDeviceSizeMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.data")

	device, err := NewCacheDevice(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	device.Close()

	// Try to reopen at a different size.
	_, err = NewCacheDevice(path, 8192)
	if err == nil {
		t.Fatal("expected error for size mismatch")
	}
}

func TestCacheDeviceWriteBeyondBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.data")
	device, err := NewCacheDevice(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	_, err = device.WriteAt([]byte("x"), 1024)
	if err == nil {
		t.Error("expected error writing beyond device bounds")
	}
}

func TestCacheDeviceReadBeyondBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.data")
	device, err := NewCacheDevice(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	buffer := make([]byte, 1)
	_, err = device.ReadAt(buffer, 1024)
	if err == nil {
		t.Error("expected error reading beyond device bounds")
	}
}

// --- BlockRing tests ---

func newTestRing(t *testing.T, blockSize int64, blockCount int) (*CacheDevice, *BlockRing) {
	t.Helper()
	deviceSize := blockSize * int64(blockCount)
	path := filepath.Join(t.TempDir(), "ring.data")
	device, err := NewCacheDevice(path, deviceSize)
	if err != nil {
		t.Fatalf("NewCacheDevice: %v", err)
	}
	t.Cleanup(func() { device.Close() })

	ring, err := NewBlockRing(device, blockSize, blockCount)
	if err != nil {
		t.Fatalf("NewBlockRing: %v", err)
	}
	return device, ring
}

func TestBlockRingWriteAndRead(t *testing.T) {
	_, ring := newTestRing(t, 4096, 4)

	data := []byte("test container data")
	result, err := ring.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if result.Location.Block != 0 {
		t.Errorf("Block = %d, want 0", result.Location.Block)
	}
	if result.Location.Generation != 1 {
		t.Errorf("Generation = %d, want 1", result.Location.Generation)
	}
	if result.Location.Offset != 0 {
		t.Errorf("Offset = %d, want 0", result.Location.Offset)
	}
	if result.Location.Length != int64(len(data)) {
		t.Errorf("Length = %d, want %d", result.Location.Length, len(data))
	}
	if result.EvictedBlock != -1 {
		t.Errorf("EvictedBlock = %d, want -1", result.EvictedBlock)
	}

	// Read it back.
	readBuffer := make([]byte, result.Location.Length)
	if _, err := ring.Read(result.Location, readBuffer); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(readBuffer, data) {
		t.Errorf("read-back = %q, want %q", readBuffer, data)
	}
}

func TestBlockRingSequentialWrites(t *testing.T) {
	_, ring := newTestRing(t, 1024, 4)

	data1 := bytes.Repeat([]byte("A"), 400)
	data2 := bytes.Repeat([]byte("B"), 400)

	result1, err := ring.Write(data1)
	if err != nil {
		t.Fatal(err)
	}
	result2, err := ring.Write(data2)
	if err != nil {
		t.Fatal(err)
	}

	// Both should be in block 0 (400+400 < 1024).
	if result1.Location.Block != 0 || result2.Location.Block != 0 {
		t.Errorf("expected both in block 0, got block %d and %d",
			result1.Location.Block, result2.Location.Block)
	}
	if result2.Location.Offset != 400 {
		t.Errorf("second write offset = %d, want 400", result2.Location.Offset)
	}
}

func TestBlockRingBlockAdvance(t *testing.T) {
	_, ring := newTestRing(t, 1024, 4)

	// Fill block 0.
	data := bytes.Repeat([]byte("X"), 900)
	result1, err := ring.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if result1.Location.Block != 0 {
		t.Fatalf("first write in block %d, want 0", result1.Location.Block)
	}

	// This write won't fit in block 0 (900 + 200 > 1024), so it
	// should advance to block 1.
	data2 := bytes.Repeat([]byte("Y"), 200)
	result2, err := ring.Write(data2)
	if err != nil {
		t.Fatal(err)
	}
	if result2.Location.Block != 1 {
		t.Errorf("second write in block %d, want 1", result2.Location.Block)
	}
	if result2.Location.Offset != 0 {
		t.Errorf("second write offset = %d, want 0", result2.Location.Offset)
	}
	// Block 1 was previously unused (generation 0), so no eviction.
	if result2.EvictedBlock != -1 {
		t.Errorf("EvictedBlock = %d, want -1 (first use)", result2.EvictedBlock)
	}
}

func TestBlockRingEviction(t *testing.T) {
	_, ring := newTestRing(t, 256, 4)

	// Fill all 4 blocks.
	var locations [4]CacheLocation
	for i := 0; i < 4; i++ {
		data := bytes.Repeat([]byte{byte('A' + i)}, 200)
		result, err := ring.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		locations[i] = result.Location
		// Force block advance by filling the block.
		if i < 3 {
			fill := bytes.Repeat([]byte{0}, 56)
			ring.Write(fill)
		}
	}

	// Write data that doesn't fit in block 3 — forces advance to block 0,
	// which has generation 1, so this triggers eviction.
	evictData := bytes.Repeat([]byte("E"), 57)
	result, err := ring.Write(evictData)
	if err != nil {
		t.Fatal(err)
	}

	if result.EvictedBlock != 0 {
		t.Errorf("EvictedBlock = %d, want 0", result.EvictedBlock)
	}
	if result.EvictedGeneration != 1 {
		t.Errorf("EvictedGeneration = %d, want 1", result.EvictedGeneration)
	}

	// The original data in block 0 should no longer be readable.
	if ring.IsValid(locations[0]) {
		t.Error("block 0 location still valid after eviction")
	}
}

func TestBlockRingGenerationCheck(t *testing.T) {
	_, ring := newTestRing(t, 256, 4)

	data := []byte("generation test")
	result, err := ring.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	if !ring.IsValid(result.Location) {
		t.Error("fresh location should be valid")
	}

	// Tamper with the generation.
	tampered := result.Location
	tampered.Generation = 99
	if ring.IsValid(tampered) {
		t.Error("tampered generation should be invalid")
	}
}

func TestBlockRingReadInvalidGeneration(t *testing.T) {
	_, ring := newTestRing(t, 256, 4)

	data := []byte("test")
	result, err := ring.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	// Try to read with wrong generation.
	badLocation := result.Location
	badLocation.Generation = 99

	buffer := make([]byte, badLocation.Length)
	_, err = ring.Read(badLocation, buffer)
	if err == nil {
		t.Error("expected error reading with wrong generation")
	}
}

func TestBlockRingMinBlockCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ring.data")
	device, err := NewCacheDevice(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	// Too few blocks.
	_, err = NewBlockRing(device, 1024, 3)
	if err == nil {
		t.Error("expected error for < MinBlockCount blocks")
	}

	// Exactly MinBlockCount.
	_, err = NewBlockRing(device, 1024, 4)
	if err != nil {
		t.Errorf("MinBlockCount should work: %v", err)
	}
}

func TestBlockRingSetAndWriteState(t *testing.T) {
	_, ring := newTestRing(t, 1024, 4)

	// Set state.
	generations := []uint64{5, 3, 7, 1}
	if err := ring.SetState(2, 512, generations); err != nil {
		t.Fatal(err)
	}

	writeBlock, writeOffset := ring.WriteState()
	if writeBlock != 2 || writeOffset != 512 {
		t.Errorf("WriteState = (%d, %d), want (2, 512)", writeBlock, writeOffset)
	}

	for i, expected := range generations {
		if ring.Generation(i) != expected {
			t.Errorf("Generation(%d) = %d, want %d", i, ring.Generation(i), expected)
		}
	}
}

// --- CacheIndex tests ---

func TestCacheIndexPutAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.idx")
	index, err := NewCacheIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close(nil)

	hash := HashChunk([]byte("test"))
	location := CacheLocation{Block: 1, Generation: 5, Offset: 100, Length: 200}

	if err := index.Put(hash, location); err != nil {
		t.Fatal(err)
	}

	got, found := index.Get(hash)
	if !found {
		t.Fatal("entry not found after Put")
	}
	if got != location {
		t.Errorf("Get = %+v, want %+v", got, location)
	}
}

func TestCacheIndexRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.idx")
	index, err := NewCacheIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close(nil)

	hash := HashChunk([]byte("removeme"))
	index.Put(hash, CacheLocation{Block: 0, Generation: 1})
	index.Remove(hash)

	if _, found := index.Get(hash); found {
		t.Error("entry still found after Remove")
	}
}

func TestCacheIndexRemoveBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.idx")
	index, err := NewCacheIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close(nil)

	// Add entries in blocks 0 and 1.
	hash0a := HashChunk([]byte("block0-a"))
	hash0b := HashChunk([]byte("block0-b"))
	hash1 := HashChunk([]byte("block1"))

	index.Put(hash0a, CacheLocation{Block: 0, Generation: 1})
	index.Put(hash0b, CacheLocation{Block: 0, Generation: 1})
	index.Put(hash1, CacheLocation{Block: 1, Generation: 1})

	if index.Len() != 3 {
		t.Fatalf("Len = %d, want 3", index.Len())
	}

	// Remove block 0 entries.
	index.RemoveBlock(0, 1)

	if index.Len() != 1 {
		t.Errorf("Len after RemoveBlock = %d, want 1", index.Len())
	}
	if _, found := index.Get(hash1); !found {
		t.Error("block 1 entry should survive RemoveBlock(0)")
	}
}

func TestCacheIndexRecordEncodeDecode(t *testing.T) {
	hash := HashChunk([]byte("encode test"))
	location := CacheLocation{
		Block:      42,
		Generation: 12345678,
		Offset:     1024000,
		Length:     65536,
	}

	encoded := encodeIndexRecord(hash, location)
	decoded, err := decodeIndexRecord(encoded[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.hash != hash {
		t.Error("hash mismatch")
	}
	if decoded.location != location {
		t.Errorf("location = %+v, want %+v", decoded.location, location)
	}
}

func TestCacheIndexRecordCRCDetectsCorruption(t *testing.T) {
	hash := HashChunk([]byte("crc test"))
	location := CacheLocation{Block: 1, Generation: 1, Offset: 0, Length: 100}

	encoded := encodeIndexRecord(hash, location)

	// Corrupt one byte.
	encoded[10] ^= 0xFF

	_, err := decodeIndexRecord(encoded[:])
	if err == nil {
		t.Error("expected CRC error for corrupted record")
	}
}

func TestCacheIndexPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "test.idx")
	devicePath := filepath.Join(dir, "ring.data")

	// Create a device and ring.
	device, err := NewCacheDevice(devicePath, 4*1024)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := NewBlockRing(device, 1024, 4)
	if err != nil {
		device.Close()
		t.Fatal(err)
	}

	// Write some data to the ring so it has state.
	ring.Write([]byte("data in block 0"))

	// Create index, add entries, compact (writes header + records).
	index, err := NewCacheIndex(idxPath)
	if err != nil {
		device.Close()
		t.Fatal(err)
	}

	hash1 := HashChunk([]byte("entry1"))
	hash2 := HashChunk([]byte("entry2"))
	index.Put(hash1, CacheLocation{Block: 0, Generation: 1, Offset: 0, Length: 15})
	index.Put(hash2, CacheLocation{Block: 0, Generation: 1, Offset: 15, Length: 20})

	// Compact to write the header.
	if err := index.Compact(ring); err != nil {
		device.Close()
		t.Fatal(err)
	}
	index.Close(nil)
	device.Close()

	// Reopen everything.
	device, err = NewCacheDevice(devicePath, 4*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	ring, err = NewBlockRing(device, 1024, 4)
	if err != nil {
		t.Fatal(err)
	}

	index, err = OpenCacheIndex(idxPath, ring)
	if err != nil {
		t.Fatalf("OpenCacheIndex: %v", err)
	}
	defer index.Close(nil)

	if index.Len() != 2 {
		t.Errorf("Len after reload = %d, want 2", index.Len())
	}

	if loc, found := index.Get(hash1); !found {
		t.Error("hash1 not found after reload")
	} else if loc.Offset != 0 || loc.Length != 15 {
		t.Errorf("hash1 location = %+v", loc)
	}

	if _, found := index.Get(hash2); !found {
		t.Error("hash2 not found after reload")
	}
}

func TestCacheIndexCompaction(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "test.idx")
	devicePath := filepath.Join(dir, "ring.data")

	device, err := NewCacheDevice(devicePath, 4*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	ring, err := NewBlockRing(device, 1024, 4)
	if err != nil {
		t.Fatal(err)
	}

	index, err := NewCacheIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close(nil)

	// Add many entries, then remove most of them.
	var hashes []Hash
	for i := 0; i < 100; i++ {
		hash := HashChunk([]byte(fmt.Sprintf("entry-%d", i)))
		hashes = append(hashes, hash)
		index.Put(hash, CacheLocation{Block: 0, Generation: 1, Offset: int64(i * 10), Length: 10})
	}

	// Remove 90 entries.
	for i := 0; i < 90; i++ {
		index.Remove(hashes[i])
	}

	if !index.NeedsCompaction() {
		t.Error("should need compaction (100 records, 10 live)")
	}

	if err := index.Compact(ring); err != nil {
		t.Fatal(err)
	}

	if index.NeedsCompaction() {
		t.Error("should not need compaction after compact")
	}

	// Verify live entries survived.
	for i := 90; i < 100; i++ {
		if _, found := index.Get(hashes[i]); !found {
			t.Errorf("entry %d not found after compaction", i)
		}
	}
}

// --- Cache integration tests ---

func newTestCache(t *testing.T, blockSize int64, blockCount int) *Cache {
	t.Helper()
	cache, err := NewCache(CacheConfig{
		Path:       filepath.Join(t.TempDir(), "cache"),
		DeviceSize: blockSize * int64(blockCount),
		BlockSize:  blockSize,
	})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(func() { cache.Close() })
	return cache
}

// buildTestContainer creates a small container with the given data
// and returns its hash and serialized bytes.
func buildTestContainer(t *testing.T, data []byte) (Hash, []byte) {
	t.Helper()
	chunkHash := HashChunk(data)
	compressed, tag, err := compressWithFallback(data, CompressionNone)
	if err != nil {
		t.Fatal(err)
	}

	builder := NewContainerBuilder()
	builder.AddChunk(chunkHash, compressed, tag, uint32(len(data)))

	var buffer bytes.Buffer
	containerHash, err := builder.Flush(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	return containerHash, buffer.Bytes()
}

func TestCachePutAndGet(t *testing.T) {
	cache := newTestCache(t, 4096, 4)

	containerHash, containerData := buildTestContainer(t, []byte("hello cache"))

	if err := cache.Put(containerHash, containerData); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := cache.Get(containerHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, containerData) {
		t.Error("Get returned different data than Put")
	}
}

func TestCacheContains(t *testing.T) {
	cache := newTestCache(t, 4096, 4)

	containerHash, containerData := buildTestContainer(t, []byte("contains test"))

	if cache.Contains(containerHash) {
		t.Error("Contains should be false before Put")
	}

	cache.Put(containerHash, containerData)

	if !cache.Contains(containerHash) {
		t.Error("Contains should be true after Put")
	}
}

func TestCacheDedup(t *testing.T) {
	cache := newTestCache(t, 4096, 4)

	containerHash, containerData := buildTestContainer(t, []byte("dedup test"))

	cache.Put(containerHash, containerData)
	cache.Put(containerHash, containerData) // should be idempotent

	if cache.Stats().LiveContainers != 1 {
		t.Errorf("LiveContainers = %d, want 1 after dedup", cache.Stats().LiveContainers)
	}
}

func TestCacheMiss(t *testing.T) {
	cache := newTestCache(t, 4096, 4)

	var unknownHash Hash
	unknownHash[0] = 0xFF

	_, err := cache.Get(unknownHash)
	if err == nil {
		t.Error("expected error for cache miss")
	}
}

func TestCacheEviction(t *testing.T) {
	// Small blocks so eviction happens quickly.
	cache := newTestCache(t, 512, 4)

	// Write containers until eviction happens. Each container is
	// ~100 bytes, so 512 / 100 ≈ 5 containers per block, and we
	// need 4 blocks full + 1 to trigger eviction of block 0.
	var hashes []Hash
	for i := 0; i < 30; i++ {
		data := []byte(fmt.Sprintf("container-data-%03d-padding-to-make-it-bigger", i))
		hash, containerData := buildTestContainer(t, data)
		if err := cache.Put(hash, containerData); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		hashes = append(hashes, hash)
	}

	// Some early containers should be evicted.
	evictedCount := 0
	for _, hash := range hashes {
		if !cache.Contains(hash) {
			evictedCount++
		}
	}

	if evictedCount == 0 {
		t.Error("expected some containers to be evicted")
	}

	t.Logf("evicted %d of %d containers", evictedCount, len(hashes))

	// Recent containers should still be cached.
	lastHash := hashes[len(hashes)-1]
	if !cache.Contains(lastHash) {
		t.Error("most recent container should still be cached")
	}
}

func TestCacheReopenWithIndex(t *testing.T) {
	dir := t.TempDir()
	config := CacheConfig{
		Path:       filepath.Join(dir, "cache"),
		DeviceSize: 4 * 4096,
		BlockSize:  4096,
	}

	// Create, write, sync, close.
	cache, err := NewCache(config)
	if err != nil {
		t.Fatal(err)
	}

	containerHash, containerData := buildTestContainer(t, []byte("persist across restart"))
	cache.Put(containerHash, containerData)
	cache.Sync()
	cache.Close()

	// Reopen.
	cache, err = NewCache(config)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cache.Close()

	// The container should still be accessible.
	got, err := cache.Get(containerHash)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !bytes.Equal(got, containerData) {
		t.Error("data mismatch after reopen")
	}
}

func TestCacheStats(t *testing.T) {
	cache := newTestCache(t, 1024, 8)

	stats := cache.Stats()
	if stats.BlockSize != 1024 {
		t.Errorf("BlockSize = %d, want 1024", stats.BlockSize)
	}
	if stats.BlockCount != 8 {
		t.Errorf("BlockCount = %d, want 8", stats.BlockCount)
	}
	if stats.DeviceSize != 8*1024 {
		t.Errorf("DeviceSize = %d, want %d", stats.DeviceSize, 8*1024)
	}
	if stats.LiveContainers != 0 {
		t.Errorf("LiveContainers = %d, want 0", stats.LiveContainers)
	}
}

func TestCacheMultipleContainers(t *testing.T) {
	cache := newTestCache(t, 4096, 8)

	// Write several containers and verify all are readable.
	type entry struct {
		hash Hash
		data []byte
	}
	var entries []entry

	for i := 0; i < 10; i++ {
		data := []byte(fmt.Sprintf("container-%d-with-some-payload-data", i))
		hash, containerData := buildTestContainer(t, data)
		if err := cache.Put(hash, containerData); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		entries = append(entries, entry{hash, containerData})
	}

	for i, e := range entries {
		got, err := cache.Get(e.hash)
		if err != nil {
			t.Errorf("Get %d: %v", i, err)
			continue
		}
		if !bytes.Equal(got, e.data) {
			t.Errorf("container %d data mismatch", i)
		}
	}
}

// --- ContainerReader.TotalSize test ---

func TestContainerReaderTotalSize(t *testing.T) {
	data := []byte("test data for total size")
	chunkHash := HashChunk(data)

	builder := NewContainerBuilder()
	builder.AddChunk(chunkHash, data, CompressionNone, uint32(len(data)))

	var buffer bytes.Buffer
	builder.Flush(&buffer)

	reader, err := ReadContainerIndex(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if reader.TotalSize() != int64(buffer.Len()) {
		t.Errorf("TotalSize = %d, want %d", reader.TotalSize(), buffer.Len())
	}
}

// --- Index file format tests ---

func TestIndexHeaderFormat(t *testing.T) {
	// Verify the header format by manually constructing one and
	// checking the bytes.
	var header [indexFixedHeaderSize]byte
	copy(header[0:4], indexMagic)
	binary.LittleEndian.PutUint32(header[4:8], indexVersion)
	binary.LittleEndian.PutUint32(header[8:12], 4) // blockCount
	binary.LittleEndian.PutUint64(header[12:20], 1024)
	binary.LittleEndian.PutUint32(header[20:24], 0) // writeBlock
	binary.LittleEndian.PutUint64(header[24:32], 0) // writeOffset

	if string(header[0:4]) != "BACI" {
		t.Error("magic bytes wrong")
	}
	if binary.LittleEndian.Uint32(header[4:8]) != 1 {
		t.Error("version wrong")
	}
}

// --- Benchmarks ---

func BenchmarkCachePut(b *testing.B) {
	dir := b.TempDir()
	cache, err := NewCache(CacheConfig{
		Path:       filepath.Join(dir, "cache"),
		DeviceSize: 256 * 1024 * 1024, // 256MB
		BlockSize:  64 * 1024 * 1024,  // 64MB blocks
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	// Pre-build containers.
	type testContainer struct {
		hash Hash
		data []byte
	}
	containers := make([]testContainer, b.N)
	for i := range containers {
		data := []byte(fmt.Sprintf("benchmark-container-%d-payload-data-padding-to-add-size", i))
		chunkHash := HashChunk(data)
		builder := NewContainerBuilder()
		builder.AddChunk(chunkHash, data, CompressionNone, uint32(len(data)))
		var buf bytes.Buffer
		hash, _ := builder.Flush(&buf)
		containers[i] = testContainer{hash, buf.Bytes()}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cache.Put(containers[i].hash, containers[i].data)
	}
}

func BenchmarkCacheGet(b *testing.B) {
	dir := b.TempDir()
	cache, err := NewCache(CacheConfig{
		Path:       filepath.Join(dir, "cache"),
		DeviceSize: 256 * 1024 * 1024,
		BlockSize:  64 * 1024 * 1024,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	// Store a container.
	data := bytes.Repeat([]byte("benchmark-get-data"), 100)
	chunkHash := HashChunk(data)
	builder := NewContainerBuilder()
	builder.AddChunk(chunkHash, data, CompressionNone, uint32(len(data)))
	var buf bytes.Buffer
	containerHash, _ := builder.Flush(&buf)
	cache.Put(containerHash, buf.Bytes())

	b.SetBytes(int64(buf.Len()))
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		cache.Get(containerHash)
	}
}

func BenchmarkCacheIndexPut(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.idx")
	index, err := NewCacheIndex(path)
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close(nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hash := HashChunk([]byte(fmt.Sprintf("key-%d", i)))
		index.Put(hash, CacheLocation{Block: i % 4, Generation: 1, Offset: int64(i * 100), Length: 100})
	}
}

func BenchmarkCacheDeviceReadAt(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.data")
	device, err := NewCacheDevice(path, 1024*1024) // 1MB
	if err != nil {
		b.Fatal(err)
	}
	defer device.Close()

	// Write some data.
	data := bytes.Repeat([]byte("X"), 4096)
	device.WriteAt(data, 0)

	readBuffer := make([]byte, 4096)
	b.SetBytes(4096)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		device.ReadAt(readBuffer, 0)
	}
}

// Verify index file has the expected layout on disk.
func TestIndexFileLayout(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "layout.idx")
	devicePath := filepath.Join(dir, "ring.data")

	device, err := NewCacheDevice(devicePath, 4*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	ring, err := NewBlockRing(device, 1024, 4)
	if err != nil {
		t.Fatal(err)
	}

	index, err := NewCacheIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	hash := HashChunk([]byte("layout test"))
	index.Put(hash, CacheLocation{Block: 2, Generation: 3, Offset: 512, Length: 100})
	index.Compact(ring)
	index.Close(nil)

	// Read the raw file and verify structure.
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	// Header: 32 fixed + 4*8 generations + 4 CRC = 68 bytes
	// 1 record: 64 bytes
	// Total: 132 bytes
	expectedSize := indexFixedHeaderSize + 4*8 + 4 + indexRecordSize
	if len(raw) != expectedSize {
		t.Errorf("file size = %d, want %d", len(raw), expectedSize)
	}

	// Verify magic.
	if string(raw[0:4]) != indexMagic {
		t.Error("magic mismatch")
	}

	// Verify CRC of header.
	crcOffset := indexFixedHeaderSize + 4*8
	headerData := raw[:crcOffset]
	expectedCRC := crc32.Checksum(headerData, crc32cTable)
	gotCRC := binary.LittleEndian.Uint32(raw[crcOffset : crcOffset+4])
	if gotCRC != expectedCRC {
		t.Errorf("header CRC = %08x, want %08x", gotCRC, expectedCRC)
	}

	// Verify the record CRC.
	recordStart := crcOffset + 4
	recordData := raw[recordStart : recordStart+60]
	expectedRecordCRC := crc32.Checksum(recordData, crc32cTable)
	gotRecordCRC := binary.LittleEndian.Uint32(raw[recordStart+60 : recordStart+64])
	if gotRecordCRC != expectedRecordCRC {
		t.Errorf("record CRC = %08x, want %08x", gotRecordCRC, expectedRecordCRC)
	}
}
