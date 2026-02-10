// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"testing"
)

func TestNew_ValidSize(t *testing.T) {
	buffer, err := New(64)
	if err != nil {
		t.Fatalf("New(64) failed: %v", err)
	}
	defer buffer.Close()

	if buffer.Len() != 64 {
		t.Errorf("expected length 64, got %d", buffer.Len())
	}

	data := buffer.Bytes()
	if len(data) != 64 {
		t.Errorf("expected Bytes() length 64, got %d", len(data))
	}

	// Memory should be zero-initialized by mmap.
	for index, value := range data {
		if value != 0 {
			t.Fatalf("expected zero at index %d, got %d", index, value)
		}
	}
}

func TestNew_ZeroSize(t *testing.T) {
	_, err := New(0)
	if err == nil {
		t.Fatal("expected error for zero size")
	}
}

func TestNew_NegativeSize(t *testing.T) {
	_, err := New(-1)
	if err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestNewFromBytes(t *testing.T) {
	source := []byte("super-secret-password")
	originalContent := string(source)

	buffer, err := NewFromBytes(source)
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer buffer.Close()

	// The buffer should contain the original data.
	if got := buffer.String(); got != originalContent {
		t.Errorf("expected %q, got %q", originalContent, got)
	}

	// The source slice should have been zeroed.
	for index, value := range source {
		if value != 0 {
			t.Fatalf("source byte %d was not zeroed: got %d", index, value)
		}
	}
}

func TestNewFromBytes_Empty(t *testing.T) {
	_, err := NewFromBytes([]byte{})
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestBuffer_WriteAndRead(t *testing.T) {
	buffer, err := New(16)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer buffer.Close()

	// Write directly into the buffer.
	data := buffer.Bytes()
	copy(data, []byte("hello, secrets!"))

	if got := buffer.String(); got != "hello, secrets!\x00" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestBuffer_Close_ZerosMemory(t *testing.T) {
	buffer, err := New(32)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Write some data.
	data := buffer.Bytes()
	copy(data, []byte("this should be zeroed"))

	if err := buffer.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// After close, internal data is nil.
	if buffer.data != nil {
		t.Error("expected data to be nil after Close")
	}
}

func TestBuffer_Close_Idempotent(t *testing.T) {
	buffer, err := New(16)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if err := buffer.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}

	// Second close should be a no-op.
	if err := buffer.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

func TestBuffer_Bytes_PanicsAfterClose(t *testing.T) {
	buffer, err := New(16)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	buffer.Close()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic on Bytes() after Close")
		}
	}()

	buffer.Bytes()
}

func TestBuffer_String_PanicsAfterClose(t *testing.T) {
	buffer, err := New(16)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	buffer.Close()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic on String() after Close")
		}
	}()

	buffer.String()
}
