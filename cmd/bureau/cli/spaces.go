// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// GetSpaceChildren returns the room IDs of all child rooms in a Matrix
// space. Reads the space's m.space.child state events and parses the
// state keys as room IDs. Entries with unparseable state keys are
// silently skipped (they represent invalid or removed children).
//
// Used by enable commands (ticket, pipeline) to discover rooms in a
// space for per-room configuration.
func GetSpaceChildren(ctx context.Context, session messaging.Session, spaceRoomID ref.RoomID) ([]ref.RoomID, error) {
	events, err := session.GetRoomState(ctx, spaceRoomID)
	if err != nil {
		return nil, fmt.Errorf("fetching space state for %s: %w", spaceRoomID, err)
	}

	var children []ref.RoomID
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			childID, parseError := ref.ParseRoomID(*event.StateKey)
			if parseError != nil {
				continue
			}
			children = append(children, childID)
		}
	}
	return children, nil
}
