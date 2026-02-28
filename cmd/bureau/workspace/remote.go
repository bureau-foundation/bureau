// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
	"github.com/bureau-foundation/bureau/messaging"
)

// resolveWorkspaceRoom validates the alias and resolves it to a Matrix
// room ID. The alias is the Bureau localpart (e.g., "iree/amdgpu/inference")
// without the leading # or trailing :server.
func resolveWorkspaceRoom(ctx context.Context, session messaging.Session, alias string, serverName ref.ServerName) (ref.RoomID, error) {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return ref.RoomID{}, cli.Validation("invalid alias %q: %w", alias, err)
	}

	fullAlias, err := ref.ParseRoomAlias(schema.FullRoomAlias(alias, serverName))
	if err != nil {
		return ref.RoomID{}, cli.Validation("invalid room alias %q: %w", alias, err)
	}
	roomID, err := session.ResolveAlias(ctx, fullAlias)
	if err != nil {
		return ref.RoomID{}, cli.NotFound("workspace room %s not found: %w", fullAlias, err).
			WithHint("Run 'bureau workspace list' to see workspaces.")
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
func findParentWorkspace(ctx context.Context, session messaging.Session, alias string, serverName ref.ServerName) (ref.RoomID, *workspace.WorkspaceState, string, error) {
	// Walk up the alias path, dropping the last segment each time.
	remaining := alias
	for {
		lastSlash := strings.LastIndex(remaining, "/")
		if lastSlash < 0 {
			return ref.RoomID{}, nil, "", cli.NotFound("no parent workspace found for %q", alias).
				WithHint("Run 'bureau workspace list' to see workspaces.")
		}
		candidate := remaining[:lastSlash]
		remaining = candidate

		fullAlias, parseError := ref.ParseRoomAlias(schema.FullRoomAlias(candidate, serverName))
		if parseError != nil {
			return ref.RoomID{}, nil, "", cli.Internal("invalid room alias for %q: %w", candidate, parseError)
		}
		roomID, err := session.ResolveAlias(ctx, fullAlias)
		if err != nil {
			// Room doesn't exist at this level — keep walking up.
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				continue
			}
			return ref.RoomID{}, nil, "", cli.Internal("resolving %s: %w", fullAlias, err)
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
func readWorkspaceState(ctx context.Context, session messaging.Session, roomID ref.RoomID) (*workspace.WorkspaceState, error) {
	state, err := messaging.GetState[workspace.WorkspaceState](ctx, session, roomID, schema.EventTypeWorkspace, "")
	if err != nil {
		return nil, cli.Internal("reading workspace state: %w", err)
	}
	return &state, nil
}
