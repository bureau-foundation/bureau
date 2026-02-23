// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/heap"
	"context"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/cron"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// deadlineHeapID is the sentinel gate ID used for deadline monitoring
// entries in the timer heap. When the timer loop pops an entry with
// this ID, it adds a "Deadline passed" note to the ticket instead of
// satisfying a gate.
const deadlineHeapID = "__deadline__"

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
func (ts *TicketService) evaluateGatesForEvents(ctx context.Context, roomID ref.RoomID, state *roomState, events []messaging.Event) {
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
func (ts *TicketService) evaluateGatesForEvent(ctx context.Context, roomID ref.RoomID, state *roomState, event messaging.Event) {
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

		satisfiedBy := event.EventID.String()
		if satisfiedBy == "" {
			// Fallback: use event type + state key as an
			// identifier when the event ID is missing
			// (shouldn't happen in production, but defensive).
			satisfiedBy = string(event.Type)
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
func matchGateEvent(gate *ticket.TicketGate, event messaging.Event) bool {
	switch gate.Type {
	case "pipeline":
		return matchPipelineGate(gate, event)
	case "ticket":
		return matchTicketGate(gate, event)
	case "review":
		return matchReviewGate(gate, event)
	case "state_event":
		return matchStateEventGate(gate, event)
	default:
		return false
	}
}

// matchPipelineGate checks whether a pipeline result event satisfies
// a pipeline gate. Matches on event type, PipelineRef, and optionally
// Conclusion.
func matchPipelineGate(gate *ticket.TicketGate, event messaging.Event) bool {
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
func matchTicketGate(gate *ticket.TicketGate, event messaging.Event) bool {
	if event.Type != schema.EventTypeTicket {
		return false
	}
	if event.StateKey == nil || *event.StateKey != gate.TicketID {
		return false
	}

	status, _ := event.Content["status"].(string)
	return status == "closed"
}

// matchReviewGate checks whether a ticket state event indicates that
// all reviewers have approved. The review gate watches its own ticket's
// state event and is satisfied when every reviewer in the review field
// has disposition "approved".
func matchReviewGate(gate *ticket.TicketGate, event messaging.Event) bool {
	if event.Type != schema.EventTypeTicket {
		return false
	}

	// The event content is a map[string]any from the raw Matrix JSON.
	// Extract the review field and check reviewer dispositions.
	reviewRaw, exists := event.Content["review"]
	if !exists {
		return false
	}
	reviewMap, ok := reviewRaw.(map[string]any)
	if !ok {
		return false
	}

	reviewersRaw, exists := reviewMap["reviewers"]
	if !exists {
		return false
	}
	reviewersList, ok := reviewersRaw.([]any)
	if !ok || len(reviewersList) == 0 {
		return false
	}

	for _, entryRaw := range reviewersList {
		entry, ok := entryRaw.(map[string]any)
		if !ok {
			return false
		}
		disposition, _ := entry["disposition"].(string)
		if disposition != "approved" {
			return false
		}
	}

	return true
}

// matchStateEventGate checks whether a state event satisfies a
// same-room state_event gate. Gates with RoomAlias set are cross-room
// and evaluated separately by evaluateCrossRoomGates.
func matchStateEventGate(gate *ticket.TicketGate, event messaging.Event) bool {
	if !gate.RoomAlias.IsZero() {
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
func matchStateEventCondition(gate *ticket.TicketGate, event messaging.Event) bool {
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
// messaging.Session interface includes this method. Tests substitute a
// fake implementation.
type aliasResolver interface {
	ResolveAlias(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error)
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
func (ts *TicketService) evaluateCrossRoomGates(ctx context.Context, joinedRooms map[ref.RoomID]messaging.JoinedRoom) {
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
				if gate.Status != "pending" || gate.Type != "state_event" || gate.RoomAlias.IsZero() {
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

					satisfiedBy := event.EventID.String()
					if satisfiedBy == "" {
						satisfiedBy = string(event.Type)
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
func (ts *TicketService) resolveAliasWithCache(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
	if roomID, cached := ts.aliasCache[alias]; cached {
		return roomID, nil
	}

	resolved, err := ts.resolver.ResolveAlias(ctx, alias)
	if err != nil {
		return ref.RoomID{}, err
	}

	ts.aliasCache[alias] = resolved
	return resolved, nil
}

// satisfyGate marks a gate as satisfied, writes the updated ticket to
// Matrix, and updates the local index. The content parameter must be
// the current version of the ticket from the index (not a stale copy).
func (ts *TicketService) satisfyGate(
	ctx context.Context,
	roomID ref.RoomID,
	state *roomState,
	ticketID string,
	content ticket.TicketContent,
	gateIndex int,
	satisfiedBy string,
) error {
	now := ts.clock.Now().UTC().Format(time.RFC3339)

	content.Gates[gateIndex].Status = "satisfied"
	content.Gates[gateIndex].SatisfiedAt = now
	content.Gates[gateIndex].SatisfiedBy = satisfiedBy
	content.UpdatedAt = now

	return ts.putWithEcho(ctx, roomID, state, ticketID, content)
}

// timerExpired checks whether a timer gate's target time has been
// reached. Target is the pre-computed absolute time at which the gate
// fires, set at creation time by enrichTimerTargets (for base="created")
// or later by resolveUnblockedTimerTargets (for base="unblocked").
//
// Returns false with no error if Target is empty — this means the
// timer has not started yet (e.g., base="unblocked" with open
// blockers).
func timerExpired(gate *ticket.TicketGate, now time.Time) (bool, error) {
	if gate.Target == "" {
		return false, nil
	}
	target, err := time.Parse(time.RFC3339, gate.Target)
	if err != nil {
		return false, fmt.Errorf("invalid target: %w", err)
	}
	return !now.Before(target), nil
}

// --- Timer target computation ---

// computeTimerTarget computes the absolute Target for a timer gate
// from its Duration and the given base time. No-op if the gate already
// has a Target, has no Duration, or is not a timer gate. Mutates the
// gate in place.
func computeTimerTarget(gate *ticket.TicketGate, baseTime time.Time) error {
	if gate.Type != "timer" || gate.Target != "" || gate.Duration == "" {
		return nil
	}
	duration, err := time.ParseDuration(gate.Duration)
	if err != nil {
		return fmt.Errorf("gate %q: invalid duration: %w", gate.ID, err)
	}
	gate.Target = baseTime.Add(duration).UTC().Format(time.RFC3339)
	return nil
}

// enrichTimerTargets computes Target for timer gates at ticket creation
// time. For base="created" (default), Target = CreatedAt + Duration.
// For base="unblocked" when all blockers are already closed (or there
// are none), Target = CreatedAt + Duration. For base="unblocked" when
// blockers are still open, Target remains empty until blockers clear
// via resolveUnblockedTimerTargets.
//
// Called from handleCreate and handleBatchCreate after gate CreatedAt
// enrichment.
func enrichTimerTargets(content *ticket.TicketContent, index *ticketindex.Index) {
	for i := range content.Gates {
		gate := &content.Gates[i]
		if gate.Type != "timer" || gate.Target != "" || gate.Duration == "" {
			continue
		}

		base := gate.Base
		if base == "" {
			base = "created"
		}

		switch base {
		case "created":
			createdAt, err := time.Parse(time.RFC3339, gate.CreatedAt)
			if err != nil {
				// CreatedAt was just set by the caller; this shouldn't
				// fail. Validation will catch it later.
				continue
			}
			_ = computeTimerTarget(gate, createdAt)

		case "unblocked":
			if len(content.BlockedBy) == 0 || index.AllBlockersClosed(content) {
				// No blockers or all already closed — the ticket is
				// effectively unblocked at creation time.
				createdAt, err := time.Parse(time.RFC3339, gate.CreatedAt)
				if err != nil {
					continue
				}
				_ = computeTimerTarget(gate, createdAt)
			}
			// Otherwise Target stays empty. resolveUnblockedTimerTargets
			// will compute it when all blockers close.
		}
	}
}

// resolveUnblockedTimerTargets checks whether any tickets that depend
// on the given closed ticket IDs have just become fully unblocked, and
// if so, computes timer targets for their base="unblocked" gates.
//
// Called during the sync loop after ticket indexing, before gate
// evaluation. When a ticket transitions to "closed", its dependents
// (tickets with it in blocked_by) may become unblocked. If a dependent
// has timer gates with base="unblocked" and empty Target, the target
// is computed using the current clock time as the base (the moment
// the ticket observes it has become unblocked).
func (ts *TicketService) resolveUnblockedTimerTargets(ctx context.Context, roomID ref.RoomID, state *roomState, closedTicketIDs []string) {
	now := ts.clock.Now()

	for _, closedID := range closedTicketIDs {
		dependentIDs := state.index.Blocks(closedID)

		for _, dependentID := range dependentIDs {
			content, exists := state.index.Get(dependentID)
			if !exists || content.Status != "open" {
				continue
			}

			if !state.index.AllBlockersClosed(&content) {
				continue
			}

			// All blockers are now closed. Check for timer gates
			// with base="unblocked" that need target computation.
			needsWrite := false
			for i := range content.Gates {
				gate := &content.Gates[i]
				if gate.Type != "timer" || gate.Status != "pending" {
					continue
				}
				if gate.Base != "unblocked" || gate.Target != "" || gate.Duration == "" {
					continue
				}

				// Compute target: base time is NOW (the moment
				// blockers cleared).
				if err := computeTimerTarget(gate, now); err != nil {
					ts.logger.Warn("failed to compute timer target for unblocked gate",
						"ticket_id", dependentID,
						"gate_id", gate.ID,
						"error", err,
					)
					continue
				}
				needsWrite = true

				ts.logger.Info("timer target computed on blocker resolution",
					"ticket_id", dependentID,
					"gate_id", gate.ID,
					"target", gate.Target,
					"room_id", roomID,
				)
			}

			if needsWrite {
				content.UpdatedAt = now.UTC().Format(time.RFC3339)
				if err := ts.putWithEcho(ctx, roomID, state, dependentID, content); err != nil {
					ts.logger.Error("failed to write timer target update",
						"ticket_id", dependentID,
						"error", err,
					)
				} else {
					ts.pushTimerGates(roomID, dependentID, &content)
				}
			}
		}
	}
}

// --- Timer heap ---

// timerHeapEntry represents a single pending timer in the priority queue.
// Entries are ordered by target time (earliest first). Lazy deletion:
// when an entry is popped, the caller must verify the gate is still
// pending and the target matches before firing it.
type timerHeapEntry struct {
	target   time.Time
	roomID   ref.RoomID
	ticketID string
	gateID   string
}

// timerHeap is a min-heap of timer entries ordered by target time.
// Implements container/heap.Interface.
type timerHeap []timerHeapEntry

func (h timerHeap) Len() int           { return len(h) }
func (h timerHeap) Less(i, j int) bool { return h[i].target.Before(h[j].target) }
func (h timerHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)        { *h = append(*h, x.(timerHeapEntry)) }
func (h *timerHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	*h = old[:len(old)-1]
	return entry
}

// pushTimerGates adds all pending timer gates with a non-empty Target
// from the given ticket content to the timer heap, and reschedules
// the timer if the new entries affect the earliest deadline.
// Must be called with ts.mu held.
func (ts *TicketService) pushTimerGates(roomID ref.RoomID, ticketID string, content *ticket.TicketContent) {
	pushed := false
	for i := range content.Gates {
		gate := &content.Gates[i]
		if gate.Type != "timer" || gate.Status != "pending" || gate.Target == "" {
			continue
		}
		target, err := time.Parse(time.RFC3339, gate.Target)
		if err != nil {
			continue
		}
		heap.Push(&ts.timers, timerHeapEntry{
			target:   target,
			roomID:   roomID,
			ticketID: ticketID,
			gateID:   gate.ID,
		})
		pushed = true
	}
	if pushed {
		ts.scheduleNextTimerLocked()
	}
}

// pushDeadlineEntry adds a deadline monitoring entry to the timer heap
// if the ticket has a non-empty deadline and is not closed. The entry
// uses the sentinel deadlineHeapID so the timer loop knows to add a
// note instead of satisfying a gate. Stale entries from a previous
// deadline are handled by lazy deletion in fireExpiredTimersLocked.
// Must be called with ts.mu held.
func (ts *TicketService) pushDeadlineEntry(roomID ref.RoomID, ticketID string, content *ticket.TicketContent) {
	if content.Deadline == "" || content.Status == "closed" {
		return
	}
	target, err := time.Parse(time.RFC3339, content.Deadline)
	if err != nil {
		return
	}
	heap.Push(&ts.timers, timerHeapEntry{
		target:   target,
		roomID:   roomID,
		ticketID: ticketID,
		gateID:   deadlineHeapID,
	})
	ts.scheduleNextTimerLocked()
}

// rebuildTimerHeap clears the timer heap and repopulates it from all
// pending timer gates and deadlines across all tracked rooms. Called
// once after initial sync to seed the heap with pre-existing timers
// and deadlines. Must be called with ts.mu held (or before concurrent
// access begins).
func (ts *TicketService) rebuildTimerHeap() {
	ts.timers = ts.timers[:0]
	for roomID, state := range ts.rooms {
		// Add pending timer gates.
		for _, entry := range state.index.PendingGates() {
			for i := range entry.Content.Gates {
				gate := &entry.Content.Gates[i]
				if gate.Type != "timer" || gate.Status != "pending" || gate.Target == "" {
					continue
				}
				target, err := time.Parse(time.RFC3339, gate.Target)
				if err != nil {
					continue
				}
				ts.timers = append(ts.timers, timerHeapEntry{
					target:   target,
					roomID:   roomID,
					ticketID: entry.ID,
					gateID:   gate.ID,
				})
			}
		}

		// Add deadline monitoring entries for non-closed tickets.
		for _, entry := range state.index.List(ticketindex.Filter{}) {
			if entry.Content.Deadline == "" || entry.Content.Status == "closed" {
				continue
			}
			target, err := time.Parse(time.RFC3339, entry.Content.Deadline)
			if err != nil {
				continue
			}
			ts.timers = append(ts.timers, timerHeapEntry{
				target:   target,
				roomID:   roomID,
				ticketID: entry.ID,
				gateID:   deadlineHeapID,
			})
		}
	}
	heap.Init(&ts.timers)
}

// --- Timer scheduling ---

// scheduleNextTimerLocked stops the current timer and schedules a new
// one based on the heap minimum. If the earliest entry is already
// expired, signals the timer notify channel immediately (inline, no
// AfterFunc) to avoid FakeClock deadlock when called under ts.mu.
// Must be called with ts.mu held.
func (ts *TicketService) scheduleNextTimerLocked() {
	if ts.timerNotify == nil {
		return
	}

	if ts.timerFunc != nil {
		ts.timerFunc.Stop()
		ts.timerFunc = nil
	}

	if ts.timers.Len() == 0 {
		return
	}

	delay := ts.timers[0].target.Sub(ts.clock.Now())
	if delay <= 0 {
		// Already expired — signal immediately. Do NOT use
		// AfterFunc(d<=0) here: FakeClock calls the callback
		// synchronously, which would deadlock if ts.mu is held.
		ts.signalTimerNotify()
		return
	}

	ts.timerFunc = ts.clock.AfterFunc(delay, ts.signalTimerNotify)
}

// signalTimerNotify sends a non-blocking signal to the timer loop.
// Safe to call from AfterFunc callbacks (does not acquire ts.mu).
func (ts *TicketService) signalTimerNotify() {
	if ts.timerNotify == nil {
		return
	}
	select {
	case ts.timerNotify <- struct{}{}:
	default:
	}
}

// fireExpiredTimersLocked pops all expired entries from the heap and
// satisfies their gates (or handles deadline notifications). Uses
// lazy deletion: each popped entry is verified against the current
// index state before firing. Must be called with ts.mu held.
func (ts *TicketService) fireExpiredTimersLocked(ctx context.Context) {
	now := ts.clock.Now()

	for ts.timers.Len() > 0 {
		// Peek at the earliest entry.
		earliest := ts.timers[0]
		if earliest.target.After(now) {
			break
		}
		heap.Pop(&ts.timers)

		// Lazy deletion: verify the ticket still exists and is
		// open (deadline monitoring only applies to open tickets).
		state, exists := ts.rooms[earliest.roomID]
		if !exists {
			continue
		}
		content, exists := state.index.Get(earliest.ticketID)
		if !exists || content.Status == "closed" {
			continue
		}

		// Deadline monitoring entries: add a note instead of
		// satisfying a gate. Lazy-delete if the deadline was
		// removed or changed since this entry was pushed.
		if earliest.gateID == deadlineHeapID {
			ts.fireDeadlineLocked(ctx, earliest, state, content)
			continue
		}

		// Gate timer entries require open status specifically
		// (non-open, non-closed statuses like blocked/in_progress
		// should not fire gates).
		if content.Status != "open" {
			continue
		}

		// Find the gate by ID (gate indices can shift if gates
		// are added or removed between heap push and pop).
		gateIndex := -1
		for i := range content.Gates {
			if content.Gates[i].ID == earliest.gateID {
				gateIndex = i
				break
			}
		}
		if gateIndex == -1 {
			continue
		}

		gate := &content.Gates[gateIndex]
		if gate.Status != "pending" || gate.Type != "timer" {
			continue
		}

		// Verify the target matches. The ticket may have been
		// updated with a different target since this entry was
		// pushed; the updated target should already have its
		// own heap entry.
		if gate.Target != earliest.target.UTC().Format(time.RFC3339) {
			continue
		}

		if err := ts.satisfyGate(ctx, earliest.roomID, state, earliest.ticketID, content, gateIndex, "timer"); err != nil {
			ts.logger.Error("failed to satisfy timer gate",
				"ticket_id", earliest.ticketID,
				"gate_id", earliest.gateID,
				"error", err,
			)
		} else {
			ts.logger.Info("timer gate satisfied",
				"ticket_id", earliest.ticketID,
				"gate_id", earliest.gateID,
				"room_id", earliest.roomID,
			)
		}
	}
}

// fireDeadlineLocked handles a popped deadline monitoring entry. It
// verifies the ticket still has a matching deadline, adds a
// "Deadline passed" note, and writes the updated ticket to Matrix.
// Must be called with ts.mu held.
func (ts *TicketService) fireDeadlineLocked(ctx context.Context, entry timerHeapEntry, state *roomState, content ticket.TicketContent) {
	// Lazy deletion: verify the ticket still has the same deadline.
	if content.Deadline == "" {
		return
	}
	deadlineTarget, err := time.Parse(time.RFC3339, content.Deadline)
	if err != nil || !deadlineTarget.Equal(entry.target) {
		return
	}

	// Add a "Deadline passed" note. Generate a note ID from
	// existing count.
	noteID := fmt.Sprintf("n-%d", len(content.Notes)+1)
	now := ts.clock.Now().UTC().Format(time.RFC3339)

	content.Notes = append(content.Notes, ticket.TicketNote{
		ID:        noteID,
		Author:    ts.service.UserID(),
		CreatedAt: now,
		Body:      fmt.Sprintf("Deadline passed: %s", content.Deadline),
	})
	content.UpdatedAt = now

	if err := ts.putWithEcho(ctx, entry.roomID, state, entry.ticketID, content); err != nil {
		ts.logger.Error("failed to add deadline note",
			"ticket_id", entry.ticketID,
			"deadline", content.Deadline,
			"room_id", entry.roomID,
			"error", err,
		)
	} else {
		ts.logger.Warn("deadline passed for ticket",
			"ticket_id", entry.ticketID,
			"deadline", content.Deadline,
			"room_id", entry.roomID,
		)
	}
}

// startTimerLoop runs the event-driven timer loop. Replaces the
// polling startTimerTicker: instead of scanning all pending gates
// every 30 seconds, it uses a min-heap and a single AfterFunc to
// wake up precisely when the next timer fires. Blocks until ctx is
// cancelled.
func (ts *TicketService) startTimerLoop(ctx context.Context) {
	// Schedule the first timer from whatever is in the heap.
	ts.mu.Lock()
	ts.scheduleNextTimerLocked()
	ts.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			ts.mu.Lock()
			if ts.timerFunc != nil {
				ts.timerFunc.Stop()
				ts.timerFunc = nil
			}
			ts.mu.Unlock()
			return
		case <-ts.timerNotify:
			ts.mu.Lock()
			ts.fireExpiredTimersLocked(ctx)
			ts.scheduleNextTimerLocked()
			ts.mu.Unlock()
		}
	}
}

// --- Recurring gate re-arm ---

// hasRecurringGates reports whether any gates on the ticket are
// recurring timer gates that are eligible to re-arm. A gate is
// eligible if it is a timer with Schedule or Interval set, it has
// been satisfied (or will be during close), and has not exhausted
// its MaxOccurrences limit.
func hasRecurringGates(content *ticket.TicketContent) bool {
	for i := range content.Gates {
		if content.Gates[i].IsRecurring() {
			return true
		}
	}
	return false
}

// rearmRecurringGates resets recurring timer gates for the next
// cycle. For each recurring gate on the ticket:
//
//   - If MaxOccurrences > 0 and FireCount >= MaxOccurrences, the
//     gate has exhausted its limit and is left satisfied (no re-arm).
//   - Otherwise, compute the next Target (from Schedule via cron, or
//     Interval relative to now), reset Status to "pending", clear
//     SatisfiedAt/SatisfiedBy, increment FireCount, set LastFiredAt.
//
// Returns true if at least one gate was re-armed (meaning the ticket
// should reopen instead of closing). Returns false if all recurring
// gates are exhausted and the ticket should close normally.
func rearmRecurringGates(content *ticket.TicketContent, now time.Time) (bool, error) {
	rearmed := false
	nowStr := now.UTC().Format(time.RFC3339)

	for i := range content.Gates {
		gate := &content.Gates[i]
		if !gate.IsRecurring() {
			continue
		}

		// Check MaxOccurrences limit. FireCount reflects past
		// fires; we are about to record the current fire, so
		// the effective count after this cycle is FireCount+1.
		if gate.MaxOccurrences > 0 && gate.FireCount+1 >= gate.MaxOccurrences {
			// Gate has reached its limit. Record the final fire
			// metadata but leave the gate satisfied.
			gate.FireCount++
			gate.LastFiredAt = nowStr
			continue
		}

		// Compute next Target.
		var nextTarget time.Time
		if gate.Schedule != "" {
			schedule, err := cron.Parse(gate.Schedule)
			if err != nil {
				return false, fmt.Errorf("gate %q: invalid schedule %q: %w", gate.ID, gate.Schedule, err)
			}
			next, err := schedule.Next(now)
			if err != nil {
				return false, fmt.Errorf("gate %q: cannot compute next cron occurrence: %w", gate.ID, err)
			}
			nextTarget = next
		} else {
			// Interval-based: next occurrence is relative to now
			// (drift-based, not anchored to wall clock).
			interval, err := time.ParseDuration(gate.Interval)
			if err != nil {
				return false, fmt.Errorf("gate %q: invalid interval %q: %w", gate.ID, gate.Interval, err)
			}
			nextTarget = now.Add(interval)
		}

		// Re-arm the gate.
		gate.FireCount++
		gate.LastFiredAt = nowStr
		gate.Status = "pending"
		gate.SatisfiedAt = ""
		gate.SatisfiedBy = ""
		gate.Target = nextTarget.UTC().Format(time.RFC3339)
		rearmed = true
	}

	return rearmed, nil
}

// stripRecurringGates removes all recurring timer gates from the
// ticket content. Used when EndRecurrence is true on a close
// request — the caller wants to permanently close the ticket without
// it re-arming.
func stripRecurringGates(content *ticket.TicketContent) {
	kept := content.Gates[:0]
	for i := range content.Gates {
		if !content.Gates[i].IsRecurring() {
			kept = append(kept, content.Gates[i])
		}
	}
	content.Gates = kept
}
