// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"testing"
	"time"
)

func TestBlacklist_RevokeAndCheck(t *testing.T) {
	blacklist := NewBlacklist()

	tokenExpiry := time.Now().Add(5 * time.Minute)
	blacklist.Revoke("token-1", tokenExpiry)

	if !blacklist.IsRevoked("token-1") {
		t.Error("token-1 should be revoked")
	}
	if blacklist.IsRevoked("token-2") {
		t.Error("token-2 should not be revoked")
	}
	if blacklist.Len() != 1 {
		t.Errorf("Len = %d, want 1", blacklist.Len())
	}
}

func TestBlacklist_Cleanup(t *testing.T) {
	blacklist := NewBlacklist()

	t1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)  // expires at 10:00
	t2 := time.Date(2026, 3, 1, 10, 5, 0, 0, time.UTC)  // expires at 10:05
	t3 := time.Date(2026, 3, 1, 10, 10, 0, 0, time.UTC) // expires at 10:10

	blacklist.Revoke("token-1", t1)
	blacklist.Revoke("token-2", t2)
	blacklist.Revoke("token-3", t3)

	if blacklist.Len() != 3 {
		t.Fatalf("Len = %d, want 3", blacklist.Len())
	}

	// Cleanup at 10:02: token-1 has expired.
	cleanupTime := time.Date(2026, 3, 1, 10, 2, 0, 0, time.UTC)
	removed := blacklist.Cleanup(cleanupTime)
	if removed != 1 {
		t.Errorf("Cleanup at 10:02 removed = %d, want 1", removed)
	}
	if blacklist.IsRevoked("token-1") {
		t.Error("token-1 should have been cleaned up")
	}
	if !blacklist.IsRevoked("token-2") {
		t.Error("token-2 should still be revoked")
	}

	// Cleanup at 10:07: token-2 has expired.
	cleanupTime = time.Date(2026, 3, 1, 10, 7, 0, 0, time.UTC)
	removed = blacklist.Cleanup(cleanupTime)
	if removed != 1 {
		t.Errorf("Cleanup at 10:07 removed = %d, want 1", removed)
	}
	if blacklist.Len() != 1 {
		t.Errorf("Len after cleanup = %d, want 1", blacklist.Len())
	}

	// Cleanup at 10:15: all expired.
	cleanupTime = time.Date(2026, 3, 1, 10, 15, 0, 0, time.UTC)
	removed = blacklist.Cleanup(cleanupTime)
	if removed != 1 {
		t.Errorf("Cleanup at 10:15 removed = %d, want 1", removed)
	}
	if blacklist.Len() != 0 {
		t.Errorf("Len after final cleanup = %d, want 0", blacklist.Len())
	}
}

func TestBlacklist_CleanupNoEntries(t *testing.T) {
	blacklist := NewBlacklist()
	removed := blacklist.Cleanup(time.Now())
	if removed != 0 {
		t.Errorf("Cleanup on empty blacklist removed = %d, want 0", removed)
	}
}

func TestBlacklist_DuplicateRevoke(t *testing.T) {
	blacklist := NewBlacklist()

	expiry := time.Now().Add(5 * time.Minute)
	blacklist.Revoke("token-1", expiry)
	blacklist.Revoke("token-1", expiry) // duplicate

	if blacklist.Len() != 1 {
		t.Errorf("Len after duplicate = %d, want 1", blacklist.Len())
	}
}
