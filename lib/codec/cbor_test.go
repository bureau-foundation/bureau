// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"bytes"
	"strings"
	"testing"
)

// sampleMessage is a representative Bureau internal message using cbor
// struct tags (the convention for purely-internal types).
type sampleMessage struct {
	Action    string `cbor:"action"`
	Principal string `cbor:"principal,omitempty"`
	Count     int    `cbor:"count"`
}

// sampleDualMessage uses json struct tags (the convention for types
// that serve both JSON and CBOR, relying on fxamacker's fallback).
type sampleDualMessage struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

func TestMarshalUnmarshalRoundtrip(t *testing.T) {
	original := sampleMessage{
		Action:    "create-sandbox",
		Principal: "iree/amdgpu/pm",
		Count:     42,
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("Marshal produced empty output")
	}

	var decoded sampleMessage
	if err := Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded != original {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMarshalDeterministic(t *testing.T) {
	message := sampleMessage{
		Action:    "status",
		Principal: "test/agent",
		Count:     7,
	}

	first, err := Marshal(message)
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}

	second, err := Marshal(message)
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("deterministic encoding violated: %x != %x", first, second)
	}
}

func TestEncoderDecoderStreamRoundtrip(t *testing.T) {
	messages := []sampleMessage{
		{Action: "create-sandbox", Principal: "a/b", Count: 1},
		{Action: "destroy-sandbox", Principal: "c/d", Count: 2},
		{Action: "status", Count: 0},
	}

	var buffer bytes.Buffer
	encoder := NewEncoder(&buffer)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}

	decoder := NewDecoder(&buffer)
	for i, want := range messages {
		var got sampleMessage
		if err := decoder.Decode(&got); err != nil {
			t.Fatalf("Decode message %d: %v", i, err)
		}
		if got != want {
			t.Errorf("message %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestJSONTagFallback(t *testing.T) {
	// Types with json tags (no cbor tags) should encode/decode
	// correctly through our modes, using json tag names as CBOR
	// map keys.
	original := sampleDualMessage{Version: 3, Name: "artifact"}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded sampleDualMessage
	if err := Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded != original {
		t.Errorf("json-tag roundtrip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestOmitemptyRespected(t *testing.T) {
	// A zero-value omitempty field should not appear in output.
	withPrincipal := sampleMessage{Action: "a", Principal: "x", Count: 1}
	withoutPrincipal := sampleMessage{Action: "a", Count: 1}

	dataWith, err := Marshal(withPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	dataWithout, err := Marshal(withoutPrincipal)
	if err != nil {
		t.Fatal(err)
	}

	// The encoding without the principal field should be shorter
	// because the omitted field is not present.
	if len(dataWithout) >= len(dataWith) {
		t.Errorf("omitempty not effective: without=%d bytes, with=%d bytes",
			len(dataWithout), len(dataWith))
	}
}

func TestUnmarshalInvalidCBOR(t *testing.T) {
	var message sampleMessage
	err := Unmarshal([]byte{0xFF, 0xFE, 0xFD}, &message)
	if err == nil {
		t.Error("Unmarshal should reject invalid CBOR")
	}
}

func TestByteStringRoundtrip(t *testing.T) {
	// Verify that []byte fields encode as CBOR byte strings (major
	// type 2), not text strings. This matters for carrying
	// pre-serialized JSON payloads and binary tokens.
	type envelope struct {
		Payload []byte `cbor:"payload"`
	}

	original := envelope{Payload: []byte(`{"key":"value"}`)}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded envelope
	if err := Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !bytes.Equal(decoded.Payload, original.Payload) {
		t.Errorf("byte string roundtrip: got %q, want %q", decoded.Payload, original.Payload)
	}
}

func BenchmarkMarshal(b *testing.B) {
	message := sampleMessage{
		Action:    "create-sandbox",
		Principal: "iree/amdgpu/pm",
		Count:     42,
	}

	b.ReportAllocs()
	for b.Loop() {
		Marshal(message)
	}
}

func TestDiagnose(t *testing.T) {
	data, err := Marshal(map[string]any{"action": "status"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	notation, err := Diagnose(data)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	if !strings.Contains(notation, `"action"`) {
		t.Errorf("notation %q does not contain \"action\"", notation)
	}
	if !strings.Contains(notation, `"status"`) {
		t.Errorf("notation %q does not contain \"status\"", notation)
	}
}

func TestDiagnoseFirst(t *testing.T) {
	item1, err := Marshal("hello")
	if err != nil {
		t.Fatalf("Marshal item 1: %v", err)
	}
	item2, err := Marshal(int64(42))
	if err != nil {
		t.Fatalf("Marshal item 2: %v", err)
	}

	var sequence []byte
	sequence = append(sequence, item1...)
	sequence = append(sequence, item2...)

	notation, remaining, err := DiagnoseFirst(sequence)
	if err != nil {
		t.Fatalf("DiagnoseFirst: %v", err)
	}

	if !strings.Contains(notation, `"hello"`) {
		t.Errorf("first item notation %q does not contain \"hello\"", notation)
	}
	if len(remaining) == 0 {
		t.Fatal("expected remaining bytes after first item")
	}

	notation2, remaining2, err := DiagnoseFirst(remaining)
	if err != nil {
		t.Fatalf("DiagnoseFirst second: %v", err)
	}
	if !strings.Contains(notation2, "42") {
		t.Errorf("second item notation %q does not contain \"42\"", notation2)
	}
	if len(remaining2) != 0 {
		t.Errorf("expected no remaining bytes, got %d", len(remaining2))
	}
}

// sampleKeyasintMessage uses integer map keys, matching the CBOR wire
// format used by model.Request and similar high-frequency service types.
type sampleKeyasintMessage struct {
	Model    string   `cbor:"1,keyasint"`
	Messages []string `cbor:"2,keyasint,omitempty"`
	Stream   bool     `cbor:"3,keyasint,omitempty"`
}

func TestUnmarshalMixedKeysPreservesIntegerKeys(t *testing.T) {
	original := sampleKeyasintMessage{
		Model:    "test-model",
		Messages: []string{"hello"},
		Stream:   true,
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// UnmarshalMixedKeys should decode into map[any]any with uint64 keys.
	var result map[any]any
	if err := UnmarshalMixedKeys(data, &result); err != nil {
		t.Fatalf("UnmarshalMixedKeys: %v", err)
	}

	// Verify integer keys survived as uint64.
	model, ok := result[uint64(1)]
	if !ok {
		t.Fatalf("missing key 1; keys = %v", mapKeys(result))
	}
	if model != "test-model" {
		t.Errorf("key 1: got %q, want %q", model, "test-model")
	}

	stream, ok := result[uint64(3)]
	if !ok {
		t.Fatalf("missing key 3; keys = %v", mapKeys(result))
	}
	if stream != true {
		t.Errorf("key 3: got %v, want true", stream)
	}
}

func TestUnmarshalRejectsIntegerKeysIntoStringMap(t *testing.T) {
	// This test documents why UnmarshalMixedKeys exists: the standard
	// Unmarshal forces map[string]any which cannot hold integer keys.
	original := sampleKeyasintMessage{
		Model:  "test-model",
		Stream: true,
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var result map[string]any
	err = Unmarshal(data, &result)
	if err == nil {
		t.Fatal("expected Unmarshal to fail on integer keys with map[string]any target")
	}
}

func TestUnmarshalMixedKeysStringKeysStillWork(t *testing.T) {
	// String-keyed structs should also work with UnmarshalMixedKeys.
	original := sampleMessage{
		Action:    "status",
		Principal: "test/agent",
		Count:     7,
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var result map[any]any
	if err := UnmarshalMixedKeys(data, &result); err != nil {
		t.Fatalf("UnmarshalMixedKeys: %v", err)
	}

	action, ok := result["action"]
	if !ok {
		t.Fatalf("missing key 'action'; keys = %v", mapKeys(result))
	}
	if action != "status" {
		t.Errorf("action: got %q, want %q", action, "status")
	}
}

// mapKeys returns the keys of a map[any]any for diagnostic messages.
func mapKeys(m map[any]any) []any {
	keys := make([]any, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func BenchmarkUnmarshal(b *testing.B) {
	message := sampleMessage{
		Action:    "create-sandbox",
		Principal: "iree/amdgpu/pm",
		Count:     42,
	}
	data, err := Marshal(message)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		var decoded sampleMessage
		Unmarshal(data, &decoded)
	}
}
