// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipelinedef

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// PushResult holds the result of a successful pipeline push.
type PushResult struct {
	// EventID is the Matrix event ID of the published m.bureau.pipeline
	// state event.
	EventID ref.EventID

	// RoomID is the Matrix room ID where the pipeline was published.
	RoomID ref.RoomID

	// RoomAlias is the full room alias of the target room.
	RoomAlias ref.RoomAlias
}

// Push publishes a pipeline as an m.bureau.pipeline state event. The pipeline
// reference determines which room and state key to use: the room is resolved
// from the pipeline ref's room alias, and the state key is the pipeline name.
//
// The session must have permission to resolve the target room alias and write
// state events to it.
func Push(ctx context.Context, session messaging.Session, pipelineRef schema.PipelineRef, content pipeline.PipelineContent, serverName ref.ServerName) (*PushResult, error) {
	// Resolve the target room.
	roomAlias := pipelineRef.RoomAlias(serverName)
	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving target room %q: %w", roomAlias, err)
	}

	// Publish the pipeline as a state event.
	eventID, err := session.SendStateEvent(ctx, roomID, schema.EventTypePipeline, pipelineRef.Pipeline, content)
	if err != nil {
		return nil, fmt.Errorf("publishing pipeline %q to room %q (%s): %w", pipelineRef.Pipeline, roomAlias, roomID, err)
	}

	return &PushResult{
		EventID:   eventID,
		RoomID:    roomID,
		RoomAlias: roomAlias,
	}, nil
}
