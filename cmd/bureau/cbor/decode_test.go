// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func TestDecodeCBOR(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		compact bool
		want    any // decoded JSON value to compare
	}{
		{
			name:  "simple map",
			input: map[string]any{"action": "status", "count": int64(42)},
			want:  map[string]any{"action": "status", "count": float64(42)},
		},
		{
			name:    "compact output",
			input:   map[string]any{"key": "value"},
			compact: true,
			want:    map[string]any{"key": "value"},
		},
		{
			name:  "nested structure",
			input: map[string]any{"outer": map[string]any{"inner": "deep"}},
			want:  map[string]any{"outer": map[string]any{"inner": "deep"}},
		},
		{
			name:  "array",
			input: []any{"a", "b", "c"},
			want:  []any{"a", "b", "c"},
		},
		{
			name:  "integer values preserved as numbers",
			input: map[string]any{"small": int64(1), "large": int64(1000000)},
			want:  map[string]any{"small": float64(1), "large": float64(1000000)},
		},
		{
			name:  "boolean and null",
			input: map[string]any{"flag": true, "empty": nil},
			want:  map[string]any{"flag": true, "empty": nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cborData, err := codec.Marshal(tt.input)
			if err != nil {
				t.Fatalf("marshal CBOR: %v", err)
			}

			var output bytes.Buffer
			if err := decodeCBOR(cborData, &output, tt.compact, false); err != nil {
				t.Fatalf("decodeCBOR: %v", err)
			}

			// Parse the JSON output and compare.
			var got any
			if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &got); err != nil {
				t.Fatalf("parse output JSON: %v (output was: %q)", err, output.String())
			}

			assertJSONEqual(t, tt.want, got)
		})
	}
}

func TestDecodeCBOR_IntegerKeys(t *testing.T) {
	// keyasint encoding produces CBOR maps with integer keys.
	// When decoded to any, these become string keys in JSON.
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
	if err := decodeCBOR(cborData, &output, false, false); err != nil {
		t.Fatalf("decodeCBOR: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &got); err != nil {
		t.Fatalf("parse output JSON: %v", err)
	}

	// Integer keys 1, 2 become string keys "1", "2".
	if got["1"] != "test/agent" {
		t.Errorf("key \"1\" = %v, want \"test/agent\"", got["1"])
	}
	if got["2"] != "workstation" {
		t.Errorf("key \"2\" = %v, want \"workstation\"", got["2"])
	}
}

func TestDecodeCBOR_Slurp(t *testing.T) {
	// Create a CBOR sequence: two items concatenated.
	item1, err := codec.Marshal(map[string]any{"index": int64(0)})
	if err != nil {
		t.Fatalf("marshal item 1: %v", err)
	}
	item2, err := codec.Marshal(map[string]any{"index": int64(1)})
	if err != nil {
		t.Fatalf("marshal item 2: %v", err)
	}

	var sequence []byte
	sequence = append(sequence, item1...)
	sequence = append(sequence, item2...)

	var output bytes.Buffer
	if err := decodeCBOR(sequence, &output, true, true); err != nil {
		t.Fatalf("decodeCBOR slurp: %v", err)
	}

	var got []any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &got); err != nil {
		t.Fatalf("parse output JSON array: %v (output was: %q)", err, output.String())
	}

	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}

	first, ok := got[0].(map[string]any)
	if !ok {
		t.Fatalf("item 0 is %T, want map[string]any", got[0])
	}
	if first["index"] != float64(0) {
		t.Errorf("item 0 index = %v, want 0", first["index"])
	}

	second, ok := got[1].(map[string]any)
	if !ok {
		t.Fatalf("item 1 is %T, want map[string]any", got[1])
	}
	if second["index"] != float64(1) {
		t.Errorf("item 1 index = %v, want 1", second["index"])
	}
}

func TestDecodeCBOR_CompactFormat(t *testing.T) {
	input := map[string]any{"key": "value"}
	cborData, err := codec.Marshal(input)
	if err != nil {
		t.Fatalf("marshal CBOR: %v", err)
	}

	// Compact: single line, no indentation.
	var compact bytes.Buffer
	if err := decodeCBOR(cborData, &compact, true, false); err != nil {
		t.Fatalf("decodeCBOR compact: %v", err)
	}
	compactStr := strings.TrimSpace(compact.String())
	if strings.Contains(compactStr, "\n") {
		t.Errorf("compact output contains newlines: %q", compactStr)
	}
	if strings.Contains(compactStr, "  ") {
		t.Errorf("compact output contains indentation: %q", compactStr)
	}

	// Pretty: has indentation.
	var pretty bytes.Buffer
	if err := decodeCBOR(cborData, &pretty, false, false); err != nil {
		t.Fatalf("decodeCBOR pretty: %v", err)
	}
	prettyStr := strings.TrimSpace(pretty.String())
	if !strings.Contains(prettyStr, "\n") {
		t.Errorf("pretty output should contain newlines: %q", prettyStr)
	}
}

func TestDecodeCBOR_EmptyInput(t *testing.T) {
	var output bytes.Buffer
	err := decodeCBOR(nil, &output, false, false)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty input") {
		t.Errorf("error = %q, want to contain \"empty input\"", err.Error())
	}
}

// assertJSONEqual compares two JSON-decoded values for semantic equality.
func assertJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("JSON mismatch:\n  want: %s\n  got:  %s", wantJSON, gotJSON)
	}
}
