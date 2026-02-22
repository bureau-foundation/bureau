// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// RoomWatcher captures a position in the Matrix /sync stream for a
// specific room. Create one with WatchRoom BEFORE triggering the action
// that generates the expected event, then call WaitForEvent to receive
// events arriving after the checkpoint.
//
// All waiting uses Matrix /sync long-polling: the server holds the
// connection until new events arrive, then returns immediately. There
// is no client-side polling interval.
//
// RoomWatcher is not safe for concurrent use by multiple goroutines.
// For fan-out, create multiple independent watchers — each maintains
// its own sync position on the same Session. This works because
// Session.Sync is stateless: the since token travels as a query
// parameter, not server-side session state.
type RoomWatcher struct {
	session   Session
	roomID    ref.RoomID
	filter    string  // optional /sync filter (inline JSON)
	nextBatch string  // sync token at the captured position
	pending   []Event // events received but not yet consumed
}

// WatchRoom captures the current position in the Matrix /sync stream.
// The returned RoomWatcher only sees events arriving after this call.
//
// This performs an immediate /sync (timeout=0) to obtain the current
// next_batch token without blocking. The token anchors all subsequent
// long-poll calls. Pass an empty filter to receive all event types.
func WatchRoom(ctx context.Context, session Session, roomID ref.RoomID, filter string) (*RoomWatcher, error) {
	if roomID.IsZero() {
		return nil, fmt.Errorf("messaging: WatchRoom requires a non-zero room ID")
	}
	response, err := session.Sync(ctx, SyncOptions{
		SetTimeout: true,
		Timeout:    0,
		Filter:     filter,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: initial sync for room watch: %w", err)
	}
	return &RoomWatcher{
		session:   session,
		roomID:    roomID,
		filter:    filter,
		nextBatch: response.NextBatch,
	}, nil
}

// maxSyncRetries is the number of consecutive /sync failures allowed
// before WaitForEvent returns an error. Each retry uses a 1-second
// server-side timeout so the HTTP round-trip itself provides backoff.
const maxSyncRetries = 5

// longPollTimeout is the server-side long-poll hold time in
// milliseconds for normal /sync calls. The server holds the connection
// for up to this duration, returning immediately when new events
// arrive. 30 seconds matches the Matrix client-server spec
// recommendation.
const longPollTimeout = 30000

// retryTimeout is the server-side timeout in milliseconds used after
// a /sync error. Short so the retry completes quickly and the next
// attempt can proceed.
const retryTimeout = 1000

// WaitForEvent blocks until an event matching the predicate arrives in
// the watched room. Events are buffered: when a /sync response
// delivers multiple events, all are stored in pending. The predicate
// scans pending events before issuing a new /sync, so events are never
// dropped when multiple matching events arrive in the same batch.
//
// Uses a 30-second server-side long-poll hold. Bounded by ctx. On
// transient /sync errors, retries up to 5 times with 1-second server
// timeout (the HTTP round-trip provides backoff). Resets idle
// connections on error if the Session supports it.
func (w *RoomWatcher) WaitForEvent(ctx context.Context, predicate func(Event) bool) (Event, error) {
	var syncRetries int
	for {
		// Scan pending events from previous sync responses before
		// issuing a new /sync. This is critical for correctness: when
		// a sync response contains multiple matching events (e.g., two
		// command_result messages in the same batch), the first
		// WaitForEvent call consumes one and the second call must find
		// the other here without needing another round-trip.
		for i, event := range w.pending {
			if predicate(event) {
				w.pending = append(w.pending[:i], w.pending[i+1:]...)
				return event, nil
			}
		}

		// On retry after a sync error, use a short server-side
		// timeout (1s) so the HTTP round-trip itself provides
		// backoff. On first attempt or after success, use the
		// normal 30s long-poll hold.
		syncTimeout := longPollTimeout
		if syncRetries > 0 {
			syncTimeout = retryTimeout
		}
		response, err := w.session.Sync(ctx, SyncOptions{
			Since:      w.nextBatch,
			SetTimeout: true,
			Timeout:    syncTimeout,
			Filter:     w.filter,
		})
		if err != nil {
			if ctx.Err() != nil {
				return Event{}, fmt.Errorf("context cancelled waiting for event in room %s: %w", w.roomID, ctx.Err())
			}
			syncRetries++
			// TCP-level errors (connection reset, EOF) often indicate
			// a poisoned connection in Go's HTTP pool. Drop idle
			// connections so the next attempt opens a fresh socket.
			if closer, ok := w.session.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
			if syncRetries > maxSyncRetries {
				return Event{}, fmt.Errorf("sync failed %d consecutive times waiting for event in room %s: %w",
					syncRetries, w.roomID, err)
			}
			slog.Debug("room watcher sync error, retrying",
				"room_id", w.roomID,
				"attempt", syncRetries,
				"max_attempts", maxSyncRetries,
				"error", err,
			)
			continue
		}
		syncRetries = 0
		w.nextBatch = response.NextBatch

		if joined, ok := response.Rooms.Join[w.roomID]; ok {
			w.pending = append(w.pending, joined.State.Events...)
			w.pending = append(w.pending, joined.Timeline.Events...)
		}
	}
}

// SyncPosition returns the current sync stream position token.
func (w *RoomWatcher) SyncPosition() string {
	return w.nextBatch
}

// RoomID returns the room being watched.
func (w *RoomWatcher) RoomID() ref.RoomID {
	return w.roomID
}
