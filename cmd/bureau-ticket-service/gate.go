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
// Performance: this iterates all tickets with pending gates for each
// event. For rooms with many pending gates, a watch map keyed by
// (eventType, stateKey) would reduce this to O(matching gates) per
// event. The current approach is correct and fast for the expected
// scale (hundreds of tickets, single-digit pending gates per ticket).
// The evaluator's interface (events in → gate matches out) is stable
// regardless of the lookup strategy.
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

// evaluateGatesForEvent checks a single state event against all pending
// gates in the room. Called per-event so that gate satisfaction from
// earlier events in the batch is visible to later evaluations.
func (ts *TicketService) evaluateGatesForEvent(ctx context.Context, roomID string, state *roomState, event messaging.Event) {
	// Snapshot pending gates. We re-query after each satisfaction
	// because satisfying a gate modifies the ticket in the index,
	// which changes what PendingGates returns.
	candidates := state.index.PendingGates()

	for _, entry := range candidates {
		for gateIndex := range entry.Content.Gates {
			gate := &entry.Content.Gates[gateIndex]
			if gate.Status != "pending" {
				continue
			}
			if gate.Type == "human" || gate.Type == "timer" {
				// Human gates are never auto-satisfied. Timer
				// gates are evaluated by the timer ticker, not
				// by incoming events.
				continue
			}

			if !matchGateEvent(gate, event) {
				continue
			}

			// Re-read the ticket from the index to get the
			// most current version (a prior iteration may have
			// satisfied another gate on this same ticket).
			current, exists := state.index.Get(entry.ID)
			if !exists {
				continue
			}
			if gateIndex >= len(current.Gates) {
				continue
			}
			currentGate := &current.Gates[gateIndex]
			if currentGate.ID != gate.ID || currentGate.Status != "pending" {
				// Gate was already satisfied or the ticket
				// was re-indexed with different gates.
				continue
			}

			satisfiedBy := event.EventID
			if satisfiedBy == "" {
				// Fallback: use event type + state key as an
				// identifier when the event ID is missing
				// (shouldn't happen in production, but
				// defensive).
				satisfiedBy = event.Type
				if event.StateKey != nil {
					satisfiedBy += "/" + *event.StateKey
				}
			}

			if err := ts.satisfyGate(ctx, roomID, state, entry.ID, current, gateIndex, satisfiedBy); err != nil {
				ts.logger.Error("failed to satisfy gate",
					"ticket_id", entry.ID,
					"gate_id", gate.ID,
					"gate_type", gate.Type,
					"error", err,
				)
			} else {
				ts.logger.Info("gate satisfied",
					"ticket_id", entry.ID,
					"gate_id", gate.ID,
					"gate_type", gate.Type,
					"satisfied_by", satisfiedBy,
					"room_id", roomID,
				)
			}
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
// general-purpose state_event gate. Matches on event type, optionally
// state key, and optionally content match criteria.
//
// Cross-room gates (RoomAlias set) are not evaluated here — they
// require alias resolution and cross-room event routing, which is a
// separate feature. Gates with RoomAlias set are skipped; they can
// still be resolved manually via the update-gate API.
func matchStateEventGate(gate *schema.TicketGate, event messaging.Event) bool {
	// Cross-room gates require alias resolution. Skip for now;
	// these are handled by the update-gate API or future cross-room
	// evaluation.
	if gate.RoomAlias != "" {
		return false
	}

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
// exceeds CreatedAt + Duration.
func (ts *TicketService) evaluateTimerGates(ctx context.Context) {
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
