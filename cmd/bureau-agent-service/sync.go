// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"slices"
	"sort"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the agent
// service needs. The service watches the machine config room for agent
// state events (session, context, context commits, metrics) to maintain
// awareness of changes made by other instances or manual operator edits.
var syncFilter = buildSyncFilter()

func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		agent.EventTypeAgentSession,
		agent.EventTypeAgentContext,
		agent.EventTypeAgentContextCommit,
		agent.EventTypeAgentMetrics,
	}

	// Timeline includes the same state event types — state events
	// appear as timeline events during incremental /sync.
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
				"limit": 50,
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

// initialSync performs the first /sync and reads current agent state
// from the config room. Returns the since token for incremental sync.
func (agentService *AgentService) initialSync(ctx context.Context) (string, error) {
	sinceToken, response, err := service.InitialSync(ctx, agentService.session, syncFilter)
	if err != nil {
		return "", err
	}

	// Accept pending invites from before service startup. In the
	// sandbox path, principal.Create and daemon reconciliation handle
	// all room invitations, and the proxy's acceptPendingInvites joins
	// them before the binary runs. This handles any invites that
	// arrived between proxy startup and initial sync.
	if len(response.Rooms.Invite) > 0 {
		service.AcceptInvites(ctx, agentService.session, response.Rooms.Invite, agentService.logger)
	}

	agentService.mutex.Lock()
	defer agentService.mutex.Unlock()

	// Process state from the config room if it appeared in the
	// initial sync response.
	if joined, ok := response.Rooms.Join[agentService.configRoomID]; ok {
		agentService.processStateEvents(joined.State.Events)
	}

	agentService.logger.Info("initial sync complete",
		"since_token", sinceToken,
		"config_room", agentService.configRoomID,
	)

	return sinceToken, nil
}

// handleSync processes incremental /sync responses.
func (agentService *AgentService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept pending invites from the incremental sync. The daemon may
	// invite the service to additional rooms during operation (e.g.,
	// config rooms for new room service bindings).
	if len(response.Rooms.Invite) > 0 {
		service.AcceptInvites(ctx, agentService.session, response.Rooms.Invite, agentService.logger)
	}

	agentService.mutex.Lock()
	defer agentService.mutex.Unlock()

	// Process events from the config room.
	if joined, ok := response.Rooms.Join[agentService.configRoomID]; ok {
		agentService.processStateEvents(joined.State.Events)
		agentService.processStateEvents(joined.Timeline.Events)
	}
}

// processStateEvents updates the in-memory state from a batch of
// Matrix events. Events that are not agent state events or that belong
// to rooms other than the config room are ignored.
func (agentService *AgentService) processStateEvents(events []messaging.Event) {
	for _, event := range events {
		switch event.Type {
		case agent.EventTypeAgentSession:
			agentService.logger.Debug("observed agent session event",
				"state_key", event.StateKey,
				"sender", event.Sender,
			)
		case agent.EventTypeAgentContext:
			agentService.logger.Debug("observed agent context event",
				"state_key", event.StateKey,
				"sender", event.Sender,
			)
		case agent.EventTypeAgentContextCommit:
			if event.StateKey == nil {
				continue
			}
			commitID := *event.StateKey

			// Re-marshal map[string]any content to JSON so we can
			// unmarshal into the typed struct.
			contentJSON, err := json.Marshal(event.Content)
			if err != nil {
				agentService.logger.Warn("failed to marshal context commit event content",
					"state_key", commitID,
					"error", err,
				)
				continue
			}
			var content agent.ContextCommitContent
			if err := json.Unmarshal(contentJSON, &content); err != nil {
				agentService.logger.Warn("failed to unmarshal context commit event",
					"state_key", commitID,
					"error", err,
				)
				continue
			}

			agentService.indexCommit(commitID, content)
			agentService.logger.Debug("indexed context commit event",
				"state_key", commitID,
				"principal", content.Principal.Localpart(),
				"created_at", content.CreatedAt,
			)
		case agent.EventTypeAgentMetrics:
			agentService.logger.Debug("observed agent metrics event",
				"state_key", event.StateKey,
				"sender", event.Sender,
			)
		}
	}
}

// indexCommit adds or updates a context commit in the in-memory index.
// If the commit is new (not previously indexed), it is also inserted
// into the appropriate principal's checkpoint timeline. If the commit
// already exists, only the content is updated (Summary may change via
// update-context-metadata) — the timeline is not modified since
// CreatedAt and Principal are immutable.
//
// Must be called under the write lock.
func (agentService *AgentService) indexCommit(commitID string, content agent.ContextCommitContent) {
	if _, exists := agentService.commitIndex[commitID]; exists {
		agentService.commitIndex[commitID] = content
		return
	}

	agentService.commitIndex[commitID] = content

	if content.Principal.IsZero() {
		return
	}
	principalLocal := content.Principal.Localpart()

	entry := timelineEntry{
		CreatedAt: content.CreatedAt,
		CommitID:  commitID,
	}

	// Insert in sorted order by CreatedAt. ISO 8601 timestamps with
	// consistent formatting sort lexicographically for UTC.
	timeline := agentService.principalTimelines[principalLocal]
	insertPosition := sort.Search(len(timeline), func(i int) bool {
		return timeline[i].CreatedAt > entry.CreatedAt
	})
	timeline = slices.Insert(timeline, insertPosition, entry)
	agentService.principalTimelines[principalLocal] = timeline
}
