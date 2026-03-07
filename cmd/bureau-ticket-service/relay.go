// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/relayauth"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// relayEntry tracks the association between an origin ticket and its
// relay tickets in ops rooms. Created when the ticket service initiates
// relay for a ticket with a Reservation.
type relayEntry struct {
	// originRoom is the room containing the origin ticket.
	originRoom ref.RoomID

	// originTicket is the ID of the origin ticket.
	originTicket string

	// relayTickets maps ops room ID → relay ticket reference for
	// each claim that was successfully relayed.
	relayTickets map[ref.RoomID]relayTicketRef
}

// relayTicketRef identifies a relay ticket in an ops room and its
// corresponding claim in the origin ticket.
type relayTicketRef struct {
	// ticketID is the relay ticket's ID in the ops room.
	ticketID string

	// claimIndex is the index into the origin ticket's
	// Reservation.Claims slice that this relay ticket represents.
	claimIndex int

	// outboundFilter controls what information from the ops room
	// crosses back to the origin ticket. Nil means default
	// filtering (status + close_reason only). Derived from the
	// relay policy at relay time and re-derived on startup.
	outboundFilter *schema.RelayFilter
}

// isRelayReady reports whether a ticket with a Reservation is ready
// for relay: all existing gates (non-reservation) are satisfied, and
// the ticket has not already been relayed. Any ticket type can carry
// a Reservation — resource_request tickets for operator-initiated
// locks, pipeline tickets for resource-bound execution, etc. The
// relay mechanism adds state_event gates after relay; those gates
// don't exist yet and don't block this check.
func isRelayReady(content *ticket.TicketContent) bool {
	if content.Reservation == nil || len(content.Reservation.Claims) == 0 {
		return false
	}
	if content.Status != ticket.StatusOpen {
		return false
	}

	// All existing gates must be satisfied for relay to proceed.
	// Reservation state_event gates will be added after relay, so
	// they don't exist yet and don't block this check.
	for index := range content.Gates {
		if content.Gates[index].Status != ticket.GateSatisfied {
			return false
		}
	}

	return true
}

// isRelayed reports whether the ticket has already had relay tickets
// created. We detect this by looking for state_event gates that watch
// for EventTypeReservation — these are the gates the relay mechanism
// adds.
func isRelayed(content *ticket.TicketContent) bool {
	for index := range content.Gates {
		gate := &content.Gates[index]
		if gate.Type == ticket.GateStateEvent && gate.EventType == schema.EventTypeReservation {
			return true
		}
	}
	return false
}

// checkAndInitiateRelay checks whether a resource_request ticket is
// ready for relay and, if so, creates relay tickets in the target ops
// rooms. Must be called with ts.mu held (write lock).
//
// This is called from two paths:
//   - After ticket creation (if the ticket has no gates, it's
//     immediately relay-ready)
//   - After gate satisfaction (if all non-reservation gates are now
//     satisfied)
func (ts *TicketService) checkAndInitiateRelay(ctx context.Context, roomID ref.RoomID, state *roomState, ticketID string, content ticket.TicketContent) {
	if !isRelayReady(&content) {
		if content.Reservation != nil {
			ts.logger.Info("relay check: not ready",
				"ticket_id", ticketID,
				"room_id", roomID,
				"type", content.Type,
				"has_claims", len(content.Reservation.Claims) > 0,
				"status", content.Status,
				"gate_count", len(content.Gates),
			)
		}
		return
	}
	if isRelayed(&content) {
		return
	}

	if err := ts.initiateRelay(ctx, roomID, state, ticketID, &content); err != nil {
		ts.logger.Error("relay initiation failed",
			"ticket_id", ticketID,
			"room_id", roomID,
			"error", err,
		)
		// Add a note to the ticket explaining the failure so the
		// creator can see why their reservation didn't proceed.
		ts.addRelayFailureNote(ctx, roomID, state, ticketID, content, err)
	}
}

// initiateRelay creates relay tickets in ops rooms for each claim in
// the reservation and adds state_event gates to the origin ticket.
// Must be called with ts.mu held.
func (ts *TicketService) initiateRelay(
	ctx context.Context,
	originRoomID ref.RoomID,
	originState *roomState,
	originTicketID string,
	content *ticket.TicketContent,
) error {
	// Extract the fleet from the origin room's alias.
	fleet, err := fleetFromRoomAlias(originState.alias)
	if err != nil {
		return fmt.Errorf("cannot determine fleet from origin room: %w", err)
	}

	// Build the source room identity for relay policy evaluation.
	var sourceAlias ref.RoomAlias
	if originState.alias != "" {
		sourceAlias, err = ref.ParseRoomAlias(originState.alias)
		if err != nil {
			return fmt.Errorf("invalid origin room alias %q: %w", originState.alias, err)
		}
	}
	source := relayauth.SourceRoom{
		RoomID: originRoomID,
		Alias:  sourceAlias,
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	reservation := content.Reservation

	// Track relay tickets for lifecycle management.
	entry := &relayEntry{
		originRoom:   originRoomID,
		originTicket: originTicketID,
		relayTickets: make(map[ref.RoomID]relayTicketRef),
	}

	for claimIndex := range reservation.Claims {
		claim := &reservation.Claims[claimIndex]

		// Resolve the ops room for this claim's resource.
		opsAlias, err := relayauth.ResolveOpsRoom(claim.Resource, fleet)
		if err != nil {
			return fmt.Errorf("claim %d: resolve ops room: %w", claimIndex, err)
		}

		opsRoomID, err := ts.resolveAliasWithCache(ctx, opsAlias)
		if err != nil {
			return fmt.Errorf("claim %d: resolve ops room alias %s: %w", claimIndex, opsAlias, err)
		}

		// Verify the ticket service is a member of the ops room
		// and the room has ticket management.
		opsState, tracked := ts.rooms[opsRoomID]
		if !tracked {
			return fmt.Errorf("claim %d: ops room %s is not tracked (missing ticket_config or service not invited)", claimIndex, opsAlias)
		}

		// Fetch and evaluate the relay policy.
		policy, err := ts.fetchRelayPolicy(ctx, opsRoomID)
		if err != nil {
			return fmt.Errorf("claim %d: fetch relay policy from %s: %w", claimIndex, opsAlias, err)
		}

		result := relayauth.Evaluate(policy, source, string(content.Type))
		if !result.Authorized {
			return fmt.Errorf("claim %d: relay denied by policy in %s: %s", claimIndex, opsAlias, result.DenialReason)
		}

		// Validate the ticket creator can be represented as a fleet
		// entity before creating any ops room state. The relay link
		// requires the requester as a ref.Entity, and the fleet
		// controller uses it as the reservation holder. Failing after
		// creating the relay ticket would leave an orphaned ticket in
		// the ops room with no relay link for provenance.
		requester, err := ref.ParseEntityUserID(content.CreatedBy.String())
		if err != nil {
			return fmt.Errorf("claim %d: ticket creator %s is not a fleet entity (required for relay): %w", claimIndex, content.CreatedBy, err)
		}

		// Mark the claim as pending before building the relay ticket.
		// The relay ticket copies the claim, so the status must be set
		// before the copy happens. The origin ticket is written with
		// the updated claims after all relay tickets are created.
		claim.Status = schema.ClaimPending
		claim.StatusAt = now

		// Prepare the relay ticket content and ID without publishing.
		relayTicketID, relayContent, err := ts.prepareRelayTicket(opsState, opsRoomID, originRoomID, originTicketID, content, claim, now)
		if err != nil {
			return fmt.Errorf("claim %d: prepare relay ticket for %s: %w", claimIndex, opsAlias, err)
		}

		// Publish the relay link BEFORE the relay ticket. Both are
		// separate Matrix state events delivered via /sync. The fleet
		// controller's two-pass design processes relay links (pass 1)
		// before tickets (pass 2), but only within a single sync cycle.
		// If the ticket arrives in a different sync batch than the link,
		// the FC processes the ticket without the link, yielding a
		// zero-value holder for the reservation grant. Publishing the
		// link first ensures the FC always has it indexed when it sees
		// the ticket.
		relayLink := schema.RelayLink{
			OriginRoom:   originRoomID,
			OriginTicket: originTicketID,
			Requester:    requester,
		}
		if _, err := ts.writer.SendStateEvent(ctx, opsRoomID, schema.EventTypeRelayLink, relayTicketID, relayLink); err != nil {
			return fmt.Errorf("claim %d: publish relay link in %s: %w", claimIndex, opsAlias, err)
		}

		// Publish the relay ticket after the link is persisted.
		if err := ts.putWithEcho(ctx, opsRoomID, opsState, relayTicketID, relayContent); err != nil {
			return fmt.Errorf("claim %d: publish relay ticket in %s: %w", claimIndex, opsAlias, err)
		}

		entry.relayTickets[opsRoomID] = relayTicketRef{
			ticketID:       relayTicketID,
			claimIndex:     claimIndex,
			outboundFilter: result.OutboundFilter,
		}

		// Add a state_event gate to the origin ticket that
		// watches the ops room for an m.bureau.reservation event
		// with the holder matching the origin ticket's creator.
		//
		// The gate's state key is the creator's user ID in
		// state_key format (localpart:server), matching the
		// reservation state event convention.
		holderStateKey := content.CreatedBy.StateKey()
		gateID := fmt.Sprintf("rsv-%d", claimIndex)
		gate := ticket.TicketGate{
			ID:        gateID,
			Type:      ticket.GateStateEvent,
			Status:    ticket.GatePending,
			EventType: schema.EventTypeReservation,
			StateKey:  holderStateKey,
			RoomAlias: opsAlias,
			ContentMatch: schema.ContentMatch{
				"holder": schema.Eq(content.CreatedBy.String()),
			},
			CreatedAt: now,
		}
		content.Gates = append(content.Gates, gate)

		// Register a cross-room watch so that when a reservation
		// event arrives from the ops room, the gate is evaluated
		// event-driven rather than requiring a gate-scan.
		gateIndex := len(content.Gates) - 1
		ts.addCrossRoomWatch(opsRoomID, gate.EventType, gate.StateKey, originRoomID, originTicketID, gateIndex, gateID)

		ts.logger.Info("relay ticket created",
			"origin_ticket", originTicketID,
			"relay_ticket", relayTicketID,
			"ops_room", opsAlias,
			"claim_index", claimIndex,
			"resource_type", claim.Resource.Type,
			"resource_target", claim.Resource.Target,
		)
	}

	// Write the updated origin ticket with the new reservation
	// gates back to Matrix.
	content.UpdatedAt = now
	if err := ts.putWithEcho(ctx, originRoomID, originState, originTicketID, *content); err != nil {
		return fmt.Errorf("update origin ticket with reservation gates: %w", err)
	}

	// Store the relay entry for lifecycle management.
	ts.relayEntries[originTicketID] = entry

	ts.logger.Info("relay initiated",
		"origin_ticket", originTicketID,
		"claims", len(reservation.Claims),
		"relay_tickets", len(entry.relayTickets),
	)

	return nil
}

// prepareRelayTicket builds the relay ticket content and generates its
// ID without publishing. The caller is responsible for publishing the
// relay link first (via SendStateEvent) and then the ticket content
// (via putWithEcho). Separating preparation from publishing allows the
// relay link to be persisted to Matrix before the relay ticket, ensuring
// the fleet controller always has the link indexed when it processes the
// ticket regardless of /sync batching.
func (ts *TicketService) prepareRelayTicket(
	opsState *roomState,
	opsRoomID ref.RoomID,
	originRoomID ref.RoomID,
	originTicketID string,
	originContent *ticket.TicketContent,
	claim *ticket.ResourceClaim,
	now string,
) (string, ticket.TicketContent, error) {
	// Build a relay ticket that describes the resource claim. The
	// relay ticket carries enough information for the resource owner
	// (fleet controller) to evaluate and grant the reservation.
	relayContent := ticket.TicketContent{
		Version:  ticket.TicketContentVersion,
		Title:    fmt.Sprintf("Resource request: %s/%s", claim.Resource.Type, claim.Resource.Target),
		Body:     fmt.Sprintf("Relay for %s/%s (%s)", claim.Resource.Type, claim.Resource.Target, claim.Mode),
		Status:   ticket.StatusOpen,
		Priority: originContent.Priority,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{*claim},
		},
		CreatedBy: ts.service.UserID(),
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Copy MaxDuration from the origin reservation if set.
	if originContent.Reservation.MaxDuration != "" {
		relayContent.Reservation.MaxDuration = originContent.Reservation.MaxDuration
	}

	if err := relayContent.Validate(); err != nil {
		return "", ticket.TicketContent{}, fmt.Errorf("invalid relay ticket content: %w", err)
	}

	relayTicketID := ts.generateTicketID(opsState, ticket.TypeResourceRequest, opsRoomID.String(), now, relayContent.Title, nil)

	return relayTicketID, relayContent, nil
}

// fetchRelayPolicy fetches the m.bureau.relay_policy state event from
// an ops room. Returns nil (not an error) if the event doesn't exist
// — the caller handles nil as "no policy published".
func (ts *TicketService) fetchRelayPolicy(ctx context.Context, roomID ref.RoomID) (*schema.RelayPolicy, error) {
	raw, err := ts.session.GetStateEvent(ctx, roomID, schema.EventTypeRelayPolicy, "")
	if err != nil {
		// Check if the error is "not found" (no policy published).
		if isNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}

	var policy schema.RelayPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("parse relay policy: %w", err)
	}
	return &policy, nil
}

// isNotFoundError checks if an error indicates a Matrix 404 (event
// not found).
func isNotFoundError(err error) bool {
	return messaging.IsMatrixError(err, messaging.ErrCodeNotFound)
}

// addRelayFailureNote adds a note to the origin ticket explaining
// why relay failed. This provides visibility to the ticket creator.
func (ts *TicketService) addRelayFailureNote(
	ctx context.Context,
	roomID ref.RoomID,
	state *roomState,
	ticketID string,
	content ticket.TicketContent,
	relayErr error,
) {
	now := ts.clock.Now().UTC().Format(time.RFC3339)
	noteID := fmt.Sprintf("n-%d", len(content.Notes)+1)

	content.Notes = append(content.Notes, ticket.TicketNote{
		ID:        noteID,
		Author:    ts.service.UserID(),
		CreatedAt: now,
		Body:      fmt.Sprintf("Reservation relay failed: %v", relayErr),
	})
	content.UpdatedAt = now

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		ts.logger.Error("failed to add relay failure note",
			"ticket_id", ticketID,
			"error", err,
		)
	}
}

// --- Claim status tracking ---

// findRelayEntryByOpsRoom looks up the relay entry and claim index
// for a relay ticket in a given ops room. Returns nil if no relay
// entry tracks a ticket with the given ID in the given room.
func (ts *TicketService) findRelayEntryByOpsRoom(opsRoomID ref.RoomID, relayTicketID string) (*relayEntry, string, int) {
	for originTicketID, entry := range ts.relayEntries {
		for entryOpsRoom, relayRef := range entry.relayTickets {
			if entryOpsRoom == opsRoomID && relayRef.ticketID == relayTicketID {
				return entry, originTicketID, relayRef.claimIndex
			}
		}
	}
	return nil, "", -1
}

// updateClaimStatus updates a specific claim's status on the origin
// ticket and writes the update to Matrix. Must be called with ts.mu
// held.
func (ts *TicketService) updateClaimStatus(
	ctx context.Context,
	entry *relayEntry,
	originTicketID string,
	claimIndex int,
	status schema.ClaimStatus,
	statusReason string,
) {
	originState, tracked := ts.rooms[entry.originRoom]
	if !tracked {
		return
	}

	content, exists := originState.index.Get(originTicketID)
	if !exists || content.Reservation == nil {
		return
	}
	if claimIndex >= len(content.Reservation.Claims) {
		ts.logger.Error("claim index out of range",
			"origin_ticket", originTicketID,
			"claim_index", claimIndex,
			"claim_count", len(content.Reservation.Claims),
		)
		return
	}

	claim := &content.Reservation.Claims[claimIndex]

	// Skip if the claim is already in a terminal state. Terminal
	// states are sticky — once denied, released, or preempted,
	// the claim does not transition further.
	if claim.Status.IsTerminal() {
		return
	}

	// Skip if the status hasn't changed.
	if claim.Status == status {
		return
	}

	// Apply outbound filter. The status transition itself always
	// crosses the boundary, but the reason detail is controlled by
	// the relay policy's outbound filter.
	filter := ts.outboundFilterForClaim(entry, claimIndex)
	if statusReason != "" && !filter.Allows("status_reason") {
		statusReason = ""
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	claim.Status = status
	claim.StatusAt = now
	if statusReason != "" {
		claim.StatusReason = statusReason
	}
	content.UpdatedAt = now

	if err := ts.putWithEcho(ctx, entry.originRoom, originState, originTicketID, content); err != nil {
		ts.logger.Error("failed to update claim status",
			"origin_ticket", originTicketID,
			"claim_index", claimIndex,
			"status", status,
			"error", err,
		)
	} else {
		ts.logger.Info("claim status updated",
			"origin_ticket", originTicketID,
			"claim_index", claimIndex,
			"status", status,
		)
	}
}

// outboundFilterForClaim returns the outbound filter for a specific
// claim in a relay entry. Returns nil (default filtering) if the
// claim has no filter configured.
func (ts *TicketService) outboundFilterForClaim(entry *relayEntry, claimIndex int) *schema.RelayFilter {
	for _, relayRef := range entry.relayTickets {
		if relayRef.claimIndex == claimIndex {
			return relayRef.outboundFilter
		}
	}
	return nil
}

// mirrorRelayTicketStatus checks whether a relay ticket status change
// in an ops room should be mirrored to the corresponding claim in the
// origin ticket. Called from the sync loop for ticket events in
// tracked rooms.
//
// Status mapping:
//   - relay in_progress → claim approved
//   - relay closed (no prior grant) → claim denied (handled by denial cascade)
//   - relay closed (after grant) → claim preempted
//   - relay closed (origin closed) → no claim update (origin initiated)
//
// Must be called with ts.mu held.
func (ts *TicketService) mirrorRelayTicketStatus(ctx context.Context, opsRoomID ref.RoomID, relayTicketID string, relayContent ticket.TicketContent) {
	entry, originTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	switch relayContent.Status {
	case ticket.StatusInProgress:
		ts.updateClaimStatus(ctx, entry, originTicketID, claimIndex, schema.ClaimApproved, "")

	case ticket.StatusClosed:
		// Closure is handled by handleRelayTicketClosed for the
		// denial cascade and origin ticket closure. Here we only need
		// to update the claim status for closures that don't
		// trigger a denial cascade (origin-closed is a no-op).
		//
		// If the claim was previously granted, ops-side closure
		// means preemption. If it was never granted, it's denial
		// (handled by cascadeDenial which closes the origin
		// ticket entirely).
		if relayContent.CloseReason == "origin closed" {
			return
		}

		// Check if the claim was previously granted.
		originState, tracked := ts.rooms[entry.originRoom]
		if !tracked {
			return
		}
		originContent, exists := originState.index.Get(originTicketID)
		if !exists || originContent.Reservation == nil || claimIndex >= len(originContent.Reservation.Claims) {
			return
		}
		if originContent.Reservation.Claims[claimIndex].Status == schema.ClaimGranted {
			// Apply outbound filter to the ops room close reason
			// before writing it back to the origin ticket.
			filter := ts.outboundFilterForClaim(entry, claimIndex)
			filteredReason := relayContent.CloseReason
			if !filter.Allows("close_reason") {
				filteredReason = ""
			}
			ts.updateClaimStatus(ctx, entry, originTicketID, claimIndex, schema.ClaimPreempted, filteredReason)
			ts.closeTicketOnPreemption(ctx, entry.originRoom, originTicketID, filteredReason)
			ts.closeRelayTicketsForOrigin(ctx, originTicketID)
			delete(ts.relayEntries, originTicketID)
		}
		// Non-granted closure → denial cascade handles it.
	}
}

// mirrorReservationGrant updates the claim status to granted when an
// m.bureau.reservation event is published in an ops room. Called from
// the gate satisfaction path when a reservation gate fires.
//
// Must be called with ts.mu held.
func (ts *TicketService) mirrorReservationGrant(ctx context.Context, opsRoomID ref.RoomID, relayTicketID string) {
	entry, originTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	ts.updateClaimStatus(ctx, entry, originTicketID, claimIndex, schema.ClaimGranted, "")
}

// --- Lifecycle mirroring ---

// handleRelayTicketClosed processes the closure of a relay ticket in
// an ops room. Three cases:
//
//   - "origin closed": origin side initiated the closure, no action
//   - Claim was previously granted (preemption): the resource was
//     revoked after being granted. mirrorRelayTicketStatus handles the
//     claim status update to ClaimPreempted; no cascade here because
//     the origin ticket stays open (the resource was taken, not denied)
//   - Claim was not granted (denial): triggers denial cascade — closes
//     all other relay tickets and the origin ticket
//
// Must be called with ts.mu held.
func (ts *TicketService) handleRelayTicketClosed(ctx context.Context, opsRoomID ref.RoomID, relayTicketID string, relayContent ticket.TicketContent) {
	entry, originTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	closeReason := relayContent.CloseReason

	// If the relay ticket was closed with reason "origin closed",
	// the origin side initiated the closure — no cascade needed.
	if closeReason == "origin closed" {
		return
	}

	// Check whether the corresponding origin claim was already
	// granted. A granted claim that gets closed is preemption, not
	// denial — the origin ticket stays open and the claim status
	// is updated to ClaimPreempted by mirrorRelayTicketStatus.
	originState, tracked := ts.rooms[entry.originRoom]
	if tracked {
		originContent, exists := originState.index.Get(originTicketID)
		if exists && originContent.Reservation != nil &&
			claimIndex < len(originContent.Reservation.Claims) &&
			originContent.Reservation.Claims[claimIndex].Status == schema.ClaimGranted {
			ts.logger.Info("relay ticket closed after grant (preemption), skipping denial cascade",
				"origin_ticket", originTicketID,
				"relay_ticket", relayTicketID,
				"ops_room", opsRoomID,
				"reason", closeReason,
			)
			return
		}
	}

	// The relay ticket was closed by the ops side without a prior
	// grant — this is a denial. Cascade to all other relay tickets
	// and close the origin ticket. Apply the outbound filter to the
	// close reason before writing it back to the origin ticket.
	filter := ts.outboundFilterForClaim(entry, claimIndex)
	filteredReason := closeReason
	if !filter.Allows("close_reason") {
		filteredReason = ""
	}

	ts.logger.Warn("relay ticket denied, cascading closure",
		"origin_ticket", originTicketID,
		"relay_ticket", relayTicketID,
		"ops_room", opsRoomID,
		"reason", closeReason,
	)

	ts.cascadeDenial(ctx, entry, originTicketID, relayTicketID, opsRoomID, filteredReason)
}

// cascadeDenial closes all relay tickets for a reservation (except
// the one that triggered the cascade) and closes the origin ticket.
// Must be called with ts.mu held.
func (ts *TicketService) cascadeDenial(
	ctx context.Context,
	entry *relayEntry,
	originTicketID string,
	triggerRelayTicketID string,
	triggerOpsRoom ref.RoomID,
	reason string,
) {
	now := ts.clock.Now().UTC().Format(time.RFC3339)

	// Close all other relay tickets with reason explaining the cascade.
	for opsRoomID, relayRef := range entry.relayTickets {
		if opsRoomID == triggerOpsRoom && relayRef.ticketID == triggerRelayTicketID {
			continue
		}

		opsState, tracked := ts.rooms[opsRoomID]
		if !tracked {
			continue
		}

		relayContent, exists := opsState.index.Get(relayRef.ticketID)
		if !exists || relayContent.Status == ticket.StatusClosed {
			continue
		}

		relayContent.Status = ticket.StatusClosed
		relayContent.ClosedAt = now
		relayContent.CloseReason = fmt.Sprintf("denial cascade: %s", reason)
		relayContent.UpdatedAt = now

		if err := ts.putWithEcho(ctx, opsRoomID, opsState, relayRef.ticketID, relayContent); err != nil {
			ts.logger.Error("failed to close cascaded relay ticket",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
				"error", err,
			)
		} else {
			ts.logger.Info("closed cascaded relay ticket",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
			)
		}
	}

	// Close the origin ticket with the denial reason.
	originState, tracked := ts.rooms[entry.originRoom]
	if !tracked {
		return
	}

	originContent, exists := originState.index.Get(originTicketID)
	if !exists || originContent.Status == ticket.StatusClosed {
		return
	}

	originContent.Status = ticket.StatusClosed
	originContent.ClosedAt = now
	if reason != "" {
		originContent.CloseReason = fmt.Sprintf("reservation denied: %s", reason)
	} else {
		originContent.CloseReason = "reservation denied"
	}
	originContent.UpdatedAt = now

	if err := ts.putWithEcho(ctx, entry.originRoom, originState, originTicketID, originContent); err != nil {
		ts.logger.Error("failed to close origin ticket after denial",
			"origin_ticket", originTicketID,
			"error", err,
		)
	} else {
		ts.logger.Info("closed origin ticket after denial cascade",
			"origin_ticket", originTicketID,
			"reason", reason,
		)
	}

	// Clean up the relay entry.
	delete(ts.relayEntries, originTicketID)
}

// closeTicketOnPreemption closes a ticket whose reservation was
// preempted. The preempting resource owner closed the relay ticket in
// the ops room, so the reservation is being forcibly reclaimed. Closing
// the ticket cascades: for pipeline tickets, the executor detects
// closure via its cancellation poll; for relay tickets,
// closeRelayTicketsForOrigin (called by the caller after this)
// releases remaining relay tickets.
//
// Must be called with ts.mu held.
func (ts *TicketService) closeTicketOnPreemption(ctx context.Context, roomID ref.RoomID, ticketID string, reason string) {
	state, tracked := ts.rooms[roomID]
	if !tracked {
		return
	}
	content, exists := state.index.Get(ticketID)
	if !exists || content.Status == ticket.StatusClosed {
		return
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	content.Status = ticket.StatusClosed
	content.ClosedAt = now
	if reason != "" {
		content.CloseReason = fmt.Sprintf("preempted: %s", reason)
	} else {
		content.CloseReason = "preempted"
	}
	content.UpdatedAt = now

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		ts.logger.Error("failed to close ticket on preemption",
			"ticket_id", ticketID,
			"room_id", roomID,
			"error", err,
		)
	} else {
		ts.logger.Info("closed ticket on preemption",
			"ticket_id", ticketID,
			"room_id", roomID,
			"reason", reason,
		)
	}
}

// closeRelayTicketsForOrigin closes all relay tickets associated
// with a origin ticket that is being closed. Called when the
// origin ticket is closed by the user or by the system.
// Must be called with ts.mu held.
func (ts *TicketService) closeRelayTicketsForOrigin(ctx context.Context, originTicketID string) {
	entry, exists := ts.relayEntries[originTicketID]
	if !exists {
		return
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)

	for opsRoomID, relayRef := range entry.relayTickets {
		opsState, tracked := ts.rooms[opsRoomID]
		if !tracked {
			continue
		}

		relayContent, exists := opsState.index.Get(relayRef.ticketID)
		if !exists || relayContent.Status == ticket.StatusClosed {
			continue
		}

		relayContent.Status = ticket.StatusClosed
		relayContent.ClosedAt = now
		relayContent.CloseReason = "origin closed"
		relayContent.UpdatedAt = now

		if err := ts.putWithEcho(ctx, opsRoomID, opsState, relayRef.ticketID, relayContent); err != nil {
			ts.logger.Error("failed to close relay ticket on origin close",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
				"error", err,
			)
		} else {
			ts.logger.Info("closed relay ticket (origin closed)",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
			)
		}
	}

	delete(ts.relayEntries, originTicketID)
}

// --- Fleet extraction ---

// fleetFromRoomAlias extracts the fleet identity from a Bureau room
// alias string. Bureau room aliases follow the convention:
//
//	#namespace/fleet/name/...:server
//
// This parses the alias, finds the "/fleet/" segment in the localpart,
// and constructs a ref.Fleet from the namespace and fleet name.
func fleetFromRoomAlias(aliasString string) (ref.Fleet, error) {
	if aliasString == "" {
		return ref.Fleet{}, fmt.Errorf("empty room alias")
	}

	alias, err := ref.ParseRoomAlias(aliasString)
	if err != nil {
		return ref.Fleet{}, fmt.Errorf("parse room alias: %w", err)
	}

	localpart := alias.Localpart()
	server := alias.Server()

	// Find "/fleet/" in the localpart to extract namespace and fleet name.
	fleetSegment := "/fleet/"
	fleetIndex := strings.Index(localpart, fleetSegment)
	if fleetIndex < 0 {
		return ref.Fleet{}, fmt.Errorf("room alias %q does not contain /fleet/ segment", aliasString)
	}

	namespaceName := localpart[:fleetIndex]
	rest := localpart[fleetIndex+len(fleetSegment):]

	// Fleet name is up to the next "/".
	slashIndex := strings.IndexByte(rest, '/')
	var fleetName string
	if slashIndex >= 0 {
		fleetName = rest[:slashIndex]
	} else {
		fleetName = rest
	}

	namespace, err := ref.NewNamespace(server, namespaceName)
	if err != nil {
		return ref.Fleet{}, fmt.Errorf("invalid namespace %q in alias: %w", namespaceName, err)
	}

	return ref.NewFleet(namespace, fleetName)
}

// --- Relay reconstruction on startup ---

// rebuildRelayEntries scans all tracked rooms for tickets with
// reservations that have been relayed (have state_event gates
// watching for EventTypeReservation) and reconstructs the relay
// entry map. Called after initial sync to restore lifecycle
// management state. Must be called before concurrent access begins.
func (ts *TicketService) rebuildRelayEntries() {
	for originRoomID, state := range ts.rooms {
		for _, indexEntry := range state.index.List(ticketindex.Filter{}) {
			content := indexEntry.Content
			if content.Reservation == nil {
				continue
			}
			if !isRelayed(&content) {
				continue
			}

			// Reconstruct the relay entry from the gates.
			entry := &relayEntry{
				originRoom:   originRoomID,
				originTicket: indexEntry.ID,
				relayTickets: make(map[ref.RoomID]relayTicketRef),
			}

			// Each reservation gate watches an ops room. Resolve
			// the alias to get the ops room ID, then find the
			// relay ticket in that room by looking for tickets
			// with a matching RelayLink. The gate ID encodes the
			// claim index: "rsv-0" → claim 0, "rsv-1" → claim 1.
			for gateIndex := range content.Gates {
				gate := &content.Gates[gateIndex]
				if gate.Type != ticket.GateStateEvent || gate.EventType != schema.EventTypeReservation {
					continue
				}
				if gate.RoomAlias.IsZero() {
					continue
				}

				// Extract claim index from gate ID "rsv-N".
				claimIndex := -1
				if _, err := fmt.Sscanf(gate.ID, "rsv-%d", &claimIndex); err != nil || claimIndex < 0 {
					ts.logger.Warn("unexpected reservation gate ID format",
						"origin_ticket", indexEntry.ID,
						"gate_id", gate.ID,
					)
					continue
				}

				opsRoomID, err := ts.resolveAliasWithCache(context.Background(), gate.RoomAlias)
				if err != nil {
					ts.logger.Warn("failed to resolve ops room alias during relay rebuild",
						"origin_ticket", indexEntry.ID,
						"alias", gate.RoomAlias,
						"error", err,
					)
					continue
				}

				// Register cross-room watch for pending gates
				// so events from the ops room are evaluated
				// against the gate when they arrive.
				if gate.Status == ticket.GatePending {
					ts.addCrossRoomWatch(opsRoomID, gate.EventType, gate.StateKey, originRoomID, indexEntry.ID, gateIndex, gate.ID)
				}

				// Re-evaluate relay policy to recover the outbound
				// filter. The filter is policy state, not persisted
				// on the relay ticket — it must be re-derived on
				// startup.
				var outboundFilter *schema.RelayFilter
				policy, policyErr := ts.fetchRelayPolicy(context.Background(), opsRoomID)
				if policyErr == nil && policy != nil {
					outboundFilter = policy.OutboundFilter
				}

				// Find the relay ticket in the ops room. It's
				// the ticket created by us (service entity) with
				// type resource_request.
				opsState, tracked := ts.rooms[opsRoomID]
				if !tracked {
					continue
				}
				for _, opsEntry := range opsState.index.List(ticketindex.Filter{}) {
					if opsEntry.Content.Type == ticket.TypeResourceRequest &&
						opsEntry.Content.CreatedBy == ts.service.UserID() &&
						opsEntry.Content.Status != ticket.StatusClosed {
						entry.relayTickets[opsRoomID] = relayTicketRef{
							ticketID:       opsEntry.ID,
							claimIndex:     claimIndex,
							outboundFilter: outboundFilter,
						}
						break
					}
				}
			}

			if len(entry.relayTickets) > 0 {
				ts.relayEntries[indexEntry.ID] = entry
				ts.logger.Info("rebuilt relay entry",
					"origin_ticket", indexEntry.ID,
					"relay_tickets", len(entry.relayTickets),
				)
			}
		}
	}
}
