// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"bytes"
	"io"
	"strings"
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

	_ = buffer.String()
}

func TestNewFromString(t *testing.T) {
	buffer, err := NewFromString("test-secret")
	if err != nil {
		t.Fatalf("NewFromString failed: %v", err)
	}
	defer buffer.Close()

	if got := buffer.String(); got != "test-secret" {
		t.Errorf("expected %q, got %q", "test-secret", got)
	}
	if buffer.Len() != len("test-secret") {
		t.Errorf("expected length %d, got %d", len("test-secret"), buffer.Len())
	}
}

func TestNewFromString_Empty(t *testing.T) {
	_, err := NewFromString("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

func TestConcat_StringAndBuffer(t *testing.T) {
	token, err := NewFromBytes([]byte("my-token"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer token.Close()

	result, err := Concat("prefix:", token)
	if err != nil {
		t.Fatalf("Concat failed: %v", err)
	}
	defer result.Close()

	if got := result.String(); got != "prefix:my-token" {
		t.Errorf("expected %q, got %q", "prefix:my-token", got)
	}
}

func TestConcat_MultipleSegments(t *testing.T) {
	first, err := NewFromBytes([]byte("secret1"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer first.Close()

	second, err := NewFromBytes([]byte("secret2"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer second.Close()

	result, err := Concat("a:", first, ":", second, ":z")
	if err != nil {
		t.Fatalf("Concat failed: %v", err)
	}
	defer result.Close()

	if got := result.String(); got != "a:secret1:secret2:z" {
		t.Errorf("expected %q, got %q", "a:secret1:secret2:z", got)
	}
}

func TestConcat_StringsOnly(t *testing.T) {
	result, err := Concat("hello", " ", "world")
	if err != nil {
		t.Fatalf("Concat failed: %v", err)
	}
	defer result.Close()

	if got := result.String(); got != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", got)
	}
}

func TestConcat_SingleBuffer(t *testing.T) {
	original, err := NewFromBytes([]byte("only-this"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer original.Close()

	result, err := Concat(original)
	if err != nil {
		t.Fatalf("Concat failed: %v", err)
	}
	defer result.Close()

	if got := result.String(); got != "only-this" {
		t.Errorf("expected %q, got %q", "only-this", got)
	}
}

func TestConcat_EmptyResult(t *testing.T) {
	_, err := Concat("")
	if err == nil {
		t.Fatal("expected error for empty Concat result")
	}
}

func TestConcat_UnsupportedType(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic for unsupported segment type")
		}
	}()

	Concat("prefix", 42)
}

func TestNewFromReader(t *testing.T) {
	reader := strings.NewReader("secret-from-reader")
	buffer, err := NewFromReader(reader, 1024)
	if err != nil {
		t.Fatalf("NewFromReader failed: %v", err)
	}
	defer buffer.Close()

	if got := buffer.String(); got != "secret-from-reader" {
		t.Errorf("expected %q, got %q", "secret-from-reader", got)
	}
	if buffer.Len() != len("secret-from-reader") {
		t.Errorf("expected length %d, got %d", len("secret-from-reader"), buffer.Len())
	}
}

func TestNewFromReader_Empty(t *testing.T) {
	reader := strings.NewReader("")
	_, err := NewFromReader(reader, 1024)
	if err == nil {
		t.Fatal("expected error for empty reader")
	}
}

func TestNewFromReader_ExceedsMaxSize(t *testing.T) {
	reader := strings.NewReader("this is longer than five bytes")
	_, err := NewFromReader(reader, 5)
	if err == nil {
		t.Fatal("expected error when reader exceeds maxSize")
	}
}

func TestNewFromReader_ExactMaxSize(t *testing.T) {
	content := "exact"
	reader := strings.NewReader(content)
	buffer, err := NewFromReader(reader, len(content))
	if err != nil {
		t.Fatalf("NewFromReader at exact maxSize failed: %v", err)
	}
	defer buffer.Close()

	if got := buffer.String(); got != content {
		t.Errorf("expected %q, got %q", content, got)
	}
}

func TestNewFromReader_InvalidMaxSize(t *testing.T) {
	reader := strings.NewReader("data")
	_, err := NewFromReader(reader, 0)
	if err == nil {
		t.Fatal("expected error for zero maxSize")
	}

	_, err = NewFromReader(reader, -1)
	if err == nil {
		t.Fatal("expected error for negative maxSize")
	}
}

func TestNewFromReader_ReadError(t *testing.T) {
	reader := io.NopCloser(&failingReader{})
	_, err := NewFromReader(reader, 1024)
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
}

type failingReader struct{}

func (r *failingReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestEqual_Match(t *testing.T) {
	first, err := NewFromBytes([]byte("same-secret"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer first.Close()

	second, err := NewFromBytes([]byte("same-secret"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer second.Close()

	if !first.Equal(second) {
		t.Error("expected Equal to return true for identical content")
	}
}

func TestEqual_Mismatch(t *testing.T) {
	first, err := NewFromBytes([]byte("secret-a"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer first.Close()

	second, err := NewFromBytes([]byte("secret-b"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer second.Close()

	if first.Equal(second) {
		t.Error("expected Equal to return false for different content")
	}
}

func TestEqual_DifferentLengths(t *testing.T) {
	first, err := NewFromBytes([]byte("short"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer first.Close()

	second, err := NewFromBytes([]byte("much-longer-secret"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer second.Close()

	if first.Equal(second) {
		t.Error("expected Equal to return false for different lengths")
	}
}

func TestWriteTo(t *testing.T) {
	buffer, err := NewFromBytes([]byte("write-me-out"))
	if err != nil {
		t.Fatalf("NewFromBytes failed: %v", err)
	}
	defer buffer.Close()

	var output bytes.Buffer
	written, err := buffer.WriteTo(&output)
	if err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	if written != int64(len("write-me-out")) {
		t.Errorf("expected %d bytes written, got %d", len("write-me-out"), written)
	}
	if output.String() != "write-me-out" {
		t.Errorf("expected %q, got %q", "write-me-out", output.String())
	}
}

func TestWriteTo_PanicsAfterClose(t *testing.T) {
	buffer, err := New(16)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	buffer.Close()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic on WriteTo after Close")
		}
	}()

	var output bytes.Buffer
	buffer.WriteTo(&output)
}
