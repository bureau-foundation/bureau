// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// PushResult holds the result of a successful template push.
type PushResult struct {
	// EventID is the Matrix event ID of the published m.bureau.template
	// state event.
	EventID string

	// RoomID is the Matrix room ID where the template was published.
	RoomID string

	// RoomAlias is the full room alias of the target room.
	RoomAlias string
}

// Push publishes a template as an m.bureau.template state event. The template
// reference determines which room and state key to use. If the template
// inherits from a parent, the parent's existence in Matrix is verified before
// publishing.
//
// The session must have permission to read the target room (for alias
// resolution and parent verification) and write state events to it.
func Push(ctx context.Context, session *messaging.Session, ref schema.TemplateRef, content schema.TemplateContent, serverName string) (*PushResult, error) {
	// Resolve the target room.
	roomAlias := ref.RoomAlias(serverName)
	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving target room %q: %w", roomAlias, err)
	}

	// Verify the parent template exists if inheritance is specified.
	if content.Inherits != "" {
		parentRef, err := schema.ParseTemplateRef(content.Inherits)
		if err != nil {
			return nil, fmt.Errorf("invalid inherits reference %q: %w", content.Inherits, err)
		}
		if _, err := Fetch(ctx, session, parentRef, serverName); err != nil {
			return nil, fmt.Errorf("parent template %q not found: %w", content.Inherits, err)
		}
	}

	// Publish the template state event.
	eventID, err := session.SendStateEvent(ctx, roomID, schema.EventTypeTemplate, ref.Template, content)
	if err != nil {
		return nil, fmt.Errorf("publishing template %q to room %q (%s): %w", ref.Template, roomAlias, roomID, err)
	}

	return &PushResult{
		EventID:   eventID,
		RoomID:    roomID,
		RoomAlias: roomAlias,
	}, nil
}
