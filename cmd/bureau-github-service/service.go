// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/forgesub"
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
	manager       *forgesub.Manager
	ticketSyncer  *TicketSyncer // nil if ticket service not configured
	clock         clock.Clock
	logger        *slog.Logger
}

// handleEvent processes a translated forge event from the webhook
// handler. Evaluates auto-subscribe rules, routes to the subscription
// manager for delivery to connected agents, then syncs to the ticket
// service if configured.
func (gs *GitHubService) handleEvent(event *forge.Event) {
	gs.logger.Info("forge event received",
		"type", event.Type,
		"summary", eventSummary(event),
	)

	// Evaluate auto-subscribe before dispatch so that any newly
	// created pending subscriptions are recorded before the event
	// reaches existing subscribers.
	involved := extractInvolvedUsers(event)
	if len(involved) > 0 {
		gs.manager.ProcessAutoSubscribe(event, involved)
	}

	gs.manager.Dispatch(event)

	// Sync issue events to the ticket service if configured.
	if gs.ticketSyncer != nil {
		rooms := gs.manager.RoomsForEvent(event)
		if len(rooms) > 0 {
			gs.ticketSyncer.SyncEvent(context.Background(), event, rooms)
		}
	}
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

// processRoomEvents handles state events from a single room. Parses
// repository bindings and forge config into the subscription manager,
// and logs identity and auto-subscribe events for future handling.
func (gs *GitHubService) processRoomEvents(roomID ref.RoomID, events []messaging.Event) {
	for _, event := range events {
		switch event.Type {
		case forge.EventTypeRepository:
			gs.processRepositoryBinding(roomID, event)
		case forge.EventTypeForgeConfig:
			gs.processForgeConfig(roomID, event)
		case forge.EventTypeForgeIdentity:
			gs.processForgeIdentity(event)
		case forge.EventTypeForgeAutoSubscribe:
			gs.processAutoSubscribeRules(event)
		case forge.EventTypeForgeWorkIdentity:
			gs.logger.Debug("work identity updated",
				"room_id", roomID,
				"state_key", event.StateKey,
			)
		}
	}
}

// processRepositoryBinding parses an m.bureau.repository state event
// and updates the subscription manager's binding index.
func (gs *GitHubService) processRepositoryBinding(roomID ref.RoomID, event messaging.Event) {
	// Empty content means the binding was redacted/tombstoned.
	if len(event.Content) == 0 {
		stateKey := ""
		if event.StateKey != nil {
			stateKey = *event.StateKey
		}
		// State key format is "provider/owner/repo". Extract
		// provider and combined "owner/repo" string.
		provider, repo := parseBindingStateKey(stateKey)
		if provider != "" && repo != "" {
			gs.manager.RemoveRoomBinding(roomID, provider, repo)
		}
		return
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		gs.logger.Warn("failed to marshal repository binding content",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	var binding forge.RepositoryBinding
	if err := json.Unmarshal(contentJSON, &binding); err != nil {
		gs.logger.Warn("failed to parse repository binding",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	if err := binding.Validate(); err != nil {
		gs.logger.Warn("invalid repository binding",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	gs.manager.UpdateRoomBinding(roomID, binding)
}

// processForgeConfig parses an m.bureau.forge_config state event and
// updates the subscription manager's filter config.
func (gs *GitHubService) processForgeConfig(roomID ref.RoomID, event messaging.Event) {
	if len(event.Content) == 0 {
		return
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		gs.logger.Warn("failed to marshal forge config content",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	var config forge.ForgeConfig
	if err := json.Unmarshal(contentJSON, &config); err != nil {
		gs.logger.Warn("failed to parse forge config",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	if err := config.Validate(); err != nil {
		gs.logger.Warn("invalid forge config",
			"room_id", roomID,
			"error", err,
		)
		return
	}

	gs.manager.UpdateForgeConfig(roomID, config)
}

// parseBindingStateKey splits a binding state key
// ("provider/owner/repo") into provider and "owner/repo". Returns
// empty strings if the key is malformed.
func parseBindingStateKey(stateKey string) (string, string) {
	// Format: "provider/owner/repo" — split on first "/" only.
	slashIndex := 0
	for i, char := range stateKey {
		if char == '/' {
			slashIndex = i
			break
		}
	}
	if slashIndex == 0 || slashIndex >= len(stateKey)-1 {
		return "", ""
	}
	return stateKey[:slashIndex], stateKey[slashIndex+1:]
}

// processForgeIdentity parses an m.bureau.forge_identity state event
// and updates the Manager's identity reverse lookup index.
func (gs *GitHubService) processForgeIdentity(event messaging.Event) {
	if len(event.Content) == 0 {
		// Identity redacted — remove from index.
		stateKey := ""
		if event.StateKey != nil {
			stateKey = *event.StateKey
		}
		provider, forgeUser := parseBindingStateKey(stateKey)
		if provider != "" && forgeUser != "" {
			gs.manager.RemoveIdentity(provider, forgeUser)
		}
		return
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		gs.logger.Warn("failed to marshal forge identity content",
			"error", err,
		)
		return
	}

	var identity forge.ForgeIdentity
	if err := json.Unmarshal(contentJSON, &identity); err != nil {
		gs.logger.Warn("failed to parse forge identity",
			"error", err,
		)
		return
	}

	if err := identity.Validate(); err != nil {
		gs.logger.Warn("invalid forge identity",
			"error", err,
		)
		return
	}

	gs.manager.UpdateIdentity(identity)
}

// processAutoSubscribeRules parses an m.bureau.forge_auto_subscribe
// state event and updates the Manager's per-agent rules index.
func (gs *GitHubService) processAutoSubscribeRules(event messaging.Event) {
	stateKey := ""
	if event.StateKey != nil {
		stateKey = *event.StateKey
	}

	if len(event.Content) == 0 || stateKey == "" {
		return
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		gs.logger.Warn("failed to marshal auto-subscribe rules content",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	var rules forge.ForgeAutoSubscribeRules
	if err := json.Unmarshal(contentJSON, &rules); err != nil {
		gs.logger.Warn("failed to parse auto-subscribe rules",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	if err := rules.Validate(); err != nil {
		gs.logger.Warn("invalid auto-subscribe rules",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	gs.manager.UpdateAutoSubscribeRules(stateKey, rules)
}

// extractInvolvedUsers extracts forge usernames and their roles from
// a translated forge event. Provider-specific: reads fields populated
// by the GitHub webhook translator. Also scans markdown body fields
// for @mentions.
func extractInvolvedUsers(event *forge.Event) []forgesub.InvolvedUser {
	var users []forgesub.InvolvedUser

	// Track usernames already seen in structured roles so we don't
	// duplicate them as mentions.
	seen := make(map[string]struct{})
	add := func(username string, role forgesub.InvolvementRole) {
		lower := strings.ToLower(username)
		seen[lower] = struct{}{}
		users = append(users, forgesub.InvolvedUser{
			ForgeUsername: username,
			Role:          role,
		})
	}

	// Collect body text for mention scanning.
	var bodyTexts []string

	switch event.Type {
	case forge.EventCategoryPush:
		if event.Push != nil && event.Push.Sender != "" {
			add(event.Push.Sender, forgesub.RoleAuthor)
		}

	case forge.EventCategoryPullRequest:
		if event.PullRequest != nil {
			if event.PullRequest.Author != "" {
				add(event.PullRequest.Author, forgesub.RoleAuthor)
			}
			if event.PullRequest.RequestedReviewer != "" {
				add(event.PullRequest.RequestedReviewer, forgesub.RoleReviewRequested)
			}
			if event.PullRequest.Title != "" {
				bodyTexts = append(bodyTexts, event.PullRequest.Title)
			}
		}

	case forge.EventCategoryIssues:
		if event.Issue != nil {
			if event.Issue.Author != "" {
				add(event.Issue.Author, forgesub.RoleAuthor)
			}
			if event.Issue.Assignee != "" {
				add(event.Issue.Assignee, forgesub.RoleAssignee)
			}
			if event.Issue.Body != "" {
				bodyTexts = append(bodyTexts, event.Issue.Body)
			}
		}

	case forge.EventCategoryReview:
		if event.Review != nil {
			if event.Review.Reviewer != "" {
				add(event.Review.Reviewer, forgesub.RoleAuthor)
			}
			if event.Review.Body != "" {
				bodyTexts = append(bodyTexts, event.Review.Body)
			}
		}

	case forge.EventCategoryComment:
		if event.Comment != nil {
			if event.Comment.Author != "" {
				add(event.Comment.Author, forgesub.RoleAuthor)
			}
			if event.Comment.Body != "" {
				bodyTexts = append(bodyTexts, event.Comment.Body)
			}
		}
	}

	// Scan body text for @mentions and add any that weren't already
	// captured in structured roles.
	for _, body := range bodyTexts {
		mentioned := forgesub.ExtractMentions(body)
		for _, username := range mentioned {
			if _, already := seen[username]; already {
				continue
			}
			seen[username] = struct{}{}
			users = append(users, forgesub.InvolvedUser{
				ForgeUsername: username,
				Role:          forgesub.RoleMentioned,
			})
		}
	}

	return users
}

// registerActions registers CBOR socket API actions on the service's
// Unix socket server.
func (gs *GitHubService) registerActions(server *service.SocketServer) {
	server.Handle("status", gs.handleStatus)
	server.HandleAuthStream(
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionSubscribe),
		gs.handleSubscribe,
	)
	server.HandleAuth(
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionAutoSubscribeConfig),
		gs.handleAutoSubscribeConfig,
	)
}

// handleStatus returns basic service health information.
func (gs *GitHubService) handleStatus(_ context.Context, _ []byte) (any, error) {
	return map[string]any{
		"service": "github",
		"status":  "running",
	}, nil
}
