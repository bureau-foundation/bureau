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

// relayEntry tracks the association between a workspace ticket and its
// relay tickets in ops rooms. Created when the ticket service initiates
// relay for a resource_request ticket.
type relayEntry struct {
	// workspaceRoom is the room containing the workspace ticket.
	workspaceRoom ref.RoomID

	// workspaceTicket is the ID of the workspace ticket.
	workspaceTicket string

	// relayTickets maps ops room ID → relay ticket reference for
	// each claim that was successfully relayed.
	relayTickets map[ref.RoomID]relayTicketRef
}

// relayTicketRef identifies a relay ticket in an ops room and its
// corresponding claim in the workspace ticket.
type relayTicketRef struct {
	// ticketID is the relay ticket's ID in the ops room.
	ticketID string

	// claimIndex is the index into the workspace ticket's
	// Reservation.Claims slice that this relay ticket represents.
	claimIndex int
}

// isRelayReady reports whether a resource_request ticket is ready for
// relay: all existing gates (non-reservation) are satisfied, and the
// ticket has not already been relayed. The reservation will add new
// state_event gates after relay; those gates don't exist yet.
func isRelayReady(content *ticket.TicketContent) bool {
	if content.Type != ticket.TypeResourceRequest {
		return false
	}
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
		if content.Type == ticket.TypeResourceRequest {
			ts.logger.Info("relay check: not ready",
				"ticket_id", ticketID,
				"room_id", roomID,
				"has_reservation", content.Reservation != nil,
				"has_claims", content.Reservation != nil && len(content.Reservation.Claims) > 0,
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
// the reservation and adds state_event gates to the workspace ticket.
// Must be called with ts.mu held.
func (ts *TicketService) initiateRelay(
	ctx context.Context,
	workspaceRoomID ref.RoomID,
	workspaceState *roomState,
	workspaceTicketID string,
	content *ticket.TicketContent,
) error {
	// Extract the fleet from the workspace room's alias.
	fleet, err := fleetFromRoomAlias(workspaceState.alias)
	if err != nil {
		return fmt.Errorf("cannot determine fleet from workspace room: %w", err)
	}

	// Build the source room identity for relay policy evaluation.
	var sourceAlias ref.RoomAlias
	if workspaceState.alias != "" {
		sourceAlias, err = ref.ParseRoomAlias(workspaceState.alias)
		if err != nil {
			return fmt.Errorf("invalid workspace room alias %q: %w", workspaceState.alias, err)
		}
	}
	source := relayauth.SourceRoom{
		RoomID: workspaceRoomID,
		Alias:  sourceAlias,
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	reservation := content.Reservation

	// Track relay tickets for lifecycle management.
	entry := &relayEntry{
		workspaceRoom:   workspaceRoomID,
		workspaceTicket: workspaceTicketID,
		relayTickets:    make(map[ref.RoomID]relayTicketRef),
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
		// before the copy happens. The workspace ticket is written with
		// the updated claims after all relay tickets are created.
		claim.Status = schema.ClaimPending
		claim.StatusAt = now

		// Prepare the relay ticket content and ID without publishing.
		relayTicketID, relayContent, err := ts.prepareRelayTicket(opsState, opsRoomID, workspaceRoomID, workspaceTicketID, content, claim, now)
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
			OriginRoom:   workspaceRoomID,
			OriginTicket: workspaceTicketID,
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
			ticketID:   relayTicketID,
			claimIndex: claimIndex,
		}

		// Add a state_event gate to the workspace ticket that
		// watches the ops room for an m.bureau.reservation event
		// with the holder matching the workspace ticket's creator.
		//
		// The gate's state key is the creator's localpart (the
		// convention for reservation state events). The
		// content_match checks that the holder field matches.
		holderLocalpart := content.CreatedBy.Localpart()
		gateID := fmt.Sprintf("rsv-%d", claimIndex)
		gate := ticket.TicketGate{
			ID:        gateID,
			Type:      ticket.GateStateEvent,
			Status:    ticket.GatePending,
			EventType: schema.EventTypeReservation,
			StateKey:  holderLocalpart,
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
		ts.addCrossRoomWatch(opsRoomID, gate.EventType, gate.StateKey, workspaceRoomID, workspaceTicketID, gateIndex, gateID)

		ts.logger.Info("relay ticket created",
			"workspace_ticket", workspaceTicketID,
			"relay_ticket", relayTicketID,
			"ops_room", opsAlias,
			"claim_index", claimIndex,
			"resource_type", claim.Resource.Type,
			"resource_target", claim.Resource.Target,
		)
	}

	// Write the updated workspace ticket with the new reservation
	// gates back to Matrix.
	content.UpdatedAt = now
	if err := ts.putWithEcho(ctx, workspaceRoomID, workspaceState, workspaceTicketID, *content); err != nil {
		return fmt.Errorf("update workspace ticket with reservation gates: %w", err)
	}

	// Store the relay entry for lifecycle management.
	ts.relayEntries[workspaceTicketID] = entry

	ts.logger.Info("relay initiated",
		"workspace_ticket", workspaceTicketID,
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
	workspaceRoomID ref.RoomID,
	workspaceTicketID string,
	workspaceContent *ticket.TicketContent,
	claim *ticket.ResourceClaim,
	now string,
) (string, ticket.TicketContent, error) {
	// Build a relay ticket that describes the resource claim. The
	// relay ticket carries enough information for the resource owner
	// (fleet controller) to evaluate and grant the reservation.
	relayContent := ticket.TicketContent{
		Version:  ticket.TicketContentVersion,
		Title:    fmt.Sprintf("Resource request: %s/%s", claim.Resource.Type, claim.Resource.Target),
		Body:     fmt.Sprintf("Relay from workspace ticket %s in room %s.\nMode: %s", workspaceTicketID, workspaceRoomID, claim.Mode),
		Status:   ticket.StatusOpen,
		Priority: workspaceContent.Priority,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{*claim},
		},
		CreatedBy: ts.service.UserID(),
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Copy MaxDuration from the workspace reservation if set.
	if workspaceContent.Reservation.MaxDuration != "" {
		relayContent.Reservation.MaxDuration = workspaceContent.Reservation.MaxDuration
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

// addRelayFailureNote adds a note to the workspace ticket explaining
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
	for workspaceTicketID, entry := range ts.relayEntries {
		for entryOpsRoom, relayRef := range entry.relayTickets {
			if entryOpsRoom == opsRoomID && relayRef.ticketID == relayTicketID {
				return entry, workspaceTicketID, relayRef.claimIndex
			}
		}
	}
	return nil, "", -1
}

// updateClaimStatus updates a specific claim's status on the workspace
// ticket and writes the update to Matrix. Must be called with ts.mu
// held.
func (ts *TicketService) updateClaimStatus(
	ctx context.Context,
	entry *relayEntry,
	workspaceTicketID string,
	claimIndex int,
	status schema.ClaimStatus,
	statusReason string,
) {
	workspaceState, tracked := ts.rooms[entry.workspaceRoom]
	if !tracked {
		return
	}

	content, exists := workspaceState.index.Get(workspaceTicketID)
	if !exists || content.Reservation == nil {
		return
	}
	if claimIndex >= len(content.Reservation.Claims) {
		ts.logger.Error("claim index out of range",
			"workspace_ticket", workspaceTicketID,
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

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	claim.Status = status
	claim.StatusAt = now
	if statusReason != "" {
		claim.StatusReason = statusReason
	}
	content.UpdatedAt = now

	if err := ts.putWithEcho(ctx, entry.workspaceRoom, workspaceState, workspaceTicketID, content); err != nil {
		ts.logger.Error("failed to update claim status",
			"workspace_ticket", workspaceTicketID,
			"claim_index", claimIndex,
			"status", status,
			"error", err,
		)
	} else {
		ts.logger.Info("claim status updated",
			"workspace_ticket", workspaceTicketID,
			"claim_index", claimIndex,
			"status", status,
		)
	}
}

// mirrorRelayTicketStatus checks whether a relay ticket status change
// in an ops room should be mirrored to the corresponding claim in the
// workspace ticket. Called from the sync loop for ticket events in
// tracked rooms.
//
// Status mapping:
//   - relay in_progress → claim approved
//   - relay closed (no prior grant) → claim denied (handled by denial cascade)
//   - relay closed (after grant) → claim preempted
//   - relay closed (origin closed) → no claim update (workspace initiated)
//
// Must be called with ts.mu held.
func (ts *TicketService) mirrorRelayTicketStatus(ctx context.Context, opsRoomID ref.RoomID, relayTicketID string, relayContent ticket.TicketContent) {
	entry, workspaceTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	switch relayContent.Status {
	case ticket.StatusInProgress:
		ts.updateClaimStatus(ctx, entry, workspaceTicketID, claimIndex, schema.ClaimApproved, "")

	case ticket.StatusClosed:
		// Closure is handled by handleRelayTicketClosed for the
		// denial cascade and workspace closure. Here we only need
		// to update the claim status for closures that don't
		// trigger a denial cascade (origin-closed is a no-op).
		//
		// If the claim was previously granted, ops-side closure
		// means preemption. If it was never granted, it's denial
		// (handled by cascadeDenial which closes the workspace
		// ticket entirely).
		if relayContent.CloseReason == "origin closed" {
			return
		}

		// Check if the claim was previously granted.
		workspaceState, tracked := ts.rooms[entry.workspaceRoom]
		if !tracked {
			return
		}
		wsContent, exists := workspaceState.index.Get(workspaceTicketID)
		if !exists || wsContent.Reservation == nil || claimIndex >= len(wsContent.Reservation.Claims) {
			return
		}
		if wsContent.Reservation.Claims[claimIndex].Status == schema.ClaimGranted {
			ts.updateClaimStatus(ctx, entry, workspaceTicketID, claimIndex, schema.ClaimPreempted, relayContent.CloseReason)
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
	entry, workspaceTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	ts.updateClaimStatus(ctx, entry, workspaceTicketID, claimIndex, schema.ClaimGranted, "")
}

// --- Lifecycle mirroring ---

// handleRelayTicketClosed processes the closure of a relay ticket in
// an ops room. Three cases:
//
//   - "origin closed": workspace side initiated the closure, no action
//   - Claim was previously granted (preemption): the resource was
//     revoked after being granted. mirrorRelayTicketStatus handles the
//     claim status update to ClaimPreempted; no cascade here because
//     the workspace ticket stays open (the resource was taken, not denied)
//   - Claim was not granted (denial): triggers denial cascade — closes
//     all other relay tickets and the workspace ticket
//
// Must be called with ts.mu held.
func (ts *TicketService) handleRelayTicketClosed(ctx context.Context, opsRoomID ref.RoomID, relayTicketID string, relayContent ticket.TicketContent) {
	entry, workspaceTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomID, relayTicketID)
	if entry == nil {
		return
	}

	closeReason := relayContent.CloseReason

	// If the relay ticket was closed with reason "origin closed",
	// the workspace side initiated the closure — no cascade needed.
	if closeReason == "origin closed" {
		return
	}

	// Check whether the corresponding workspace claim was already
	// granted. A granted claim that gets closed is preemption, not
	// denial — the workspace ticket stays open and the claim status
	// is updated to ClaimPreempted by mirrorRelayTicketStatus.
	workspaceState, tracked := ts.rooms[entry.workspaceRoom]
	if tracked {
		wsContent, exists := workspaceState.index.Get(workspaceTicketID)
		if exists && wsContent.Reservation != nil &&
			claimIndex < len(wsContent.Reservation.Claims) &&
			wsContent.Reservation.Claims[claimIndex].Status == schema.ClaimGranted {
			ts.logger.Info("relay ticket closed after grant (preemption), skipping denial cascade",
				"workspace_ticket", workspaceTicketID,
				"relay_ticket", relayTicketID,
				"ops_room", opsRoomID,
				"reason", closeReason,
			)
			return
		}
	}

	// The relay ticket was closed by the ops side without a prior
	// grant — this is a denial. Cascade to all other relay tickets
	// and close the workspace ticket.
	ts.logger.Warn("relay ticket denied, cascading closure",
		"workspace_ticket", workspaceTicketID,
		"relay_ticket", relayTicketID,
		"ops_room", opsRoomID,
		"reason", closeReason,
	)

	ts.cascadeDenial(ctx, entry, workspaceTicketID, relayTicketID, opsRoomID, closeReason)
}

// cascadeDenial closes all relay tickets for a reservation (except
// the one that triggered the cascade) and closes the workspace ticket.
// Must be called with ts.mu held.
func (ts *TicketService) cascadeDenial(
	ctx context.Context,
	entry *relayEntry,
	workspaceTicketID string,
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

	// Close the workspace ticket with the denial reason.
	workspaceState, tracked := ts.rooms[entry.workspaceRoom]
	if !tracked {
		return
	}

	workspaceContent, exists := workspaceState.index.Get(workspaceTicketID)
	if !exists || workspaceContent.Status == ticket.StatusClosed {
		return
	}

	workspaceContent.Status = ticket.StatusClosed
	workspaceContent.ClosedAt = now
	workspaceContent.CloseReason = fmt.Sprintf("reservation denied: %s", reason)
	workspaceContent.UpdatedAt = now

	if err := ts.putWithEcho(ctx, entry.workspaceRoom, workspaceState, workspaceTicketID, workspaceContent); err != nil {
		ts.logger.Error("failed to close workspace ticket after denial",
			"workspace_ticket", workspaceTicketID,
			"error", err,
		)
	} else {
		ts.logger.Info("closed workspace ticket after denial cascade",
			"workspace_ticket", workspaceTicketID,
			"reason", reason,
		)
	}

	// Clean up the relay entry.
	delete(ts.relayEntries, workspaceTicketID)
}

// closeRelayTicketsForWorkspace closes all relay tickets associated
// with a workspace ticket that is being closed. Called when the
// workspace ticket is closed by the user or by the system.
// Must be called with ts.mu held.
func (ts *TicketService) closeRelayTicketsForWorkspace(ctx context.Context, workspaceTicketID string) {
	entry, exists := ts.relayEntries[workspaceTicketID]
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
			ts.logger.Error("failed to close relay ticket on workspace close",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
				"error", err,
			)
		} else {
			ts.logger.Info("closed relay ticket (workspace closed)",
				"relay_ticket", relayRef.ticketID,
				"ops_room", opsRoomID,
			)
		}
	}

	delete(ts.relayEntries, workspaceTicketID)
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

// rebuildRelayEntries scans all tracked rooms for resource_request
// tickets that have been relayed (have state_event gates watching
// for EventTypeReservation) and reconstructs the relay entry map.
// Called after initial sync to restore lifecycle management state.
// Must be called before concurrent access begins.
func (ts *TicketService) rebuildRelayEntries() {
	for workspaceRoomID, state := range ts.rooms {
		for _, indexEntry := range state.index.List(ticketindex.Filter{}) {
			content := indexEntry.Content
			if content.Type != ticket.TypeResourceRequest || content.Reservation == nil {
				continue
			}
			if !isRelayed(&content) {
				continue
			}

			// Reconstruct the relay entry from the gates.
			entry := &relayEntry{
				workspaceRoom:   workspaceRoomID,
				workspaceTicket: indexEntry.ID,
				relayTickets:    make(map[ref.RoomID]relayTicketRef),
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
						"workspace_ticket", indexEntry.ID,
						"gate_id", gate.ID,
					)
					continue
				}

				opsRoomID, err := ts.resolveAliasWithCache(context.Background(), gate.RoomAlias)
				if err != nil {
					ts.logger.Warn("failed to resolve ops room alias during relay rebuild",
						"workspace_ticket", indexEntry.ID,
						"alias", gate.RoomAlias,
						"error", err,
					)
					continue
				}

				// Register cross-room watch for pending gates
				// so events from the ops room are evaluated
				// against the gate when they arrive.
				if gate.Status == ticket.GatePending {
					ts.addCrossRoomWatch(opsRoomID, gate.EventType, gate.StateKey, workspaceRoomID, indexEntry.ID, gateIndex, gate.ID)
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
							ticketID:   opsEntry.ID,
							claimIndex: claimIndex,
						}
						break
					}
				}
			}

			if len(entry.relayTickets) > 0 {
				ts.relayEntries[indexEntry.ID] = entry
				ts.logger.Info("rebuilt relay entry",
					"workspace_ticket", indexEntry.ID,
					"relay_tickets", len(entry.relayTickets),
				)
			}
		}
	}
}
