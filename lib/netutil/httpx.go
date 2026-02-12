// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package netutil provides network and HTTP I/O utilities for Bureau.
//
// HTTP response helpers (ReadResponse, DecodeResponse, ErrorBody) bound all
// response body reads at MaxResponseSize to prevent unbounded memory
// allocation from a misbehaving or malicious server. These are for JSON API
// responses (Matrix client-server API, admin API, observe handshakes) — not
// for streaming responses (SSE, chunked transfers) or large binary downloads,
// which should be read incrementally with io.Copy.
//
// Connection error helpers (IsExpectedCloseError) classify errors that occur
// during normal connection teardown in bidirectional bridges.
package netutil

import (
	"encoding/json"
	"fmt"
	"io"
)

// MaxResponseSize is the bound on JSON API response body reads: 256 MB. This
// exists solely to prevent a pathological response from exhausting system
// memory. Legitimate JSON API responses are orders of magnitude smaller; the
// limit is intentionally generous so that it never interferes with normal
// operation.
const MaxResponseSize int64 = 256 << 20

// ReadResponse reads a JSON API response body up to MaxResponseSize bytes.
// Use instead of io.ReadAll when reading HTTP response bodies.
func ReadResponse(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, MaxResponseSize))
}

// DecodeResponse reads a JSON API response body (up to MaxResponseSize bytes)
// and JSON-decodes it into v. Replaces the common io.ReadAll + json.Unmarshal
// pattern.
func DecodeResponse(body io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(body, MaxResponseSize))
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	return json.Unmarshal(data, v)
}

// ErrorBody reads an HTTP error response body and returns it as a string for
// diagnostic error messages. Read errors are silently ignored — a partial or
// empty body is still useful in an error message.
func ErrorBody(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, MaxResponseSize))
	return string(data)
}
