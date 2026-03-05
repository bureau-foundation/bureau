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

// StateWithSender holds a typed state event together with the Matrix
// user ID of the principal who published it. Used for authorization
// checks that need to verify the publisher's identity (e.g., template
// author credential access checks).
type StateWithSender[T any] struct {
	Content T
	Sender  ref.UserID
}

// GetStateWithSender reads a typed state event from a Matrix room and
// returns both the typed content and the sender who published it.
//
// Unlike GetState, which uses the /state/{type}/{key} endpoint (which
// returns only content), this fetches the full room state to find the
// matching event with its envelope metadata. Use this only when the
// sender identity is needed — GetState is more efficient for content-
// only reads.
//
// Returns an error if the state event does not exist or if the content
// cannot be unmarshaled into T.
func GetStateWithSender[T any](ctx context.Context, session Session, roomID ref.RoomID, eventType ref.EventType, stateKey string) (StateWithSender[T], error) {
	var zero StateWithSender[T]

	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return zero, fmt.Errorf("fetching room state from %s: %w", roomID, err)
	}

	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		if event.StateKey == nil || *event.StateKey != stateKey {
			continue
		}

		contentBytes, marshalErr := json.Marshal(event.Content)
		if marshalErr != nil {
			return zero, fmt.Errorf("re-marshaling %s[%q] content from room %s: %w", eventType, stateKey, roomID, marshalErr)
		}
		var result T
		if unmarshalErr := json.Unmarshal(contentBytes, &result); unmarshalErr != nil {
			return zero, fmt.Errorf("unmarshaling %s[%q] from room %s: %w", eventType, stateKey, roomID, unmarshalErr)
		}

		return StateWithSender[T]{Content: result, Sender: event.Sender}, nil
	}

	return zero, fmt.Errorf("state event %s[%q] not found in room %s", eventType, stateKey, roomID)
}
