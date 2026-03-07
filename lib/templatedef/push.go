// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package templatedef

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// PushResult holds the result of a successful template push.
type PushResult struct {
	// EventID is the Matrix event ID of the published m.bureau.template
	// state event.
	EventID ref.EventID

	// RoomID is the Matrix room ID where the template was published.
	RoomID ref.RoomID

	// RoomAlias is the full room alias of the target room.
	RoomAlias ref.RoomAlias
}

// Push publishes a template as an m.bureau.template state event. The template
// reference determines which room and state key to use. If the template
// inherits from parent templates, each parent's existence in Matrix is
// verified before publishing.
//
// The session must have permission to read the target room (for alias
// resolution and parent verification) and write state events to it.
func Push(ctx context.Context, session messaging.Session, templateRef schema.TemplateRef, content schema.TemplateContent, serverName ref.ServerName) (*PushResult, error) {
	// Resolve the target room.
	roomAlias := templateRef.RoomAlias(serverName)
	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving target room %q: %w", roomAlias, err)
	}

	// Verify all parent templates exist if inheritance is specified.
	for index, parentRefString := range content.Inherits {
		parentRef, err := schema.ParseTemplateRef(parentRefString)
		if err != nil {
			return nil, fmt.Errorf("inherits[%d] reference %q is invalid: %w", index, parentRefString, err)
		}
		if _, err := Fetch(ctx, session, parentRef, serverName); err != nil {
			return nil, fmt.Errorf("parent template %q not found: %w", parentRefString, err)
		}
	}

	// Publish the template state event.
	eventID, err := session.SendStateEvent(ctx, roomID, schema.EventTypeTemplate, templateRef.Template, content)
	if err != nil {
		return nil, fmt.Errorf("publishing template %q to room %q (%s): %w", templateRef.Template, roomAlias, roomID, err)
	}

	// Verify the published state event is readable. The Matrix spec
	// guarantees read-after-write consistency for state events: once
	// PUT returns 200, GET must return the current state. If this
	// read-back fails, the homeserver has a linearizability bug. We
	// surface it here rather than letting callers (principal creation,
	// template inheritance verification) fail with a confusing
	// "template not found" error far from the publish site.
	if _, err := Fetch(ctx, session, templateRef, serverName); err != nil {
		return nil, fmt.Errorf("template %q published (event_id=%s) but read-back failed — homeserver state inconsistency: %w",
			templateRef.Template, eventID, err)
	}

	return &PushResult{
		EventID:   eventID,
		RoomID:    roomID,
		RoomAlias: roomAlias,
	}, nil
}
