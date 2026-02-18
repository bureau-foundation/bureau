// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// commandResult is the parsed content of an m.bureau.command_result
// message, received as a threaded reply to a command event.
type commandResult struct {
	Status     string         `json:"status"`
	Result     map[string]any `json:"result"`
	Error      string         `json:"error"`
	DurationMS int64          `json:"duration_ms"`
	RequestID  string         `json:"request_id"`
}

// resolveWorkspaceRoom validates the alias and resolves it to a Matrix
// room ID. The alias is the Bureau localpart (e.g., "iree/amdgpu/inference")
// without the leading # or trailing :server.
func resolveWorkspaceRoom(ctx context.Context, session messaging.Session, alias, serverName string) (string, error) {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return "", cli.Validation("invalid alias %q: %w", alias, err)
	}

	fullAlias := principal.RoomAlias(alias, serverName)
	roomID, err := session.ResolveAlias(ctx, fullAlias)
	if err != nil {
		return "", cli.NotFound("resolving room %s: %w", fullAlias, err)
	}
	return roomID, nil
}

// findParentWorkspace walks up the alias path to find the first ancestor
// room containing an m.bureau.workspace state event. For example, given
// alias "iree/feature/amdgpu", it tries "#iree/feature:server" then
// "#iree:server".
//
// Returns the parent workspace's room ID, its parsed workspace state,
// and the alias localpart that matched. Returns an error if no parent
// workspace is found.
func findParentWorkspace(ctx context.Context, session messaging.Session, alias, serverName string) (string, *schema.WorkspaceState, string, error) {
	// Walk up the alias path, dropping the last segment each time.
	remaining := alias
	for {
		lastSlash := strings.LastIndex(remaining, "/")
		if lastSlash < 0 {
			return "", nil, "", cli.NotFound("no parent workspace found for %q (walked up to root)", alias)
		}
		candidate := remaining[:lastSlash]
		remaining = candidate

		fullAlias := principal.RoomAlias(candidate, serverName)
		roomID, err := session.ResolveAlias(ctx, fullAlias)
		if err != nil {
			// Room doesn't exist at this level — keep walking up.
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				continue
			}
			return "", nil, "", cli.Internal("resolving %s: %w", fullAlias, err)
		}

		// Room exists — check for workspace state.
		state, err := readWorkspaceState(ctx, session, roomID)
		if err != nil {
			// No workspace state at this level — keep walking.
			continue
		}

		return roomID, state, candidate, nil
	}
}

// sendWorkspaceCommand sends an m.bureau.command message to a workspace
// room and returns the event ID and request ID. The command is sent as
// an m.room.message event with msgtype m.bureau.command, which the
// daemon processes via /sync.
func sendWorkspaceCommand(
	ctx context.Context,
	session messaging.Session,
	roomID string,
	commandName string,
	workspace string,
	parameters map[string]any,
) (string, string, error) {
	requestID, err := cli.GenerateRequestID()
	if err != nil {
		return "", "", cli.Internal("generating request ID: %w", err)
	}

	command := schema.CommandMessage{
		MsgType:    schema.MsgTypeCommand,
		Body:       fmt.Sprintf("%s %s", commandName, workspace),
		Command:    commandName,
		Workspace:  workspace,
		RequestID:  requestID,
		Parameters: parameters,
	}

	eventID, err := session.SendEvent(ctx, roomID, "m.room.message", command)
	if err != nil {
		return "", "", cli.Internal("sending %s command: %w", commandName, err)
	}

	return eventID, requestID, nil
}

// waitForCommandResult polls the thread of a command event until a
// m.bureau.command_result reply appears with a matching request ID, or
// the context expires.
//
// The poll interval starts at 250ms and grows to 2s. This balances
// responsiveness for fast commands (status, du) with efficiency for
// slow ones (fetch, pipeline execution).
func waitForCommandResult(
	ctx context.Context,
	session messaging.Session,
	roomID string,
	commandEventID string,
	requestID string,
) (*commandResult, error) {
	pollInterval := 250 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil, cli.Transient("timed out waiting for command result (request_id=%s): %w", requestID, ctx.Err())
		default:
		}

		response, err := session.ThreadMessages(ctx, roomID, commandEventID, messaging.ThreadMessagesOptions{
			Limit: 10,
		})
		if err != nil {
			// Transient errors during polling are expected (homeserver
			// hiccups, brief network issues). Log-and-retry would be
			// appropriate in a daemon, but for a CLI command a context
			// timeout handles this naturally.
			select {
			case <-ctx.Done():
				return nil, cli.Transient("timed out waiting for command result: %w", ctx.Err())
			case <-time.After(pollInterval):
				continue
			}
		}

		for _, event := range response.Chunk {
			msgtype, _ := event.Content["msgtype"].(string)
			if msgtype != schema.MsgTypeCommandResult {
				continue
			}

			// Match on request_id if present. This prevents picking up
			// stale results from previous invocations of the same command
			// in the same thread.
			eventRequestID, _ := event.Content["request_id"].(string)
			if requestID != "" && eventRequestID != requestID {
				continue
			}

			// Parse the result.
			contentJSON, err := json.Marshal(event.Content)
			if err != nil {
				return nil, cli.Internal("marshaling result content: %w", err)
			}

			var result commandResult
			if err := json.Unmarshal(contentJSON, &result); err != nil {
				return nil, cli.Internal("parsing command result: %w", err)
			}

			return &result, nil
		}

		// Back off gradually.
		select {
		case <-ctx.Done():
			return nil, cli.Transient("timed out waiting for command result: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
		if pollInterval < maxInterval {
			pollInterval = pollInterval * 3 / 2
			if pollInterval > maxInterval {
				pollInterval = maxInterval
			}
		}
	}
}

// extractSubpath returns the portion of alias that follows the workspace
// alias prefix. For example, extractSubpath("iree/feature/amdgpu", "iree")
// returns "feature/amdgpu". Returns an error if the workspace alias is not
// a proper prefix of the full alias separated by "/".
func extractSubpath(alias, workspaceAlias string) (string, error) {
	prefix := workspaceAlias + "/"
	if !strings.HasPrefix(alias, prefix) {
		return "", cli.Internal("alias %q does not start with workspace prefix %q", alias, prefix)
	}
	subpath := alias[len(prefix):]
	if subpath == "" {
		return "", cli.Validation("alias %q is identical to workspace alias (no worktree subpath)", alias)
	}
	return subpath, nil
}

// readWorkspaceState reads and parses the m.bureau.workspace state
// event from a room. Returns an error if the event doesn't exist or
// can't be parsed.
func readWorkspaceState(ctx context.Context, session messaging.Session, roomID string) (*schema.WorkspaceState, error) {
	raw, err := session.GetStateEvent(ctx, roomID, schema.EventTypeWorkspace, "")
	if err != nil {
		return nil, cli.Internal("reading workspace state: %w", err)
	}

	var state schema.WorkspaceState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, cli.Internal("parsing workspace state: %w", err)
	}
	return &state, nil
}
