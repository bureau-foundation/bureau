// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the agent
// service needs. The service watches the machine config room for agent
// state events (session, context, metrics) to maintain awareness of
// changes made by other instances or manual operator edits.
var syncFilter = buildSyncFilter()

func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		agent.EventTypeAgentSession,
		agent.EventTypeAgentContext,
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

	// Accept pending invites from before service startup.
	if len(response.Rooms.Invite) > 0 {
		acceptedRooms := service.AcceptInvites(ctx, agentService.session, response.Rooms.Invite, agentService.logger)
		for _, roomID := range acceptedRooms {
			if roomID == agentService.configRoomID {
				agentService.mutex.Lock()
				agentService.configRoomJoined = true
				agentService.mutex.Unlock()
				agentService.logger.Info("joined config room via invite during initial sync", "config_room", roomID)
			}
		}
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
		"config_room_joined", agentService.configRoomJoined,
	)

	return sinceToken, nil
}

// handleSync processes incremental /sync responses.
func (agentService *AgentService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept pending invites. The daemon invites the service to config
	// rooms when resolving room service bindings during principal
	// deployment. The service may also be invited to additional rooms
	// by operators.
	if len(response.Rooms.Invite) > 0 {
		acceptedRooms := service.AcceptInvites(ctx, agentService.session, response.Rooms.Invite, agentService.logger)
		for _, roomID := range acceptedRooms {
			if roomID == agentService.configRoomID {
				agentService.mutex.Lock()
				agentService.configRoomJoined = true
				agentService.mutex.Unlock()
				agentService.logger.Info("joined config room via invite", "config_room", roomID)
			}
		}
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
		case agent.EventTypeAgentMetrics:
			agentService.logger.Debug("observed agent metrics event",
				"state_key", event.StateKey,
				"sender", event.Sender,
			)
		}
	}
}
