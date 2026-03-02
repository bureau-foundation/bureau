// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

// rateLimitTracker tracks GitHub API rate limit state from response
// headers. Each response updates the tracker with the latest limit,
// remaining count, and reset timestamp. The tracker provides preemptive
// blocking: before a request is sent, it checks whether the rate limit
// is exhausted and sleeps until the reset window if so.
type rateLimitTracker struct {
	mu        sync.Mutex
	remaining int
	reset     time.Time
	known     bool // true after the first response with rate limit headers
	clock     clock.Clock
}

func newRateLimitTracker(clock clock.Clock) *rateLimitTracker {
	return &rateLimitTracker{clock: clock}
}

// update records rate limit state from HTTP response headers. Called
// after every API response.
func (tracker *rateLimitTracker) update(header http.Header) {
	remainingStr := header.Get("X-RateLimit-Remaining")
	resetStr := header.Get("X-RateLimit-Reset")

	if remainingStr == "" || resetStr == "" {
		return
	}

	remaining, err := strconv.Atoi(remainingStr)
	if err != nil {
		return
	}

	resetUnix, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		return
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	tracker.remaining = remaining
	tracker.reset = time.Unix(resetUnix, 0)
	tracker.known = true
}

// wait blocks until the rate limit window resets if the tracker knows
// the limit is exhausted. Returns immediately if the limit is not
// exhausted, not yet known, or the reset time has passed. Respects
// context cancellation.
//
// Returns an error only if the context is cancelled while waiting.
func (tracker *rateLimitTracker) wait(ctx context.Context) error {
	tracker.mu.Lock()
	if !tracker.known || tracker.remaining > 0 {
		tracker.mu.Unlock()
		return nil
	}

	sleepDuration := tracker.reset.Sub(tracker.clock.Now())
	tracker.mu.Unlock()

	if sleepDuration <= 0 {
		return nil
	}

	select {
	case <-tracker.clock.After(sleepDuration):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfter computes the backoff duration from a rate-limited response.
// Checks the Retry-After header first (secondary rate limits), then
// falls back to the X-RateLimit-Reset timestamp. Returns zero if no
// backoff information is available.
func (tracker *rateLimitTracker) retryAfter(header http.Header) time.Duration {
	// Secondary rate limits use Retry-After (seconds).
	if retryStr := header.Get("Retry-After"); retryStr != "" {
		if seconds, err := strconv.Atoi(retryStr); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	// Primary rate limits use X-RateLimit-Reset (Unix timestamp).
	if resetStr := header.Get("X-RateLimit-Reset"); resetStr != "" {
		if resetUnix, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
			duration := time.Unix(resetUnix, 0).Sub(tracker.clock.Now())
			if duration > 0 {
				return duration
			}
		}
	}

	return 0
}
