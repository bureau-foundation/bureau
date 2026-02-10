// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import "sync"

// DefaultRingBufferSize is the default ring buffer capacity in bytes.
// 1 MB holds roughly 500K-1M lines of typical terminal output — hours
// of coding agent activity.
const DefaultRingBufferSize = 1024 * 1024

// RingBuffer is a fixed-size circular buffer that stores raw terminal
// output bytes with sequence number tracking. It preserves terminal
// escape sequences for full-fidelity history replay.
//
// The buffer tracks a monotonically increasing byte offset so observers
// can request "everything since offset N" for reconnection gap-fill.
// New writes overwrite the oldest data when the buffer is full.
//
// All methods are safe for concurrent use.
type RingBuffer struct {
	mutex    sync.Mutex
	data     []byte
	capacity int
	// writePosition is the next position to write within the circular buffer
	// (0 to capacity-1).
	writePosition int
	// totalWritten is the total number of bytes ever written. The current
	// buffer contents span from offset (totalWritten - stored) to totalWritten,
	// where stored = min(totalWritten, capacity).
	totalWritten uint64
}

// NewRingBuffer creates a ring buffer with the given capacity in bytes.
// Use DefaultRingBufferSize for the standard 1 MB buffer.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		data:     make([]byte, capacity),
		capacity: capacity,
	}
}

// Write appends bytes to the ring buffer, advancing the sequence offset.
// Overwrites the oldest data if the buffer is full.
func (ring *RingBuffer) Write(data []byte) {
	ring.mutex.Lock()
	defer ring.mutex.Unlock()

	for offset := 0; offset < len(data); {
		available := ring.capacity - ring.writePosition
		copyLength := len(data) - offset
		if copyLength > available {
			copyLength = available
		}
		copy(ring.data[ring.writePosition:ring.writePosition+copyLength], data[offset:offset+copyLength])
		ring.writePosition = (ring.writePosition + copyLength) % ring.capacity
		offset += copyLength
	}
	ring.totalWritten += uint64(len(data))
}

// ReadFrom returns all bytes written since the given sequence offset.
// If the offset is older than the buffer's oldest retained data, returns
// everything currently in the buffer (the caller missed some data).
// Returns nil if offset is at or beyond the current write position.
func (ring *RingBuffer) ReadFrom(offset uint64) []byte {
	ring.mutex.Lock()
	defer ring.mutex.Unlock()

	if offset >= ring.totalWritten {
		return nil
	}

	storedLength := ring.totalWritten
	if storedLength > uint64(ring.capacity) {
		storedLength = uint64(ring.capacity)
	}
	oldestOffset := ring.totalWritten - storedLength

	// If the requested offset is older than what we have, return everything.
	readOffset := offset
	if readOffset < oldestOffset {
		readOffset = oldestOffset
	}

	bytesToRead := ring.totalWritten - readOffset
	if bytesToRead == 0 {
		return nil
	}

	result := make([]byte, bytesToRead)

	// Calculate the read start position in the circular buffer.
	// writePosition points to the next write location. Data runs from
	// (writePosition - storedLength) to writePosition, wrapping around.
	readPosition := (ring.writePosition - int(storedLength) + int(readOffset-oldestOffset)) % ring.capacity
	if readPosition < 0 {
		readPosition += ring.capacity
	}

	for copied := 0; copied < int(bytesToRead); {
		available := ring.capacity - readPosition
		copyLength := int(bytesToRead) - copied
		if copyLength > available {
			copyLength = available
		}
		copy(result[copied:copied+copyLength], ring.data[readPosition:readPosition+copyLength])
		readPosition = (readPosition + copyLength) % ring.capacity
		copied += copyLength
	}

	return result
}

// CurrentOffset returns the total number of bytes written to the buffer.
// This is the sequence number an observer should store and pass to
// ReadFrom on reconnect.
func (ring *RingBuffer) CurrentOffset() uint64 {
	ring.mutex.Lock()
	defer ring.mutex.Unlock()
	return ring.totalWritten
}
