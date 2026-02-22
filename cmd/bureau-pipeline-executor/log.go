// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// threadLogger posts pipeline progress to a Matrix thread. A root message
// is created at pipeline start, and each step logs a reply to that thread.
//
// All methods are nil-safe: when the receiver is nil, they are no-ops. This
// lets the executor call logging methods unconditionally without checking
// whether thread logging is configured.
type threadLogger struct {
	logger      *slog.Logger
	session     messaging.Session
	roomID      ref.RoomID
	rootEventID ref.EventID
}

// newThreadLogger creates a thread in the specified room for pipeline
// progress logging. The root message announces the pipeline start and
// its event ID anchors all subsequent thread replies.
//
// When room starts with "#", it is resolved to a room ID via the proxy.
//
// Returns an error if the thread cannot be created. The caller should
// treat this as fatal when observation is configured — running a pipeline
// with no record of what happened is worse than not running at all.
func newThreadLogger(ctx context.Context, session messaging.Session, room string, pipelineName string, stepCount int, logger *slog.Logger) (*threadLogger, error) {
	var roomID ref.RoomID
	if len(room) > 0 && room[0] == '#' {
		alias, err := ref.ParseRoomAlias(room)
		if err != nil {
			return nil, fmt.Errorf("invalid log room alias %q: %w", room, err)
		}
		resolved, err := session.ResolveAlias(ctx, alias)
		if err != nil {
			return nil, fmt.Errorf("resolving log room %q: %w", room, err)
		}
		roomID = resolved
	} else {
		parsed, err := ref.ParseRoomID(room)
		if err != nil {
			return nil, fmt.Errorf("invalid log room ID %q: %w", room, err)
		}
		roomID = parsed
	}

	body := fmt.Sprintf("Pipeline %s started (%d steps)", pipelineName, stepCount)
	rootEventID, err := session.SendMessage(ctx, roomID, messaging.NewTextMessage(body))
	if err != nil {
		return nil, fmt.Errorf("creating pipeline thread in %s: %w", roomID, err)
	}

	return &threadLogger{
		logger:      logger,
		session:     session,
		roomID:      roomID,
		rootEventID: rootEventID,
	}, nil
}

// logEventID returns the root event ID of the pipeline thread, or
// empty string if thread logging is not configured. The daemon uses
// this to link command results to the detailed execution log thread.
func (l *threadLogger) logEventID() ref.EventID {
	if l == nil {
		return ref.EventID{}
	}
	return l.rootEventID
}

// logRoomID returns the resolved Matrix room ID where this logger posts
// thread messages, or empty string if thread logging is not configured.
// Used by the result event publisher to publish to the same room.
func (l *threadLogger) logRoomID() ref.RoomID {
	if l == nil {
		return ref.RoomID{}
	}
	return l.roomID
}

// logStep posts a step status update as a thread reply.
func (l *threadLogger) logStep(ctx context.Context, index, total int, name, status string, duration time.Duration) {
	if l == nil {
		return
	}

	body := fmt.Sprintf("step %d/%d: %s... %s (%s)", index+1, total, name, status, formatDuration(duration))
	l.sendThreadReply(ctx, body)
}

// logComplete posts a pipeline completion message as a thread reply.
func (l *threadLogger) logComplete(ctx context.Context, name string, duration time.Duration) {
	if l == nil {
		return
	}

	body := fmt.Sprintf("Pipeline %s: complete (%s)", name, formatDuration(duration))
	l.sendThreadReply(ctx, body)
}

// logFailed posts a pipeline failure message as a thread reply.
func (l *threadLogger) logFailed(ctx context.Context, name, stepName string, err error) {
	if l == nil {
		return
	}

	body := fmt.Sprintf("Pipeline %s: failed at step %q: %v", name, stepName, err)
	l.sendThreadReply(ctx, body)
}

// logAborted posts a pipeline abort message as a thread reply. Abort is
// distinct from failure: it means a precondition check determined the
// pipeline's work is no longer needed (e.g., the resource state changed
// between queuing and execution).
func (l *threadLogger) logAborted(ctx context.Context, name, stepName string, err error) {
	if l == nil {
		return
	}

	body := fmt.Sprintf("Pipeline %s: aborted at step %q: %v", name, stepName, err)
	l.sendThreadReply(ctx, body)
}

func (l *threadLogger) sendThreadReply(ctx context.Context, body string) {
	// Thread replies are best-effort within a running pipeline: if one
	// reply fails to send, we log the error but don't abort the pipeline.
	// The root message creation (in newThreadLogger) is the mandatory
	// check — if we can't create the thread at all, the pipeline doesn't
	// start.
	if _, err := l.session.SendMessage(ctx, l.roomID, messaging.NewThreadReply(l.rootEventID, body)); err != nil {
		l.logger.Warn("failed to send thread reply", "error", err)
	}
}

// formatDuration formats a duration for human-readable status output.
// Uses seconds with one decimal place.
func formatDuration(duration time.Duration) string {
	return fmt.Sprintf("%.1fs", duration.Seconds())
}
