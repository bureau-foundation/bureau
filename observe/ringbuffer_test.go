// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bytes"
	"testing"
)

func TestRingBufferBasicWriteRead(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	ring.Write([]byte("hello"))
	ring.Write([]byte(" world"))

	got := ring.ReadFrom(0)
	if !bytes.Equal(got, []byte("hello world")) {
		t.Errorf("ReadFrom(0): got %q, want %q", got, "hello world")
	}
}

func TestRingBufferReadFromOffset(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	ring.Write([]byte("abcde"))
	ring.Write([]byte("fghij"))

	// Read from offset 5 should skip "abcde" and return "fghij".
	got := ring.ReadFrom(5)
	if !bytes.Equal(got, []byte("fghij")) {
		t.Errorf("ReadFrom(5): got %q, want %q", got, "fghij")
	}
}

func TestRingBufferReadFromCurrentOffset(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	ring.Write([]byte("data"))

	// Reading from the current offset should return nil (nothing new).
	offset := ring.CurrentOffset()
	got := ring.ReadFrom(offset)
	if got != nil {
		t.Errorf("ReadFrom(current): got %q, want nil", got)
	}
}

func TestRingBufferReadFromFutureOffset(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	ring.Write([]byte("data"))

	// Reading from beyond the current offset should return nil.
	got := ring.ReadFrom(ring.CurrentOffset() + 100)
	if got != nil {
		t.Errorf("ReadFrom(future): got %q, want nil", got)
	}
}

func TestRingBufferWrapAround(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(10)

	// Write 15 bytes into a 10-byte buffer. The first 5 bytes are lost.
	ring.Write([]byte("abcdefghijklmno"))

	got := ring.ReadFrom(0)
	if !bytes.Equal(got, []byte("fghijklmno")) {
		t.Errorf("ReadFrom(0) after wrap: got %q, want %q", got, "fghijklmno")
	}

	if ring.CurrentOffset() != 15 {
		t.Errorf("CurrentOffset: got %d, want 15", ring.CurrentOffset())
	}
}

func TestRingBufferWrapAroundPartialRead(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(10)

	ring.Write([]byte("abcdefghijklmno")) // 15 bytes, buffer holds "fghijklmno"

	// Read from offset 8 should return "ijklmno" (bytes 8-14).
	got := ring.ReadFrom(8)
	if !bytes.Equal(got, []byte("ijklmno")) {
		t.Errorf("ReadFrom(8): got %q, want %q", got, "ijklmno")
	}
}

func TestRingBufferIncrementalWrites(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(10)

	// Write byte by byte to test wrapping with small writes.
	for i := 0; i < 25; i++ {
		ring.Write([]byte{byte('a' + i%26)})
	}

	// Buffer should hold the last 10 bytes: "pqrstuvwxy"
	got := ring.ReadFrom(0)
	want := []byte("pqrstuvwxy")
	if !bytes.Equal(got, want) {
		t.Errorf("ReadFrom(0): got %q, want %q", got, want)
	}
}

func TestRingBufferCurrentOffset(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	if ring.CurrentOffset() != 0 {
		t.Errorf("initial offset: got %d, want 0", ring.CurrentOffset())
	}

	ring.Write([]byte("hello"))
	if ring.CurrentOffset() != 5 {
		t.Errorf("after write: got %d, want 5", ring.CurrentOffset())
	}

	ring.Write([]byte(" world"))
	if ring.CurrentOffset() != 11 {
		t.Errorf("after second write: got %d, want 11", ring.CurrentOffset())
	}
}

func TestRingBufferEmptyRead(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	got := ring.ReadFrom(0)
	if got != nil {
		t.Errorf("ReadFrom(0) on empty buffer: got %q, want nil", got)
	}
}

func TestRingBufferPreservesEscapeSequences(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(1024)

	// Terminal escape sequences must be preserved byte-for-byte.
	escapeData := []byte("\x1b[31mred\x1b[0m \x1b[1;32mbold green\x1b[0m\n")
	ring.Write(escapeData)

	got := ring.ReadFrom(0)
	if !bytes.Equal(got, escapeData) {
		t.Errorf("escape sequences not preserved: got %v, want %v", got, escapeData)
	}
}

func TestRingBufferLargeWrite(t *testing.T) {
	t.Parallel()
	ring := NewRingBuffer(100)

	// Write more than the buffer capacity in a single call.
	data := make([]byte, 250)
	for i := range data {
		data[i] = byte(i % 256)
	}
	ring.Write(data)

	got := ring.ReadFrom(0)
	if len(got) != 100 {
		t.Fatalf("ReadFrom(0): got %d bytes, want 100", len(got))
	}
	// Should contain the last 100 bytes of the input.
	if !bytes.Equal(got, data[150:]) {
		t.Error("large write: buffer does not contain the last 100 bytes")
	}
}
