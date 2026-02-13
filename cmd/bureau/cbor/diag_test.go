// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func TestDiagCBOR(t *testing.T) {
	tests := []struct {
		name  string
		input any
		// Substrings that must appear in the diagnostic output.
		wantContains []string
	}{
		{
			name:         "string value",
			input:        map[string]any{"action": "status"},
			wantContains: []string{`"action"`, `"status"`},
		},
		{
			name:         "integer value",
			input:        map[string]any{"count": int64(42)},
			wantContains: []string{`"count"`, "42"},
		},
		{
			name:         "boolean and null",
			input:        map[string]any{"flag": true, "empty": nil},
			wantContains: []string{"true", "null"},
		},
		{
			name:         "array",
			input:        []any{int64(1), int64(2), int64(3)},
			wantContains: []string{"1", "2", "3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cborData, err := codec.Marshal(tt.input)
			if err != nil {
				t.Fatalf("marshal CBOR: %v", err)
			}

			var output bytes.Buffer
			if err := diagCBOR(bytes.NewReader(cborData), &output); err != nil {
				t.Fatalf("diagCBOR: %v", err)
			}

			result := output.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(result, want) {
					t.Errorf("output %q does not contain %q", result, want)
				}
			}
		})
	}
}

func TestDiagCBOR_IntegerKeys(t *testing.T) {
	// keyasint encoding: integer keys should show as integers in
	// diagnostic notation (not strings).
	type intKeyStruct struct {
		Subject string `cbor:"1,keyasint"`
		Machine string `cbor:"2,keyasint"`
	}

	input := intKeyStruct{Subject: "test/agent", Machine: "workstation"}
	cborData, err := codec.Marshal(input)
	if err != nil {
		t.Fatalf("marshal CBOR: %v", err)
	}

	var output bytes.Buffer
	if err := diagCBOR(bytes.NewReader(cborData), &output); err != nil {
		t.Fatalf("diagCBOR: %v", err)
	}

	result := output.String()
	// Diagnostic notation should show integer keys like {1: "test/agent", 2: "workstation"}
	// not string keys like {"1": "test/agent"}.
	if !strings.Contains(result, "1:") && !strings.Contains(result, "1 :") {
		t.Errorf("expected integer key 1 in diagnostic notation, got: %q", result)
	}
	if !strings.Contains(result, `"test/agent"`) {
		t.Errorf("expected value \"test/agent\" in diagnostic notation, got: %q", result)
	}
}

func TestDiagCBOR_Sequence(t *testing.T) {
	// Two CBOR items concatenated should produce two lines.
	item1, err := codec.Marshal("hello")
	if err != nil {
		t.Fatalf("marshal item 1: %v", err)
	}
	item2, err := codec.Marshal(int64(42))
	if err != nil {
		t.Fatalf("marshal item 2: %v", err)
	}

	var sequence []byte
	sequence = append(sequence, item1...)
	sequence = append(sequence, item2...)

	var output bytes.Buffer
	if err := diagCBOR(bytes.NewReader(sequence), &output); err != nil {
		t.Fatalf("diagCBOR: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), output.String())
	}
	if !strings.Contains(lines[0], `"hello"`) {
		t.Errorf("line 0 = %q, want to contain '\"hello\"'", lines[0])
	}
	if !strings.Contains(lines[1], "42") {
		t.Errorf("line 1 = %q, want to contain \"42\"", lines[1])
	}
}

func TestDiagCBOR_EmptyInput(t *testing.T) {
	var output bytes.Buffer
	err := diagCBOR(bytes.NewReader(nil), &output)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty input") {
		t.Errorf("error = %q, want to contain \"empty input\"", err.Error())
	}
}
