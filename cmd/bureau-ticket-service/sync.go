// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
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
	stateEventTypes := []ref.EventType{
		schema.EventTypeTicket,
		schema.EventTypeTicketConfig,
		pipeline.EventTypePipelineResult,
		schema.EventTypeStewardship,
		schema.MatrixEventTypeTombstone,
		schema.MatrixEventTypeRoomMember,
		schema.MatrixEventTypeCanonicalAlias,
	}

	// Timeline includes the same state event types (state events can
	// appear as timeline events with a non-nil state_key during
	// incremental sync).
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
			"types": []string{"m.presence"},
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
	config *ticket.TicketConfigContent

	// index is the in-memory ticket index for this room.
	index *ticketindex.Index

	// pendingEchoes tracks event IDs of ticket writes made by
	// mutation handlers (or gate satisfaction) that have not yet
	// been echoed back via /sync. The sync loop checks this map
	// in indexTicketEvent: events for a ticket with a pending echo
	// are skipped unless they ARE the echo, preventing stale
	// pre-echo events from overwriting optimistic local updates.
	//
	// Keyed by ticket ID (state_key), value is the event ID
	// returned by SendStateEvent. Cleared when the echo arrives.
	pendingEchoes map[string]ref.EventID

	// alias is the room's canonical alias (e.g.,
	// "#iree/general:bureau.local"). Extracted from room state
	// events when the room is first tracked, updated when
	// canonical alias changes arrive via /sync. Used for
	// subscription matching (resolveRoomString) and logging.
	alias string
}

// roomMember stores the display name of a joined room member. Used
// by the membership index (membersByRoom on TicketService) to serve
// list-members queries without synchronous HTTP calls to the
// homeserver.
type roomMember struct {
	DisplayName string
}

// indexMemberEvent processes an m.room.member state event and updates
// the in-memory membership index. Join events add/update the member
// entry; leave and ban events remove it. Other membership states
// (invite, knock) are ignored — only currently-joined members are
// tracked.
//
// Called for ALL rooms (not just ticket-configured rooms) so that
// list-members queries have complete membership data.
func (ts *TicketService) indexMemberEvent(roomID ref.RoomID, event messaging.Event) {
	if event.StateKey == nil {
		return
	}
	userID, err := ref.ParseUserID(*event.StateKey)
	if err != nil {
		// Not a valid user ID (shouldn't happen for real member
		// events, but be defensive).
		return
	}

	membership, _ := event.Content["membership"].(string)
	switch membership {
	case "join":
		displayName, _ := event.Content["displayname"].(string)
		if ts.membersByRoom[roomID] == nil {
			ts.membersByRoom[roomID] = make(map[ref.UserID]roomMember)
		}
		ts.membersByRoom[roomID][userID] = roomMember{
			DisplayName: displayName,
		}
	case "leave", "ban":
		if members := ts.membersByRoom[roomID]; members != nil {
			delete(members, userID)
		}
	}
}

// indexStewardshipEvent processes an m.bureau.stewardship state event
// and updates the in-memory stewardship index. Non-empty content is
// parsed and stored; empty content (redacted/removed declaration)
// triggers removal.
//
// Called for ALL rooms (not just ticket-configured rooms) because
// stewardship declarations in any room can affect tickets in other
// rooms.
func (ts *TicketService) indexStewardshipEvent(ctx context.Context, roomID ref.RoomID, event messaging.Event) {
	if event.StateKey == nil {
		return
	}
	stateKey := *event.StateKey

	// Empty content means the stewardship declaration was removed.
	// Flush any pending digest notifications before removing.
	if len(event.Content) == 0 {
		ts.flushAndRemoveDigest(ctx, digestKey{roomID: roomID, stateKey: stateKey})
		ts.stewardshipIndex.Remove(roomID, stateKey)
		return
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		ts.logger.Warn("failed to marshal stewardship content",
			"room_id", roomID,
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	var content stewardship.StewardshipContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		ts.logger.Warn("failed to parse stewardship content",
			"room_id", roomID,
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	ts.stewardshipIndex.Put(roomID, stateKey, content)
}

// removeStewardshipForRoom removes all stewardship declarations for
// the given room from the stewardship index. Called during room leave
// and tombstone cleanup.
func (ts *TicketService) removeStewardshipForRoom(ctx context.Context, roomID ref.RoomID) {
	for _, declaration := range ts.stewardshipIndex.DeclarationsInRoom(roomID) {
		ts.flushAndRemoveDigest(ctx, digestKey{roomID: roomID, stateKey: declaration.StateKey})
		ts.stewardshipIndex.Remove(roomID, declaration.StateKey)
	}
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

		// Announce readiness for newly-tracked rooms so that
		// consumers waiting via EnsureServiceInRoom unblock.
		if _, tracked := ts.rooms[roomID]; tracked {
			service.AnnounceReady(ctx, ts.session, roomID, ts.serviceType, ts.capabilities, ts.logger)
		}
	}

	// Populate initial presence state from the sync response. The
	// initial sync may include presence events for users who share
	// rooms with the service.
	ts.processPresence(response.Presence.Events)

	ts.logger.Info("ticket index built",
		"ticketed_rooms", len(ts.rooms),
		"total_tickets", totalTickets,
		"rooms_with_members", len(ts.membersByRoom),
		"stewardship_declarations", ts.stewardshipIndex.Len(),
	)

	return sinceToken, nil
}

// processRoomState examines state and timeline events from a room to
// determine if it has ticket management enabled and, if so, indexes
// all ticket events. Also indexes member and stewardship events for
// all non-tombstoned rooms regardless of ticket configuration.
// Returns the number of tickets indexed.
//
// Called during initial sync for each joined room and can be called
// when the service joins a new room during incremental sync.
func (ts *TicketService) processRoomState(ctx context.Context, roomID ref.RoomID, stateEvents, timelineEvents []messaging.Event) int {
	// Pass 1: detect tombstone, ticket_config, and canonical alias.
	// Check both state and timeline events (timeline events with a
	// state_key are state changes).
	var config *ticket.TicketConfigContent
	var tombstoned bool
	var canonicalAlias string
	for _, event := range stateEvents {
		if event.Type == schema.MatrixEventTypeTombstone {
			tombstoned = true
		}
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
		if event.Type == schema.MatrixEventTypeCanonicalAlias {
			canonicalAlias = extractCanonicalAlias(event)
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.MatrixEventTypeTombstone {
			tombstoned = true
		}
		if event.Type == schema.EventTypeTicketConfig && event.StateKey != nil {
			config = ts.parseTicketConfig(event)
		}
		if event.Type == schema.MatrixEventTypeCanonicalAlias {
			canonicalAlias = extractCanonicalAlias(event)
		}
	}

	// Skip tombstoned rooms entirely — they are being replaced.
	if tombstoned {
		return 0
	}

	// Pass 2: index member and stewardship events for all
	// non-tombstoned rooms. These indexes are global: stewardship
	// declarations in any room can affect tickets in other rooms,
	// and the membership index serves list-members queries without
	// synchronous HTTP calls.
	for _, event := range stateEvents {
		if event.Type == schema.MatrixEventTypeRoomMember && event.StateKey != nil {
			ts.indexMemberEvent(roomID, event)
		}
		if event.Type == schema.EventTypeStewardship && event.StateKey != nil {
			ts.indexStewardshipEvent(ctx, roomID, event)
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.MatrixEventTypeRoomMember && event.StateKey != nil {
			ts.indexMemberEvent(roomID, event)
		}
		if event.Type == schema.EventTypeStewardship && event.StateKey != nil {
			ts.indexStewardshipEvent(ctx, roomID, event)
		}
	}

	// Skip rooms without ticket management.
	if config == nil {
		return 0
	}

	// This room has ticket management. Create or update room state.
	state, exists := ts.rooms[roomID]
	if !exists {
		// Prefer the canonical alias extracted from state events
		// (reliable — no network call). Fall back to a direct
		// GetStateEvent only when events don't include the alias
		// (e.g., initial /sync with a filtered event list).
		alias := canonicalAlias
		if alias == "" {
			alias = ts.resolveRoomAlias(ctx, roomID)
		}
		state = &roomState{
			config:        config,
			index:         ticketindex.NewIndex(),
			pendingEchoes: make(map[string]ref.EventID),
			alias:         alias,
		}
		ts.rooms[roomID] = state
	} else {
		state.config = config
		// Update alias if a newer canonical_alias event arrived.
		if canonicalAlias != "" {
			state.alias = canonicalAlias
		}
	}

	// Pass 3: index all ticket events.
	ticketCount := 0
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			if ts.indexTicketEvent(roomID, state, event) {
				ticketCount++
			}
		}
	}
	for _, event := range timelineEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			if ts.indexTicketEvent(roomID, state, event) {
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
// sync loop for each response. Holds a write lock for the entire
// batch because it reads and writes both the rooms map and ticket
// indexes (indexing events, evaluating gates, processing config
// changes, handling leaves).
func (ts *TicketService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Accept invites to new rooms. Fetch full state for each
	// accepted room and process it immediately — the sync filter
	// restricts which state event types the homeserver returns in
	// Rooms.Join, so newly-joined rooms may appear with no useful
	// state on the next sync cycle.
	if len(response.Rooms.Invite) > 0 {
		acceptedRooms := service.AcceptInvites(ctx, ts.session, response.Rooms.Invite, ts.logger)
		for _, roomID := range acceptedRooms {
			events, err := ts.session.GetRoomState(ctx, roomID)
			if err != nil {
				ts.logger.Error("failed to fetch state for accepted room",
					"room_id", roomID,
					"error", err,
				)
				continue
			}
			ts.processRoomState(ctx, roomID, events, nil)
			if _, tracked := ts.rooms[roomID]; tracked {
				service.AnnounceReady(ctx, ts.session, roomID, ts.serviceType, ts.capabilities, ts.logger)
			}
		}
		if len(acceptedRooms) > 0 {
			ts.logger.Info("accepted room invites", "count", len(acceptedRooms))
		}
	}

	// Clean up state for rooms the service has been removed from.
	// This handles kick, ban, and explicit leave. The room appears
	// in the Leave section when the membership transitions away from
	// "join". Process leaves before joins so that a leave+rejoin in
	// the same sync batch re-indexes cleanly from the join state.
	//
	// Membership and stewardship data is cleaned up for ALL rooms
	// in Leave, including rooms that were never ticket-configured.
	for roomID := range response.Rooms.Leave {
		delete(ts.membersByRoom, roomID)
		ts.removeStewardshipForRoom(ctx, roomID)

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

	// Update presence cache from any m.presence events in this batch.
	ts.processPresence(response.Presence.Events)

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
func (ts *TicketService) processRoomSync(ctx context.Context, roomID ref.RoomID, room messaging.JoinedRoom) {
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
			ts.handleRoomTombstone(ctx, roomID, event)
			return
		}
	}

	// Index member and stewardship events, check for ticket_config
	// changes, and update canonical aliases. Member and stewardship
	// events are indexed for all rooms; ticket_config determines
	// whether the room has ticket management.
	for _, event := range stateEvents {
		if event.Type == schema.MatrixEventTypeRoomMember && event.StateKey != nil {
			ts.indexMemberEvent(roomID, event)
		}
		if event.Type == schema.EventTypeStewardship && event.StateKey != nil {
			ts.indexStewardshipEvent(ctx, roomID, event)
		}
		if event.Type == schema.EventTypeTicketConfig {
			ts.handleTicketConfigChange(ctx, roomID, event)
		}
		if event.Type == schema.MatrixEventTypeCanonicalAlias {
			if state, exists := ts.rooms[roomID]; exists {
				if alias := extractCanonicalAlias(event); alias != "" {
					state.alias = alias
				}
			}
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
	// Collect IDs of tickets that transitioned to "closed" for
	// unblocked timer target resolution in Phase 1.5. Also push
	// timer gates to the heap for tickets arriving from external
	// sources (echoed writes already have heap entries, but the
	// lazy-deletion design handles duplicates).
	var closedTicketIDs []string
	for _, event := range stateEvents {
		if event.Type == schema.EventTypeTicket && event.StateKey != nil {
			ts.indexTicketEvent(roomID, state, event)
			if status, _ := event.Content["status"].(string); status == "closed" {
				closedTicketIDs = append(closedTicketIDs, *event.StateKey)
			}
			if content, exists := state.index.Get(*event.StateKey); exists {
				ts.pushTimerGates(roomID, *event.StateKey, &content)
			}
		}
	}

	// Phase 1.5: Resolve timer targets for tickets that became
	// unblocked due to ticket closures in this batch. Must run after
	// all ticket events are indexed (so allBlockersClosed sees current
	// state) and before gate evaluation (so timer gates can fire in
	// the same sync cycle if their target has already passed).
	if len(closedTicketIDs) > 0 {
		ts.resolveUnblockedTimerTargets(ctx, roomID, state, closedTicketIDs)
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
func (ts *TicketService) handleRoomTombstone(ctx context.Context, roomID ref.RoomID, event messaging.Event) {
	replacementRoom := ""
	if body, ok := event.Content["replacement_room"]; ok {
		if replacement, ok := body.(string); ok {
			replacementRoom = replacement
		}
	}

	// Clean up membership and stewardship data regardless of
	// whether this room had ticket management.
	delete(ts.membersByRoom, roomID)
	ts.removeStewardshipForRoom(ctx, roomID)

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
func (ts *TicketService) handleTicketConfigChange(ctx context.Context, roomID ref.RoomID, event messaging.Event) {
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
			config:        config,
			index:         ticketindex.NewIndex(),
			pendingEchoes: make(map[string]ref.EventID),
		}
		ts.rooms[roomID] = state

		// Backfill: the room may already contain ticket events from
		// before this service started tracking it. Fetch the full
		// room state and index any existing tickets. The backfill
		// also extracts the canonical alias from the full room state
		// (reliable — GetRoomState returns all events, not filtered).
		backfilled := ts.backfillRoomTickets(ctx, roomID, state)

		// Push timer gates from backfilled tickets to the heap.
		for _, entry := range state.index.PendingGates() {
			ts.pushTimerGates(roomID, entry.ID, &entry.Content)
		}

		ts.logger.Info("ticket management enabled for room",
			"room_id", roomID,
			"room_alias", state.alias,
			"prefix", config.Prefix,
			"backfilled_tickets", backfilled,
		)

		// Announce readiness so consumers waiting via
		// EnsureServiceInRoom unblock deterministically.
		service.AnnounceReady(ctx, ts.session, roomID, ts.serviceType, ts.capabilities, ts.logger)
	} else {
		state.config = config
	}
}

// backfillRoomTickets fetches the full state of a room and indexes
// any existing ticket events. Also extracts the canonical alias from
// the full state and sets it on the roomState. Called when a room
// gains ticket_config mid-operation — the initial sync path processes
// state events from the sync response directly and doesn't need
// this. Returns the number of tickets indexed.
func (ts *TicketService) backfillRoomTickets(ctx context.Context, roomID ref.RoomID, state *roomState) int {
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
			if ts.indexTicketEvent(roomID, state, event) {
				count++
			}
		}
		if event.Type == schema.MatrixEventTypeCanonicalAlias {
			if alias := extractCanonicalAlias(event); alias != "" {
				state.alias = alias
			}
		}
	}
	return count
}

// resolveRoomAlias fetches the canonical alias for a room via a
// direct GetStateEvent call. This is the fallback path used when the
// alias is not available from state events (e.g., initial /sync with
// a filtered event list). Prefer extractCanonicalAlias when state
// events are already available. Returns empty string if the room has
// no alias or the fetch fails. The alias is used for subscription
// matching (resolveRoomString) and logging.
func (ts *TicketService) resolveRoomAlias(ctx context.Context, roomID ref.RoomID) string {
	if ts.session == nil {
		return ""
	}
	raw, err := ts.session.GetStateEvent(ctx, roomID, schema.MatrixEventTypeCanonicalAlias, "")
	if err != nil {
		ts.logger.Warn("failed to fetch canonical alias for room",
			"room_id", roomID,
			"error", err,
		)
		return ""
	}
	var content struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		ts.logger.Warn("failed to parse canonical alias for room",
			"room_id", roomID,
			"error", err,
		)
		return ""
	}
	return content.Alias
}

// extractCanonicalAlias extracts the alias from a m.room.canonical_alias
// state event. Returns empty string if the event content has no alias.
func extractCanonicalAlias(event messaging.Event) string {
	alias, _ := event.Content["alias"].(string)
	return alias
}

// parseTicketConfig parses a ticket_config event's content. Returns
// nil if the content is empty or unparseable (room does not have
// ticket management).
func (ts *TicketService) parseTicketConfig(event messaging.Event) *ticket.TicketConfigContent {
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

	var config ticket.TicketConfigContent
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
//
// If the ticket has a pending echo (from a local write via a mutation
// handler or gate satisfaction), this method skips the event unless it
// IS the expected echo. This prevents the sync loop from overwriting
// optimistic local updates with stale events that were in-flight when
// the local write happened.
func (ts *TicketService) indexTicketEvent(roomID ref.RoomID, state *roomState, event messaging.Event) bool {
	if event.StateKey == nil {
		return false
	}
	ticketID := *event.StateKey

	// Check for a pending echo from a local write.
	if expectedEventID, pending := state.pendingEchoes[ticketID]; pending {
		if event.EventID == expectedEventID {
			// This is the echo of our write. Clear the pending
			// entry and fall through to index the authoritative
			// server version.
			delete(state.pendingEchoes, ticketID)
		} else {
			// This event predates our write (it was in-flight
			// when we wrote). Skip it to preserve the optimistic
			// local update.
			return false
		}
	}

	// Empty content means the ticket was redacted.
	if len(event.Content) == 0 {
		state.index.Remove(ticketID)
		ts.notifySubscribers(roomID, "remove", ticketID, ticket.TicketContent{})
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

	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		ts.logger.Warn("failed to parse ticket content",
			"ticket_id", ticketID,
			"error", err,
		)
		return false
	}

	state.index.Put(ticketID, content)
	ts.notifySubscribers(roomID, "put", ticketID, content)
	return true
}

// putWithEcho writes a ticket to Matrix and updates the local index,
// recording the returned event ID as a pending echo. The sync loop
// will skip /sync events for this ticket until the echo arrives,
// preventing stale pre-echo events from overwriting this optimistic
// update.
//
// All mutation paths that write ticket state events and update the
// local index must use this method instead of calling SendStateEvent
// and index.Put directly. This includes socket handlers, gate
// satisfaction, and any future write path.
func (ts *TicketService) putWithEcho(ctx context.Context, roomID ref.RoomID, state *roomState, ticketID string, content ticket.TicketContent) error {
	eventID, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, ticketID, content)
	if err != nil {
		return err
	}
	state.pendingEchoes[ticketID] = eventID
	state.index.Put(ticketID, content)
	ts.notifySubscribers(roomID, "put", ticketID, content)
	return nil
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
func (ts *TicketService) roomIndex(roomID ref.RoomID) *ticketindex.Index {
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
	RoomID     ref.RoomID     `cbor:"room_id"`
	Tickets    int            `cbor:"tickets"`
	ByStatus   map[string]int `cbor:"by_status"`
	ByPriority map[int]int    `cbor:"by_priority"`
}

// processPresence updates the in-memory presence cache from m.presence
// events received via /sync. Each event replaces the previous state for
// that user. Caller must hold ts.mu (write lock).
func (ts *TicketService) processPresence(events []messaging.PresenceEvent) {
	for _, event := range events {
		if event.Type != "m.presence" {
			continue
		}
		if event.Sender.IsZero() {
			continue
		}
		ts.presence[event.Sender] = event.Content
	}
}
