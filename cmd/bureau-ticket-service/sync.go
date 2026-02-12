// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the ticket
// service cares about. This includes:
//   - m.bureau.ticket: ticket state events (core data)
//   - m.bureau.ticket_config: room ticket configuration (enables/disables
//     ticket management for a room)
//   - m.bureau.room_service: room service bindings (which service handles
//     which role in a room)
//
// The timeline section includes the same types as state events can
// appear as timeline events during incremental sync. The limit is
// generous since the service needs to see all ticket mutations, not
// just the latest few.
//
// Gate evaluation will add more event types to this filter as gate
// types are implemented (e.g., m.bureau.pipeline_result for pipeline
// gates). For now, only ticket-related types are included.
const syncFilter = `{
	"room": {
		"state": {
			"types": [
				"m.bureau.ticket",
				"m.bureau.ticket_config",
				"m.bureau.room_service"
			]
		},
		"timeline": {
			"types": [
				"m.bureau.ticket",
				"m.bureau.ticket_config",
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

// roomState holds the per-room ticket index and configuration for
// rooms that have ticket management enabled.
type roomState struct {
	// config is the room's ticket configuration from
	// m.bureau.ticket_config. Nil means the room has no ticket
	// management and shouldn't be tracked (but this struct only
	// exists for rooms that do have ticket management, so it should
	// always be non-nil).
	config *schema.TicketConfigContent

	// index is the in-memory ticket index for this room.
	index *ticket.Index
}

// initialSync performs the first /sync and builds the ticket index
// from current room state. Returns the since token for incremental
// sync.
func (ts *TicketService) initialSync(ctx context.Context) (string, error) {
	sinceToken, response, err := service.InitialSync(ctx, ts.session, syncFilter)
	if err != nil {
		return "", err
	}

	ts.logger.Info("initial sync complete",
		"next_batch", sinceToken,
		"joined_rooms", len(response.Rooms.Join),
		"pending_invites", len(response.Rooms.Invite),
	)

	// Accept pending invites. The service may have been invited to
	// rooms while it was offline.
	service.AcceptInvites(ctx, ts.session, response.Rooms.Invite, ts.logger)

	// Build the ticket index from all joined rooms' state.
	totalTickets := 0
	for roomID, room := range response.Rooms.Join {
		tickets := ts.processRoomState(roomID, room.State.Events, room.Timeline.Events)
		totalTickets += tickets
	}

	ts.logger.Info("ticket index built",
		"ticketed_rooms", len(ts.rooms),
		"total_tickets", totalTickets,
	)

	return sinceToken, nil
}

// processRoomState examines state and timeline events from a room to
// determine if it has ticket management enabled and, if so, indexes
// all ticket events. Returns the number of tickets indexed.
//
// Called during initial sync for each joined room and can be called
// when the service joins a new room during incremental sync.
func (ts *TicketService) processRoomState(roomID string, stateEvents, timelineEvents []messaging.Event) int {
	// First pass: look for ticket_config to determine if this room
	// has ticket management enabled. Check both state and timeline
	// events (timeline events with a state_key are state changes).
	var config *schema.TicketConfigContent
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
	}

	if config == nil {
		return 0
	}

	// This room has ticket management. Create or update room state.
	state, exists := ts.rooms[roomID]
	if !exists {
		state = &roomState{
			config: config,
			index:  ticket.NewIndex(),
		}
		ts.rooms[roomID] = state
	} else {
		state.config = config
	}

	// Second pass: index all ticket events.
	ticketCount := 0
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			if ts.indexTicketEvent(state, event) {
				ticketCount++
			}
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			if ts.indexTicketEvent(state, event) {
				ticketCount++
			}
		}
	}

	if ticketCount > 0 {
		ts.logger.Info("room tickets indexed",
			"room_id", roomID,
			"tickets", ticketCount,
			"total_in_room", state.index.Len(),
		)
	}

	return ticketCount
}

// handleSync processes an incremental /sync response. Called by the
// sync loop for each response.
func (ts *TicketService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept invites to new rooms.
	if len(response.Rooms.Invite) > 0 {
		accepted := service.AcceptInvites(ctx, ts.session, response.Rooms.Invite, ts.logger)
		if accepted > 0 {
			ts.logger.Info("accepted room invites", "count", accepted)
		}
	}

	// Process state changes in joined rooms.
	for roomID, room := range response.Rooms.Join {
		ts.processRoomSync(ctx, roomID, room)
	}
}

// processRoomSync handles state changes in a single room during
// incremental sync.
func (ts *TicketService) processRoomSync(ctx context.Context, roomID string, room messaging.JoinedRoom) {
	// Collect all state events from both the state and timeline sections.
	// State events in the timeline section have a non-nil StateKey.
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

	// Check for ticket_config changes first. A room might be gaining
	// or losing ticket management.
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicketConfig {
			ts.handleTicketConfigChange(roomID, event)
		}
	}

	// Process ticket events if this room has ticket management.
	state, exists := ts.rooms[roomID]
	if !exists {
		return
	}

	for _, event := range stateEvents {
		switch event.Type {
		case schema.EventTypeTicket:
			if event.StateKey != nil {
				ts.indexTicketEvent(state, event)
			}
		default:
			// Other state events may satisfy pending gates. Gate
			// evaluation will be implemented here: check if any
			// pending gates in this room match the event, and if so,
			// update the affected tickets.
		}
	}
}

// handleTicketConfigChange processes a change to a room's ticket
// configuration.
func (ts *TicketService) handleTicketConfigChange(roomID string, event messaging.Event) {
	config := ts.parseTicketConfig(event)
	if config == nil {
		// Config was removed or cleared — ticket management disabled.
		if _, exists := ts.rooms[roomID]; exists {
			ts.logger.Info("ticket management disabled for room",
				"room_id", roomID,
			)
			delete(ts.rooms, roomID)
		}
		return
	}

	state, exists := ts.rooms[roomID]
	if !exists {
		// New ticketed room. We need a full state fetch to pick up
		// any existing tickets since we don't have them in the sync
		// response.
		state = &roomState{
			config: config,
			index:  ticket.NewIndex(),
		}
		ts.rooms[roomID] = state
		ts.logger.Info("ticket management enabled for room",
			"room_id", roomID,
			"prefix", config.Prefix,
		)
	} else {
		state.config = config
	}
}

// parseTicketConfig parses a ticket_config event's content. Returns
// nil if the content is empty or unparseable (room does not have
// ticket management).
func (ts *TicketService) parseTicketConfig(event messaging.Event) *schema.TicketConfigContent {
	if len(event.Content) == 0 {
		return nil
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		ts.logger.Warn("failed to marshal ticket_config content",
			"error", err,
		)
		return nil
	}

	var config schema.TicketConfigContent
	if err := json.Unmarshal(contentJSON, &config); err != nil {
		ts.logger.Warn("failed to parse ticket_config",
			"error", err,
		)
		return nil
	}

	return &config
}

// indexTicketEvent parses a ticket event and adds it to the room's
// index. Returns true if the ticket was successfully indexed.
func (ts *TicketService) indexTicketEvent(state *roomState, event messaging.Event) bool {
	if event.StateKey == nil {
		return false
	}
	ticketID := *event.StateKey

	// Empty content means the ticket was redacted.
	if len(event.Content) == 0 {
		state.index.Remove(ticketID)
		return false
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		ts.logger.Warn("failed to marshal ticket content",
			"ticket_id", ticketID,
			"error", err,
		)
		return false
	}

	var content schema.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		ts.logger.Warn("failed to parse ticket content",
			"ticket_id", ticketID,
			"error", err,
		)
		return false
	}

	state.index.Put(ticketID, content)
	return true
}

// totalTickets returns the total number of tickets across all rooms.
func (ts *TicketService) totalTickets() int {
	total := 0
	for _, state := range ts.rooms {
		total += state.index.Len()
	}
	return total
}

// findTicket looks up a ticket by ID across all rooms. Returns the
// room state, ticket ID, content, and whether it was found.
func (ts *TicketService) findTicket(ticketID string) (*roomState, schema.TicketContent, bool) {
	for _, state := range ts.rooms {
		if content, exists := state.index.Get(ticketID); exists {
			return state, content, true
		}
	}
	return nil, schema.TicketContent{}, false
}

// roomIndex returns the ticket index for a room. Returns nil if the
// room doesn't have ticket management or isn't tracked by this service.
func (ts *TicketService) roomIndex(roomID string) *ticket.Index {
	state, exists := ts.rooms[roomID]
	if !exists {
		return nil
	}
	return state.index
}

// roomStats returns a summary of all tracked rooms and their ticket
// counts.
func (ts *TicketService) roomStats() []roomSummary {
	summaries := make([]roomSummary, 0, len(ts.rooms))
	for roomID, state := range ts.rooms {
		stats := state.index.Stats()
		summaries = append(summaries, roomSummary{
			RoomID:     roomID,
			Tickets:    stats.Total,
			ByStatus:   stats.ByStatus,
			ByPriority: stats.ByPriority,
		})
	}
	return summaries
}

type roomSummary struct {
	RoomID     string         `json:"room_id"`
	Tickets    int            `json:"tickets"`
	ByStatus   map[string]int `json:"by_status"`
	ByPriority map[int]int    `json:"by_priority"`
}
