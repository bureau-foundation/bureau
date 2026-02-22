// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package artifactstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CacheConfig configures a local artifact cache.
type CacheConfig struct {
	// Path is the directory for cache files. The directory is
	// created if it does not exist.
	Path string

	// DeviceSize is the total size of the cache data file in bytes.
	// Must be at least BlockSize * MinBlockCount.
	DeviceSize int64

	// BlockSize is the size of each block in bytes. Defaults to
	// DefaultBlockSize (256 MiB) if zero. Containers larger than
	// BlockSize cannot be cached.
	BlockSize int64
}

// Cache is a bounded, self-cleaning local artifact cache. Containers
// are stored in a circular block ring backed by a memory-mapped file.
// When the ring is full, the oldest block is reclaimed and all index
// entries pointing to it become stale.
//
// The cache is safe for concurrent reads with a single writer. Reads
// use atomic generation checks and reader reference counts — no locks
// on the hot path beyond a shared RLock on the index map.
//
// Pin support: pinned containers are stored in a separate directory
// keyed by container hash. Unlike the ring, pinned containers are
// never evicted.
type Cache struct {
	config CacheConfig
	device *CacheDevice
	ring   *BlockRing
	index  *CacheIndex
	pinDir string

	// writeMu serializes all write operations (Put, Pin).
	writeMu sync.Mutex
}

// NewCache creates a new cache at the configured path. If the path
// already contains cache files from a previous run, they are loaded.
// If they are incompatible (different block size or device size),
// an error is returned — delete the directory to start fresh.
func NewCache(config CacheConfig) (*Cache, error) {
	if config.Path == "" {
		return nil, fmt.Errorf("cache path is required")
	}
	if config.BlockSize == 0 {
		config.BlockSize = DefaultBlockSize
	}
	if config.DeviceSize <= 0 {
		return nil, fmt.Errorf("cache device size must be positive, got %d", config.DeviceSize)
	}

	blockCount := int(config.DeviceSize / config.BlockSize)
	if blockCount < MinBlockCount {
		return nil, fmt.Errorf("device size %d with block size %d gives %d blocks, need at least %d",
			config.DeviceSize, config.BlockSize, blockCount, MinBlockCount)
	}

	// Align device size to block boundaries.
	alignedDeviceSize := config.BlockSize * int64(blockCount)

	if err := os.MkdirAll(config.Path, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache directory: %w", err)
	}

	// Create the pin directory for containers exempt from ring eviction.
	pinDir := filepath.Join(config.Path, "pinned")
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating pin directory: %w", err)
	}

	// Open the device.
	devicePath := filepath.Join(config.Path, "cache.data")
	device, err := NewCacheDevice(devicePath, alignedDeviceSize)
	if err != nil {
		return nil, fmt.Errorf("creating cache device: %w", err)
	}

	// Create the ring.
	ring, err := NewBlockRing(device, config.BlockSize, blockCount)
	if err != nil {
		device.Close()
		return nil, fmt.Errorf("creating block ring: %w", err)
	}

	// Try to open existing index, or create a new one.
	indexPath := filepath.Join(config.Path, "cache.idx")
	index, err := OpenCacheIndex(indexPath, ring)
	if err != nil {
		// Index is missing or corrupt — create a new one and
		// attempt to rebuild from the device.
		index, err = NewCacheIndex(indexPath)
		if err != nil {
			device.Close()
			return nil, fmt.Errorf("creating cache index: %w", err)
		}

		// Rebuild is best-effort. If the device is also fresh,
		// there's nothing to rebuild and that's fine.
		_ = index.RebuildFromDevice(device, ring)
	}

	return &Cache{
		config: config,
		device: device,
		ring:   ring,
		index:  index,
		pinDir: pinDir,
	}, nil
}

// Get retrieves a container from the cache by its hash. Returns the
// raw container bytes and nil on cache hit, or nil and an error on
// cache miss. Checks both the ring and the pin store.
//
// Get is safe for concurrent use from multiple goroutines. The read
// path is lock-free beyond an RLock on the index map.
func (c *Cache) Get(containerHash Hash) ([]byte, error) {
	// Check pin directory first (pinned containers are never evicted).
	if data, err := os.ReadFile(c.pinnedPath(containerHash)); err == nil {
		return data, nil
	}

	// Look up in the ring index.
	location, found := c.index.Get(containerHash)
	if !found {
		return nil, fmt.Errorf("container %s not in cache", FormatHash(containerHash))
	}

	// Validate generation before allocating a read buffer.
	if !c.ring.IsValid(location) {
		c.index.Remove(containerHash)
		return nil, fmt.Errorf("container %s evicted (stale generation)", FormatHash(containerHash))
	}

	// Read from the ring.
	buffer := make([]byte, location.Length)
	if _, err := c.ring.Read(location, buffer); err != nil {
		c.index.Remove(containerHash)
		return nil, fmt.Errorf("reading container %s from ring: %w", FormatHash(containerHash), err)
	}

	// Verify the container hash. This catches torn reads (should
	// not happen due to reader counts, but defense in depth) and
	// data corruption.
	reader, err := ReadContainerIndex(bytes.NewReader(buffer))
	if err != nil {
		c.index.Remove(containerHash)
		return nil, fmt.Errorf("parsing cached container %s: %w", FormatHash(containerHash), err)
	}
	if reader.Hash != containerHash {
		c.index.Remove(containerHash)
		return nil, fmt.Errorf("container hash mismatch: expected %s, got %s",
			FormatHash(containerHash), FormatHash(reader.Hash))
	}

	return buffer, nil
}

// Put stores a container in the cache ring. The containerHash must
// match the actual hash of the data (the caller is responsible for
// computing it). Duplicate puts for the same hash are idempotent.
func (c *Cache) Put(containerHash Hash, data []byte) error {
	// Skip if already cached (and still valid).
	if location, found := c.index.Get(containerHash); found {
		if c.ring.IsValid(location) {
			return nil // already cached
		}
		c.index.Remove(containerHash)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	result, err := c.ring.Write(data)
	if err != nil {
		return fmt.Errorf("writing container %s to ring: %w", FormatHash(containerHash), err)
	}

	// If a block was evicted, remove its entries from the index.
	if result.EvictedBlock >= 0 {
		c.index.RemoveBlock(result.EvictedBlock, result.EvictedGeneration)
	}

	if err := c.index.Put(containerHash, result.Location); err != nil {
		return fmt.Errorf("indexing container %s: %w", FormatHash(containerHash), err)
	}

	// Auto-compact the index if it has grown large.
	if c.index.NeedsCompaction() {
		if err := c.index.Compact(c.ring); err != nil {
			// Compaction failure is non-fatal — the index still
			// works, it's just larger than necessary on disk.
			_ = err
		}
	}

	return nil
}

// Pin stores a container in the pin directory, exempt from ring
// eviction. Pinned containers persist until explicitly unpinned.
// The data is written atomically via temp file + rename, keyed
// by the container hash (not the CAS file-domain hash).
func (c *Cache) Pin(containerHash Hash, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	finalPath := c.pinnedPath(containerHash)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("creating pin shard directory: %w", err)
	}

	// Atomic write: temp file + rename.
	tmpFile, err := os.CreateTemp(c.pinDir, "pin-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp pin file: %w", err)
	}
	tmpPath := tmpFile.Name()

	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing pin data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp pin file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("renaming pin file: %w", err)
	}

	success = true
	return nil
}

// Unpin removes a container from the pin directory. The container may
// still exist in the ring cache if it was also Put there.
func (c *Cache) Unpin(containerHash Hash) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	path := c.pinnedPath(containerHash)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %s is not pinned", FormatHash(containerHash))
		}
		return fmt.Errorf("removing pinned container %s: %w", FormatHash(containerHash), err)
	}
	return nil
}

// Contains checks whether a container is in the cache (ring or pin
// store).
func (c *Cache) Contains(containerHash Hash) bool {
	if _, err := os.Stat(c.pinnedPath(containerHash)); err == nil {
		return true
	}
	location, found := c.index.Get(containerHash)
	if !found {
		return false
	}
	return c.ring.IsValid(location)
}

// Sync flushes the cache device and compacts the index.
func (c *Cache) Sync() error {
	if err := c.device.Sync(); err != nil {
		return fmt.Errorf("syncing cache device: %w", err)
	}
	if err := c.index.Compact(c.ring); err != nil {
		return fmt.Errorf("compacting index: %w", err)
	}
	return nil
}

// Close syncs and closes all cache resources.
func (c *Cache) Close() error {
	var firstErr error

	if err := c.device.Sync(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("syncing device on close: %w", err)
	}
	if err := c.index.Close(c.ring); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing index: %w", err)
	}
	if err := c.device.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing device: %w", err)
	}
	return firstErr
}

// CacheStats holds cache utilization metrics.
type CacheStats struct {
	DeviceSize       int64
	BlockSize        int64
	BlockCount       int
	LiveContainers   int
	PinnedContainers int
}

// Stats returns current cache utilization metrics.
func (c *Cache) Stats() CacheStats {
	pinnedCount := 0
	filepath.WalkDir(c.pinDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		// Count non-tmp files as pinned containers.
		if filepath.Ext(entry.Name()) != ".tmp" {
			pinnedCount++
		}
		return nil
	})

	return CacheStats{
		DeviceSize:       c.device.Size(),
		BlockSize:        c.ring.BlockSize(),
		BlockCount:       c.ring.BlockCount(),
		LiveContainers:   c.index.Len(),
		PinnedContainers: pinnedCount,
	}
}

// IsPinned returns whether a container is in the pin directory.
func (c *Cache) IsPinned(containerHash Hash) bool {
	_, err := os.Stat(c.pinnedPath(containerHash))
	return err == nil
}

// pinnedPath returns the sharded filesystem path for a pinned
// container. Uses two-level sharding by container hash hex:
// <pinDir>/<hex[:2]>/<hex[2:4]>/<hex>
func (c *Cache) pinnedPath(containerHash Hash) string {
	hex := FormatHash(containerHash)
	return filepath.Join(c.pinDir, hex[:2], hex[2:4], hex)
}
