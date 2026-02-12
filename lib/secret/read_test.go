// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFromPath_File(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			name:     "plain value",
			content:  "my-secret-token",
			expected: "my-secret-token",
		},
		{
			name:     "trailing newline",
			content:  "my-secret-token\n",
			expected: "my-secret-token",
		},
		{
			name:     "trailing whitespace",
			content:  "my-secret-token  \n",
			expected: "my-secret-token",
		},
		{
			name:     "leading whitespace",
			content:  "  my-secret-token",
			expected: "my-secret-token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(tempDir, test.name)
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatalf("writing test file: %v", err)
			}

			result, err := ReadFromPath(path)
			if err != nil {
				t.Fatalf("ReadFromPath() error: %v", err)
			}
			defer result.Close()
			if result.String() != test.expected {
				t.Errorf("ReadFromPath() = %q, want %q", result.String(), test.expected)
			}
		})
	}
}

func TestReadFromPath_FileNotFound(t *testing.T) {
	_, err := ReadFromPath("/nonexistent/path/to/secret")
	if err == nil {
		t.Error("ReadFromPath() with nonexistent file should return error")
	}
}

func TestReadFromPath_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	_, err := ReadFromPath(path)
	if err == nil {
		t.Error("ReadFromPath() with empty file should return error")
	}
}

func TestReadFromPath_WhitespaceOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitespace")
	if err := os.WriteFile(path, []byte("   \n\t\n"), 0600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	_, err := ReadFromPath(path)
	if err == nil {
		t.Error("ReadFromPath() with whitespace-only file should return error")
	}
}
