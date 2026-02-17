// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"strings"
	"testing"
)

func TestSSEScannerBasic(t *testing.T) {
	t.Parallel()

	input := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: ping\ndata: {}\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	// First event.
	if !scanner.Next() {
		t.Fatal("expected first event")
	}
	event := scanner.Event()
	if event.Type != "message_start" {
		t.Errorf("event.Type = %q, want message_start", event.Type)
	}
	if event.Data != `{"type":"message_start"}` {
		t.Errorf("event.Data = %q, want JSON", event.Data)
	}

	// Second event.
	if !scanner.Next() {
		t.Fatal("expected second event")
	}
	event = scanner.Event()
	if event.Type != "ping" {
		t.Errorf("event.Type = %q, want ping", event.Type)
	}

	// No more events.
	if scanner.Next() {
		t.Error("expected no more events")
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSSEScannerMultipleDataLines(t *testing.T) {
	t.Parallel()

	// Per the SSE spec, multiple data lines are joined with newlines.
	input := "data: line one\ndata: line two\ndata: line three\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Type != "" {
		t.Errorf("event.Type = %q, want empty", event.Type)
	}
	expected := "line one\nline two\nline three"
	if event.Data != expected {
		t.Errorf("event.Data = %q, want %q", event.Data, expected)
	}
}

func TestSSEScannerComments(t *testing.T) {
	t.Parallel()

	// Comment lines (starting with ":") should be ignored.
	input := ": this is a comment\nevent: test\ndata: hello\n: another comment\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Type != "test" {
		t.Errorf("event.Type = %q, want test", event.Type)
	}
	if event.Data != "hello" {
		t.Errorf("event.Data = %q, want hello", event.Data)
	}
}

func TestSSEScannerNoEventType(t *testing.T) {
	t.Parallel()

	input := "data: just data\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Type != "" {
		t.Errorf("event.Type = %q, want empty", event.Type)
	}
	if event.Data != "just data" {
		t.Errorf("event.Data = %q, want 'just data'", event.Data)
	}
}

func TestSSEScannerEmptyDataLines(t *testing.T) {
	t.Parallel()

	// "data:" with no value should produce an empty string.
	// "data: " with a trailing space should produce empty too (space is stripped).
	input := "data:\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Data != "" {
		t.Errorf("event.Data = %q, want empty", event.Data)
	}
}

func TestSSEScannerConsecutiveBlanks(t *testing.T) {
	t.Parallel()

	// Consecutive blank lines without data don't produce events.
	input := "\n\n\ndata: hello\n\n\n\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Data != "hello" {
		t.Errorf("event.Data = %q, want hello", event.Data)
	}

	if scanner.Next() {
		t.Error("expected no more events")
	}
}

func TestSSEScannerNoTrailingNewline(t *testing.T) {
	t.Parallel()

	// Input that ends without a trailing blank line — the accumulated
	// event should still be emitted.
	input := "event: final\ndata: last event"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Type != "final" {
		t.Errorf("event.Type = %q, want final", event.Type)
	}
	if event.Data != "last event" {
		t.Errorf("event.Data = %q, want 'last event'", event.Data)
	}

	if scanner.Next() {
		t.Error("expected no more events after EOF")
	}
}

func TestSSEScannerCarriageReturn(t *testing.T) {
	t.Parallel()

	// Windows-style line endings should work.
	input := "event: test\r\ndata: hello\r\n\r\n"
	scanner := NewSSEScanner(strings.NewReader(input))

	if !scanner.Next() {
		t.Fatal("expected event")
	}
	event := scanner.Event()
	if event.Type != "test" {
		t.Errorf("event.Type = %q, want test", event.Type)
	}
	if event.Data != "hello" {
		t.Errorf("event.Data = %q, want hello", event.Data)
	}
}

func TestSSEScannerAnthropicStream(t *testing.T) {
	t.Parallel()

	// Realistic Anthropic streaming output.
	input := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":100,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}

event: message_stop
data: {"type":"message_stop"}

`
	scanner := NewSSEScanner(strings.NewReader(input))

	expectedTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}

	for i, expectedType := range expectedTypes {
		if !scanner.Next() {
			t.Fatalf("event %d: expected event of type %q, got EOF", i, expectedType)
		}
		event := scanner.Event()
		if event.Type != expectedType {
			t.Errorf("event %d: Type = %q, want %q", i, event.Type, expectedType)
		}
		if event.Data == "" {
			t.Errorf("event %d: Data is empty", i)
		}
	}

	if scanner.Next() {
		t.Errorf("expected no more events, got type=%q", scanner.Event().Type)
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
