// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Digest accumulator types ---

// digestKey uniquely identifies a stewardship declaration's digest
// accumulator. One digest per (roomID, stateKey) pair.
type digestKey struct {
	roomID   ref.RoomID
	stateKey string
}

// digestEntry holds a pending notification digest for a single
// stewardship declaration. Tickets are accumulated until the flush
// timer fires or a P0/P1 ticket bypasses the digest.
type digestEntry struct {
	// declaration is the stewardship declaration that produced this
	// digest. Used to resolve principals for the notification message.
	declaration stewardshipindex.Declaration

	// tickets is the list of tickets accumulated since the last flush
	// (or since the digest was created). Each entry contains enough
	// information to construct the digest message.
	tickets []digestTicket

	// timer is the AfterFunc timer for the next scheduled flush. Nil
	// when no flush is pending (empty accumulator or immediate mode).
	// The timer callback sends on ts.digestNotify to wake the digest
	// loop.
	timer *clock.Timer

	// flushAt is when the current timer is scheduled to fire. Used
	// by the digest loop to determine which entries are ready for
	// flushing (clock.Now() >= flushAt) without racing with the
	// AfterFunc callback.
	flushAt time.Time
}

// digestTicket holds the ticket metadata needed for a digest
// notification message. Captured at queue time so that the message
// reflects the ticket's state when the notification was triggered,
// not when the digest fires.
type digestTicket struct {
	ticketID     string
	title        string
	ticketType   ticket.TicketType
	priority     int
	ticketRoomID ref.RoomID
}

// --- Notification resolution ---

// resolveStewardshipNotifications resolves ticket affects entries
// against the stewardship index and returns declarations whose
// NotifyTypes include the ticket's type. This is the notification
// counterpart of resolveStewardshipGates: where that function
// filters by GateTypes and builds review gates, this function
// filters by NotifyTypes and returns matched declarations for
// notification queueing.
//
// Deduplicates by (roomID, stateKey). Returns nil if no
// declarations match NotifyTypes for this ticket type.
func (ts *TicketService) resolveStewardshipNotifications(
	affects []string,
	ticketType ticket.TicketType,
) []stewardshipindex.Declaration {
	if len(affects) == 0 {
		return nil
	}

	matches := ts.stewardshipIndex.Resolve(affects)
	if len(matches) == 0 {
		return nil
	}

	// Deduplicate matches by declaration.
	type declarationKey struct {
		roomID   ref.RoomID
		stateKey string
	}
	seen := make(map[declarationKey]bool)
	var result []stewardshipindex.Declaration
	for _, match := range matches {
		key := declarationKey{
			roomID:   match.Declaration.RoomID,
			stateKey: match.Declaration.StateKey,
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		// Only keep declarations where the ticket type triggers
		// notification (not review gates).
		if typeInList(ticketType, match.Declaration.Content.NotifyTypes) {
			result = append(result, match.Declaration)
		}
	}

	return result
}

// --- Notification queueing ---

// queueStewardshipNotifications queues notifications for stewardship
// declarations matching a ticket's NotifyTypes. For P0/P1 tickets
// (priority <= 1), notifications bypass the digest and send
// immediately. For empty or zero DigestInterval, notifications also
// send immediately. Otherwise, tickets are accumulated in
// per-declaration digest buffers and flushed on the declaration's
// configured interval.
//
// Must be called with ts.mu held (reads/writes digestTimers and
// membersByRoom).
func (ts *TicketService) queueStewardshipNotifications(
	ctx context.Context,
	ticketRoomID ref.RoomID,
	ticketID string,
	content ticket.TicketContent,
	declarations []stewardshipindex.Declaration,
) {
	for _, declaration := range declarations {
		// P0/P1 tickets always bypass digest batching.
		if content.Priority <= 1 {
			ts.sendImmediateNotification(ctx, declaration, ticketRoomID, ticketID, content)
			continue
		}

		// Empty or zero DigestInterval means immediate.
		interval := parseDuration(declaration.Content.DigestInterval)
		if interval <= 0 {
			ts.sendImmediateNotification(ctx, declaration, ticketRoomID, ticketID, content)
			continue
		}

		// Accumulate in digest buffer.
		key := digestKey{
			roomID:   declaration.RoomID,
			stateKey: declaration.StateKey,
		}
		entry, exists := ts.digestTimers[key]
		if !exists {
			entry = &digestEntry{declaration: declaration}
			ts.digestTimers[key] = entry
		}

		entry.tickets = append(entry.tickets, digestTicket{
			ticketID:     ticketID,
			title:        content.Title,
			ticketType:   content.Type,
			priority:     content.Priority,
			ticketRoomID: ticketRoomID,
		})

		// Start a flush timer if this is the first ticket in the
		// accumulator (no active timer).
		if entry.timer == nil {
			entry.flushAt = ts.clock.Now().Add(interval)
			entry.timer = ts.clock.AfterFunc(interval, ts.signalDigestNotify)
		}
	}
}

// sendImmediateNotification sends a notification message to the
// declaration's room mentioning all steward principals resolved from
// room membership.
func (ts *TicketService) sendImmediateNotification(
	ctx context.Context,
	declaration stewardshipindex.Declaration,
	ticketRoomID ref.RoomID,
	ticketID string,
	content ticket.TicketContent,
) {
	principals := ts.resolveNotificationPrincipals(declaration)
	if len(principals) == 0 {
		ts.logger.Warn("stewardship notification: no principals resolved",
			"state_key", declaration.StateKey,
			"room_id", declaration.RoomID,
		)
		return
	}

	message := formatImmediateNotification(ticketID, content, ticketRoomID, principals)
	_, err := ts.messenger.SendMessage(ctx, declaration.RoomID, message)
	if err != nil {
		ts.logger.Error("failed to send stewardship notification",
			"ticket_id", ticketID,
			"state_key", declaration.StateKey,
			"room_id", declaration.RoomID,
			"error", err,
		)
	} else {
		ts.logger.Info("sent stewardship notification",
			"ticket_id", ticketID,
			"state_key", declaration.StateKey,
			"room_id", declaration.RoomID,
		)
	}
}

// resolveNotificationPrincipals resolves all stewardship tier
// principal patterns against the declaration's room membership and
// returns the matched user IDs. Deduplicates across tiers.
func (ts *TicketService) resolveNotificationPrincipals(
	declaration stewardshipindex.Declaration,
) []ref.UserID {
	members := ts.membersByRoom[declaration.RoomID]
	if len(members) == 0 {
		return nil
	}

	seen := make(map[ref.UserID]bool)
	var principals []ref.UserID

	for _, tier := range declaration.Content.Tiers {
		for _, pattern := range tier.Principals {
			for userID := range members {
				if seen[userID] {
					continue
				}
				if principal.MatchUserID(pattern, userID.String()) {
					seen[userID] = true
					principals = append(principals, userID)
				}
			}
		}
	}

	// Sort for deterministic message output.
	sort.Slice(principals, func(i, j int) bool {
		return principals[i].String() < principals[j].String()
	})

	return principals
}

// --- Digest loop ---

// startDigestLoop runs the event-driven digest loop. Blocks until
// ctx is cancelled. On shutdown, flushes all pending digests before
// returning. Parallel to startTimerLoop.
func (ts *TicketService) startDigestLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			ts.mu.Lock()
			ts.flushAllDigestsLocked(ctx)
			ts.mu.Unlock()
			return
		case <-ts.digestNotify:
			ts.mu.Lock()
			ts.fireExpiredDigestsLocked(ctx)
			ts.mu.Unlock()
		}
	}
}

// signalDigestNotify sends a non-blocking signal to the digest loop.
// Safe to call from AfterFunc callbacks (does not acquire ts.mu).
func (ts *TicketService) signalDigestNotify() {
	select {
	case ts.digestNotify <- struct{}{}:
	default:
	}
}

// fireExpiredDigestsLocked flushes all digest entries whose timers
// have expired. Must be called with ts.mu held.
func (ts *TicketService) fireExpiredDigestsLocked(ctx context.Context) {
	now := ts.clock.Now()
	for key, entry := range ts.digestTimers {
		if entry.timer == nil || len(entry.tickets) == 0 {
			continue
		}
		if now.Before(entry.flushAt) {
			continue
		}
		ts.flushDigestEntryLocked(ctx, key, entry)
	}
}

// flushAllDigestsLocked flushes every pending digest entry. Called
// on shutdown to avoid silently dropping accumulated notifications.
// Must be called with ts.mu held.
func (ts *TicketService) flushAllDigestsLocked(ctx context.Context) {
	for key, entry := range ts.digestTimers {
		if len(entry.tickets) == 0 {
			continue
		}
		ts.flushDigestEntryLocked(ctx, key, entry)
	}
}

// flushDigestEntryLocked formats and sends a digest notification
// message to the declaration's room, then resets the accumulator.
// Must be called with ts.mu held.
func (ts *TicketService) flushDigestEntryLocked(
	ctx context.Context,
	key digestKey,
	entry *digestEntry,
) {
	principals := ts.resolveNotificationPrincipals(entry.declaration)
	if len(principals) == 0 {
		ts.logger.Warn("stewardship digest flush: no principals resolved",
			"state_key", entry.declaration.StateKey,
			"room_id", entry.declaration.RoomID,
			"tickets", len(entry.tickets),
		)
		entry.tickets = nil
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		return
	}

	tickets := entry.tickets
	message := formatDigestNotification(entry.declaration, tickets, principals)

	// Clear accumulator before sending so we don't double-flush
	// if the send is slow and another signal arrives.
	entry.tickets = nil
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}

	_, err := ts.messenger.SendMessage(ctx, entry.declaration.RoomID, message)
	if err != nil {
		ts.logger.Error("failed to send stewardship digest",
			"state_key", entry.declaration.StateKey,
			"room_id", entry.declaration.RoomID,
			"tickets", len(tickets),
			"error", err,
		)
	} else {
		ts.logger.Info("sent stewardship digest",
			"state_key", entry.declaration.StateKey,
			"room_id", entry.declaration.RoomID,
			"tickets", len(tickets),
		)
	}
}

// flushAndRemoveDigest flushes a pending digest for the given
// declaration (if any) and removes it from the map. Called when a
// stewardship declaration is removed from the index.
// Must be called with ts.mu held.
func (ts *TicketService) flushAndRemoveDigest(ctx context.Context, key digestKey) {
	entry, exists := ts.digestTimers[key]
	if !exists {
		return
	}
	if len(entry.tickets) > 0 {
		ts.flushDigestEntryLocked(ctx, key, entry)
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	delete(ts.digestTimers, key)
}

// --- Message formatting ---

// formatImmediateNotification builds a MessageContent for a single
// ticket's immediate notification. Includes ticket metadata and
// @-mentions of steward principals.
func formatImmediateNotification(
	ticketID string,
	content ticket.TicketContent,
	ticketRoomID ref.RoomID,
	principals []ref.UserID,
) messaging.MessageContent {
	var builder strings.Builder

	fmt.Fprintf(&builder, "Stewardship notification: %s", ticketID)
	if content.Title != "" {
		fmt.Fprintf(&builder, " — %s", content.Title)
	}
	builder.WriteString("\n")

	fmt.Fprintf(&builder, "Type: %s", content.Type)
	if content.Priority >= 0 {
		fmt.Fprintf(&builder, " | Priority: P%d", content.Priority)
	}
	builder.WriteString("\n")

	if len(content.Affects) > 0 {
		fmt.Fprintf(&builder, "Affects: %s\n", strings.Join(content.Affects, ", "))
	}

	// Mention steward principals.
	builder.WriteString("\nStewards: ")
	mentionStrings := make([]string, len(principals))
	for index, userID := range principals {
		mentionStrings[index] = userID.String()
	}
	builder.WriteString(strings.Join(mentionStrings, " "))

	return messaging.MessageContent{
		MsgType: "m.text",
		Body:    builder.String(),
		Mentions: &messaging.Mentions{
			UserIDs: mentionStrings,
		},
	}
}

// formatDigestNotification builds a MessageContent for a batched
// digest notification containing all accumulated tickets.
func formatDigestNotification(
	declaration stewardshipindex.Declaration,
	tickets []digestTicket,
	principals []ref.UserID,
) messaging.MessageContent {
	var builder strings.Builder

	description := declaration.StateKey
	if declaration.Content.Description != "" {
		description = declaration.Content.Description
	}
	fmt.Fprintf(&builder, "Stewardship digest for %s (%d tickets)\n", description, len(tickets))

	for _, ticket := range tickets {
		fmt.Fprintf(&builder, "\n- %s: %s (%s, P%d)",
			ticket.ticketID,
			ticket.title,
			ticket.ticketType,
			ticket.priority,
		)
	}

	// Mention steward principals.
	builder.WriteString("\n\nStewards: ")
	mentionStrings := make([]string, len(principals))
	for index, userID := range principals {
		mentionStrings[index] = userID.String()
	}
	builder.WriteString(strings.Join(mentionStrings, " "))

	return messaging.MessageContent{
		MsgType: "m.text",
		Body:    builder.String(),
		Mentions: &messaging.Mentions{
			UserIDs: mentionStrings,
		},
	}
}

// --- Helpers ---

// parseDuration parses a Go duration string, returning 0 for empty
// or invalid values. The stewardship schema Validate() already
// rejects invalid DigestInterval values, so this is a best-effort
// fallback for runtime.
func parseDuration(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0
	}
	return duration
}
