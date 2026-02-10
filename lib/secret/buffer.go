// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package secret provides a memory-safe buffer for sensitive data such as
// passwords, access tokens, and encryption keys.
//
// Buffer allocates memory outside the Go heap via mmap(MAP_ANONYMOUS),
// locks it into physical RAM via mlock (preventing swap), and marks it
// excluded from core dumps via madvise(MADV_DONTDUMP). On Close, the
// memory is zeroed, unlocked, and unmapped.
//
// Because the memory is allocated outside the Go heap, the garbage
// collector never sees it and cannot copy or relocate it. This is the
// only way to guarantee that secret material does not persist in memory
// after it is no longer needed.
package secret

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// Buffer holds sensitive data in memory that is locked against swapping,
// excluded from core dumps, and zeroed on close. The backing memory is
// allocated via mmap outside the Go heap.
//
// A Buffer must not be copied after creation. Use Close to release the
// memory when the secret is no longer needed. After Close, any access
// to the buffer's contents will panic.
type Buffer struct {
	mu     sync.Mutex
	data   []byte
	length int
	closed bool
}

// New allocates a new secret buffer of the given size. The buffer is
// backed by an anonymous mmap region that is:
//   - Locked into physical RAM (mlock), preventing swap
//   - Excluded from core dumps (MADV_DONTDUMP)
//   - Outside the Go heap, invisible to the garbage collector
//
// The caller must call Close when the secret is no longer needed.
func New(size int) (*Buffer, error) {
	if size <= 0 {
		return nil, fmt.Errorf("secret: buffer size must be positive, got %d", size)
	}

	// Allocate anonymous memory outside the Go heap.
	data, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("secret: mmap failed: %w", err)
	}

	// Lock the memory to prevent it from being swapped to disk.
	if err := unix.Mlock(data); err != nil {
		unix.Munmap(data)
		return nil, fmt.Errorf("secret: mlock failed: %w", err)
	}

	// Exclude from core dumps.
	if err := unix.Madvise(data, unix.MADV_DONTDUMP); err != nil {
		// Non-fatal: the secret is still protected against swap.
		// MADV_DONTDUMP may not be supported on all kernels.
		unix.Munlock(data)
		unix.Munmap(data)
		return nil, fmt.Errorf("secret: madvise(MADV_DONTDUMP) failed: %w", err)
	}

	return &Buffer{
		data:   data,
		length: size,
	}, nil
}

// NewFromBytes creates a secret buffer from existing data. The source
// bytes are copied into the protected region and then zeroed in place,
// so the caller's original slice no longer holds the secret.
func NewFromBytes(source []byte) (*Buffer, error) {
	if len(source) == 0 {
		return nil, fmt.Errorf("secret: cannot create buffer from empty source")
	}

	buffer, err := New(len(source))
	if err != nil {
		return nil, err
	}

	copy(buffer.data, source)

	// Zero the caller's copy.
	for index := range source {
		source[index] = 0
	}

	return buffer, nil
}

// Bytes returns the secret data. The returned slice points directly into
// the mmap region — do not hold references to it beyond the lifetime of
// the Buffer. Panics if the buffer has been closed.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		panic("secret: read from closed buffer")
	}

	return b.data[:b.length]
}

// String returns the secret data as a string. The returned string is
// backed by a heap-allocated copy (Go strings are immutable and must
// live on the heap), so this should only be used at API boundaries
// that require string arguments. Prefer Bytes() when possible.
//
// Panics if the buffer has been closed.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		panic("secret: read from closed buffer")
	}

	return string(b.data[:b.length])
}

// Len returns the size of the secret data.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.length
}

// Close zeros the buffer contents, unlocks and unmaps the memory.
// After Close, any access to the buffer's Bytes() will panic.
// Close is idempotent.
func (b *Buffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}
	b.closed = true

	// Zero the contents before releasing.
	for index := range b.data {
		b.data[index] = 0
	}

	// Unlock and unmap. Errors here are logged but not fatal —
	// the memory will be released when the process exits regardless.
	var firstError error
	if err := unix.Munlock(b.data); err != nil && firstError == nil {
		firstError = fmt.Errorf("secret: munlock failed: %w", err)
	}
	if err := unix.Munmap(b.data); err != nil && firstError == nil {
		firstError = fmt.Errorf("secret: munmap failed: %w", err)
	}

	b.data = nil
	return firstError
}
