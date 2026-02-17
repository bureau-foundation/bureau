// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureInlineOutput(t *testing.T) {
	t.Parallel()

	t.Run("trailing newline trimmed", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "output.txt")
		if err := os.WriteFile(path, []byte("abc123\n"), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		value, err := captureInlineOutput(path)
		if err != nil {
			t.Fatalf("captureInlineOutput: %v", err)
		}
		if value != "abc123" {
			t.Errorf("value = %q, want %q", value, "abc123")
		}
	})

	t.Run("trailing whitespace trimmed leading preserved", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "output.txt")
		if err := os.WriteFile(path, []byte("  value  \n\n"), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		value, err := captureInlineOutput(path)
		if err != nil {
			t.Fatalf("captureInlineOutput: %v", err)
		}
		if value != "  value" {
			t.Errorf("value = %q, want %q", value, "  value")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "does-not-exist.txt")
		_, err := captureInlineOutput(path)
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("exceeds size limit", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "large.txt")

		// Create a file that exceeds maxInlineOutputSize (64 KB).
		oversizedContent := make([]byte, maxInlineOutputSize+1)
		for index := range oversizedContent {
			oversizedContent[index] = 'x'
		}
		if err := os.WriteFile(path, oversizedContent, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := captureInlineOutput(path)
		if err == nil {
			t.Fatal("expected error for oversized file")
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("error should mention limit, got: %v", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()

		directory := t.TempDir()
		path := filepath.Join(directory, "empty.txt")
		if err := os.WriteFile(path, []byte{}, 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		value, err := captureInlineOutput(path)
		if err != nil {
			t.Fatalf("captureInlineOutput: %v", err)
		}
		if value != "" {
			t.Errorf("value = %q, want empty string", value)
		}
	})
}
