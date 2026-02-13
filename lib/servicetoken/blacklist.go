// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"sync"
	"time"
)

// blacklistEntry tracks a revoked token ID and its natural expiry time.
// The expiry is used for automatic cleanup — once a token's natural TTL
// has passed, keeping it in the blacklist is unnecessary since expired
// tokens are rejected by Verify regardless.
type blacklistEntry struct {
	tokenExpiresAt time.Time
}

// Blacklist is a thread-safe in-memory set of revoked token IDs. The
// daemon sends revocation notices to services when a sandbox is
// destroyed or when a principal's grants change mid-lifecycle. The
// service adds the token ID to the blacklist, and subsequent Verify
// calls that return a token whose ID is blacklisted should be rejected.
//
// The blacklist auto-cleans: entries whose token expiry has passed are
// removed during Cleanup. Since tokens have short TTLs (default 5
// minutes), the blacklist stays small.
type Blacklist struct {
	mu      sync.RWMutex
	entries map[string]blacklistEntry
}

// NewBlacklist creates an empty token blacklist.
func NewBlacklist() *Blacklist {
	return &Blacklist{
		entries: make(map[string]blacklistEntry),
	}
}

// Revoke adds a token ID to the blacklist. The tokenExpiresAt parameter
// is the token's natural expiry time — the blacklist entry is
// automatically removed after this time during Cleanup.
func (b *Blacklist) Revoke(tokenID string, tokenExpiresAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[tokenID] = blacklistEntry{tokenExpiresAt: tokenExpiresAt}
}

// IsRevoked checks whether a token ID has been revoked.
func (b *Blacklist) IsRevoked(tokenID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, exists := b.entries[tokenID]
	return exists
}

// Cleanup removes blacklist entries whose token's natural expiry has
// passed. Call this periodically (e.g., every minute or on each
// Revoke call) to prevent unbounded growth.
func (b *Blacklist) Cleanup(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	removed := 0
	for tokenID, entry := range b.entries {
		if !now.Before(entry.tokenExpiresAt) {
			delete(b.entries, tokenID)
			removed++
		}
	}
	return removed
}

// Len returns the current number of entries in the blacklist.
func (b *Blacklist) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}
