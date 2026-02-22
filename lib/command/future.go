// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Future represents an in-flight command whose result will arrive
// asynchronously via the Matrix /sync stream. Create one with [Send].
//
// A Future owns a [messaging.RoomWatcher]. The caller MUST call
// [Discard] when the future is no longer needed — after Wait or
// Collect returns, or when abandoning the command in --no-wait mode.
type Future struct {
	watcher   *messaging.RoomWatcher
	requestID string
}

// Wait blocks until a single command result matching this future's
// request ID arrives. For synchronous commands (status, du, fetch,
// worktree.list), this is the only call needed.
//
// The caller should call [Discard] after Wait returns.
func (f *Future) Wait(ctx context.Context) (*Result, error) {
	event, err := f.watcher.WaitForEvent(ctx, f.isCommandResult)
	if err != nil {
		return nil, fmt.Errorf("waiting for command result (request_id=%s): %w", f.requestID, err)
	}
	return parseResult(event)
}

// Collect blocks until count command results matching this future's
// request ID arrive. Results are returned in arrival order. For
// phased commands (worktree.add returns "accepted" then pipeline
// result), use Collect(ctx, 2).
//
// The caller should call [Discard] after Collect returns.
func (f *Future) Collect(ctx context.Context, count int) ([]*Result, error) {
	var collected []*Result
	for len(collected) < count {
		event, err := f.watcher.WaitForEvent(ctx, f.isCommandResult)
		if err != nil {
			return collected, fmt.Errorf("waiting for command result %d/%d (request_id=%s): %w",
				len(collected)+1, count, f.requestID, err)
		}
		result, parseError := parseResult(event)
		if parseError != nil {
			return collected, fmt.Errorf("parsing command result %d/%d: %w",
				len(collected)+1, count, parseError)
		}
		collected = append(collected, result)
	}
	return collected, nil
}

// Discard releases the future's resources. Safe to call multiple
// times. Use this for --no-wait mode or after Wait/Collect returns.
func (f *Future) Discard() {
	// RoomWatcher has no background goroutine to stop — it only does
	// work when WaitForEvent is called. Discard is a semantic marker
	// that the caller is done with this future. If RoomWatcher gains
	// resource cleanup, call it here.
}

// RequestID returns the request ID used to correlate this command
// with its results.
func (f *Future) RequestID() string {
	return f.requestID
}

// isCommandResult returns true if the event is a command_result
// message matching this future's request ID.
func (f *Future) isCommandResult(event messaging.Event) bool {
	if event.Type != schema.MatrixEventTypeMessage {
		return false
	}
	msgtype, _ := event.Content["msgtype"].(string)
	if msgtype != schema.MsgTypeCommandResult {
		return false
	}
	eventRequestID, _ := event.Content["request_id"].(string)
	return eventRequestID == f.requestID
}
