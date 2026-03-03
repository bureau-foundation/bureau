// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Retry helpers for transient Matrix API failures. When the homeserver
// is under load (many daemons syncing, many events in flight), event
// sends can fail with connection errors, 429 rate limits, or 5xx
// server errors. These are transient — retrying with backoff succeeds.
//
// The retry loop runs until success, permanent error, or context
// cancellation. There is no retry count limit for transient errors.
// A lost event causes callers (agents, operators, tests) to hang
// indefinitely waiting for a response that will never arrive — that
// is always worse than blocking the sync loop for longer. The
// daemon's context (tied to its process lifetime) is the natural
// bound: if the daemon is still running, it should keep trying.

import (
	"context"
	"errors"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// sendEventMaxBackoff caps the exponential backoff at 30 seconds.
// This matches the sync loop's max backoff and ensures the retry
// doesn't block indefinitely between attempts.
const sendEventMaxBackoff = 30 * time.Second

// sendEventRetry sends a Matrix event, retrying on transient errors
// until success, permanent error, or context cancellation. Transient
// errors are connection failures, HTTP 429 (rate limit), and HTTP 5xx
// (server error). Client errors (4xx except 429) are permanent and
// returned immediately.
//
// Uses exponential backoff starting at 1 second, capped at 30
// seconds. The context bounds the total retry time — when the daemon
// shuts down, the context cancels and retries stop. There is no
// attempt limit: a lost event is always worse than a delayed one.
func (d *Daemon) sendEventRetry(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, content any) (ref.EventID, error) {
	backoff := time.Second

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ref.EventID{}, ctx.Err()
			case <-d.clock.After(backoff):
			}
			backoff *= 2
			if backoff > sendEventMaxBackoff {
				backoff = sendEventMaxBackoff
			}
		}

		eventID, err := d.session.SendEvent(ctx, roomID, eventType, content)
		if err == nil {
			if attempt > 0 {
				d.logger.Info("event send succeeded after retries",
					"room_id", roomID, "room", d.displayRoom(roomID),
					"event_type", eventType,
					"attempts", attempt+1,
				)
			}
			return eventID, nil
		}

		if !isTransientError(err) {
			return ref.EventID{}, err
		}

		// If the server sent a 429 with retry_after_ms, use it
		// instead of our own exponential backoff. The server knows
		// its own rate limit window better than we do.
		if serverBackoff := retryAfterFromError(err); serverBackoff > 0 {
			backoff = serverBackoff
		}

		d.logger.Warn("transient event send failure, retrying",
			"room_id", roomID, "room", d.displayRoom(roomID),
			"event_type", eventType,
			"attempt", attempt+1,
			"backoff", backoff,
			"error", err,
		)
	}
}

// retryAfterFromError extracts the server-recommended retry delay
// from a 429 MatrixError. Returns zero if the error is not a 429
// or the server didn't include retry_after_ms.
func retryAfterFromError(err error) time.Duration {
	var matrixErr *messaging.MatrixError
	if !errors.As(err, &matrixErr) {
		return 0
	}
	if matrixErr.StatusCode != 429 || matrixErr.RetryAfterMS <= 0 {
		return 0
	}
	return time.Duration(matrixErr.RetryAfterMS) * time.Millisecond
}

// isTransientError returns true for errors that are likely transient
// and worth retrying: connection failures, rate limiting (429), and
// server errors (5xx). Returns false for client errors (4xx except
// 429) which indicate a permanent problem.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	var matrixErr *messaging.MatrixError
	if errors.As(err, &matrixErr) {
		// 429 Too Many Requests — rate limit, transient.
		if matrixErr.StatusCode == 429 {
			return true
		}
		// 5xx — server error, transient.
		if matrixErr.StatusCode >= 500 {
			return true
		}
		// 4xx (except 429) — client error, permanent.
		if matrixErr.StatusCode >= 400 {
			return false
		}
	}

	// Non-Matrix errors (connection refused, timeout, EOF) are transient.
	return true
}
