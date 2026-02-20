// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the artifact
// service cares about:
//   - m.bureau.artifact_scope: per-room artifact configuration (which
//     service principal manages artifacts for the room, tag patterns)
//   - m.bureau.room_service: room service bindings (used by the daemon
//     for service discovery, tracked here for awareness)
const syncFilter = `{
	"room": {
		"state": {
			"types": [
				"m.bureau.artifact_scope",
				"m.bureau.room_service"
			]
		},
		"timeline": {
			"types": [
				"m.bureau.artifact_scope",
				"m.bureau.room_service"
			],
			"limit": 100
		},
		"ephemeral": {
			"types": []
		},
		"account_data": {
			"types": []
		}
	},
	"presence": {
		"types": []
	},
	"account_data": {
		"types": []
	}
}`

// artifactRoomState holds per-room artifact scope configuration.
// Only rooms with a valid m.bureau.artifact_scope event are tracked.
type artifactRoomState struct {
	scope *schema.ArtifactScope
}

// initialSync performs the first /sync and discovers rooms with
// artifact scope configured. Returns the since token for incremental
// sync.
func (as *ArtifactService) initialSync(ctx context.Context) (string, error) {
	sinceToken, response, err := service.InitialSync(ctx, as.session, syncFilter)
	if err != nil {
		return "", err
	}

	as.logger.Info("initial sync complete",
		"next_batch", sinceToken,
		"joined_rooms", len(response.Rooms.Join),
		"pending_invites", len(response.Rooms.Invite),
	)

	// Accept pending invites.
	acceptedRooms := service.AcceptInvites(ctx, as.session, response.Rooms.Invite, as.logger)

	// Discover rooms with artifact scope.
	for roomID, room := range response.Rooms.Join {
		as.processRoomState(roomID, room.State.Events, room.Timeline.Events)
	}

	// Accepted rooms don't appear in Rooms.Join until the next /sync
	// batch. Fetch their full state directly so they are discovered
	// before the socket opens — without this, callers connecting
	// right after the socket appears can reference rooms the service
	// hasn't tracked yet.
	for _, roomID := range acceptedRooms {
		events, err := as.session.GetRoomState(ctx, roomID)
		if err != nil {
			as.logger.Error("failed to fetch state for accepted room",
				"room_id", roomID,
				"error", err,
			)
			continue
		}
		as.processRoomState(roomID, events, nil)
	}

	as.logger.Info("artifact rooms discovered",
		"rooms", len(as.rooms),
	)

	return sinceToken, nil
}

// processRoomState examines state and timeline events to find
// artifact_scope configuration. Called during initial sync and when
// the service joins a new room.
func (as *ArtifactService) processRoomState(roomID ref.RoomID, stateEvents, timelineEvents []messaging.Event) {
	// Look for artifact_scope in both state and timeline sections.
	// Timeline events with a state_key are state changes.
	var scope *schema.ArtifactScope
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeArtifactScope && event.StateKey != nil {
			scope = as.parseArtifactScope(event)
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.EventTypeArtifactScope && event.StateKey != nil {
			scope = as.parseArtifactScope(event)
		}
	}

	if scope == nil {
		return
	}

	as.rooms[roomID] = &artifactRoomState{scope: scope}
	as.logger.Debug("room has artifact scope",
		"room_id", roomID,
		"service_principal", scope.ServicePrincipal,
	)
}

// handleSync processes an incremental /sync response.
func (as *ArtifactService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept invites to new rooms.
	if len(response.Rooms.Invite) > 0 {
		acceptedRooms := service.AcceptInvites(ctx, as.session, response.Rooms.Invite, as.logger)
		if len(acceptedRooms) > 0 {
			as.logger.Info("accepted room invites", "count", len(acceptedRooms))
		}
	}

	// Process state changes in joined rooms.
	for roomID, room := range response.Rooms.Join {
		as.processRoomSync(roomID, room)
	}
}

// processRoomSync handles state changes in a single room during
// incremental sync.
func (as *ArtifactService) processRoomSync(roomID ref.RoomID, room messaging.JoinedRoom) {
	// Collect state events from both state and timeline sections.
	var stateEvents []messaging.Event
	stateEvents = append(stateEvents, room.State.Events...)
	for _, event := range room.Timeline.Events {
		if event.StateKey != nil {
			stateEvents = append(stateEvents, event)
		}
	}

	if len(stateEvents) == 0 {
		return
	}

	for _, event := range stateEvents {
		if event.Type == schema.EventTypeArtifactScope {
			as.handleArtifactScopeChange(roomID, event)
		}
	}
}

// handleArtifactScopeChange processes a change to a room's artifact
// scope configuration.
func (as *ArtifactService) handleArtifactScopeChange(roomID ref.RoomID, event messaging.Event) {
	scope := as.parseArtifactScope(event)
	if scope == nil {
		// Scope was removed or cleared — artifact integration disabled.
		if _, exists := as.rooms[roomID]; exists {
			as.logger.Info("artifact scope removed for room",
				"room_id", roomID,
			)
			delete(as.rooms, roomID)
		}
		return
	}

	if _, exists := as.rooms[roomID]; !exists {
		as.logger.Info("artifact scope added for room",
			"room_id", roomID,
			"service_principal", scope.ServicePrincipal,
		)
	}
	as.rooms[roomID] = &artifactRoomState{scope: scope}
}

// parseArtifactScope parses an artifact_scope event's content.
// Returns nil if the content is empty or unparseable.
func (as *ArtifactService) parseArtifactScope(event messaging.Event) *schema.ArtifactScope {
	if len(event.Content) == 0 {
		return nil
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		as.logger.Warn("failed to marshal artifact_scope content",
			"error", err,
		)
		return nil
	}

	var scope schema.ArtifactScope
	if err := json.Unmarshal(contentJSON, &scope); err != nil {
		as.logger.Warn("failed to parse artifact_scope",
			"error", err,
		)
		return nil
	}

	if err := scope.Validate(); err != nil {
		as.logger.Warn("invalid artifact_scope",
			"error", err,
		)
		return nil
	}

	return &scope
}
