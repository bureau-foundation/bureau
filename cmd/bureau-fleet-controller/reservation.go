// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// machineReservation tracks per-machine reservation state: the active
// reservation (if any) and a priority-ordered queue of pending
// requests. Only one active reservation per machine — the fleet
// controller publishes a single m.bureau.reservation state event per
// ops room.
type machineReservation struct {
	// active is the currently granted reservation, or nil if the
	// machine has no active reservation.
	active *activeReservation

	// queue holds pending relay tickets ordered by priority
	// (descending) then creation time (ascending). The head of
	// the queue is granted when the machine becomes available.
	queue []queuedReservation
}

// activeReservation tracks a granted reservation on a machine.
type activeReservation struct {
	relayTicketID string
	opsRoomID     ref.RoomID
	holder        ref.Entity
	mode          schema.ReservationMode
	grantedAt     time.Time
	expiresAt     time.Time
	priority      int
}

// queuedReservation is a pending relay ticket waiting for a machine.
type queuedReservation struct {
	relayTicketID string
	opsRoomID     ref.RoomID
	holder        ref.Entity
	mode          schema.ReservationMode
	priority      int
	maxDuration   time.Duration
	createdAt     time.Time
}

// --- Ops room classification ---

// classifyOpsRoom checks if a ticket event's content identifies its
// room as a machine ops room. If the ticket is a resource_request
// with a machine resource, registers the room as that machine's ops
// room and returns the machine localpart. Returns ("", false) if
// the room cannot be classified from this event.
//
// Caller must hold fc.mu.
func (fc *FleetController) classifyOpsRoom(roomID ref.RoomID, content *ticket.TicketContent) (string, bool) {
	if content.Type != ticket.TypeResourceRequest {
		return "", false
	}
	if content.Reservation == nil || len(content.Reservation.Claims) == 0 {
		return "", false
	}

	// Find the first machine claim to determine the ops room's machine.
	for _, claim := range content.Reservation.Claims {
		if claim.Resource.Type != schema.ResourceMachine {
			continue
		}

		// Build the full machine localpart from the short target name.
		machine, err := ref.NewMachine(fc.fleet, claim.Resource.Target)
		if err != nil {
			fc.logger.Warn("cannot build machine ref from resource target",
				"target", claim.Resource.Target,
				"error", err,
			)
			continue
		}

		localpart := machine.Localpart()
		if _, exists := fc.opsRoomMachines[roomID]; !exists {
			fc.opsRooms[localpart] = roomID
			fc.opsRoomMachines[roomID] = localpart
			fc.logger.Info("ops room classified from ticket",
				"machine", localpart,
				"room_id", roomID,
			)
		}
		return localpart, true
	}

	return "", false
}

// --- Relay link tracking ---

// processRelayLinkEvent stores relay link data from an ops room.
// The relay link connects a relay ticket to its origin workspace
// ticket and identifies the requester. The fleet controller uses
// the requester as the reservation holder.
//
// Caller must hold fc.mu.
func (fc *FleetController) processRelayLinkEvent(roomID ref.RoomID, event messaging.Event) {
	if len(event.Content) == 0 {
		return
	}

	content, err := parseEventContent[schema.RelayLink](event)
	if err != nil {
		fc.logger.Warn("failed to parse relay link event",
			"room_id", roomID,
			"state_key", *event.StateKey,
			"error", err,
		)
		return
	}

	ticketID := *event.StateKey
	key := opsTicketKey{roomID: roomID, ticketID: ticketID}
	fc.relayLinks[key] = *content
}

// opsTicketKey uniquely identifies a relay ticket within an ops room.
type opsTicketKey struct {
	roomID   ref.RoomID
	ticketID string
}

// --- Ticket event processing ---

// processOpsRoomTicketEvent handles a ticket state event. If the
// event is in a known ops room (or classifiable as one), it manages
// the reservation queue accordingly.
//
// Caller must hold fc.mu.
func (fc *FleetController) processOpsRoomTicketEvent(ctx context.Context, roomID ref.RoomID, event messaging.Event) {
	ticketID := *event.StateKey

	if len(event.Content) == 0 {
		// Empty content means the ticket state was cleared.
		machineLocalpart, isOpsRoom := fc.opsRoomMachines[roomID]
		if isOpsRoom {
			fc.handleRelayTicketRemoved(ctx, machineLocalpart, roomID, ticketID)
		}
		return
	}

	content, err := parseEventContent[ticket.TicketContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse ticket event",
			"room_id", roomID,
			"ticket_id", ticketID,
			"error", err,
		)
		return
	}

	// Only handle resource_request tickets.
	if content.Type != ticket.TypeResourceRequest {
		return
	}

	// Try to classify this room as an ops room if not already known.
	machineLocalpart, isOpsRoom := fc.opsRoomMachines[roomID]
	if !isOpsRoom {
		machineLocalpart, isOpsRoom = fc.classifyOpsRoom(roomID, content)
		if !isOpsRoom {
			return
		}
	}

	switch content.Status {
	case ticket.StatusOpen:
		fc.enqueueReservation(machineLocalpart, roomID, ticketID, content, event.OriginServerTS)
	case ticket.StatusClosed:
		fc.handleRelayTicketClosed(ctx, machineLocalpart, roomID, ticketID, content.CloseReason)
	case ticket.StatusInProgress:
		// This is either our own echo (we set it to in_progress) or
		// another service accepted the ticket. Either way, no action
		// needed — the grant is tracked via the active reservation.
	}
}

// enqueueReservation adds a relay ticket to the machine's reservation
// queue if not already queued or active.
//
// Caller must hold fc.mu.
func (fc *FleetController) enqueueReservation(machineLocalpart string, opsRoomID ref.RoomID, ticketID string, content *ticket.TicketContent, originServerTS int64) {
	reservation := fc.ensureReservation(machineLocalpart)

	// Skip if this ticket is already the active reservation.
	if reservation.active != nil && reservation.active.relayTicketID == ticketID {
		return
	}

	// Skip if already queued.
	for _, queued := range reservation.queue {
		if queued.relayTicketID == ticketID {
			return
		}
	}

	if content.Reservation == nil || len(content.Reservation.Claims) == 0 {
		fc.logger.Warn("resource_request ticket has no claims",
			"machine", machineLocalpart,
			"ticket_id", ticketID,
		)
		return
	}

	maxDuration, err := time.ParseDuration(content.Reservation.MaxDuration)
	if err != nil {
		fc.logger.Warn("invalid max_duration on relay ticket",
			"machine", machineLocalpart,
			"ticket_id", ticketID,
			"max_duration", content.Reservation.MaxDuration,
			"error", err,
		)
		return
	}

	// Extract the holder from the relay link (preferred) or fall back
	// to the ticket assignee. The relay link's requester is the agent
	// that created the original workspace ticket.
	holder := ref.Entity{}
	key := opsTicketKey{roomID: opsRoomID, ticketID: ticketID}
	if relayLink, exists := fc.relayLinks[key]; exists {
		holder = relayLink.Requester
	}

	mode := content.Reservation.Claims[0].Mode

	// Use the Matrix event timestamp for queue ordering. This is more
	// reliable than the fleet controller's clock because it reflects
	// when the ticket was actually created on the homeserver.
	createdAt := time.UnixMilli(originServerTS)

	entry := queuedReservation{
		relayTicketID: ticketID,
		opsRoomID:     opsRoomID,
		holder:        holder,
		mode:          mode,
		priority:      content.Priority,
		maxDuration:   maxDuration,
		createdAt:     createdAt,
	}

	reservation.queue = append(reservation.queue, entry)
	sortReservationQueue(reservation.queue)

	fc.logger.Info("reservation enqueued",
		"machine", machineLocalpart,
		"ticket_id", ticketID,
		"priority", content.Priority,
		"mode", mode,
		"queue_depth", len(reservation.queue),
	)
}

// handleRelayTicketClosed handles a relay ticket closure in an ops
// room. If the closed ticket is the active reservation, the
// reservation is released and the next queued ticket is advanced.
// If the ticket is in the queue, it is removed.
//
// Caller must hold fc.mu.
func (fc *FleetController) handleRelayTicketClosed(ctx context.Context, machineLocalpart string, opsRoomID ref.RoomID, ticketID string, closeReason string) {
	reservation, exists := fc.reservations[machineLocalpart]
	if !exists {
		return
	}

	// Check if this is the active reservation.
	if reservation.active != nil && reservation.active.relayTicketID == ticketID {
		fc.releaseReservation(ctx, machineLocalpart, reservation, "relay ticket closed: "+closeReason)
		return
	}

	// Remove from queue if present.
	for i, queued := range reservation.queue {
		if queued.relayTicketID == ticketID {
			reservation.queue = append(reservation.queue[:i], reservation.queue[i+1:]...)
			fc.logger.Info("queued reservation removed",
				"machine", machineLocalpart,
				"ticket_id", ticketID,
				"reason", closeReason,
			)
			return
		}
	}
}

// handleRelayTicketRemoved handles a relay ticket state event being
// cleared (empty content). Treated the same as closure.
//
// Caller must hold fc.mu.
func (fc *FleetController) handleRelayTicketRemoved(ctx context.Context, machineLocalpart string, opsRoomID ref.RoomID, ticketID string) {
	fc.handleRelayTicketClosed(ctx, machineLocalpart, opsRoomID, ticketID, "state cleared")
}

// --- Grant logic ---

// advanceReservationQueues tries to grant the next queued reservation
// for each machine that has no active reservation. Also checks for
// preemption: if a queued ticket has higher priority than the active
// reservation, the active reservation is preempted.
//
// Caller must hold fc.mu.
func (fc *FleetController) advanceReservationQueues(ctx context.Context) {
	for machineLocalpart, reservation := range fc.reservations {
		if len(reservation.queue) == 0 {
			continue
		}

		if reservation.active == nil {
			// No active reservation — grant the queue head.
			fc.grantReservation(ctx, machineLocalpart, reservation)
			continue
		}

		// Preemption check: if the queue head has strictly higher
		// priority (lower number) than the active reservation, preempt.
		queueHead := reservation.queue[0]
		if queueHead.priority < reservation.active.priority {
			fc.preemptReservation(ctx, machineLocalpart, reservation)
		}
	}
}

// grantReservation grants the queue head reservation for a machine.
// Sets the relay ticket to in_progress, publishes
// m.bureau.machine_drain (for exclusive mode), and publishes
// m.bureau.reservation with the grant details.
//
// Caller must hold fc.mu.
func (fc *FleetController) grantReservation(ctx context.Context, machineLocalpart string, reservation *machineReservation) {
	if len(reservation.queue) == 0 {
		return
	}

	queued := reservation.queue[0]
	reservation.queue = reservation.queue[1:]

	now := fc.clock.Now()
	expiresAt := now.Add(queued.maxDuration)

	// Set the relay ticket to in_progress. This signals to the ticket
	// service that the resource owner has accepted the request.
	if err := fc.setRelayTicketInProgress(ctx, queued.opsRoomID, queued.relayTicketID); err != nil {
		fc.logger.Error("failed to set relay ticket in_progress",
			"machine", machineLocalpart,
			"ticket_id", queued.relayTicketID,
			"error", err,
		)
		return
	}

	// For exclusive mode, publish a drain event to stop services on
	// the machine from accepting new work.
	if queued.mode == schema.ModeExclusive {
		if err := fc.publishMachineDrain(ctx, machineLocalpart, queued); err != nil {
			fc.logger.Error("failed to publish machine drain",
				"machine", machineLocalpart,
				"ticket_id", queued.relayTicketID,
				"error", err,
			)
			// Continue granting despite drain failure — the drain
			// protocol is a best-effort optimization. The reservation
			// grant tells the ticket service the resource is ready.
		}
	}

	// Publish the reservation grant.
	grant := schema.ReservationGrant{
		Holder:      queued.holder,
		Resource:    fc.machineResourceRef(machineLocalpart),
		Mode:        queued.mode,
		GrantedAt:   now.UTC().Format(time.RFC3339),
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
		RelayTicket: queued.relayTicketID,
	}

	if err := fc.publishReservationGrant(ctx, queued.opsRoomID, queued.holder, grant); err != nil {
		fc.logger.Error("failed to publish reservation grant",
			"machine", machineLocalpart,
			"ticket_id", queued.relayTicketID,
			"error", err,
		)
		return
	}

	reservation.active = &activeReservation{
		relayTicketID: queued.relayTicketID,
		opsRoomID:     queued.opsRoomID,
		holder:        queued.holder,
		mode:          queued.mode,
		grantedAt:     now,
		expiresAt:     expiresAt,
		priority:      queued.priority,
	}

	fc.logger.Info("reservation granted",
		"machine", machineLocalpart,
		"ticket_id", queued.relayTicketID,
		"holder", queued.holder,
		"mode", queued.mode,
		"expires_at", expiresAt.UTC().Format(time.RFC3339),
	)
}

// preemptReservation preempts the active reservation in favor of the
// queue head. Closes the active relay ticket with reason "Preempted
// by higher priority request", clears the reservation grant, then
// grants the queue head.
//
// Caller must hold fc.mu.
func (fc *FleetController) preemptReservation(ctx context.Context, machineLocalpart string, reservation *machineReservation) {
	if reservation.active == nil {
		return
	}

	preemptedTicketID := reservation.active.relayTicketID

	// Close the active relay ticket.
	if err := fc.closeRelayTicket(ctx, reservation.active.opsRoomID, reservation.active.relayTicketID, "Preempted by higher priority request"); err != nil {
		fc.logger.Error("failed to close preempted relay ticket",
			"machine", machineLocalpart,
			"ticket_id", reservation.active.relayTicketID,
			"error", err,
		)
		return
	}

	// Clear the reservation grant and drain events.
	fc.clearReservation(ctx, machineLocalpart, reservation)

	fc.logger.Info("reservation preempted",
		"machine", machineLocalpart,
		"preempted_ticket", preemptedTicketID,
		"preempting_ticket", reservation.queue[0].relayTicketID,
	)

	// Grant the queue head.
	fc.grantReservation(ctx, machineLocalpart, reservation)
}

// releaseReservation clears the active reservation for a machine and
// attempts to grant the next queued ticket.
//
// Caller must hold fc.mu.
func (fc *FleetController) releaseReservation(ctx context.Context, machineLocalpart string, reservation *machineReservation, reason string) {
	if reservation.active == nil {
		return
	}

	releasedTicketID := reservation.active.relayTicketID
	fc.clearReservation(ctx, machineLocalpart, reservation)

	fc.logger.Info("reservation released",
		"machine", machineLocalpart,
		"ticket_id", releasedTicketID,
		"reason", reason,
	)

	// Try to grant the next queued ticket.
	if len(reservation.queue) > 0 {
		fc.grantReservation(ctx, machineLocalpart, reservation)
	}
}

// clearReservation clears the m.bureau.reservation and
// m.bureau.machine_drain state events in the ops room, and sets
// reservation.active to nil.
//
// Caller must hold fc.mu.
func (fc *FleetController) clearReservation(ctx context.Context, machineLocalpart string, reservation *machineReservation) {
	if reservation.active == nil {
		return
	}

	opsRoomID := reservation.active.opsRoomID
	holder := reservation.active.holder

	// Clear the reservation grant (empty content).
	if _, err := fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeReservation, holder.Localpart(), json.RawMessage("{}")); err != nil {
		fc.logger.Error("failed to clear reservation grant",
			"machine", machineLocalpart,
			"room_id", opsRoomID,
			"error", err,
		)
	}

	// Clear the machine drain (empty content) if we published one.
	if reservation.active.mode == schema.ModeExclusive {
		if _, err := fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeMachineDrain, "", json.RawMessage("{}")); err != nil {
			fc.logger.Error("failed to clear machine drain",
				"machine", machineLocalpart,
				"room_id", opsRoomID,
				"error", err,
			)
		}
	}

	reservation.active = nil
}

// --- Duration watchdog ---

// checkReservationExpiry checks all active reservations for duration
// expiry. Expired reservations have their relay ticket closed and
// the reservation cleared.
//
// Caller must hold fc.mu.
func (fc *FleetController) checkReservationExpiry(ctx context.Context) {
	now := fc.clock.Now()

	for machineLocalpart, reservation := range fc.reservations {
		if reservation.active == nil {
			continue
		}

		if now.Before(reservation.active.expiresAt) {
			continue
		}

		fc.logger.Warn("reservation expired",
			"machine", machineLocalpart,
			"ticket_id", reservation.active.relayTicketID,
			"expired_at", reservation.active.expiresAt.UTC().Format(time.RFC3339),
		)

		// Close the relay ticket with expiry reason.
		if err := fc.closeRelayTicket(ctx, reservation.active.opsRoomID, reservation.active.relayTicketID, "Duration exceeded"); err != nil {
			fc.logger.Error("failed to close expired relay ticket",
				"machine", machineLocalpart,
				"ticket_id", reservation.active.relayTicketID,
				"error", err,
			)
		}

		fc.releaseReservation(ctx, machineLocalpart, reservation, "duration exceeded")
	}
}

// --- Matrix operations ---

// setRelayTicketInProgress reads the current relay ticket content,
// sets status to in_progress, and writes it back.
func (fc *FleetController) setRelayTicketInProgress(ctx context.Context, opsRoomID ref.RoomID, ticketID string) error {
	content, err := fc.readTicketContent(ctx, opsRoomID, ticketID)
	if err != nil {
		return fmt.Errorf("reading relay ticket %s: %w", ticketID, err)
	}

	content.Status = ticket.StatusInProgress
	_, err = fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeTicket, ticketID, content)
	if err != nil {
		return fmt.Errorf("setting relay ticket %s to in_progress: %w", ticketID, err)
	}
	return nil
}

// closeRelayTicket reads the current relay ticket content, sets
// status to closed with the given reason, and writes it back.
func (fc *FleetController) closeRelayTicket(ctx context.Context, opsRoomID ref.RoomID, ticketID string, reason string) error {
	content, err := fc.readTicketContent(ctx, opsRoomID, ticketID)
	if err != nil {
		return fmt.Errorf("reading relay ticket %s: %w", ticketID, err)
	}

	content.Status = ticket.StatusClosed
	content.CloseReason = reason
	_, err = fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeTicket, ticketID, content)
	if err != nil {
		return fmt.Errorf("closing relay ticket %s: %w", ticketID, err)
	}
	return nil
}

// readTicketContent reads a ticket state event from a room and
// parses it as TicketContent.
func (fc *FleetController) readTicketContent(ctx context.Context, roomID ref.RoomID, ticketID string) (*ticket.TicketContent, error) {
	raw, err := fc.configStore.GetStateEvent(ctx, roomID, schema.EventTypeTicket, ticketID)
	if err != nil {
		return nil, err
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("parsing ticket content: %w", err)
	}
	return &content, nil
}

// publishReservationGrant publishes an m.bureau.reservation state
// event in the ops room. The state key is the holder's localpart.
func (fc *FleetController) publishReservationGrant(ctx context.Context, opsRoomID ref.RoomID, holder ref.Entity, grant schema.ReservationGrant) error {
	_, err := fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeReservation, holder.Localpart(), grant)
	if err != nil {
		return fmt.Errorf("publishing reservation grant: %w", err)
	}
	return nil
}

// publishMachineDrain publishes an m.bureau.machine_drain state
// event in the ops room.
func (fc *FleetController) publishMachineDrain(ctx context.Context, machineLocalpart string, queued queuedReservation) error {
	opsRoomID, exists := fc.opsRooms[machineLocalpart]
	if !exists {
		return fmt.Errorf("no ops room for machine %s", machineLocalpart)
	}

	drain := schema.MachineDrainContent{
		ReservationHolder: queued.holder,
		RequestedAt:       fc.clock.Now().UTC().Format(time.RFC3339),
	}

	_, err := fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeMachineDrain, "", drain)
	return err
}

// machineResourceRef constructs a ResourceRef for a machine from its
// localpart. Extracts the short machine name from the full fleet-
// scoped localpart.
func (fc *FleetController) machineResourceRef(machineLocalpart string) schema.ResourceRef {
	machine, err := ref.ParseMachine(machineLocalpart, fc.serverName)
	if err != nil {
		// The localpart was already validated when the machine was
		// added to the fleet model, so this should not happen.
		return schema.ResourceRef{Type: schema.ResourceMachine, Target: machineLocalpart}
	}
	return schema.ResourceRef{Type: schema.ResourceMachine, Target: machine.Name()}
}

// --- Queue helpers ---

// ensureReservation returns the machineReservation for a machine,
// creating one if it doesn't exist.
//
// Caller must hold fc.mu.
func (fc *FleetController) ensureReservation(machineLocalpart string) *machineReservation {
	reservation, exists := fc.reservations[machineLocalpart]
	if !exists {
		reservation = &machineReservation{}
		fc.reservations[machineLocalpart] = reservation
	}
	return reservation
}

// sortReservationQueue sorts the queue by priority ascending (P0
// before P4) then by creation time ascending (earlier requests
// first).
func sortReservationQueue(queue []queuedReservation) {
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].priority != queue[j].priority {
			return queue[i].priority < queue[j].priority
		}
		return queue[i].createdAt.Before(queue[j].createdAt)
	})
}
