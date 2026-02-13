// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func TestValidateCBOR_Deterministic(t *testing.T) {
	tests := []struct {
		name string
		data func() []byte
	}{
		{
			name: "deterministic map",
			data: func() []byte {
				data, _ := codec.Marshal(map[string]any{"action": "status", "count": int64(42)})
				return data
			},
		},
		{
			name: "deterministic struct",
			data: func() []byte {
				type msg struct {
					Action string `cbor:"action"`
					Count  int    `cbor:"count"`
				}
				data, _ := codec.Marshal(msg{Action: "status", Count: 42})
				return data
			},
		},
		{
			name: "deterministic integer-key struct",
			data: func() []byte {
				type msg struct {
					Subject string `cbor:"1,keyasint"`
					Machine string `cbor:"2,keyasint"`
				}
				data, _ := codec.Marshal(msg{Subject: "test", Machine: "work"})
				return data
			},
		},
		{
			name: "simple scalar",
			data: func() []byte {
				data, _ := codec.Marshal(int64(42))
				return data
			},
		},
		{
			name: "string value",
			data: func() []byte {
				data, _ := codec.Marshal("hello world")
				return data
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := validateCBOR(tt.data(), &output, false)
			if err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !strings.Contains(output.String(), "valid") {
				t.Errorf("output = %q, want to contain \"valid\"", output.String())
			}
		})
	}
}

func TestValidateCBOR_NonDeterministic(t *testing.T) {
	// CBOR map with unsorted keys: {"b": 2, "a": 1}.
	// Core Deterministic Encoding requires sorted keys (length-first,
	// then lexicographic), so "a" must come before "b".
	// Manual bytes: A2 (map of 2) 61 62 (text "b") 02 (uint 2) 61 61 (text "a") 01 (uint 1)
	nonDeterministic := []byte{0xa2, 0x61, 0x62, 0x02, 0x61, 0x61, 0x01}

	var output bytes.Buffer
	err := validateCBOR(nonDeterministic, &output, false)
	if err == nil {
		t.Fatal("expected error for non-deterministic CBOR")
	}
	if !strings.Contains(err.Error(), "not deterministic") {
		t.Errorf("error = %q, want to contain \"not deterministic\"", err.Error())
	}
}

func TestValidateCBOR_Sequence(t *testing.T) {
	item1, _ := codec.Marshal(map[string]any{"index": int64(0)})
	item2, _ := codec.Marshal(map[string]any{"index": int64(1)})

	var sequence []byte
	sequence = append(sequence, item1...)
	sequence = append(sequence, item2...)

	var output bytes.Buffer
	err := validateCBOR(sequence, &output, true)
	if err != nil {
		t.Fatalf("expected valid sequence, got error: %v", err)
	}
	if !strings.Contains(output.String(), "valid") {
		t.Errorf("output = %q, want to contain \"valid\"", output.String())
	}
}

func TestValidateCBOR_NonDeterministicSequence(t *testing.T) {
	// First item deterministic, second item non-deterministic.
	item1, _ := codec.Marshal(map[string]any{"ok": true})
	// Manual non-deterministic CBOR: {"b": 2, "a": 1}
	item2 := []byte{0xa2, 0x61, 0x62, 0x02, 0x61, 0x61, 0x01}

	var sequence []byte
	sequence = append(sequence, item1...)
	sequence = append(sequence, item2...)

	var output bytes.Buffer
	err := validateCBOR(sequence, &output, true)
	if err == nil {
		t.Fatal("expected error for non-deterministic sequence")
	}
	if !strings.Contains(err.Error(), "not deterministic") {
		t.Errorf("error = %q, want to contain \"not deterministic\"", err.Error())
	}
}

func TestValidateCBOR_Empty(t *testing.T) {
	var output bytes.Buffer
	err := validateCBOR(nil, &output, false)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty input") {
		t.Errorf("error = %q, want to contain \"empty input\"", err.Error())
	}
}

func TestValidateCBOR_InvalidCBOR(t *testing.T) {
	var output bytes.Buffer
	err := validateCBOR([]byte{0xff, 0xfe, 0xfd}, &output, false)
	if err == nil {
		t.Fatal("expected error for invalid CBOR")
	}
	if !strings.Contains(err.Error(), "decode CBOR") {
		t.Errorf("error = %q, want to contain \"decode CBOR\"", err.Error())
	}
}
