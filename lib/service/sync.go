// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/messaging"
)

// SyncConfig configures the Matrix /sync long-poll loop.
type SyncConfig struct {
	// Filter is the inline JSON filter restricting which event types
	// the homeserver returns. Each service defines its own filter
	// matching the event types it cares about.
	Filter string

	// Timeout is the long-poll timeout in milliseconds. The homeserver
	// holds the connection open for this duration when no events are
	// available, then returns an empty response. Default: 30000 (30s).
	Timeout int

	// MaxBackoff is the maximum duration between retry attempts on
	// transient /sync errors. The loop uses exponential backoff
	// starting at 1 second. Default: 30 seconds.
	MaxBackoff time.Duration
}

// SyncHandler is called for each /sync response. Implementations
// process events (update indexes, evaluate gates, etc.) and return.
// The next /sync poll starts after the handler returns. Handlers
// should not block for extended periods.
type SyncHandler func(ctx context.Context, response *messaging.SyncResponse)

// InitialSync performs the first Matrix /sync with no since token to
// obtain a full state snapshot. Returns the next_batch token for the
// incremental loop and the full response for the caller to build
// initial state from.
//
// Unlike incremental sync, this returns immediately — the homeserver
// sends the current state without waiting for new events.
func InitialSync(ctx context.Context, session *messaging.Session, filter string) (string, *messaging.SyncResponse, error) {
	response, err := session.Sync(ctx, messaging.SyncOptions{
		Filter: filter,
	})
	if err != nil {
		return "", nil, fmt.Errorf("initial sync: %w", err)
	}
	return response.NextBatch, response, nil
}

// RunSyncLoop runs the incremental Matrix /sync long-poll loop. It
// polls the homeserver with the given since token and calls handler
// for each response. The loop continues until ctx is cancelled.
//
// On transient errors, the loop retries with exponential backoff
// (1 second to config.MaxBackoff). On context cancellation (service
// shutdown), the loop returns cleanly.
//
// The caller is responsible for performing the initial sync (via
// InitialSync) and processing that response before starting this
// loop. This separation lets services build their initial state
// (indexes, caches) synchronously before entering the event-driven
// incremental phase.
func RunSyncLoop(ctx context.Context, session *messaging.Session, config SyncConfig, sinceToken string, handler SyncHandler, clk clock.Clock, logger *slog.Logger) {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30000
	}
	maxBackoff := config.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 30 * time.Second
	}

	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		options := messaging.SyncOptions{
			Since:      sinceToken,
			Timeout:    timeout,
			SetTimeout: true,
			Filter:     config.Filter,
		}

		response, err := session.Sync(ctx, options)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("sync failed, retrying", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-clk.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = time.Second
		sinceToken = response.NextBatch

		handler(ctx, response)
	}
}

// AcceptInvites joins all rooms the service has been invited to.
// Returns the room IDs that were successfully joined. Services should
// call this during initial sync processing and on each incremental
// sync that contains invites.
func AcceptInvites(ctx context.Context, session *messaging.Session, invites map[string]messaging.InvitedRoom, logger *slog.Logger) []string {
	var accepted []string
	for roomID := range invites {
		logger.Info("accepting room invite", "room_id", roomID)
		if _, err := session.JoinRoom(ctx, roomID); err != nil {
			logger.Error("failed to accept room invite",
				"room_id", roomID,
				"error", err,
			)
			continue
		}
		accepted = append(accepted, roomID)
	}
	return accepted
}
