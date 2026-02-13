// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeHexInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{
			name:  "lowercase hex",
			input: "a1636b657963766174",
			want:  []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74},
		},
		{
			name:  "uppercase hex",
			input: "A1636B657963766174",
			want:  []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74},
		},
		{
			name:  "hex with spaces",
			input: "a1 63 6b 65 79 63 76 61 74",
			want:  []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74},
		},
		{
			name:  "hex with newlines",
			input: "a163\n6b6579\n63766174\n",
			want:  []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74},
		},
		{
			name:  "hex with tabs and mixed whitespace",
			input: "a1\t63 6b65\n79 63\t76 61 74",
			want:  []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74},
		},
		{
			name:    "invalid hex",
			input:   "not hex data",
			wantErr: true,
		},
		{
			name:    "empty after whitespace",
			input:   "   \n\t  ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeHexInput([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("got %x, want %x", got, tt.want)
			}
		})
	}
}

func TestReadInput_FileArg(t *testing.T) {
	content := []byte("test content for file arg")
	tempFile := filepath.Join(t.TempDir(), "test.cbor")
	if err := os.WriteFile(tempFile, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	data, remainingArgs, err := readInput([]string{tempFile}, false)
	if err != nil {
		t.Fatalf("readInput: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("data = %q, want %q", data, content)
	}
	if len(remainingArgs) != 0 {
		t.Errorf("remainingArgs = %v, want empty", remainingArgs)
	}
}

func TestReadInput_FileArgWithLeadingArgs(t *testing.T) {
	content := []byte("file content")
	tempFile := filepath.Join(t.TempDir(), "input.cbor")
	if err := os.WriteFile(tempFile, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	data, remainingArgs, err := readInput([]string{".action", tempFile}, false)
	if err != nil {
		t.Fatalf("readInput: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("data = %q, want %q", data, content)
	}
	if len(remainingArgs) != 1 || remainingArgs[0] != ".action" {
		t.Errorf("remainingArgs = %v, want [\".action\"]", remainingArgs)
	}
}

func TestReadInput_HexModeFromFile(t *testing.T) {
	hexContent := []byte("a1 63 6b 65 79 63 76 61 74\n")
	tempFile := filepath.Join(t.TempDir(), "test.hex")
	if err := os.WriteFile(tempFile, hexContent, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	data, _, err := readInput([]string{tempFile}, true)
	if err != nil {
		t.Fatalf("readInput: %v", err)
	}
	want := []byte{0xa1, 0x63, 0x6b, 0x65, 0x79, 0x63, 0x76, 0x61, 0x74}
	if !bytes.Equal(data, want) {
		t.Errorf("data = %x, want %x", data, want)
	}
}

func TestReadInput_DirectoryNotTreatedAsFile(t *testing.T) {
	directory := t.TempDir()

	// A directory name as the last arg should not be treated as a
	// file. readInput should fall through to stdin. Since stdin in
	// tests is /dev/null, this will return empty data.
	data, remainingArgs, err := readInput([]string{directory}, false)
	if err != nil {
		t.Fatalf("readInput: %v", err)
	}
	// The directory arg stays in remainingArgs because it wasn't consumed.
	if len(remainingArgs) != 1 {
		t.Errorf("remainingArgs length = %d, want 1", len(remainingArgs))
	}
	// Data comes from stdin (/dev/null in tests) — likely empty.
	_ = data
}
