// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"sort"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// subscriber represents a single connected subscribe stream. The
// channel receives events from the notification hooks in putWithEcho
// and indexTicketEvent. The done channel is closed by the subscriber's
// stream handler goroutine when the connection ends; the fanout logic
// detects this and removes the subscriber from the registry.
type subscriber struct {
	roomID  ref.RoomID
	channel chan subscribeEvent
	resync  atomic.Bool
	done    <-chan struct{}
}

// subscriberChannelSize is the buffer size for the per-subscriber
// event channel. Must be large enough to absorb a full sync batch
// without drops. If a subscriber's channel is full, the event is
// dropped and the subscriber is marked for resync.
const subscriberChannelSize = 256

// subscribeEvent is a single mutation event dispatched to subscribers.
type subscribeEvent struct {
	Kind     string // "put" or "remove"
	TicketID string
	Content  ticket.TicketContent // populated for "put", zero value for "remove"
}

// addSubscriber registers a subscriber for a room. Must be called
// with ts.mu held (write lock).
func (ts *TicketService) addSubscriber(subscriber *subscriber) {
	ts.subscribers[subscriber.roomID] = append(ts.subscribers[subscriber.roomID], subscriber)
}

// removeSubscriber removes a subscriber from the registry. Must be
// called with ts.mu held (write lock).
func (ts *TicketService) removeSubscriber(subscriber *subscriber) {
	roomSubscribers := ts.subscribers[subscriber.roomID]
	for i, existing := range roomSubscribers {
		if existing == subscriber {
			ts.subscribers[subscriber.roomID] = append(roomSubscribers[:i], roomSubscribers[i+1:]...)
			break
		}
	}
	if len(ts.subscribers[subscriber.roomID]) == 0 {
		delete(ts.subscribers, subscriber.roomID)
	}
}

// notifySubscribers dispatches a subscribeEvent to all subscribers
// watching the given room. Uses non-blocking sends: if a subscriber's
// channel is full, the event is dropped and the subscriber is marked
// for resync. Subscribers with a closed done channel are removed.
//
// Must be called with ts.mu held (write lock). The non-blocking send
// ensures this does not add blocking I/O under the lock.
func (ts *TicketService) notifySubscribers(roomID ref.RoomID, kind string, ticketID string, content ticket.TicketContent) {
	roomSubscribers := ts.subscribers[roomID]
	if len(roomSubscribers) == 0 {
		return
	}

	event := subscribeEvent{
		Kind:     kind,
		TicketID: ticketID,
		Content:  content,
	}

	// Iterate in reverse so that removals don't shift unvisited elements.
	for i := len(roomSubscribers) - 1; i >= 0; i-- {
		subscriber := roomSubscribers[i]

		// Clean up disconnected subscribers.
		select {
		case <-subscriber.done:
			roomSubscribers = append(roomSubscribers[:i], roomSubscribers[i+1:]...)
			continue
		default:
		}

		// Non-blocking send. On overflow, mark for resync.
		select {
		case subscriber.channel <- event:
		default:
			subscriber.resync.Store(true)
		}
	}

	if len(roomSubscribers) == 0 {
		delete(ts.subscribers, roomID)
	} else {
		ts.subscribers[roomID] = roomSubscribers
	}
}

// --- Subscribe wire protocol ---

// subscribeRequest is the decoded request body for the "subscribe"
// stream action. The Room field identifies which room to subscribe to.
type subscribeRequest struct {
	Room string `cbor:"room"`
}

// subscribeFrame is a single CBOR value written on the subscribe
// stream. The Type field discriminates frame semantics:
//
//   - "put": a ticket was created or updated (TicketID, Content populated)
//   - "remove": a ticket was deleted (TicketID populated)
//   - "open_complete": all open tickets and their closed deps have been
//     sent; the viewer can render Ready/Blocked tabs (Stats populated)
//   - "caught_up": the full snapshot is complete, live events follow
//     (Stats populated)
//   - "heartbeat": connection liveness probe (no payload)
//   - "resync": subscriber buffer overflowed; the viewer should clear
//     its local index and expect a new full snapshot
//   - "error": terminal error, connection will close (Message populated)
type subscribeFrame struct {
	Type     string               `cbor:"type"`
	TicketID string               `cbor:"ticket_id,omitempty"`
	Content  ticket.TicketContent `cbor:"content,omitempty"`
	Stats    *ticketindex.Stats   `cbor:"stats,omitempty"`
	Message  string               `cbor:"message,omitempty"`
}

// heartbeatInterval is the time between heartbeat frames on a
// subscribe stream. The client should consider the connection dead
// if no frame (of any type) arrives within 2x this interval.
const heartbeatInterval = 30 * time.Second

// --- Subscribe handler ---

// handleSubscribe is the stream handler for the "subscribe" action.
// It performs a phased snapshot of the room's ticket index, then
// enters a live event loop forwarding mutations from the sync loop.
//
// The phased snapshot sends tickets in this order for fast interactivity:
//
//  1. Closed deps of open tickets — ensures AllBlockersClosed() is
//     correct from the moment the viewer receives open tickets.
//  2. Open/in_progress tickets — the actionable set.
//  3. open_complete frame — viewer becomes interactive.
//  4. Remaining closed tickets — most recently closed first.
//  5. caught_up frame — full index available, live events follow.
//
// The write lock covers subscriber registration and snapshot
// collection (steps 1-2 of setup). All network I/O happens outside
// the lock. Events that arrive during the snapshot write are buffered
// in the subscriber channel and processed after caught_up.
func (ts *TicketService) handleSubscribe(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
	encoder := codec.NewEncoder(conn)

	// Check grant.
	if err := requireGrant(token, ticket.ActionSubscribe); err != nil {
		encoder.Encode(subscribeFrame{Type: "error", Message: err.Error()})
		return
	}

	// Decode request.
	var request subscribeRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		encoder.Encode(subscribeFrame{Type: "error", Message: "invalid request: " + err.Error()})
		return
	}

	// Validate room and register subscriber under write lock.
	ts.mu.Lock()
	roomID, _, err := ts.requireRoom(request.Room)
	if err != nil {
		ts.mu.Unlock()
		encoder.Encode(subscribeFrame{Type: "error", Message: err.Error()})
		return
	}

	done := make(chan struct{})
	sub := &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, subscriberChannelSize),
		done:    done,
	}
	ts.addSubscriber(sub)

	// Collect snapshot while still holding the lock. This ensures
	// atomicity between subscriber registration (events start buffering)
	// and state collection (no events can be missed).
	state := ts.rooms[roomID]
	snapshot := collectSnapshot(state)
	ts.mu.Unlock()

	ts.logger.Info("subscribe stream started",
		"room_id", roomID,
		"open_tickets", len(snapshot.openTickets),
		"closed_deps", len(snapshot.closedDeps),
		"remaining_closed", len(snapshot.remainingClosed),
	)

	// Ensure cleanup on exit.
	defer func() {
		close(done)
		ts.mu.Lock()
		ts.removeSubscriber(sub)
		ts.mu.Unlock()
		ts.logger.Info("subscribe stream ended", "room_id", roomID)
	}()

	// Write phased snapshot outside the lock.
	if err := writeSnapshot(encoder, snapshot); err != nil {
		ts.logger.Debug("subscribe stream write error during snapshot",
			"room_id", roomID, "error", err)
		return
	}

	// Enter the live event loop.
	ts.subscribeEventLoop(ctx, encoder, sub, roomID)
}

// --- Snapshot collection ---

// roomSnapshot holds the partitioned ticket data for a phased snapshot
// write. Collected under a lock, written outside it.
type roomSnapshot struct {
	closedDeps      []ticketindex.Entry
	openTickets     []ticketindex.Entry
	remainingClosed []ticketindex.Entry
	stats           ticketindex.Stats
}

// collectSnapshot partitions a room's index into the three phases
// needed by the subscribe protocol. Must be called with ts.mu held
// (either read or write lock).
func collectSnapshot(state *roomState) roomSnapshot {
	allTickets := state.index.List(ticketindex.Filter{})
	stats := state.index.Stats()

	// Partition into open and closed tickets.
	var openTickets []ticketindex.Entry
	closedByID := make(map[string]ticketindex.Entry)

	for _, entry := range allTickets {
		if entry.Content.Status == ticket.StatusClosed {
			closedByID[entry.ID] = entry
		} else {
			openTickets = append(openTickets, entry)
		}
	}

	// Find closed deps of open tickets: direct BlockedBy references
	// that are closed. These must be sent before open tickets so that
	// AllBlockersClosed() computes correctly from the start.
	closedDepIDs := make(map[string]struct{})
	for _, entry := range openTickets {
		for _, blockerID := range entry.Content.BlockedBy {
			if _, isClosed := closedByID[blockerID]; isClosed {
				closedDepIDs[blockerID] = struct{}{}
			}
		}
	}

	var closedDeps []ticketindex.Entry
	var remainingClosed []ticketindex.Entry
	for id, entry := range closedByID {
		if _, isDep := closedDepIDs[id]; isDep {
			closedDeps = append(closedDeps, entry)
		} else {
			remainingClosed = append(remainingClosed, entry)
		}
	}

	// Sort remaining closed tickets by ClosedAt descending (most
	// recently closed first). This gives the viewer the most relevant
	// historical context first during the backfill phase.
	sort.Slice(remainingClosed, func(i, j int) bool {
		return remainingClosed[i].Content.ClosedAt > remainingClosed[j].Content.ClosedAt
	})

	return roomSnapshot{
		closedDeps:      closedDeps,
		openTickets:     openTickets,
		remainingClosed: remainingClosed,
		stats:           stats,
	}
}

// writeSnapshot writes a complete phased snapshot to the connection.
// Returns the first write error encountered.
func writeSnapshot(encoder *codec.Encoder, snapshot roomSnapshot) error {
	// Phase 1: Closed deps of open tickets.
	for _, entry := range snapshot.closedDeps {
		if err := encoder.Encode(subscribeFrame{
			Type:     "put",
			TicketID: entry.ID,
			Content:  entry.Content,
		}); err != nil {
			return err
		}
	}

	// Phase 2: Open/in_progress tickets.
	for _, entry := range snapshot.openTickets {
		if err := encoder.Encode(subscribeFrame{
			Type:     "put",
			TicketID: entry.ID,
			Content:  entry.Content,
		}); err != nil {
			return err
		}
	}

	// Phase 3: open_complete marker with stats.
	if err := encoder.Encode(subscribeFrame{
		Type:  "open_complete",
		Stats: &snapshot.stats,
	}); err != nil {
		return err
	}

	// Phase 4: Remaining closed tickets (most recently closed first).
	for _, entry := range snapshot.remainingClosed {
		if err := encoder.Encode(subscribeFrame{
			Type:     "put",
			TicketID: entry.ID,
			Content:  entry.Content,
		}); err != nil {
			return err
		}
	}

	// Phase 5: caught_up marker with stats.
	return encoder.Encode(subscribeFrame{
		Type:  "caught_up",
		Stats: &snapshot.stats,
	})
}

// --- Live event loop ---

// subscribeEventLoop reads events from the subscriber channel and
// writes them as CBOR frames to the connection. Runs until the
// context is cancelled (server shutdown), the connection fails, or
// the room is removed.
//
// On channel overflow (resync flag set), the loop drains buffered
// events, writes a resync frame, collects a fresh snapshot, and
// writes it before resuming live event forwarding.
func (ts *TicketService) subscribeEventLoop(ctx context.Context, encoder *codec.Encoder, sub *subscriber, roomID ref.RoomID) {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case event := <-sub.channel:
			// Check resync flag before processing the event. If set,
			// all buffered events are stale (some were dropped). The
			// event we just received was applied to the index already,
			// so the fresh snapshot we collect will include its effect.
			if sub.resync.CompareAndSwap(true, false) {
				// Drain remaining stale events.
				for len(sub.channel) > 0 {
					<-sub.channel
				}

				if err := encoder.Encode(subscribeFrame{Type: "resync"}); err != nil {
					ts.logger.Debug("subscribe stream write error",
						"room_id", roomID, "error", err)
					return
				}

				ts.mu.RLock()
				state, exists := ts.rooms[roomID]
				if !exists {
					ts.mu.RUnlock()
					encoder.Encode(subscribeFrame{
						Type:    "error",
						Message: "room no longer tracked",
					})
					return
				}
				snapshot := collectSnapshot(state)
				ts.mu.RUnlock()

				if err := writeSnapshot(encoder, snapshot); err != nil {
					ts.logger.Debug("subscribe stream write error during resync",
						"room_id", roomID, "error", err)
					return
				}
				continue
			}

			// Normal event forwarding.
			if err := encoder.Encode(subscribeFrame{
				Type:     event.Kind,
				TicketID: event.TicketID,
				Content:  event.Content,
			}); err != nil {
				ts.logger.Debug("subscribe stream write error",
					"room_id", roomID, "error", err)
				return
			}

		case <-heartbeat.C:
			// Verify the room still exists. If it was removed (tombstone,
			// ticket management disabled, or service left), notify the
			// client and close the stream.
			ts.mu.RLock()
			_, roomExists := ts.rooms[roomID]
			ts.mu.RUnlock()
			if !roomExists {
				encoder.Encode(subscribeFrame{
					Type:    "error",
					Message: "room no longer tracked",
				})
				return
			}

			if err := encoder.Encode(subscribeFrame{Type: "heartbeat"}); err != nil {
				ts.logger.Debug("subscribe stream heartbeat error",
					"room_id", roomID, "error", err)
				return
			}
		}
	}
}
