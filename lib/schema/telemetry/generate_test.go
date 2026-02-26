// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "testing"

func TestNewTraceID(t *testing.T) {
	t.Parallel()

	id := NewTraceID()
	if id.IsZero() {
		t.Fatal("NewTraceID returned zero value")
	}

	// Two IDs should differ (collision probability is negligible).
	other := NewTraceID()
	if id == other {
		t.Fatalf("two NewTraceID calls returned identical values: %s", id)
	}
}

func TestNewSpanID(t *testing.T) {
	t.Parallel()

	id := NewSpanID()
	if id.IsZero() {
		t.Fatal("NewSpanID returned zero value")
	}

	other := NewSpanID()
	if id == other {
		t.Fatalf("two NewSpanID calls returned identical values: %s", id)
	}
}
