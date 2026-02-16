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
// service cares about. Built from typed constants so that event type
// renames are caught at compile time.
//
// The timeline section includes the same types as the state section
// because state events can appear as timeline events during incremental
// sync. The limit is generous since the service needs to see all ticket
// mutations, not just the latest few.
//
// The filter includes event types needed for gate evaluation:
// pipeline result events for pipeline gates, and ticket events which
// serve double duty (indexing + ticket gate evaluation).
var syncFilter = buildSyncFilter()

// buildSyncFilter constructs the Matrix /sync filter JSON from typed
// schema constants.
func buildSyncFilter() string {
	stateEventTypes := []string{
		schema.EventTypeTicket,
		schema.EventTypeTicketConfig,
		schema.EventTypeRoomService,
		schema.EventTypePipelineResult,
		schema.MatrixEventTypeTombstone,
	}

	// Timeline includes the same state event types (state events can
	// appear as timeline events with a non-nil state_key during
	// incremental sync).
	timelineEventTypes := make([]string, len(stateEventTypes))
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

	// alias is the room's canonical alias (e.g.,
	// "#iree/general:bureau.local"), resolved once when the room is
	// first tracked. Empty if the room has no alias or the fetch
	// failed. Used for operator-friendly logging only.
	alias string
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
	acceptedRooms := service.AcceptInvites(ctx, ts.session, response.Rooms.Invite, ts.logger)

	// Build the ticket index from all joined rooms' state.
	totalTickets := 0
	for roomID, room := range response.Rooms.Join {
		tickets := ts.processRoomState(ctx, roomID, room.State.Events, room.Timeline.Events)
		totalTickets += tickets
	}

	// Accepted rooms don't appear in Rooms.Join until the next /sync
	// batch. Fetch their full state directly so they are indexed
	// before the socket opens — without this, callers connecting
	// right after the socket appears can reference rooms the service
	// hasn't tracked yet.
	for _, roomID := range acceptedRooms {
		events, err := ts.session.GetRoomState(ctx, roomID)
		if err != nil {
			ts.logger.Error("failed to fetch state for accepted room",
				"room_id", roomID,
				"error", err,
			)
			continue
		}
		tickets := ts.processRoomState(ctx, roomID, events, nil)
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
func (ts *TicketService) processRoomState(ctx context.Context, roomID string, stateEvents, timelineEvents []messaging.Event) int {
	// First pass: check for tombstone and look for ticket_config to
	// determine if this room has ticket management enabled. Check
	// both state and timeline events (timeline events with a
	// state_key are state changes).
	var config *schema.TicketConfigContent
	var tombstoned bool
	for _, event := range stateEvents {
		if event.Type == schema.MatrixEventTypeTombstone {
			tombstoned = true
		}
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.MatrixEventTypeTombstone {
			tombstoned = true
		}
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
	}

	// Skip tombstoned rooms — they are being replaced and should
	// not be tracked. Also skip rooms without ticket management.
	if tombstoned || config == nil {
		return 0
	}

	// This room has ticket management. Create or update room state.
	state, exists := ts.rooms[roomID]
	if !exists {
		state = &roomState{
			config: config,
			index:  ticket.NewIndex(),
			alias:  ts.resolveRoomAlias(ctx, roomID),
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
			"room_alias", state.alias,
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
		acceptedRooms := service.AcceptInvites(ctx, ts.session, response.Rooms.Invite, ts.logger)
		if len(acceptedRooms) > 0 {
			ts.logger.Info("accepted room invites", "count", len(acceptedRooms))
		}
	}

	// Clean up state for rooms the service has been removed from.
	// This handles kick, ban, and explicit leave. The room appears
	// in the Leave section when the membership transitions away from
	// "join". Process leaves before joins so that a leave+rejoin in
	// the same sync batch re-indexes cleanly from the join state.
	for roomID := range response.Rooms.Leave {
		state, exists := ts.rooms[roomID]
		if !exists {
			continue
		}
		ts.logger.Info("room left, removing ticket state",
			"room_id", roomID,
			"room_alias", state.alias,
			"tickets_removed", state.index.Len(),
		)
		delete(ts.rooms, roomID)
	}

	// Process state changes in joined rooms.
	for roomID, room := range response.Rooms.Join {
		ts.processRoomSync(ctx, roomID, room)
	}

	// Evaluate cross-room gates. Events from rooms that aren't
	// ticket-configured may still satisfy state_event gates in
	// ticket rooms that specify a RoomAlias. This runs after
	// per-room processing so that ticket state changes (indexing,
	// same-room gate evaluation) are visible first.
	ts.evaluateCrossRoomGates(ctx, response.Rooms.Join)
}

// processRoomSync handles state changes in a single room during
// incremental sync.
func (ts *TicketService) processRoomSync(ctx context.Context, roomID string, room messaging.JoinedRoom) {
	// Collect all state events from both the state and timeline sections.
	// State events in the timeline section have a non-nil StateKey.
	stateEvents := collectStateEvents(room)

	if len(stateEvents) == 0 {
		return
	}

	// Check for tombstone first. A tombstoned room is being replaced
	// and should no longer be tracked. No point processing ticket
	// config or ticket events for a decommissioned room.
	for _, event := range stateEvents {
		if event.Type == schema.MatrixEventTypeTombstone {
			ts.handleRoomTombstone(roomID, event)
			return
		}
	}

	// Check for ticket_config changes. A room might be gaining or
	// losing ticket management.
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicketConfig {
			ts.handleTicketConfigChange(ctx, roomID, event)
		}
	}

	// Process ticket events if this room has ticket management.
	state, exists := ts.rooms[roomID]
	if !exists {
		return
	}

	// Phase 1: Index ticket events. This must complete before gate
	// evaluation so that ticket gates can see updated statuses
	// (e.g., a ticket reaching "closed" satisfies ticket gates).
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			ts.indexTicketEvent(state, event)
		}
	}

	// Phase 2: Evaluate gates against ALL state events in the batch.
	// Pipeline result events, ticket events, and any other state
	// events may satisfy pending gates. Gate evaluation is idempotent
	// (already-satisfied gates are skipped).
	ts.evaluateGatesForEvents(ctx, roomID, state, stateEvents)
}

// handleRoomTombstone processes a tombstone event for a room. The room
// is being replaced (decommissioned). If the room was tracked for
// tickets, clean up its state. The tombstone event's content contains
// a "replacement_room" field pointing to the successor room; this is
// logged for operator visibility but the service does not auto-follow
// the replacement (the replacement room needs its own ticket_config
// and service invitation).
func (ts *TicketService) handleRoomTombstone(roomID string, event messaging.Event) {
	replacementRoom := ""
	if body, ok := event.Content["replacement_room"]; ok {
		if replacement, ok := body.(string); ok {
			replacementRoom = replacement
		}
	}

	state, exists := ts.rooms[roomID]
	if !exists {
		ts.logger.Info("untracked room tombstoned",
			"room_id", roomID,
			"replacement_room", replacementRoom,
		)
		return
	}

	ts.logger.Warn("tracked room tombstoned, removing ticket state",
		"room_id", roomID,
		"room_alias", state.alias,
		"tickets_removed", state.index.Len(),
		"replacement_room", replacementRoom,
	)
	delete(ts.rooms, roomID)
}

// handleTicketConfigChange processes a change to a room's ticket
// configuration.
func (ts *TicketService) handleTicketConfigChange(ctx context.Context, roomID string, event messaging.Event) {
	config := ts.parseTicketConfig(event)
	if config == nil {
		// Config was removed or cleared — ticket management disabled.
		if state, exists := ts.rooms[roomID]; exists {
			ts.logger.Info("ticket management disabled for room",
				"room_id", roomID,
				"room_alias", state.alias,
				"tickets_removed", state.index.Len(),
			)
			delete(ts.rooms, roomID)
		}
		return
	}

	state, exists := ts.rooms[roomID]
	if !exists {
		state = &roomState{
			config: config,
			index:  ticket.NewIndex(),
			alias:  ts.resolveRoomAlias(ctx, roomID),
		}
		ts.rooms[roomID] = state

		// Backfill: the room may already contain ticket events from
		// before this service started tracking it. Fetch the full
		// room state and index any existing tickets.
		backfilled := ts.backfillRoomTickets(ctx, roomID, state)

		ts.logger.Info("ticket management enabled for room",
			"room_id", roomID,
			"room_alias", state.alias,
			"prefix", config.Prefix,
			"backfilled_tickets", backfilled,
		)
	} else {
		state.config = config
	}
}

// backfillRoomTickets fetches the full state of a room and indexes
// any existing ticket events. Called when a room gains ticket_config
// mid-operation — the initial sync path processes state events from
// the sync response directly and doesn't need this. Returns the
// number of tickets indexed.
func (ts *TicketService) backfillRoomTickets(ctx context.Context, roomID string, state *roomState) int {
	events, err := ts.session.GetRoomState(ctx, roomID)
	if err != nil {
		ts.logger.Error("failed to fetch room state for ticket backfill",
			"room_id", roomID,
			"error", err,
		)
		return 0
	}

	count := 0
	for _, event := range events {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			if ts.indexTicketEvent(state, event) {
				count++
			}
		}
	}
	return count
}

// resolveRoomAlias fetches the canonical alias for a room. Returns
// empty string if the room has no alias or the fetch fails. Called
// once per room when it is first tracked. Used for logging only.
func (ts *TicketService) resolveRoomAlias(ctx context.Context, roomID string) string {
	if ts.session == nil {
		return ""
	}
	raw, err := ts.session.GetStateEvent(ctx, roomID, schema.MatrixEventTypeCanonicalAlias, "")
	if err != nil {
		return ""
	}
	var content struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return ""
	}
	return content.Alias
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
	RoomID     string         `cbor:"room_id"`
	Tickets    int            `cbor:"tickets"`
	ByStatus   map[string]int `cbor:"by_status"`
	ByPriority map[int]int    `cbor:"by_priority"`
}
