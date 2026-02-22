// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"
)

func TestBufferFIFOOrdering(t *testing.T) {
	buffer := NewBuffer(1024)

	for i := byte(0); i < 5; i++ {
		if err := buffer.Push([]byte{i}); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	if buffer.Len() != 5 {
		t.Fatalf("expected 5 entries, got %d", buffer.Len())
	}

	for i := byte(0); i < 5; i++ {
		data := buffer.Peek()
		if !bytes.Equal(data, []byte{i}) {
			t.Fatalf("entry %d: expected [%d], got %v", i, i, data)
		}
		buffer.Pop()
	}

	if buffer.Len() != 0 {
		t.Fatalf("expected empty buffer, got %d entries", buffer.Len())
	}
}

func TestBufferSizeTracking(t *testing.T) {
	buffer := NewBuffer(1024)

	if buffer.SizeBytes() != 0 {
		t.Fatalf("expected 0 initial size, got %d", buffer.SizeBytes())
	}

	entry1 := make([]byte, 100)
	entry2 := make([]byte, 200)

	if err := buffer.Push(entry1); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if buffer.SizeBytes() != 100 {
		t.Fatalf("expected 100 bytes, got %d", buffer.SizeBytes())
	}

	if err := buffer.Push(entry2); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if buffer.SizeBytes() != 300 {
		t.Fatalf("expected 300 bytes, got %d", buffer.SizeBytes())
	}

	buffer.Pop()
	if buffer.SizeBytes() != 200 {
		t.Fatalf("expected 200 bytes after pop, got %d", buffer.SizeBytes())
	}
}

func TestBufferDropOldestOnOverflow(t *testing.T) {
	buffer := NewBuffer(300)

	// Fill with 3 entries of 100 bytes each.
	for i := byte(0); i < 3; i++ {
		entry := make([]byte, 100)
		entry[0] = i
		if err := buffer.Push(entry); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	if buffer.Dropped() != 0 {
		t.Fatalf("expected 0 drops, got %d", buffer.Dropped())
	}

	// Push a 4th entry — should drop the oldest.
	entry := make([]byte, 100)
	entry[0] = 3
	if err := buffer.Push(entry); err != nil {
		t.Fatalf("Push(3): %v", err)
	}

	if buffer.Dropped() != 1 {
		t.Fatalf("expected 1 drop, got %d", buffer.Dropped())
	}
	if buffer.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", buffer.Len())
	}

	// The oldest (0) was dropped; first entry should be 1.
	data := buffer.Peek()
	if data[0] != 1 {
		t.Fatalf("expected first entry to start with 1, got %d", data[0])
	}
}

func TestBufferDropMultipleOnOverflow(t *testing.T) {
	buffer := NewBuffer(500)

	// Fill with 5 entries of 100 bytes each (total 500).
	for i := byte(0); i < 5; i++ {
		entry := make([]byte, 100)
		entry[0] = i
		if err := buffer.Push(entry); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	// Push a 300-byte entry — must drop 3 old entries to fit.
	bigEntry := make([]byte, 300)
	bigEntry[0] = 99
	if err := buffer.Push(bigEntry); err != nil {
		t.Fatalf("Push(big): %v", err)
	}

	if buffer.Dropped() != 3 {
		t.Fatalf("expected 3 drops, got %d", buffer.Dropped())
	}
	if buffer.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", buffer.Len())
	}

	// Remaining entries: 3, 4, 99(big).
	data := buffer.Peek()
	if data[0] != 3 {
		t.Fatalf("expected first entry to start with 3, got %d", data[0])
	}
}

func TestBufferOversizedEntryRejected(t *testing.T) {
	buffer := NewBuffer(100)

	err := buffer.Push(make([]byte, 101))
	if err == nil {
		t.Fatal("expected error for oversized entry")
	}

	if buffer.Len() != 0 {
		t.Fatalf("buffer should be empty after rejected push, got %d", buffer.Len())
	}
}

func TestBufferEmptyEntryRejected(t *testing.T) {
	buffer := NewBuffer(100)

	err := buffer.Push([]byte{})
	if err == nil {
		t.Fatal("expected error for empty entry")
	}

	if buffer.Len() != 0 {
		t.Fatalf("buffer should be empty after rejected push, got %d", buffer.Len())
	}
}

func TestBufferPeekEmptyReturnsNil(t *testing.T) {
	buffer := NewBuffer(100)

	if data := buffer.Peek(); data != nil {
		t.Fatalf("expected nil from empty peek, got %v", data)
	}
}

func TestBufferPopEmptyIsNoOp(t *testing.T) {
	buffer := NewBuffer(100)

	// Should not panic.
	buffer.Pop()

	if buffer.Len() != 0 {
		t.Fatalf("expected 0 length, got %d", buffer.Len())
	}
}

func TestBufferNotifySignal(t *testing.T) {
	buffer := NewBuffer(1024)
	channel := buffer.Notify()

	// Initially no signal.
	select {
	case <-channel:
		t.Fatal("unexpected signal before push")
	default:
	}

	// Push sends a signal.
	if err := buffer.Push([]byte{1}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	select {
	case <-channel:
		// Expected.
	default:
		t.Fatal("expected signal after push")
	}

	// Second push while channel hasn't been drained should not block.
	// Drain first, then push twice.
	if err := buffer.Push([]byte{2}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := buffer.Push([]byte{3}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Should have exactly one signal queued.
	select {
	case <-channel:
	default:
		t.Fatal("expected signal after pushes")
	}

	select {
	case <-channel:
		t.Fatal("expected only one signal, got two")
	default:
	}
}

func TestBufferDropAccountingAccumulates(t *testing.T) {
	buffer := NewBuffer(100)

	// Each push of 100 bytes fills the buffer. Next push drops the
	// previous entry.
	for i := 0; i < 10; i++ {
		if err := buffer.Push(make([]byte, 100)); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}

	// First push didn't drop. Each subsequent push dropped one.
	if buffer.Dropped() != 9 {
		t.Fatalf("expected 9 drops, got %d", buffer.Dropped())
	}
}

func TestNewBufferPanicsOnNonPositiveMaxSize(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for maxSize=0")
		}
	}()
	NewBuffer(0)
}
