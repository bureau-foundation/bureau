// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sync"
)

// Buffer is a size-bounded FIFO queue of CBOR-encoded telemetry
// batches. When a Push would exceed the byte limit, the oldest
// entries are dropped until the new entry fits. This provides
// backpressure when the shipper can't keep up: the relay loses old
// data rather than exhausting memory.
//
// The notify channel (capacity 1) signals the shipper goroutine when
// new data is available. The shipper selects on Notify() alongside
// context cancellation.
//
// Thread-safe: all methods may be called concurrently.
type Buffer struct {
	mu        sync.Mutex
	entries   []bufferEntry
	totalSize int
	maxSize   int
	dropped   uint64
	notify    chan struct{}
}

// bufferEntry is a single CBOR-encoded batch with its byte size
// cached for O(1) accounting.
type bufferEntry struct {
	data []byte
	size int
}

// NewBuffer creates a Buffer with the given maximum byte capacity.
// The maxSize must be positive.
func NewBuffer(maxSize int) *Buffer {
	if maxSize <= 0 {
		panic(fmt.Sprintf("buffer: maxSize must be positive, got %d", maxSize))
	}
	return &Buffer{
		maxSize: maxSize,
		notify:  make(chan struct{}, 1),
	}
}

// Push appends a CBOR-encoded batch to the buffer. If the single
// entry exceeds maxSize, Push returns an error (this indicates a
// configuration problem — the flush threshold should be well below
// the buffer size). If adding the entry would exceed maxSize, the
// oldest entries are dropped until it fits. Each dropped entry
// increments the Dropped counter.
func (b *Buffer) Push(data []byte) error {
	size := len(data)
	if size > b.maxSize {
		return fmt.Errorf("buffer: entry size %d exceeds max buffer size %d", size, b.maxSize)
	}
	if size == 0 {
		return fmt.Errorf("buffer: refusing to push empty entry")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Drop oldest entries until there's room.
	for b.totalSize+size > b.maxSize && len(b.entries) > 0 {
		evicted := b.entries[0]
		b.entries[0] = bufferEntry{} // release data for GC
		b.entries = b.entries[1:]
		b.totalSize -= evicted.size
		b.dropped++
	}

	b.entries = append(b.entries, bufferEntry{data: data, size: size})
	b.totalSize += size

	// Non-blocking signal to the shipper.
	select {
	case b.notify <- struct{}{}:
	default:
	}

	return nil
}

// Peek returns the oldest entry without removing it. Returns nil if
// the buffer is empty.
func (b *Buffer) Peek() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return nil
	}
	return b.entries[0].data
}

// Pop removes the oldest entry. No-op if the buffer is empty.
func (b *Buffer) Pop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return
	}
	evicted := b.entries[0]
	b.entries[0] = bufferEntry{} // release data for GC
	b.entries = b.entries[1:]
	b.totalSize -= evicted.size
}

// Len returns the number of entries in the buffer.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// SizeBytes returns the total byte size of all entries in the buffer.
func (b *Buffer) SizeBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totalSize
}

// Dropped returns the total number of entries dropped due to buffer
// overflow since creation.
func (b *Buffer) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Notify returns a channel that receives a signal (at most once per
// Push) when new data is available. The shipper goroutine selects on
// this channel alongside its context to wake up for shipping.
func (b *Buffer) Notify() <-chan struct{} {
	return b.notify
}
