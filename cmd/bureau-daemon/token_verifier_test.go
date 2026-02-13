// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestTokenVerifierValidToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/account/whoami" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		token := r.Header.Get("Authorization")
		if token != "Bearer valid-token" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_UNKNOWN_TOKEN",
				"error":   "Invalid token",
			})
			return
		}
		json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
			UserID: "@bureau-admin:bureau.local",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	verifier := newTokenVerifier(client, 5*time.Minute, clock.Real(), slog.Default())

	userID, err := verifier.Verify(context.Background(), "valid-token")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if userID != "@bureau-admin:bureau.local" {
		t.Errorf("Verify returned %q, want @bureau-admin:bureau.local", userID)
	}
}

func TestTokenVerifierInvalidToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_UNKNOWN_TOKEN",
			"error":   "Invalid token",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	verifier := newTokenVerifier(client, 5*time.Minute, clock.Real(), slog.Default())

	_, err = verifier.Verify(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("Verify should have returned an error for invalid token")
	}
}

func TestTokenVerifierEmptyToken(t *testing.T) {
	t.Parallel()

	verifier := newTokenVerifier(nil, 5*time.Minute, clock.Real(), slog.Default())

	_, err := verifier.Verify(context.Background(), "")
	if err == nil {
		t.Fatal("Verify should have returned an error for empty token")
	}
}

func TestTokenVerifierCachesSuccess(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
			UserID: "@bureau-admin:bureau.local",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	verifier := newTokenVerifier(client, 5*time.Minute, clock.Real(), slog.Default())
	ctx := context.Background()

	// First call should hit the server.
	userID, err := verifier.Verify(ctx, "cached-token")
	if err != nil {
		t.Fatalf("Verify (first): %v", err)
	}
	if userID != "@bureau-admin:bureau.local" {
		t.Errorf("Verify returned %q, want @bureau-admin:bureau.local", userID)
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", callCount.Load())
	}

	// Second call with the same token should use the cache.
	userID, err = verifier.Verify(ctx, "cached-token")
	if err != nil {
		t.Fatalf("Verify (second): %v", err)
	}
	if userID != "@bureau-admin:bureau.local" {
		t.Errorf("Verify returned %q, want @bureau-admin:bureau.local", userID)
	}
	if callCount.Load() != 1 {
		t.Errorf("expected 1 server call (cached), got %d", callCount.Load())
	}
}

func TestTokenVerifierDoesNotCacheFailure(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_UNKNOWN_TOKEN",
			"error":   "Invalid token",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	verifier := newTokenVerifier(client, 5*time.Minute, clock.Real(), slog.Default())
	ctx := context.Background()

	// First call fails.
	_, err = verifier.Verify(ctx, "bad-token")
	if err == nil {
		t.Fatal("Verify should fail")
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", callCount.Load())
	}

	// Second call should hit server again (failures are not cached).
	_, err = verifier.Verify(ctx, "bad-token")
	if err == nil {
		t.Fatal("Verify should fail")
	}
	if callCount.Load() != 2 {
		t.Errorf("expected 2 server calls (not cached), got %d", callCount.Load())
	}
}

func TestTokenVerifierCacheExpiry(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
			UserID: "@bureau-admin:bureau.local",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	verifier := newTokenVerifier(client, 5*time.Minute, fakeClock, slog.Default())
	ctx := context.Background()

	// First call hits server.
	_, err = verifier.Verify(ctx, "expiring-token")
	if err != nil {
		t.Fatalf("Verify (first): %v", err)
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", callCount.Load())
	}

	// Advance past cache expiry.
	fakeClock.Advance(6 * time.Minute)

	// Second call should hit server again.
	_, err = verifier.Verify(ctx, "expiring-token")
	if err != nil {
		t.Fatalf("Verify (after expiry): %v", err)
	}
	if callCount.Load() != 2 {
		t.Errorf("expected 2 server calls (cache expired), got %d", callCount.Load())
	}
}

func TestTokenVerifierEvictExpired(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
			UserID: "@bureau-admin:bureau.local",
		})
	}))
	defer server.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	verifier := newTokenVerifier(client, 5*time.Minute, fakeClock, slog.Default())
	ctx := context.Background()

	// Populate cache.
	_, err = verifier.Verify(ctx, "evict-token-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	_, err = verifier.Verify(ctx, "evict-token-2")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	verifier.mu.RLock()
	cacheSize := len(verifier.cache)
	verifier.mu.RUnlock()
	if cacheSize != 2 {
		t.Fatalf("cache size = %d, want 2", cacheSize)
	}

	// Advance past cache expiry and evict.
	fakeClock.Advance(6 * time.Minute)
	verifier.evictExpired()

	verifier.mu.RLock()
	cacheSize = len(verifier.cache)
	verifier.mu.RUnlock()
	if cacheSize != 0 {
		t.Errorf("cache size after eviction = %d, want 0", cacheSize)
	}
}
