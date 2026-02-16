// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Retry helpers for transient Matrix API failures. When the homeserver
// is under load (many daemons syncing, many events in flight), event
// sends can fail with connection errors, 429 rate limits, or 5xx
// server errors. These are transient — retrying with backoff succeeds.
//
// Without retry, the daemon logs the error and moves on. The event is
// permanently lost: tests waiting for command results or confirmation
// messages hang until the test timeout fires. Integration stress tests
// (serialized 10x runs) reliably trigger this under load.

import (
	"context"
	"errors"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// sendEventMaxAttempts is the number of times sendEventRetry tries
// before giving up. Three attempts with exponential backoff (1s, 2s)
// covers brief rate limits and server hiccups without blocking the
// sync loop for long.
const sendEventMaxAttempts = 3

// sendEventRetry sends a Matrix event with bounded retry on transient
// errors. Transient errors are connection failures, HTTP 429 (rate
// limit), and HTTP 5xx (server error). Client errors (4xx except 429)
// are permanent and returned immediately.
//
// The context bounds the total retry time — if the daemon is shutting
// down, the context cancels and retries stop.
func (d *Daemon) sendEventRetry(ctx context.Context, roomID, eventType string, content any) (string, error) {
	var lastError error
	for attempt := 0; attempt < sendEventMaxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-d.clock.After(backoff):
			}
		}

		eventID, err := d.session.SendEvent(ctx, roomID, eventType, content)
		if err == nil {
			return eventID, nil
		}
		lastError = err

		if !isTransientError(err) {
			return "", err
		}

		d.logger.Warn("transient event send failure, retrying",
			"room_id", roomID,
			"event_type", eventType,
			"attempt", attempt+1,
			"error", err,
		)
	}
	return "", lastError
}

// sendMessageRetry sends a Matrix message with bounded retry on
// transient errors. Same retry policy as sendEventRetry.
func (d *Daemon) sendMessageRetry(ctx context.Context, roomID string, content messaging.MessageContent) (string, error) {
	var lastError error
	for attempt := 0; attempt < sendEventMaxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-d.clock.After(backoff):
			}
		}

		eventID, err := d.session.SendMessage(ctx, roomID, content)
		if err == nil {
			return eventID, nil
		}
		lastError = err

		if !isTransientError(err) {
			return "", err
		}

		d.logger.Warn("transient message send failure, retrying",
			"room_id", roomID,
			"attempt", attempt+1,
			"error", err,
		)
	}
	return "", lastError
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
