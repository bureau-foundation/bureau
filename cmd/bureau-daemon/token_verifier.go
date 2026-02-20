// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// tokenVerifier validates Matrix access tokens by calling the homeserver's
// /account/whoami endpoint and caches successful results to avoid per-request
// round-trips. Failed verifications are never cached — an invalid token is
// always re-checked in case the problem was transient.
//
// The cache is keyed by SHA-256 hash of the token (not the raw token itself)
// to minimize the window of token exposure in memory.
type tokenVerifier struct {
	client *messaging.Client
	logger *slog.Logger
	clock  clock.Clock

	// cacheDuration controls how long successful verifications are cached.
	// After expiry, the next request triggers a fresh whoami call.
	cacheDuration time.Duration

	mu    sync.RWMutex
	cache map[[sha256.Size]byte]tokenCacheEntry
}

// tokenCacheEntry holds a successful verification result.
type tokenCacheEntry struct {
	userID    string
	expiresAt time.Time
}

// newTokenVerifier creates a verifier that caches successful results for
// the given duration. The messaging.Client is used to create temporary
// sessions for whoami calls. The clock controls time for cache expiry
// checks — production code passes clock.Real(), tests pass clock.Fake().
func newTokenVerifier(client *messaging.Client, cacheDuration time.Duration, clk clock.Clock, logger *slog.Logger) *tokenVerifier {
	return &tokenVerifier{
		client:        client,
		logger:        logger,
		clock:         clk,
		cacheDuration: cacheDuration,
		cache:         make(map[[sha256.Size]byte]tokenCacheEntry),
	}
}

// Verify validates a Matrix access token and returns the authenticated
// user ID. Returns an error if the token is invalid, expired, or the
// homeserver is unreachable.
//
// Successful verifications are cached — repeated calls with the same
// token return the cached user ID without a homeserver round-trip until
// the cache entry expires.
func (v *tokenVerifier) Verify(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("empty access token")
	}

	key := sha256.Sum256([]byte(token))

	// Check cache (read lock).
	v.mu.RLock()
	entry, found := v.cache[key]
	v.mu.RUnlock()

	if found && v.clock.Now().Before(entry.expiresAt) {
		return entry.userID, nil
	}

	// Cache miss or expired — verify against the homeserver.
	// Create a temporary session with the token. We pass a zero-value
	// UserID because we don't know it yet — WhoAmI will tell us.
	session, err := v.client.SessionFromToken(ref.UserID{}, token)
	if err != nil {
		return "", fmt.Errorf("creating session for verification: %w", err)
	}
	defer session.Close()

	verifiedUserID, err := session.WhoAmI(ctx)
	if err != nil {
		v.logger.Warn("token verification failed", "error", err)
		return "", fmt.Errorf("token verification failed: %w", err)
	}

	userID := verifiedUserID.String()

	// Cache the successful result.
	v.mu.Lock()
	v.cache[key] = tokenCacheEntry{
		userID:    userID,
		expiresAt: v.clock.Now().Add(v.cacheDuration),
	}
	v.mu.Unlock()

	v.logger.Debug("token verified", "user_id", userID)
	return userID, nil
}

// evictExpired removes expired entries from the cache. Called periodically
// by the daemon to prevent unbounded growth.
func (v *tokenVerifier) evictExpired() {
	now := v.clock.Now()
	v.mu.Lock()
	defer v.mu.Unlock()

	for key, entry := range v.cache {
		if now.After(entry.expiresAt) {
			delete(v.cache, key)
		}
	}
}
