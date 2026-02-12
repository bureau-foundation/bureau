// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/hex"
	"testing"
)

func TestGenerateRequestID(t *testing.T) {
	t.Parallel()

	id, err := GenerateRequestID()
	if err != nil {
		t.Fatalf("GenerateRequestID: %v", err)
	}

	// 16 bytes → 32 hex characters.
	if len(id) != 32 {
		t.Errorf("request ID length = %d, want 32", len(id))
	}

	// Must be valid hex.
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("request ID %q is not valid hex: %v", id, err)
	}
}

func TestGenerateRequestIDUniqueness(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for range 100 {
		id, err := GenerateRequestID()
		if err != nil {
			t.Fatalf("GenerateRequestID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate request ID: %s", id)
		}
		seen[id] = true
	}
}
