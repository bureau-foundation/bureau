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

func TestEncodeCBOR(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		check func(t *testing.T, cborData []byte)
	}{
		{
			name: "simple map",
			json: `{"action":"status"}`,
			check: func(t *testing.T, cborData []byte) {
				var got map[string]any
				if err := codec.Unmarshal(cborData, &got); err != nil {
					t.Fatalf("unmarshal CBOR: %v", err)
				}
				if got["action"] != "status" {
					t.Errorf("action = %v, want \"status\"", got["action"])
				}
			},
		},
		{
			name: "integer preserved",
			json: `{"count":42}`,
			check: func(t *testing.T, cborData []byte) {
				var got map[string]any
				if err := codec.Unmarshal(cborData, &got); err != nil {
					t.Fatalf("unmarshal CBOR: %v", err)
				}
				// CBOR integer decoded to uint64 or int64 (not float64).
				count := got["count"]
				switch v := count.(type) {
				case uint64:
					if v != 42 {
						t.Errorf("count = %d, want 42", v)
					}
				case int64:
					if v != 42 {
						t.Errorf("count = %d, want 42", v)
					}
				default:
					t.Errorf("count is %T (%v), want integer type", count, count)
				}
			},
		},
		{
			name: "float preserved",
			json: `{"ratio":3.14}`,
			check: func(t *testing.T, cborData []byte) {
				var got map[string]any
				if err := codec.Unmarshal(cborData, &got); err != nil {
					t.Fatalf("unmarshal CBOR: %v", err)
				}
				ratio, ok := got["ratio"].(float64)
				if !ok {
					t.Fatalf("ratio is %T, want float64", got["ratio"])
				}
				if ratio != 3.14 {
					t.Errorf("ratio = %f, want 3.14", ratio)
				}
			},
		},
		{
			name: "nested structure",
			json: `{"outer":{"inner":[1,2,3]}}`,
			check: func(t *testing.T, cborData []byte) {
				var got map[string]any
				if err := codec.Unmarshal(cborData, &got); err != nil {
					t.Fatalf("unmarshal CBOR: %v", err)
				}
				outer, ok := got["outer"].(map[string]any)
				if !ok {
					t.Fatalf("outer is %T, want map[string]any", got["outer"])
				}
				inner, ok := outer["inner"].([]any)
				if !ok {
					t.Fatalf("inner is %T, want []any", outer["inner"])
				}
				if len(inner) != 3 {
					t.Errorf("inner length = %d, want 3", len(inner))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := encodeCBOR([]byte(tt.json), &output); err != nil {
				t.Fatalf("encodeCBOR: %v", err)
			}
			tt.check(t, output.Bytes())
		})
	}
}

func TestEncodeCBOR_Deterministic(t *testing.T) {
	// Core Deterministic Encoding produces identical bytes for
	// identical logical data, regardless of JSON key ordering.
	json1 := `{"b":"two","a":"one"}`
	json2 := `{"a":"one","b":"two"}`

	var output1, output2 bytes.Buffer
	if err := encodeCBOR([]byte(json1), &output1); err != nil {
		t.Fatalf("encode json1: %v", err)
	}
	if err := encodeCBOR([]byte(json2), &output2); err != nil {
		t.Fatalf("encode json2: %v", err)
	}

	if !bytes.Equal(output1.Bytes(), output2.Bytes()) {
		t.Errorf("deterministic encoding failed: different JSON key order produced different CBOR\n  json1 bytes: %x\n  json2 bytes: %x",
			output1.Bytes(), output2.Bytes())
	}
}

func TestEncodeCBOR_RoundTrip(t *testing.T) {
	original := `{"action":"create","count":100,"tags":["a","b"],"flag":true,"empty":null}`

	// Encode JSON → CBOR.
	var cborBuffer bytes.Buffer
	if err := encodeCBOR([]byte(original), &cborBuffer); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Decode CBOR → JSON.
	var jsonBuffer bytes.Buffer
	if err := decodeCBOR(cborBuffer.Bytes(), &jsonBuffer, true, false); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Compare the JSON values (not byte-for-byte, since key order
	// and formatting may differ).
	var want, got any
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatalf("parse original: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonBuffer.String())), &got); err != nil {
		t.Fatalf("parse round-trip: %v", err)
	}

	assertJSONEqual(t, want, got)
}

func TestConvertNumbers(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  any
	}{
		{
			name:  "integer",
			input: json.Number("42"),
			want:  int64(42),
		},
		{
			name:  "negative integer",
			input: json.Number("-7"),
			want:  int64(-7),
		},
		{
			name:  "float",
			input: json.Number("3.14"),
			want:  float64(3.14),
		},
		{
			name:  "large integer",
			input: json.Number("9007199254740992"),
			want:  int64(9007199254740992),
		},
		{
			name:  "string passthrough",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "bool passthrough",
			input: true,
			want:  true,
		},
		{
			name:  "nil passthrough",
			input: nil,
			want:  nil,
		},
		{
			name:  "nested map",
			input: map[string]any{"n": json.Number("5")},
			want:  map[string]any{"n": int64(5)},
		},
		{
			name:  "nested array",
			input: []any{json.Number("1"), json.Number("2.5")},
			want:  []any{int64(1), float64(2.5)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertNumbers(tt.input)
			wantJSON, _ := json.Marshal(tt.want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("convertNumbers() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestEncodeCBOR_InvalidJSON(t *testing.T) {
	var output bytes.Buffer
	err := encodeCBOR([]byte("not json at all"), &output)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode JSON") {
		t.Errorf("error = %q, want to contain \"decode JSON\"", err.Error())
	}
}

func TestEncodeCBOR_EmptyInput(t *testing.T) {
	var output bytes.Buffer
	err := encodeCBOR(nil, &output)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty input") {
		t.Errorf("error = %q, want to contain \"empty input\"", err.Error())
	}
}
