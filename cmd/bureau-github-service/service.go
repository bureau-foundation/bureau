// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the GitHub
// service cares about. Built from typed constants so that event type
// renames are caught at compile time.
var syncFilter = buildSyncFilter()

func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		forge.EventTypeRepository,
		forge.EventTypeForgeConfig,
		forge.EventTypeForgeIdentity,
		forge.EventTypeForgeAutoSubscribe,
		forge.EventTypeForgeWorkIdentity,
		schema.MatrixEventTypeRoomMember,
	}

	// Timeline includes the same state event types — state events can
	// appear as timeline events during incremental sync.
	timelineEventTypes := make([]ref.EventType, len(stateEventTypes))
	copy(timelineEventTypes, stateEventTypes)

	emptyTypes := []string{}

	filter := map[string]any{
		"room": map[string]any{
			"state": map[string]any{
				"types": stateEventTypes,
			},
			"timeline": map[string]any{
				"types": timelineEventTypes,
				"limit": 100,
			},
			"ephemeral": map[string]any{
				"types": emptyTypes,
			},
			"account_data": map[string]any{
				"types": emptyTypes,
			},
		},
		"presence": map[string]any{
			"types": emptyTypes,
		},
		"account_data": map[string]any{
			"types": emptyTypes,
		},
	}

	data, err := json.Marshal(filter)
	if err != nil {
		panic("building sync filter: " + err.Error())
	}
	return string(data)
}

// GitHubService is the core service state. Coordinates webhook event
// dispatch, subscriptions, and entity mapping between GitHub and
// Bureau's forge schema.
type GitHubService struct {
	session       messaging.Session
	service       ref.Service
	serviceRoomID ref.RoomID
	logger        *slog.Logger
}

// handleEvent processes a translated forge event from the webhook
// handler. Routes to the subscription manager and entity mapping
// handlers.
func (gs *GitHubService) handleEvent(event *forge.Event) {
	gs.logger.Info("forge event received",
		"type", event.Type,
		"summary", eventSummary(event),
	)
}

// eventSummary extracts a human-readable summary from a forge event.
func eventSummary(event *forge.Event) string {
	switch event.Type {
	case forge.EventCategoryPush:
		if event.Push != nil {
			return event.Push.Summary
		}
	case forge.EventCategoryPullRequest:
		if event.PullRequest != nil {
			return event.PullRequest.Summary
		}
	case forge.EventCategoryIssues:
		if event.Issue != nil {
			return event.Issue.Summary
		}
	case forge.EventCategoryReview:
		if event.Review != nil {
			return event.Review.Summary
		}
	case forge.EventCategoryComment:
		if event.Comment != nil {
			return event.Comment.Summary
		}
	case forge.EventCategoryCIStatus:
		if event.CIStatus != nil {
			return event.CIStatus.Summary
		}
	}
	return ""
}

// handleSync processes incremental /sync responses. Updates
// repository bindings, forge configuration, and identity mappings
// from room state events.
func (gs *GitHubService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept any pending room invites.
	if len(response.Rooms.Invite) > 0 {
		service.AcceptInvites(ctx, gs.session, response.Rooms.Invite, gs.logger)
	}

	// Process joined room state and timeline events.
	for roomID, room := range response.Rooms.Join {
		gs.processRoomEvents(roomID, room.State.Events)
		gs.processRoomEvents(roomID, room.Timeline.Events)
	}
}

// processRoomEvents handles state events from a single room.
func (gs *GitHubService) processRoomEvents(roomID ref.RoomID, events []messaging.Event) {
	for _, event := range events {
		switch event.Type {
		case forge.EventTypeRepository:
			gs.logger.Debug("repository binding updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		case forge.EventTypeForgeConfig:
			gs.logger.Debug("forge config updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		case forge.EventTypeForgeIdentity:
			gs.logger.Debug("forge identity updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		case forge.EventTypeForgeAutoSubscribe:
			gs.logger.Debug("auto-subscribe rules updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		case forge.EventTypeForgeWorkIdentity:
			gs.logger.Debug("work identity updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		}
	}
}

// registerActions registers CBOR socket API actions. The service
// skeleton starts with a status health check; subscription and
// entity management actions are added as the implementation
// progresses.
func (gs *GitHubService) registerActions(server *service.SocketServer) {
	server.Handle("status", gs.handleStatus)
}

// handleStatus returns basic service health information.
func (gs *GitHubService) handleStatus(_ context.Context, _ []byte) (any, error) {
	return map[string]any{
		"service": "github",
		"status":  "running",
	}, nil
}
