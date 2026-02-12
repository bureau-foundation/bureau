// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"bytes"
	"fmt"
	"testing"
)

func TestReadResponse(t *testing.T) {
	t.Run("normal body", func(t *testing.T) {
		data, err := ReadResponse(bytes.NewReader([]byte(`{"status":"ok"}`)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(data) != `{"status":"ok"}` {
			t.Fatalf("got %q, want %q", data, `{"status":"ok"}`)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		data, err := ReadResponse(bytes.NewReader(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(data) != 0 {
			t.Fatalf("expected empty, got %d bytes", len(data))
		}
	})

	t.Run("read error propagates", func(t *testing.T) {
		_, err := ReadResponse(&failReader{})
		if err == nil {
			t.Fatal("expected error from failing reader")
		}
	})
}

func TestDecodeResponse(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"name":"test","count":42}`))
		var result struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		if err := DecodeResponse(body, &result); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Name != "test" {
			t.Fatalf("name: got %q, want %q", result.Name, "test")
		}
		if result.Count != 42 {
			t.Fatalf("count: got %d, want %d", result.Count, 42)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		if err := DecodeResponse(bytes.NewReader([]byte(`not json`)), &struct{}{}); err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("read error propagates", func(t *testing.T) {
		if err := DecodeResponse(&failReader{}, &struct{}{}); err == nil {
			t.Fatal("expected error from failing reader")
		}
	})
}

func TestErrorBody(t *testing.T) {
	t.Run("returns body as string", func(t *testing.T) {
		got := ErrorBody(bytes.NewReader([]byte(`{"errcode":"M_FORBIDDEN"}`)))
		if got != `{"errcode":"M_FORBIDDEN"}` {
			t.Fatalf("got %q, want %q", got, `{"errcode":"M_FORBIDDEN"}`)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		if got := ErrorBody(bytes.NewReader(nil)); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("read error returns empty", func(t *testing.T) {
		if got := ErrorBody(&failReader{}); got != "" {
			t.Fatalf("expected empty from failing reader, got %q", got)
		}
	})
}

// failReader always returns an error on Read.
type failReader struct{}

func (*failReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("simulated read failure")
}
