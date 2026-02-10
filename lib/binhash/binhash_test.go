// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package binhash

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile(t *testing.T) {
	content := []byte("hello, bureau")
	path := filepath.Join(t.TempDir(), "test-binary")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	want := sha256.Sum256(content)
	if got != want {
		t.Errorf("HashFile = %x, want %x", got, want)
	}
}

func TestHashFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	want := sha256.Sum256(nil)
	if got != want {
		t.Errorf("HashFile(empty) = %x, want %x", got, want)
	}
}

func TestHashFileNonexistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := HashFile(path)
	if err == nil {
		t.Fatal("HashFile should fail for nonexistent file")
	}
}

func TestHashFileLarge(t *testing.T) {
	// Ensure streaming works for files larger than typical buffers.
	content := make([]byte, 256*1024) // 256KB
	for i := range content {
		content[i] = byte(i % 251) // Prime modulus to avoid simple patterns.
	}
	path := filepath.Join(t.TempDir(), "large-binary")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	want := sha256.Sum256(content)
	if got != want {
		t.Errorf("HashFile(large) = %x, want %x", got, want)
	}
}

func TestHashFileDeterministic(t *testing.T) {
	content := []byte("determinism check")
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first, err := HashFile(path)
	if err != nil {
		t.Fatalf("first HashFile: %v", err)
	}

	second, err := HashFile(path)
	if err != nil {
		t.Fatalf("second HashFile: %v", err)
	}

	if first != second {
		t.Errorf("HashFile not deterministic: %x != %x", first, second)
	}
}

func TestHashFileDifferentContent(t *testing.T) {
	directory := t.TempDir()

	path1 := filepath.Join(directory, "file1")
	if err := os.WriteFile(path1, []byte("content A"), 0755); err != nil {
		t.Fatalf("WriteFile file1: %v", err)
	}

	path2 := filepath.Join(directory, "file2")
	if err := os.WriteFile(path2, []byte("content B"), 0755); err != nil {
		t.Fatalf("WriteFile file2: %v", err)
	}

	hash1, err := HashFile(path1)
	if err != nil {
		t.Fatalf("HashFile(file1): %v", err)
	}

	hash2, err := HashFile(path2)
	if err != nil {
		t.Fatalf("HashFile(file2): %v", err)
	}

	if hash1 == hash2 {
		t.Error("different files should produce different hashes")
	}
}

func TestFormatDigest(t *testing.T) {
	digest := sha256.Sum256([]byte("test"))
	formatted := FormatDigest(digest)
	if length := len(formatted); length != 64 {
		t.Errorf("FormatDigest length = %d, want 64", length)
	}
}

func TestParseDigestRoundTrip(t *testing.T) {
	original := sha256.Sum256([]byte("round-trip"))
	formatted := FormatDigest(original)

	parsed, err := ParseDigest(formatted)
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseDigest round-trip failed: %x != %x", parsed, original)
	}
}

func TestParseDigestInvalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"not hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"too short", "abcd"},
		{"too long", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789aa"},
		{"empty", ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseDigest(test.input)
			if err == nil {
				t.Errorf("ParseDigest(%q) should fail", test.input)
			}
		})
	}
}
