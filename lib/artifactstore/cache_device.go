// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package artifactstore

import (
	"fmt"
	"io"
	"runtime/debug"

	"golang.org/x/sys/unix"
)

// CacheDevice is a fixed-size file used as the backing store for the
// artifact cache. Reads go through a read-only memory map for
// zero-syscall overhead; writes use pwrite to avoid triggering
// read-before-write page faults.
//
// CacheDevice is safe for concurrent use. ReadAt calls are lock-free
// (they access the memory map directly). WriteAt calls must be
// serialized by the caller (single writer).
type CacheDevice struct {
	fd   int
	data []byte // mmap'd MAP_SHARED, PROT_READ
	size int64
}

// NewCacheDevice creates or opens a cache device file at the given
// path. If the file does not exist, it is created at the requested
// size. If it exists and matches the requested size, it is opened
// as-is. If it exists at a different size, an error is returned —
// delete the file and let it be recreated to resize.
func NewCacheDevice(path string, size int64) (*CacheDevice, error) {
	if size <= 0 {
		return nil, fmt.Errorf("cache device size must be positive, got %d", size)
	}

	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening cache device %s: %w", path, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("stating cache device: %w", err)
	}

	if stat.Size == 0 {
		// New file — truncate to requested size.
		if err := unix.Ftruncate(fd, size); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("truncating new cache device to %d bytes: %w", size, err)
		}
	} else if stat.Size != size {
		unix.Close(fd)
		return nil, fmt.Errorf("cache device %s is %d bytes but %d was requested; delete the file to resize",
			path, stat.Size, size)
	}

	// Memory-map the file read-only. Writes go through pwrite()
	// and the kernel updates the shared mapping automatically.
	data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("memory-mapping cache device: %w", err)
	}

	return &CacheDevice{
		fd:   fd,
		data: data,
		size: size,
	}, nil
}

// ReadAt reads len(p) bytes from the device starting at byte offset
// off. Reads go through the memory map — no system call overhead for
// data that is in the page cache.
func (d *CacheDevice) ReadAt(p []byte, off int64) (readCount int, err error) {
	if off < 0 || off >= d.size {
		return 0, io.EOF
	}

	// Guard against page faults from I/O errors on the underlying
	// storage (e.g., disk failure). Without this, a SIGBUS would
	// crash the process.
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if r := recover(); r != nil {
			err = fmt.Errorf("page fault reading cache device at offset %d: %v", off, r)
		}
	}()

	readCount = copy(p, d.data[off:])
	if readCount < len(p) {
		return readCount, io.EOF
	}
	return readCount, nil
}

// WriteAt writes len(p) bytes to the device starting at byte offset
// off. Writes use pwrite() to avoid triggering read-before-write
// page faults on the memory map.
func (d *CacheDevice) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > d.size {
		return 0, fmt.Errorf("write at offset %d with length %d exceeds device size %d",
			off, len(p), d.size)
	}

	totalWritten := 0
	for len(p) > 0 {
		written, err := unix.Pwrite(d.fd, p, off)
		totalWritten += written
		if err != nil {
			return totalWritten, fmt.Errorf("pwrite at offset %d: %w", off, err)
		}
		p = p[written:]
		off += int64(written)
	}
	return totalWritten, nil
}

// Sync flushes all pending writes to the underlying storage.
func (d *CacheDevice) Sync() error {
	return unix.Fsync(d.fd)
}

// Close unmaps the memory region and closes the file descriptor.
func (d *CacheDevice) Close() error {
	var firstErr error
	if err := unix.Munmap(d.data); err != nil {
		firstErr = fmt.Errorf("unmapping cache device: %w", err)
	}
	if err := unix.Close(d.fd); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing cache device fd: %w", err)
	}
	d.data = nil
	d.fd = -1
	return firstErr
}

// Size returns the device size in bytes.
func (d *CacheDevice) Size() int64 {
	return d.size
}
