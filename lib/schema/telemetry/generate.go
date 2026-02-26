// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "crypto/rand"

// NewTraceID generates a cryptographically random 16-byte trace ID.
// Panics if the system entropy source fails — this indicates a
// system-level failure that no caller can recover from.
func NewTraceID() TraceID {
	var id TraceID
	if _, err := rand.Read(id[:]); err != nil {
		panic("telemetry: failed to generate TraceID: " + err.Error())
	}
	return id
}

// NewSpanID generates a cryptographically random 8-byte span ID.
// Panics if the system entropy source fails.
func NewSpanID() SpanID {
	var id SpanID
	if _, err := rand.Read(id[:]); err != nil {
		panic("telemetry: failed to generate SpanID: " + err.Error())
	}
	return id
}
