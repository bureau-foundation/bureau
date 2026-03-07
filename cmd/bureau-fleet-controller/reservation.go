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
// reservation (if any), a priority-ordered queue of pending requests,
// and an optional pending drain waiting for service acknowledgment.
// Only one active reservation per machine — the fleet controller
// publishes a single m.bureau.reservation state event per ops room.
type machineReservation struct {
	// active is the currently granted reservation, or nil if the
	// machine has no active reservation.
	active *activeReservation

	// pending is non-nil when a drain has been published and the
	// fleet controller is waiting for services to acknowledge
	// before granting the reservation. Set by startDrain, cleared
	// by completeGrant or cancelPendingDrain.
	pending *pendingDrain

	// queue holds pending relay tickets ordered by priority
	// (descending) then creation time (ascending). The head of
	// the queue is granted when the machine becomes available.
	queue []queuedReservation
}

// pendingDrain tracks a drain-wait state: the fleet controller has
// published m.bureau.machine_drain and is waiting for services to
// report m.bureau.drain_status with Acknowledged before granting.
type pendingDrain struct {
	queued    queuedReservation
	services  map[string]bool // service localpart → drained (Acknowledged)
	startedAt time.Time
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

// --- Drain status processing ---

// processDrainStatusEvent handles a drain status state event from an
// ops room. If the reporting service is in the pending drain's service
// list and reports Acknowledged, it is marked as
// drained. The actual grant completion happens in checkDrainCompletion.
//
// Caller must hold fc.mu.
func (fc *FleetController) processDrainStatusEvent(roomID ref.RoomID, event messaging.Event) {
	if event.StateKey == nil {
		return
	}
	serviceLocalpart := *event.StateKey

	machineLocalpart, isOpsRoom := fc.opsRoomMachines[roomID]
	if !isOpsRoom {
		return
	}

	reservation, exists := fc.reservations[machineLocalpart]
	if !exists || reservation.pending == nil {
		return
	}

	if len(event.Content) == 0 {
		return
	}

	content, err := parseEventContent[schema.DrainStatusContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse drain status event",
			"room_id", roomID,
			"state_key", serviceLocalpart,
			"error", err,
		)
		return
	}

	// Check if this service is in the pending drain's tracked services.
	if _, tracked := reservation.pending.services[serviceLocalpart]; !tracked {
		// Not a tracked service. The daemon also publishes drain_status
		// using its own localpart which may not be in the fleet service
		// list. Accept it as an additional acknowledgment but don't
		// block on it.
		fc.logger.Info("drain status from untracked service (accepted but not blocking)",
			"machine", machineLocalpart,
			"service", serviceLocalpart,
			"acknowledged", content.Acknowledged,
			"in_flight", content.InFlight,
		)
		return
	}

	if content.Acknowledged {
		reservation.pending.services[serviceLocalpart] = true
		fc.logger.Info("service drained",
			"machine", machineLocalpart,
			"service", serviceLocalpart,
			"in_flight", content.InFlight,
			"drained_at", content.DrainedAt,
		)
	} else {
		fc.logger.Info("drain status update (not yet acknowledged)",
			"machine", machineLocalpart,
			"service", serviceLocalpart,
			"acknowledged", content.Acknowledged,
			"in_flight", content.InFlight,
		)
	}
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
// If the ticket is the pending drain, the drain is cancelled.
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

	// Check if this is the pending drain's ticket.
	if reservation.pending != nil && reservation.pending.queued.relayTicketID == ticketID {
		fc.logger.Info("pending drain ticket closed externally",
			"machine", machineLocalpart,
			"ticket_id", ticketID,
			"reason", closeReason,
		)
		// Clear drain without closing the ticket again (it's already closed).
		opsRoom, exists := fc.opsRooms[machineLocalpart]
		if exists {
			if _, err := fc.configStore.SendStateEvent(ctx, opsRoom, schema.EventTypeMachineDrain, "", json.RawMessage("{}")); err != nil {
				fc.logger.Error("failed to clear drain after ticket closure",
					"machine", machineLocalpart,
					"error", err,
				)
			}
		}
		reservation.pending = nil
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
// for each machine that has no active reservation and no pending drain.
// Also checks for preemption: if a queued ticket has higher priority
// than the active reservation, the active reservation is preempted.
// If a pending drain is in progress and a higher-priority request
// arrives, the pending drain is cancelled and restarted for the
// higher-priority request.
//
// Caller must hold fc.mu.
func (fc *FleetController) advanceReservationQueues(ctx context.Context) {
	for machineLocalpart, reservation := range fc.reservations {
		if len(reservation.queue) == 0 {
			continue
		}

		// Check for preemption of a pending drain: if the queue head
		// has strictly higher priority than the pending request,
		// cancel the pending drain and start a new one.
		if reservation.pending != nil {
			queueHead := reservation.queue[0]
			if queueHead.priority < reservation.pending.queued.priority {
				fc.cancelPendingDrain(ctx, machineLocalpart, reservation, "preempted by higher priority request")
				// Fall through to start drain for the new queue head.
			} else {
				// Drain in progress, queue head is same or lower priority.
				continue
			}
		}

		if reservation.active == nil && reservation.pending == nil {
			// No active reservation and no pending drain — start
			// granting the queue head.
			fc.startDrain(ctx, machineLocalpart, reservation)
			continue
		}

		if reservation.active != nil {
			// Preemption check: if the queue head has strictly higher
			// priority (lower number) than the active reservation, preempt.
			queueHead := reservation.queue[0]
			if queueHead.priority < reservation.active.priority {
				fc.preemptReservation(ctx, machineLocalpart, reservation)
			}
		}
	}
}

// startDrain dequeues the queue head and begins the grant process. For
// exclusive reservations with fleet-managed services on the machine,
// publishes a drain event and enters the pending drain state, deferring
// the actual grant until services acknowledge. For inclusive mode or
// machines with no fleet-managed services, completes the grant
// immediately.
//
// Caller must hold fc.mu.
func (fc *FleetController) startDrain(ctx context.Context, machineLocalpart string, reservation *machineReservation) {
	if len(reservation.queue) == 0 {
		return
	}

	queued := reservation.queue[0]
	reservation.queue = reservation.queue[1:]

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

	// Inclusive mode or no drain grace period: grant immediately.
	if queued.mode != schema.ModeExclusive || fc.drainGracePeriod == 0 {
		fc.completeGrant(ctx, machineLocalpart, reservation, queued)
		return
	}

	// Capture the drain start time BEFORE the network call to publish
	// the drain event. publishMachineDrain makes a synchronous HTTP
	// PUT; external observers (tests, other services) can see the
	// drain event as soon as that call returns. If we captured the
	// time after the publish, there would be a window where an
	// observer could advance a fake clock between the publish and
	// the startedAt capture, making the timeout check permanently
	// unsatisfiable.
	drainStartedAt := fc.clock.Now()

	// Exclusive mode: publish drain and wait for services.
	drainServices := fc.fleetManagedServicesOnMachine(machineLocalpart)
	if err := fc.publishMachineDrain(ctx, machineLocalpart, queued, drainStartedAt); err != nil {
		fc.logger.Error("failed to publish machine drain",
			"machine", machineLocalpart,
			"ticket_id", queued.relayTicketID,
			"error", err,
		)
		// Grant immediately on drain publish failure — the drain
		// protocol must not block grants when the homeserver is
		// unreachable.
		fc.completeGrant(ctx, machineLocalpart, reservation, queued)
		return
	}

	// If there are no fleet-managed services on the machine, the
	// only drain responder is the machine daemon. We still wait
	// for acknowledgment (the daemon reports sandbox count) but
	// the drain is expected to complete quickly.
	servicesDrained := make(map[string]bool, len(drainServices))
	for _, localpart := range drainServices {
		servicesDrained[localpart] = false
	}

	reservation.pending = &pendingDrain{
		queued:    queued,
		services:  servicesDrained,
		startedAt: drainStartedAt,
	}

	fc.logger.Info("drain published, waiting for services",
		"machine", machineLocalpart,
		"ticket_id", queued.relayTicketID,
		"services", len(drainServices),
		"grace_period", fc.drainGracePeriod,
	)
}

// completeGrant finishes granting a reservation by publishing the
// reservation grant state event and setting the active reservation.
// Called directly for inclusive mode or after drain completion for
// exclusive mode.
//
// Caller must hold fc.mu.
func (fc *FleetController) completeGrant(ctx context.Context, machineLocalpart string, reservation *machineReservation, queued queuedReservation) {
	now := fc.clock.Now()
	expiresAt := now.Add(queued.maxDuration)

	// For exclusive mode, ensure drain event is published. If we got
	// here via startDrain the drain is already published; for
	// inclusive mode no drain is needed.
	if queued.mode == schema.ModeExclusive && reservation.pending == nil {
		// Direct call (not via drain-wait path) — publish drain for
		// signal consistency even though we're granting immediately.
		if err := fc.publishMachineDrain(ctx, machineLocalpart, queued, now); err != nil {
			fc.logger.Error("failed to publish machine drain during direct grant",
				"machine", machineLocalpart,
				"ticket_id", queued.relayTicketID,
				"error", err,
			)
		}
	}

	// Clear pending state if this completes a drain-wait.
	reservation.pending = nil

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

// checkDrainCompletion inspects all pending drains and completes the
// grant when all services have reported Acknowledged, or when the
// grace period has elapsed.
//
// For services that haven't been marked as drained yet, this function
// reads the current m.bureau.drain_status state from the ops room.
// This handles the race where drain_status events arrive in the same
// /sync batch that triggered the drain: processRoomSync processes
// drain_status events BEFORE advanceReservationQueues creates the
// pending drain, so those events are silently ignored. Reading the
// current state here ensures the FC doesn't wait for events that have
// already been published.
//
// Caller must hold fc.mu.
func (fc *FleetController) checkDrainCompletion(ctx context.Context) {
	now := fc.clock.Now()

	for machineLocalpart, reservation := range fc.reservations {
		if reservation.pending == nil {
			continue
		}

		pending := reservation.pending

		// For services not yet marked drained, read the current
		// drain_status state from the ops room. This catches
		// drain_status events that arrived in the same sync batch
		// as the ticket that started the drain (processed before
		// the pending drain existed).
		opsRoomID, hasOpsRoom := fc.opsRooms[machineLocalpart]
		if hasOpsRoom {
			for serviceLocalpart, drained := range pending.services {
				if drained {
					continue
				}
				raw, err := fc.configStore.GetStateEvent(ctx, opsRoomID,
					schema.EventTypeDrainStatus, serviceLocalpart)
				if err != nil {
					// No drain_status yet — expected for services
					// that haven't responded.
					continue
				}
				var status schema.DrainStatusContent
				if err := json.Unmarshal(raw, &status); err != nil {
					fc.logger.Warn("failed to parse drain_status state",
						"machine", machineLocalpart,
						"service", serviceLocalpart,
						"error", err,
					)
					continue
				}
				if status.Acknowledged {
					pending.services[serviceLocalpart] = true
					fc.logger.Info("service drained (from state read)",
						"machine", machineLocalpart,
						"service", serviceLocalpart,
						"in_flight", status.InFlight,
						"drained_at", status.DrainedAt,
					)
				}
			}
		}

		// Check if all tracked services have drained.
		allDrained := true
		for _, drained := range pending.services {
			if !drained {
				allDrained = false
				break
			}
		}

		if allDrained {
			fc.logger.Info("all services drained, completing grant",
				"machine", machineLocalpart,
				"ticket_id", pending.queued.relayTicketID,
				"services", len(pending.services),
			)
			fc.completeGrant(ctx, machineLocalpart, reservation, pending.queued)
			continue
		}

		// Check timeout.
		if now.Sub(pending.startedAt) >= fc.drainGracePeriod {
			// Collect which services did not acknowledge.
			var unacknowledged []string
			for service, drained := range pending.services {
				if !drained {
					unacknowledged = append(unacknowledged, service)
				}
			}
			fc.logger.Warn("drain grace period expired, granting without full acknowledgment",
				"machine", machineLocalpart,
				"ticket_id", pending.queued.relayTicketID,
				"unacknowledged_services", unacknowledged,
				"grace_period", fc.drainGracePeriod,
			)
			fc.completeGrant(ctx, machineLocalpart, reservation, pending.queued)
			continue
		}
	}
}

// cancelPendingDrain cancels a pending drain, closing the relay ticket
// associated with the pending request and clearing the drain state
// event.
//
// Caller must hold fc.mu.
func (fc *FleetController) cancelPendingDrain(ctx context.Context, machineLocalpart string, reservation *machineReservation, reason string) {
	if reservation.pending == nil {
		return
	}

	pending := reservation.pending

	// Close the relay ticket for the cancelled drain.
	if err := fc.closeRelayTicket(ctx, pending.queued.opsRoomID, pending.queued.relayTicketID, reason); err != nil {
		fc.logger.Error("failed to close cancelled drain relay ticket",
			"machine", machineLocalpart,
			"ticket_id", pending.queued.relayTicketID,
			"error", err,
		)
	}

	// Clear the drain event.
	opsRoomID, exists := fc.opsRooms[machineLocalpart]
	if exists {
		if _, err := fc.configStore.SendStateEvent(ctx, opsRoomID, schema.EventTypeMachineDrain, "", json.RawMessage("{}")); err != nil {
			fc.logger.Error("failed to clear machine drain during cancel",
				"machine", machineLocalpart,
				"room_id", opsRoomID,
				"error", err,
			)
		}
	}

	fc.logger.Info("pending drain cancelled",
		"machine", machineLocalpart,
		"ticket_id", pending.queued.relayTicketID,
		"reason", reason,
	)

	reservation.pending = nil
}

// fleetManagedServicesOnMachine returns the localparts of
// fleet-managed services that have instances placed on the given
// machine. Returns the full Matrix localpart for each service (from
// the PrincipalAssignment's Principal.Localpart()), which matches the
// state_key convention services use when publishing drain_status.
//
// Used to populate the pending drain's tracked services map so the FC
// can correlate drain_status events by state_key.
func (fc *FleetController) fleetManagedServicesOnMachine(machineLocalpart string) []string {
	var services []string
	for _, serviceState := range fc.services {
		if assignment, onMachine := serviceState.instances[machineLocalpart]; onMachine {
			services = append(services, assignment.Principal.Localpart())
		}
	}
	return services
}

// preemptReservation preempts the active reservation in favor of the
// queue head. Closes the active relay ticket with reason "Preempted
// by higher priority request", clears the reservation grant, then
// starts the drain-grant process for the queue head.
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

	// Start drain for the queue head.
	fc.startDrain(ctx, machineLocalpart, reservation)
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

	// Try to start drain/grant for the next queued ticket.
	if len(reservation.queue) > 0 {
		fc.startDrain(ctx, machineLocalpart, reservation)
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
// event in the ops room with the list of fleet-managed services on
// the machine. The requestedAt timestamp is provided by the caller
// so the drain event and the pending drain state share the same
// time reference (captured before the network call).
func (fc *FleetController) publishMachineDrain(ctx context.Context, machineLocalpart string, queued queuedReservation, requestedAt time.Time) error {
	opsRoomID, exists := fc.opsRooms[machineLocalpart]
	if !exists {
		return fmt.Errorf("no ops room for machine %s", machineLocalpart)
	}

	drain := schema.MachineDrainContent{
		Services:          fc.fleetManagedServicesOnMachine(machineLocalpart),
		ReservationHolder: queued.holder,
		RequestedAt:       requestedAt.UTC().Format(time.RFC3339),
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
