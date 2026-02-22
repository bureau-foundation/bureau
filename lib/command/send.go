// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// SendParams configures a command to send to a daemon.
type SendParams struct {
	// Session is the authenticated Matrix session used to send the
	// command and watch for results via /sync.
	Session messaging.Session

	// RoomID is the target room for the command message and result
	// watching. For workspace commands, this is the workspace room.
	// For config-room commands, this is the machine's config room.
	RoomID ref.RoomID

	// Command is the structured command name (e.g., "workspace.status",
	// "workspace.worktree.add", "pipeline.execute").
	Command string

	// Workspace is the target workspace alias for workspace-scoped
	// commands (e.g., "iree/amdgpu/inference"). Empty for global
	// commands like workspace.list.
	Workspace string

	// Parameters carries command-specific arguments. For example,
	// workspace.worktree.add includes "path" and optionally "branch".
	Parameters map[string]any

	// Filter is an optional /sync filter to control what events the
	// result watcher receives. When nil, Send uses a default filter
	// that restricts to m.room.message timeline events (sufficient
	// for command results). Set explicitly to override, e.g., when
	// the watcher also needs state events.
	Filter *messaging.SyncFilter
}

// commandSyncFilter restricts /sync to m.room.message timeline events.
// State events, ephemeral events, presence, and account data are
// excluded since command results are always m.room.message events with
// msgtype m.bureau.command_result.
var commandSyncFilter = &messaging.SyncFilter{
	TimelineTypes: []string{"m.room.message"},
	TimelineLimit: 50,
	ExcludeState:  true,
}

// Send captures the current /sync position, sends the command message,
// and returns a [Future] that watches for results. The position capture
// happens BEFORE the send to prevent a race where the result arrives
// between send and watch-start.
//
// The caller MUST call [Future.Discard] when done.
func Send(ctx context.Context, params SendParams) (*Future, error) {
	filter := params.Filter
	if filter == nil {
		filter = commandSyncFilter
	}

	// Capture sync position BEFORE sending. This ensures the watcher
	// sees the result even if the daemon processes the command and
	// posts the reply before the next /sync call.
	watcher, err := messaging.WatchRoom(ctx, params.Session, params.RoomID, filter)
	if err != nil {
		return nil, fmt.Errorf("capturing sync position: %w", err)
	}

	requestID, err := generateRequestID()
	if err != nil {
		return nil, fmt.Errorf("generating request ID: %w", err)
	}

	command := schema.CommandMessage{
		MsgType:    schema.MsgTypeCommand,
		Body:       fmt.Sprintf("%s %s", params.Command, params.Workspace),
		Command:    params.Command,
		Workspace:  params.Workspace,
		RequestID:  requestID,
		Parameters: params.Parameters,
	}

	eventID, err := params.Session.SendEvent(ctx, params.RoomID, schema.MatrixEventTypeMessage, command)
	if err != nil {
		return nil, fmt.Errorf("sending %s command: %w", params.Command, err)
	}

	return &Future{
		watcher:   watcher,
		requestID: requestID,
		eventID:   eventID,
	}, nil
}

// Execute sends a command and waits for a single result. This is the
// standard pattern for synchronous commands (status, du, fetch,
// worktree.list) where the daemon processes the command and posts
// exactly one result.
func Execute(ctx context.Context, params SendParams) (*Result, error) {
	future, err := Send(ctx, params)
	if err != nil {
		return nil, err
	}
	defer future.Discard()
	return future.Wait(ctx)
}

// ExecutePhased sends a command and collects count results. For async
// commands that return an "accepted" acknowledgment followed by a
// pipeline result, use count=2. Results are returned in arrival order.
func ExecutePhased(ctx context.Context, params SendParams, count int) ([]*Result, error) {
	future, err := Send(ctx, params)
	if err != nil {
		return nil, err
	}
	defer future.Discard()
	return future.Collect(ctx, count)
}

// generateRequestID creates a random 16-byte hex string for
// correlating command requests with their threaded result replies.
func generateRequestID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer[:]), nil
}
