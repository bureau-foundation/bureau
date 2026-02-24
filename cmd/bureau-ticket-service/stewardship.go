// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// stewardshipResult holds the outputs of stewardship resolution:
// review gates, reviewer entries, and tier thresholds. The caller
// appends gates to content.Gates and merges reviewers/thresholds
// into content.Review.
type stewardshipResult struct {
	gates      []ticket.TicketGate
	reviewers  []ticket.ReviewerEntry
	thresholds []ticket.TierThreshold
}

// resolveStewardshipGates resolves ticket affects entries against the
// stewardship index and builds auto-configured review gates from
// matching declarations. Only declarations whose GateTypes include
// the ticket's type produce gates; NotifyTypes are handled separately
// by the notification subsystem.
//
// When priority is 0 (P0/critical), all tier escalation policies are
// overridden to "immediate" — every tier is notified at gate creation
// time regardless of the declaration's last_pending setting.
//
// Returns an empty result if affects is empty, no declarations match,
// or no matching declarations' GateTypes include the ticket type.
func (ts *TicketService) resolveStewardshipGates(affects []string, ticketType ticket.TicketType, priority int) stewardshipResult {
	if len(affects) == 0 {
		return stewardshipResult{}
	}

	matches := ts.stewardshipIndex.Resolve(affects)
	if len(matches) == 0 {
		return stewardshipResult{}
	}

	// Deduplicate matches by declaration. A single declaration can
	// match multiple resources; we only need to process it once.
	type declarationKey struct {
		roomID   ref.RoomID
		stateKey string
	}
	seen := make(map[declarationKey]bool)
	var deduplicated []stewardshipindex.Declaration
	for _, match := range matches {
		key := declarationKey{
			roomID:   match.Declaration.RoomID,
			stateKey: match.Declaration.StateKey,
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		deduplicated = append(deduplicated, match.Declaration)
	}

	// Filter by GateTypes: only keep declarations where the ticket
	// type triggers a review gate.
	var gateDeclarations []stewardshipindex.Declaration
	for _, declaration := range deduplicated {
		if typeInList(ticketType, declaration.Content.GateTypes) {
			gateDeclarations = append(gateDeclarations, declaration)
		}
	}
	if len(gateDeclarations) == 0 {
		return stewardshipResult{}
	}

	// Separate by overlap policy.
	var independent []stewardshipindex.Declaration
	var cooperative []stewardshipindex.Declaration
	for _, declaration := range gateDeclarations {
		policy := declaration.Content.OverlapPolicy
		if policy == "" {
			policy = "independent"
		}
		switch policy {
		case "cooperative":
			cooperative = append(cooperative, declaration)
		default:
			independent = append(independent, declaration)
		}
	}

	var result stewardshipResult
	tierOffset := 0

	// Build independent gates: each declaration gets its own review
	// gate with tiers remapped to globally unique numbers.
	for _, declaration := range independent {
		gate, reviewers, thresholds, nextOffset := ts.buildIndependentGate(declaration, tierOffset)
		if len(reviewers) == 0 {
			ts.logger.Warn("stewardship declaration matched but no room members resolved",
				"state_key", declaration.StateKey,
				"room_id", declaration.RoomID,
			)
			continue
		}
		result.gates = append(result.gates, gate)
		result.reviewers = append(result.reviewers, reviewers...)
		result.thresholds = append(result.thresholds, thresholds...)
		tierOffset = nextOffset
	}

	// Build cooperative gate: all cooperative declarations pool
	// reviewers into a single merged gate.
	if len(cooperative) > 0 {
		gate, reviewers, thresholds := ts.buildCooperativeGate(cooperative, tierOffset)
		if len(reviewers) == 0 {
			ts.logger.Warn("cooperative stewardship declarations matched but no room members resolved")
		} else {
			result.gates = append(result.gates, gate)
			result.reviewers = append(result.reviewers, reviewers...)
			result.thresholds = append(result.thresholds, thresholds...)
		}
	}

	// P0 bypass: override all escalation to "immediate" so every
	// tier is notified at gate creation time.
	if priority == 0 {
		for i := range result.thresholds {
			result.thresholds[i].Escalation = "immediate"
		}
	}

	return result
}

// buildIndependentGate builds a review gate, reviewer entries, and
// tier thresholds for a single independent stewardship declaration.
// Tiers are remapped starting at tierOffset to avoid collision with
// other declarations' tiers. Returns the next available tier offset.
func (ts *TicketService) buildIndependentGate(
	declaration stewardshipindex.Declaration,
	tierOffset int,
) (ticket.TicketGate, []ticket.ReviewerEntry, []ticket.TierThreshold, int) {
	var allReviewers []ticket.ReviewerEntry
	var allThresholds []ticket.TierThreshold

	for tierIndex, tier := range declaration.Content.Tiers {
		remappedTier := tierOffset + tierIndex
		reviewers := ts.resolveReviewersForTier(declaration.RoomID, tier, remappedTier)
		allReviewers = append(allReviewers, reviewers...)
		allThresholds = append(allThresholds, ticket.TierThreshold{
			Tier:       remappedTier,
			Threshold:  tier.Threshold,
			Escalation: tier.Escalation,
		})
	}

	gate := ticket.TicketGate{
		ID:          "stewardship:" + declaration.StateKey,
		Type:        ticket.GateReview,
		Status:      ticket.GatePending,
		Description: stewardshipGateDescription(declaration),
	}

	nextOffset := tierOffset + len(declaration.Content.Tiers)
	return gate, allReviewers, allThresholds, nextOffset
}

// buildCooperativeGate builds a single merged review gate from
// multiple cooperative stewardship declarations. Reviewers from
// all declarations are pooled with shared tier numbering. When
// multiple declarations specify thresholds for the same tier, the
// maximum is used.
func (ts *TicketService) buildCooperativeGate(
	declarations []stewardshipindex.Declaration,
	tierOffset int,
) (ticket.TicketGate, []ticket.ReviewerEntry, []ticket.TierThreshold) {
	// Find the maximum tier depth across all declarations.
	maxTiers := 0
	for _, declaration := range declarations {
		if len(declaration.Content.Tiers) > maxTiers {
			maxTiers = len(declaration.Content.Tiers)
		}
	}

	// For each tier index, merge reviewers from all declarations
	// that have that tier. Track the maximum threshold per tier.
	var allReviewers []ticket.ReviewerEntry
	var allThresholds []ticket.TierThreshold

	for tierIndex := 0; tierIndex < maxTiers; tierIndex++ {
		remappedTier := tierOffset + tierIndex
		seenUsers := make(map[ref.UserID]bool)
		var maxThreshold *int
		// Escalation merging: "immediate" wins over "last_pending".
		// Only when all declarations agree on "last_pending" does the
		// merged tier use "last_pending". Empty defaults to "immediate".
		mergedEscalation := "last_pending"
		declarationsWithTier := 0

		for _, declaration := range declarations {
			if tierIndex >= len(declaration.Content.Tiers) {
				continue
			}
			tier := declaration.Content.Tiers[tierIndex]
			declarationsWithTier++

			// Merge reviewers, deduplicating by UserID.
			for _, reviewer := range ts.resolveReviewersForTier(declaration.RoomID, tier, remappedTier) {
				if seenUsers[reviewer.UserID] {
					continue
				}
				seenUsers[reviewer.UserID] = true
				allReviewers = append(allReviewers, reviewer)
			}

			// Take the maximum threshold. Nil means "all must
			// approve" which is stricter than any numeric value,
			// so nil wins over any non-nil.
			if tier.Threshold == nil {
				maxThreshold = nil
			} else if maxThreshold != nil && *tier.Threshold > *maxThreshold {
				threshold := *tier.Threshold
				maxThreshold = &threshold
			} else if maxThreshold == nil && tier.Threshold != nil {
				// First declaration with a threshold for this
				// tier; start tracking.
				threshold := *tier.Threshold
				maxThreshold = &threshold
			}

			// If any declaration uses "immediate" (or empty,
			// which defaults to "immediate"), the merged tier
			// is "immediate".
			escalation := tier.Escalation
			if escalation == "" {
				escalation = "immediate"
			}
			if escalation == "immediate" {
				mergedEscalation = "immediate"
			}
		}

		// If no declarations had this tier, default to "immediate".
		if declarationsWithTier == 0 {
			mergedEscalation = "immediate"
		}

		allThresholds = append(allThresholds, ticket.TierThreshold{
			Tier:       remappedTier,
			Threshold:  maxThreshold,
			Escalation: mergedEscalation,
		})
	}

	gate := ticket.TicketGate{
		ID:          "stewardship:cooperative",
		Type:        ticket.GateReview,
		Status:      ticket.GatePending,
		Description: "Cooperative stewardship review",
	}

	return gate, allReviewers, allThresholds
}

// resolveReviewersForTier resolves a stewardship tier's principal
// patterns against the room's membership to produce concrete reviewer
// entries. Each principal pattern is matched using MatchUserID against
// all joined members of the declaring room. Results are deduplicated
// by UserID — a user matching multiple patterns in the same tier
// produces one reviewer entry.
func (ts *TicketService) resolveReviewersForTier(
	roomID ref.RoomID,
	tier stewardship.StewardshipTier,
	remappedTierNumber int,
) []ticket.ReviewerEntry {
	members := ts.membersByRoom[roomID]
	if len(members) == 0 {
		return nil
	}

	seen := make(map[ref.UserID]bool)
	var reviewers []ticket.ReviewerEntry

	for _, pattern := range tier.Principals {
		for userID := range members {
			if seen[userID] {
				continue
			}
			if principal.MatchUserID(pattern, userID.String()) {
				seen[userID] = true
				reviewers = append(reviewers, ticket.ReviewerEntry{
					UserID:      userID,
					Disposition: "pending",
					Tier:        remappedTierNumber,
				})
			}
		}
	}

	return reviewers
}

// removeStewardshipGates returns a filtered copy of the gates slice
// with all stewardship-sourced gates removed. Stewardship gates are
// identified by their ID prefix "stewardship:".
func removeStewardshipGates(gates []ticket.TicketGate) []ticket.TicketGate {
	kept := gates[:0]
	for _, gate := range gates {
		if !strings.HasPrefix(gate.ID, "stewardship:") {
			kept = append(kept, gate)
		}
	}
	return kept
}

// mergeStewardshipReview merges stewardship-resolved reviewers into
// the ticket's Review field. New stewardship reviewer UserIDs replace
// existing reviewers with the same UserID, preserving their current
// disposition. Reviewers not in the new stewardship set are kept as
// manual additions. TierThresholds are replaced entirely.
//
// If the new stewardship set is empty and there are no existing
// manual reviewers, Review is left unchanged (may be nil).
func mergeStewardshipReview(
	content *ticket.TicketContent,
	newReviewers []ticket.ReviewerEntry,
	newThresholds []ticket.TierThreshold,
) {
	// Build a lookup of new stewardship reviewers by UserID.
	newByUserID := make(map[ref.UserID]ticket.ReviewerEntry, len(newReviewers))
	for _, reviewer := range newReviewers {
		newByUserID[reviewer.UserID] = reviewer
	}

	if content.Review == nil {
		if len(newReviewers) == 0 {
			return
		}
		content.Review = &ticket.TicketReview{
			Reviewers:      newReviewers,
			TierThresholds: newThresholds,
		}
		return
	}

	// Preserve existing dispositions for reviewers that appear in
	// the new stewardship set. Keep manual reviewers (those not in
	// the new set) as-is.
	var merged []ticket.ReviewerEntry
	preservedDispositions := make(map[ref.UserID]string)

	for _, existing := range content.Review.Reviewers {
		if _, inNew := newByUserID[existing.UserID]; inNew {
			// This reviewer is stewardship-managed. Preserve
			// their disposition for the new entry.
			preservedDispositions[existing.UserID] = existing.Disposition
		} else {
			// Manual reviewer — keep as-is.
			merged = append(merged, existing)
		}
	}

	// Add new stewardship reviewers with preserved dispositions.
	for _, reviewer := range newReviewers {
		if disposition, preserved := preservedDispositions[reviewer.UserID]; preserved {
			reviewer.Disposition = disposition
		}
		merged = append(merged, reviewer)
	}

	content.Review.Reviewers = merged
	content.Review.TierThresholds = newThresholds
}

// --- Helpers ---

// typeInList reports whether the given type name appears in the list.
func typeInList(typeName ticket.TicketType, list []ticket.TicketType) bool {
	for _, entry := range list {
		if entry == typeName {
			return true
		}
	}
	return false
}

// stewardshipGateDescription produces a human-readable description
// for an independent stewardship review gate.
func stewardshipGateDescription(declaration stewardshipindex.Declaration) string {
	if declaration.Content.Description != "" {
		return "Stewardship review: " + declaration.Content.Description
	}
	return "Stewardship review for " + declaration.StateKey
}

// --- Review snapshotting ---

// snapshotReview creates a shallow copy of a TicketReview suitable
// for comparing tier satisfaction before and after a mutation. The
// Reviewers and TierThresholds slices are copied so that mutations
// to the original do not affect the snapshot. Returns nil if the
// input is nil.
func snapshotReview(review *ticket.TicketReview) *ticket.TicketReview {
	if review == nil {
		return nil
	}
	snapshot := &ticket.TicketReview{
		Scope: review.Scope,
	}
	if len(review.Reviewers) > 0 {
		snapshot.Reviewers = make([]ticket.ReviewerEntry, len(review.Reviewers))
		copy(snapshot.Reviewers, review.Reviewers)
	}
	if len(review.TierThresholds) > 0 {
		snapshot.TierThresholds = make([]ticket.TierThreshold, len(review.TierThresholds))
		copy(snapshot.TierThresholds, review.TierThresholds)
	}
	return snapshot
}

// --- Escalation notifications ---

// tierSatisfied checks whether a specific tier has met its approval
// requirements given the review's current state. Returns true if the
// tier's threshold is met (or the tier has no reviewers, which is
// vacuously satisfied).
func tierSatisfied(review *ticket.TicketReview, tier int) bool {
	// Build threshold lookup.
	var threshold *int
	for i := range review.TierThresholds {
		if review.TierThresholds[i].Tier == tier {
			threshold = review.TierThresholds[i].Threshold
			break
		}
	}

	// Count reviewers in this tier.
	total := 0
	approved := 0
	for _, reviewer := range review.Reviewers {
		if reviewer.Tier == tier {
			total++
			if reviewer.Disposition == "approved" {
				approved++
			}
		}
	}

	if total == 0 {
		// No reviewers in this tier — vacuously satisfied.
		return true
	}

	if threshold == nil {
		// All must approve.
		return approved >= total
	}
	return approved >= *threshold
}

// activatedLastPendingTiers returns the tier numbers of last_pending
// tiers that should now be activated (notified). A last_pending tier
// activates when all earlier tiers are satisfied.
//
// The oldReview parameter is the review state before the disposition
// change; newReview is the state after. A tier is "newly activated" if
// it was not activatable before but is now. This prevents duplicate
// notifications on subsequent approvals that don't change which tiers
// are active.
//
// Returns nil if no last_pending tiers should be activated.
func activatedLastPendingTiers(oldReview, newReview *ticket.TicketReview) []int {
	if newReview == nil || len(newReview.TierThresholds) == 0 {
		return nil
	}

	// Collect last_pending tiers sorted by tier number.
	var lastPendingTiers []int
	for _, threshold := range newReview.TierThresholds {
		if threshold.Escalation == "last_pending" {
			lastPendingTiers = append(lastPendingTiers, threshold.Tier)
		}
	}
	if len(lastPendingTiers) == 0 {
		return nil
	}

	// Collect all distinct tier numbers for ordering.
	tierNumbers := allTierNumbers(newReview)

	var activated []int
	for _, pendingTier := range lastPendingTiers {
		// Check: all earlier tiers must be satisfied in the new state.
		allEarlierSatisfied := true
		for _, earlierTier := range tierNumbers {
			if earlierTier >= pendingTier {
				break
			}
			if !tierSatisfied(newReview, earlierTier) {
				allEarlierSatisfied = false
				break
			}
		}
		if !allEarlierSatisfied {
			continue
		}

		// Already satisfied in the new state — no need to notify.
		if tierSatisfied(newReview, pendingTier) {
			continue
		}

		// Check this wasn't already activatable in the old state.
		if oldReview != nil {
			wasActivatable := true
			for _, earlierTier := range tierNumbers {
				if earlierTier >= pendingTier {
					break
				}
				if !tierSatisfied(oldReview, earlierTier) {
					wasActivatable = false
					break
				}
			}
			if wasActivatable {
				// Was already activatable — no new notification.
				continue
			}
		}

		activated = append(activated, pendingTier)
	}

	return activated
}

// allTierNumbers returns all distinct tier numbers from the review's
// TierThresholds, sorted ascending.
func allTierNumbers(review *ticket.TicketReview) []int {
	seen := make(map[int]bool, len(review.TierThresholds))
	var numbers []int
	for _, threshold := range review.TierThresholds {
		if !seen[threshold.Tier] {
			seen[threshold.Tier] = true
			numbers = append(numbers, threshold.Tier)
		}
	}
	sort.Ints(numbers)
	return numbers
}

// escalationMessage formats a notification message for a last_pending
// tier that has just been activated. The message includes the ticket
// ID, a summary of earlier tier approvals, and a mention of the
// reviewers in the activated tier.
func escalationMessage(ticketID string, content ticket.TicketContent, activatedTier int) string {
	var builder strings.Builder

	// Header.
	fmt.Fprintf(&builder, "Stewardship escalation: %s", ticketID)
	if content.Title != "" {
		fmt.Fprintf(&builder, " (%s)", content.Title)
	}
	builder.WriteString("\n")

	// Earlier tier summary.
	builder.WriteString("\nEarlier tiers satisfied:")
	for _, reviewer := range content.Review.Reviewers {
		if reviewer.Tier < activatedTier && reviewer.Disposition == "approved" {
			fmt.Fprintf(&builder, "\n  - %s (tier %d, approved)", reviewer.UserID, reviewer.Tier)
		}
	}

	// Activated tier reviewers.
	fmt.Fprintf(&builder, "\n\nTier %d review now needed:", activatedTier)
	for _, reviewer := range content.Review.Reviewers {
		if reviewer.Tier == activatedTier {
			fmt.Fprintf(&builder, "\n  - %s", reviewer.UserID)
		}
	}

	return builder.String()
}

// sendEscalationNotifications checks whether a disposition change
// activated any last_pending tiers and sends notification messages
// to the ticket's room for each activated tier.
//
// oldReview is the review state before the change; the current
// content has the new state. This must be called after the ticket
// has been written to Matrix (via putWithEcho) so that the
// notification appears after the state event update.
func (ts *TicketService) sendEscalationNotifications(
	ctx context.Context,
	roomID ref.RoomID,
	ticketID string,
	oldReview *ticket.TicketReview,
	content ticket.TicketContent,
) {
	if ts.messenger == nil {
		return
	}

	activated := activatedLastPendingTiers(oldReview, content.Review)
	if len(activated) == 0 {
		return
	}

	for _, tier := range activated {
		message := escalationMessage(ticketID, content, tier)
		_, err := ts.messenger.SendMessage(ctx, roomID, messaging.NewTextMessage(message))
		if err != nil {
			ts.logger.Error("failed to send escalation notification",
				"ticket_id", ticketID,
				"tier", tier,
				"room_id", roomID,
				"error", err,
			)
		} else {
			ts.logger.Info("sent escalation notification",
				"ticket_id", ticketID,
				"tier", tier,
				"room_id", roomID,
			)
		}
	}
}
