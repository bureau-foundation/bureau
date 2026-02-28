// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// GetState reads a typed state event from a Matrix room. It calls
// GetStateEvent on the session and unmarshals the raw JSON content into
// T. This is the standard way to read typed Bureau state events:
//
//	devTeam, err := messaging.GetState[schema.DevTeamContent](ctx, session, roomID, schema.EventTypeDevTeam, "")
//	workspace, err := messaging.GetState[workspace.WorkspaceState](ctx, session, roomID, schema.EventTypeWorkspace, "")
//
// Returns an error if the state event does not exist (M_NOT_FOUND) or
// if the content cannot be unmarshaled into T.
func GetState[T any](ctx context.Context, session Session, roomID ref.RoomID, eventType ref.EventType, stateKey string) (T, error) {
	var zero T
	content, err := session.GetStateEvent(ctx, roomID, eventType, stateKey)
	if err != nil {
		return zero, fmt.Errorf("reading %s[%q] from room %s: %w", eventType, stateKey, roomID, err)
	}
	var result T
	if err := json.Unmarshal(content, &result); err != nil {
		return zero, fmt.Errorf("unmarshaling %s from room %s: %w", eventType, roomID, err)
	}
	return result, nil
}
