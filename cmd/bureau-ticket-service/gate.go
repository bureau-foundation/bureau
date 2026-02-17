// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// evaluateGatesForEvents checks state events from a sync batch against
// pending gates in the given room. Must be called after ticket events
// have been indexed so that ticket gates see updated statuses.
//
// For each pending gate, we check whether any of the delivered events
// satisfies its condition. When a gate matches, we mark it satisfied,
// write the updated ticket back to Matrix, and update the local index.
// Subsequent events in the batch see the post-satisfaction state because
// the index is updated immediately after each PUT.
//
// Uses the watch map index for O(1) per-event lookup of candidate
// gates instead of scanning all tickets with pending gates. The watch
// map returns candidates keyed by (eventType, stateKey); the full
// match function (matchGateEvent) is still called for content-level
// criteria (pipeline conclusion, content_match expressions).
func (ts *TicketService) evaluateGatesForEvents(ctx context.Context, roomID string, state *roomState, events []messaging.Event) {
	for _, event := range events {
		// Only state events can satisfy gates. Timeline-only events
		// (messages) don't have state keys and aren't gate conditions.
		if event.StateKey == nil {
			continue
		}

		ts.evaluateGatesForEvent(ctx, roomID, state, event)
	}
}

// evaluateGatesForEvent checks a single state event against pending
// gates that watch for its (eventType, stateKey). Called per-event so
// that gate satisfaction from earlier events in the batch is visible to
// later evaluations via the watch map update in satisfyGate → index.Put.
func (ts *TicketService) evaluateGatesForEvent(ctx context.Context, roomID string, state *roomState, event messaging.Event) {
	// The watch map returns gates whose watch criteria match this
	// event's type and state key. This is a snapshot slice — safe to
	// iterate even though satisfyGate modifies the index (and thus
	// the watch map) mid-loop.
	watches := state.index.WatchedGates(event.Type, *event.StateKey)

	for _, watch := range watches {
		// Re-read the ticket from the index to get the most
		// current version (a prior iteration may have satisfied
		// another gate on this same ticket).
		current, exists := state.index.Get(watch.TicketID)
		if !exists {
			continue
		}
		if watch.GateIndex >= len(current.Gates) {
			continue
		}
		gate := &current.Gates[watch.GateIndex]
		if gate.Status != "pending" {
			// Gate was already satisfied by a previous event
			// in this batch, or the ticket was re-indexed
			// with different gates.
			continue
		}

		// The watch map narrows candidates by (eventType, stateKey)
		// but content-level criteria (pipeline_ref, conclusion,
		// content_match) still need full match verification.
		if !matchGateEvent(gate, event) {
			continue
		}

		satisfiedBy := event.EventID
		if satisfiedBy == "" {
			// Fallback: use event type + state key as an
			// identifier when the event ID is missing
			// (shouldn't happen in production, but defensive).
			satisfiedBy = event.Type
			if event.StateKey != nil {
				satisfiedBy += "/" + *event.StateKey
			}
		}

		if err := ts.satisfyGate(ctx, roomID, state, watch.TicketID, current, watch.GateIndex, satisfiedBy); err != nil {
			ts.logger.Error("failed to satisfy gate",
				"ticket_id", watch.TicketID,
				"gate_id", gate.ID,
				"gate_type", gate.Type,
				"error", err,
			)
		} else {
			ts.logger.Info("gate satisfied",
				"ticket_id", watch.TicketID,
				"gate_id", gate.ID,
				"gate_type", gate.Type,
				"satisfied_by", satisfiedBy,
				"room_id", roomID,
			)
		}
	}
}

// matchGateEvent checks whether a state event satisfies a gate's
// condition. Dispatches to type-specific matchers. The gate must be
// pending and not a human or timer type (caller checks this).
func matchGateEvent(gate *schema.TicketGate, event messaging.Event) bool {
	switch gate.Type {
	case "pipeline":
		return matchPipelineGate(gate, event)
	case "ticket":
		return matchTicketGate(gate, event)
	case "state_event":
		return matchStateEventGate(gate, event)
	default:
		return false
	}
}

// matchPipelineGate checks whether a pipeline result event satisfies
// a pipeline gate. Matches on event type, PipelineRef, and optionally
// Conclusion.
func matchPipelineGate(gate *schema.TicketGate, event messaging.Event) bool {
	if event.Type != schema.EventTypePipelineResult {
		return false
	}

	pipelineRef, _ := event.Content["pipeline_ref"].(string)
	if pipelineRef != gate.PipelineRef {
		return false
	}

	// If the gate specifies a required conclusion, it must match.
	// An empty Conclusion means "any completed result satisfies."
	if gate.Conclusion != "" {
		conclusion, _ := event.Content["conclusion"].(string)
		if conclusion != gate.Conclusion {
			return false
		}
	}

	return true
}

// matchTicketGate checks whether a ticket state event satisfies a
// ticket gate. A ticket gate is satisfied when the referenced ticket
// reaches status "closed".
func matchTicketGate(gate *schema.TicketGate, event messaging.Event) bool {
	if event.Type != schema.EventTypeTicket {
		return false
	}
	if event.StateKey == nil || *event.StateKey != gate.TicketID {
		return false
	}

	status, _ := event.Content["status"].(string)
	return status == "closed"
}

// matchStateEventGate checks whether a state event satisfies a
// same-room state_event gate. Gates with RoomAlias set are cross-room
// and evaluated separately by evaluateCrossRoomGates.
func matchStateEventGate(gate *schema.TicketGate, event messaging.Event) bool {
	if gate.RoomAlias != "" {
		return false
	}
	return matchStateEventCondition(gate, event)
}

// matchStateEventCondition checks whether an event satisfies a
// state_event gate's event type, state key, and content match
// criteria. This is the shared matching logic used by both same-room
// evaluation (via matchStateEventGate) and cross-room evaluation
// (via evaluateCrossRoomGates). It does not check room identity —
// the caller is responsible for ensuring the event comes from the
// correct room.
func matchStateEventCondition(gate *schema.TicketGate, event messaging.Event) bool {
	if event.Type != gate.EventType {
		return false
	}

	// If the gate specifies a state key, it must match.
	if gate.StateKey != "" {
		if event.StateKey == nil || *event.StateKey != gate.StateKey {
			return false
		}
	}

	// If the gate specifies content match criteria, evaluate them.
	if len(gate.ContentMatch) > 0 {
		matched, _, err := gate.ContentMatch.Evaluate(event.Content)
		if err != nil {
			// Malformed match expression — treat as non-match.
			// The expression was validated at gate creation time,
			// so this shouldn't happen in practice.
			return false
		}
		return matched
	}

	return true
}

// --- Cross-room gate evaluation ---

// aliasResolver resolves Matrix room aliases to room IDs. The
// *messaging.Session implements this interface. Tests substitute a
// fake implementation.
type aliasResolver interface {
	ResolveAlias(ctx context.Context, alias string) (string, error)
}

// evaluateCrossRoomGates checks events from non-ticket rooms against
// pending cross-room state_event gates. A cross-room gate has
// RoomAlias set, indicating it watches a different room than the
// ticket's own room.
//
// This scans all ticket rooms' pending gates for state_event gates
// with RoomAlias, resolves each alias to a room ID, and checks if
// the sync response delivered matching events from that room.
//
// The alias resolution result is cached on the TicketService so
// subsequent sync batches don't re-resolve the same aliases.
//
// Cross-room gates can only auto-evaluate for event types included
// in the /sync filter. If a gate references an event type the filter
// doesn't include, the homeserver won't deliver those events and the
// gate must be resolved manually via the resolve-gate API.
func (ts *TicketService) evaluateCrossRoomGates(ctx context.Context, joinedRooms map[string]messaging.JoinedRoom) {
	if ts.resolver == nil {
		return
	}

	// Collect cross-room gates across all ticket rooms. For each,
	// resolve the alias and check if events arrived from that room.
	for ticketRoomID, state := range ts.rooms {
		candidates := state.index.PendingGates()
		for _, entry := range candidates {
			for gateIndex := range entry.Content.Gates {
				gate := &entry.Content.Gates[gateIndex]
				if gate.Status != "pending" || gate.Type != "state_event" || gate.RoomAlias == "" {
					continue
				}

				watchedRoomID, err := ts.resolveAliasWithCache(ctx, gate.RoomAlias)
				if err != nil {
					ts.logger.Warn("failed to resolve cross-room gate alias",
						"ticket_id", entry.ID,
						"gate_id", gate.ID,
						"room_alias", gate.RoomAlias,
						"error", err,
					)
					continue
				}

				// Check if the sync batch included events from
				// the watched room.
				watchedRoom, hasEvents := joinedRooms[watchedRoomID]
				if !hasEvents {
					continue
				}

				// Collect state events from the watched room.
				events := collectStateEvents(watchedRoom)
				for _, event := range events {
					if !matchStateEventCondition(gate, event) {
						continue
					}

					// Re-read the ticket for the current version.
					current, exists := state.index.Get(entry.ID)
					if !exists {
						break
					}
					if gateIndex >= len(current.Gates) {
						break
					}
					currentGate := &current.Gates[gateIndex]
					if currentGate.ID != gate.ID || currentGate.Status != "pending" {
						break
					}

					satisfiedBy := event.EventID
					if satisfiedBy == "" {
						satisfiedBy = event.Type
						if event.StateKey != nil {
							satisfiedBy += "/" + *event.StateKey
						}
					}

					if err := ts.satisfyGate(ctx, ticketRoomID, state, entry.ID, current, gateIndex, satisfiedBy); err != nil {
						ts.logger.Error("failed to satisfy cross-room gate",
							"ticket_id", entry.ID,
							"gate_id", gate.ID,
							"room_alias", gate.RoomAlias,
							"watched_room", watchedRoomID,
							"error", err,
						)
					} else {
						ts.logger.Info("cross-room gate satisfied",
							"ticket_id", entry.ID,
							"gate_id", gate.ID,
							"room_alias", gate.RoomAlias,
							"watched_room", watchedRoomID,
							"satisfied_by", satisfiedBy,
							"ticket_room", ticketRoomID,
						)
					}
					// Gate satisfied — stop checking events for
					// this gate.
					break
				}
			}
		}
	}
}

// collectStateEvents extracts state events from a joined room's sync
// response (both the state section and state events in the timeline).
func collectStateEvents(room messaging.JoinedRoom) []messaging.Event {
	var events []messaging.Event
	events = append(events, room.State.Events...)
	for _, event := range room.Timeline.Events {
		if event.StateKey != nil {
			events = append(events, event)
		}
	}
	return events
}

// resolveAliasWithCache resolves a room alias to a room ID, caching
// the result. Cache entries persist for the lifetime of the service.
// Aliases that fail to resolve are not cached (the room may not exist
// yet, and a future attempt should retry).
func (ts *TicketService) resolveAliasWithCache(ctx context.Context, alias string) (string, error) {
	if roomID, cached := ts.aliasCache[alias]; cached {
		return roomID, nil
	}

	roomID, err := ts.resolver.ResolveAlias(ctx, alias)
	if err != nil {
		return "", err
	}

	ts.aliasCache[alias] = roomID
	return roomID, nil
}

// satisfyGate marks a gate as satisfied, writes the updated ticket to
// Matrix, and updates the local index. The content parameter must be
// the current version of the ticket from the index (not a stale copy).
func (ts *TicketService) satisfyGate(
	ctx context.Context,
	roomID string,
	state *roomState,
	ticketID string,
	content schema.TicketContent,
	gateIndex int,
	satisfiedBy string,
) error {
	now := ts.clock.Now().UTC().Format(time.RFC3339)

	content.Gates[gateIndex].Status = "satisfied"
	content.Gates[gateIndex].SatisfiedAt = now
	content.Gates[gateIndex].SatisfiedBy = satisfiedBy
	content.UpdatedAt = now

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, ticketID, content); err != nil {
		return err
	}

	state.index.Put(ticketID, content)
	return nil
}

// evaluateTimerGates checks all pending timer gates across all rooms
// for expiration. A timer gate is satisfied when the current time
// exceeds CreatedAt + Duration. Holds a write lock because gate
// satisfaction modifies the index via satisfyGate.
func (ts *TicketService) evaluateTimerGates(ctx context.Context) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	now := ts.clock.Now()

	for roomID, state := range ts.rooms {
		candidates := state.index.PendingGates()
		for _, entry := range candidates {
			for gateIndex := range entry.Content.Gates {
				gate := &entry.Content.Gates[gateIndex]
				if gate.Status != "pending" || gate.Type != "timer" {
					continue
				}

				expired, err := timerExpired(gate, now)
				if err != nil {
					ts.logger.Warn("invalid timer gate",
						"ticket_id", entry.ID,
						"gate_id", gate.ID,
						"error", err,
					)
					continue
				}
				if !expired {
					continue
				}

				// Re-read the ticket to get the current version.
				current, exists := state.index.Get(entry.ID)
				if !exists {
					continue
				}
				if gateIndex >= len(current.Gates) {
					continue
				}
				currentGate := &current.Gates[gateIndex]
				if currentGate.ID != gate.ID || currentGate.Status != "pending" {
					continue
				}

				if err := ts.satisfyGate(ctx, roomID, state, entry.ID, current, gateIndex, "timer"); err != nil {
					ts.logger.Error("failed to satisfy timer gate",
						"ticket_id", entry.ID,
						"gate_id", gate.ID,
						"error", err,
					)
				} else {
					ts.logger.Info("timer gate satisfied",
						"ticket_id", entry.ID,
						"gate_id", gate.ID,
						"room_id", roomID,
					)
				}
			}
		}
	}
}

// timerExpired checks whether a timer gate's deadline has passed.
// Returns (expired, error) — error is non-nil only if the gate's
// CreatedAt or Duration fields are unparseable.
func timerExpired(gate *schema.TicketGate, now time.Time) (bool, error) {
	createdAt, err := time.Parse(time.RFC3339, gate.CreatedAt)
	if err != nil {
		return false, err
	}
	duration, err := time.ParseDuration(gate.Duration)
	if err != nil {
		return false, err
	}
	return now.After(createdAt.Add(duration)) || now.Equal(createdAt.Add(duration)), nil
}

// timerTickInterval is how often the service checks for expired timer
// gates. Timer granularity is limited to this interval.
const timerTickInterval = 30 * time.Second

// startTimerTicker runs a periodic loop that evaluates timer gates.
// Blocks until ctx is cancelled.
func (ts *TicketService) startTimerTicker(ctx context.Context) {
	ticker := ts.clock.NewTicker(timerTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ts.evaluateTimerGates(ctx)
		}
	}
}
