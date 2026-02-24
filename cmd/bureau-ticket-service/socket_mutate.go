// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/cron"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Ticket ID generation ---

// ticketPrefix returns the ID prefix for a ticket based on its type
// and the room configuration. Type-specific prefixes (e.g., "pip" for
// pipeline tickets) take precedence over the room's configured prefix.
// Falls back to "tkt" if neither the type nor the room config specifies
// a prefix.
func ticketPrefix(config *ticket.TicketConfigContent, ticketType string) string {
	if typePrefix := ticket.PrefixForType(ticketType); typePrefix != "" {
		return typePrefix
	}
	if config.Prefix != "" {
		return config.Prefix
	}
	return "tkt"
}

// checkAllowedType validates that the given ticket type is permitted by
// the room's AllowedTypes configuration. Returns nil if the type is
// allowed (either AllowedTypes is empty, meaning all types are allowed,
// or the type appears in the list). Returns a descriptive error if the
// type is restricted.
func checkAllowedType(config *ticket.TicketConfigContent, ticketType string) error {
	if len(config.AllowedTypes) == 0 {
		return nil
	}
	for _, allowed := range config.AllowedTypes {
		if allowed == ticketType {
			return nil
		}
	}
	return fmt.Errorf("ticket type %q is not allowed in this room (allowed: %v)", ticketType, config.AllowedTypes)
}

// generateTicketID produces a unique ticket ID by hashing the room ID,
// creation timestamp, and title, then truncating to the shortest prefix
// that avoids collision with existing tickets and the exclusion set.
// The exclusion set handles intra-batch collisions when generating
// multiple IDs in a single batch-create call; pass nil for single creates.
func (ts *TicketService) generateTicketID(state *roomState, ticketType, roomID, timestamp, title string, exclude map[string]struct{}) string {
	prefix := ticketPrefix(state.config, ticketType)
	input := roomID + "\n" + timestamp + "\n" + title
	hash := sha256.Sum256([]byte(input))
	hexHash := hex.EncodeToString(hash[:])

	for length := 4; length <= len(hexHash); length++ {
		candidate := prefix + "-" + hexHash[:length]
		if _, exists := state.index.Get(candidate); exists {
			continue
		}
		if _, excluded := exclude[candidate]; excluded {
			continue
		}
		return candidate
	}
	// SHA-256 provides 64 hex chars. Exhausting every prefix length
	// while colliding each time requires 2^128 existing tickets.
	return prefix + "-" + hexHash
}

// --- Gate synthesis ---

// synthesizeRecurringGate builds a timer gate from the convenience
// schedule or interval fields on a create request. Exactly one of
// schedule or interval must be non-empty (the caller enforces mutual
// exclusivity). The returned gate has a computed initial Target and
// is ready to be appended to the request's Gates slice.
func (ts *TicketService) synthesizeRecurringGate(schedule, interval string) (ticket.TicketGate, error) {
	now := ts.clock.Now()

	gate := ticket.TicketGate{
		Type:   "timer",
		Status: "pending",
	}

	if schedule != "" {
		parsed, err := cron.Parse(schedule)
		if err != nil {
			return ticket.TicketGate{}, fmt.Errorf("invalid schedule %q: %w", schedule, err)
		}
		target, err := parsed.Next(now)
		if err != nil {
			return ticket.TicketGate{}, fmt.Errorf("cannot compute first cron occurrence for %q: %w", schedule, err)
		}
		gate.ID = "schedule"
		gate.Schedule = schedule
		gate.Target = target.UTC().Format(time.RFC3339)
	} else {
		duration, err := time.ParseDuration(interval)
		if err != nil {
			return ticket.TicketGate{}, fmt.Errorf("invalid interval %q: %w", interval, err)
		}
		if duration < ticket.MinTimerRecurrence {
			return ticket.TicketGate{}, fmt.Errorf("interval must be >= %s", ticket.MinTimerRecurrence)
		}
		gate.ID = "interval"
		gate.Interval = interval
		gate.Target = now.Add(duration).UTC().Format(time.RFC3339)
	}

	return gate, nil
}

// synthesizeDeferGate builds a one-shot timer gate with ID "defer"
// from the convenience defer_until or defer_for fields on a create
// request. Exactly one must be non-empty (the caller enforces mutual
// exclusivity).
func (ts *TicketService) synthesizeDeferGate(until, forDuration string) (ticket.TicketGate, error) {
	now := ts.clock.Now()

	var target time.Time
	if until != "" {
		parsed, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return ticket.TicketGate{}, fmt.Errorf("invalid defer_until time: %w", err)
		}
		if !parsed.After(now) {
			return ticket.TicketGate{}, fmt.Errorf("defer_until time %s is not in the future", until)
		}
		target = parsed
	} else {
		duration, err := time.ParseDuration(forDuration)
		if err != nil {
			return ticket.TicketGate{}, fmt.Errorf("invalid defer_for duration: %w", err)
		}
		if duration <= 0 {
			return ticket.TicketGate{}, errors.New("defer_for duration must be positive")
		}
		target = now.Add(duration)
	}

	return ticket.TicketGate{
		ID:     "defer",
		Type:   "timer",
		Status: "pending",
		Target: target.UTC().Format(time.RFC3339),
	}, nil
}

// --- Status transition validation ---

// validateStatusTransition checks whether a status transition is allowed.
// Returns nil if the transition is valid, or an error describing why it
// is rejected. The currentAssignee is included in contention error
// messages so the caller knows who owns the ticket.
//
// Allowed transitions:
//   - open -> in_progress (caller must also provide assignee)
//   - open -> review (direct review request, e.g. PM use case)
//   - open -> closed (wontfix, duplicate)
//   - in_progress -> open (unclaim)
//   - in_progress -> review (author requests review)
//   - in_progress -> closed (done)
//   - in_progress -> blocked (agent hit blocker)
//   - in_progress -> in_progress: REJECTED (contention)
//   - review -> in_progress (author iterates after feedback)
//   - review -> open (drop review, release to pool)
//   - review -> closed (approved and done)
//   - review -> blocked (external blocker during review)
//   - review -> review: REJECTED (contention)
//   - blocked -> open (release back)
//   - blocked -> in_progress (resume, caller must provide assignee)
//   - blocked -> review (resume directly into review)
//   - blocked -> closed (cancelled while blocked)
//   - closed -> open (reopen)
func validateStatusTransition(currentStatus, proposedStatus ticket.TicketStatus, currentAssignee ref.UserID) error {
	if currentStatus == proposedStatus {
		switch currentStatus {
		case ticket.StatusInProgress:
			return fmt.Errorf("ticket is already in_progress (assigned to %s)", currentAssignee)
		case ticket.StatusReview:
			return fmt.Errorf("ticket is already in review (assigned to %s)", currentAssignee)
		}
		return nil
	}

	switch currentStatus {
	case ticket.StatusOpen:
		switch proposedStatus {
		case ticket.StatusInProgress, ticket.StatusReview, ticket.StatusClosed:
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case ticket.StatusInProgress:
		switch proposedStatus {
		case ticket.StatusOpen, ticket.StatusReview, ticket.StatusClosed, ticket.StatusBlocked:
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case ticket.StatusReview:
		switch proposedStatus {
		case ticket.StatusInProgress, ticket.StatusOpen, ticket.StatusClosed, ticket.StatusBlocked:
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case ticket.StatusBlocked:
		switch proposedStatus {
		case ticket.StatusOpen, ticket.StatusInProgress, ticket.StatusReview, ticket.StatusClosed:
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case ticket.StatusClosed:
		switch proposedStatus {
		case ticket.StatusOpen:
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	default:
		return fmt.Errorf("unknown current status: %s", currentStatus)
	}
}

// --- Mutation request types ---

// createRequest is the input for the "create" action. New tickets
// always start with status "open" and no assignee.
//
// The Schedule and Interval fields are convenience shortcuts that
// synthesize a recurring timer gate without the caller needing to
// construct a full TicketGate object. When set, the service creates
// a timer gate with the appropriate recurrence field and a computed
// initial Target. These are mutually exclusive with each other and
// additive with any explicit Gates in the request.
type createRequest struct {
	Room      string                           `cbor:"room"`
	Title     string                           `cbor:"title"`
	Body      string                           `cbor:"body,omitempty"`
	Type      string                           `cbor:"type"`
	Priority  int                              `cbor:"priority"`
	Labels    []string                         `cbor:"labels,omitempty"`
	Parent    string                           `cbor:"parent,omitempty"`
	BlockedBy []string                         `cbor:"blocked_by,omitempty"`
	Gates     []ticket.TicketGate              `cbor:"gates,omitempty"`
	Affects   []string                         `cbor:"affects,omitempty"`
	Origin    *ticket.TicketOrigin             `cbor:"origin,omitempty"`
	Pipeline  *ticket.PipelineExecutionContent `cbor:"pipeline,omitempty"`

	// Schedule is a cron expression (e.g., "0 7 * * *") that
	// creates a recurring timer gate with ID "schedule". The
	// initial Target is the next cron occurrence after now.
	Schedule string `cbor:"schedule,omitempty"`

	// Interval is a Go duration (e.g., "4h") that creates a
	// recurring timer gate with ID "interval". The initial
	// Target is now + interval.
	Interval string `cbor:"interval,omitempty"`

	// DeferUntil is an RFC 3339 timestamp that creates a one-shot
	// timer gate with ID "defer". The ticket won't be ready until
	// this time passes. Mutually exclusive with DeferFor.
	DeferUntil string `cbor:"defer_until,omitempty"`

	// DeferFor is a Go duration (e.g., "24h") that creates a
	// one-shot timer gate with ID "defer" targeting now + duration.
	// Mutually exclusive with DeferUntil.
	DeferFor string `cbor:"defer_for,omitempty"`

	// Deadline is the target completion time (RFC 3339 UTC). The
	// ticket service monitors deadlines and adds a note when a
	// ticket remains open past its deadline. Deadlines are
	// informational — they do not affect readiness.
	Deadline string `cbor:"deadline,omitempty"`
}

// updateRequest is the input for the "update" action. Pointer fields
// distinguish "not provided" (nil) from "set to zero value" (non-nil
// pointing to the zero value). Only non-nil fields are applied.
type updateRequest struct {
	Room      string      `cbor:"room,omitempty"`
	Ticket    string      `cbor:"ticket"`
	Title     *string     `cbor:"title,omitempty"`
	Body      *string     `cbor:"body,omitempty"`
	Status    *string     `cbor:"status,omitempty"`
	Priority  *int        `cbor:"priority,omitempty"`
	Type      *string     `cbor:"type,omitempty"`
	Labels    *[]string   `cbor:"labels,omitempty"`
	Assignee  *ref.UserID `cbor:"assignee,omitempty"`
	Parent    *string     `cbor:"parent,omitempty"`
	BlockedBy *[]string   `cbor:"blocked_by,omitempty"`
	Affects   *[]string   `cbor:"affects,omitempty"`
	Deadline  *string     `cbor:"deadline,omitempty"`

	// Review replaces the ticket's review content. When provided,
	// the entire TicketReview is replaced. Use this to set up
	// reviewers and scope before transitioning to "review" status,
	// or in the same mutation as the status transition.
	Review *ticket.TicketReview `cbor:"review,omitempty"`

	// Pipeline replaces the ticket's pipeline execution content.
	// Only valid for pipeline-type tickets. When provided, the
	// entire PipelineExecutionContent is replaced. The executor
	// uses this to update progress (current_step, conclusion).
	Pipeline *ticket.PipelineExecutionContent `cbor:"pipeline,omitempty"`
}

// closeRequest is the input for the "close" action.
type closeRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
	Reason string `cbor:"reason,omitempty"`

	// EndRecurrence stops recurring timer gates from re-arming on
	// close. When true, recurring gates are removed from the ticket
	// before closing, so the ticket stays closed permanently. When
	// false (default), tickets with recurring timer gates are
	// automatically re-armed: the gate is reset to pending with a
	// new Target, and the ticket reopens as "open" instead of
	// closing.
	EndRecurrence bool `cbor:"end_recurrence,omitempty"`
}

// reopenRequest is the input for the "reopen" action.
type reopenRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
}

// batchCreateRequest is the input for the "batch-create" action.
type batchCreateRequest struct {
	Room    string             `cbor:"room"`
	Tickets []batchCreateEntry `cbor:"tickets"`
}

// batchCreateEntry is a single ticket in a batch-create request.
// The Ref field is a symbolic name used for intra-batch blocked_by
// and parent references.
type batchCreateEntry struct {
	Ref       string                           `cbor:"ref"`
	Title     string                           `cbor:"title"`
	Body      string                           `cbor:"body,omitempty"`
	Type      string                           `cbor:"type"`
	Priority  int                              `cbor:"priority"`
	Labels    []string                         `cbor:"labels,omitempty"`
	Parent    string                           `cbor:"parent,omitempty"`
	BlockedBy []string                         `cbor:"blocked_by,omitempty"`
	Affects   []string                         `cbor:"affects,omitempty"`
	Gates     []ticket.TicketGate              `cbor:"gates,omitempty"`
	Origin    *ticket.TicketOrigin             `cbor:"origin,omitempty"`
	Pipeline  *ticket.PipelineExecutionContent `cbor:"pipeline,omitempty"`
}

// importRequest is the input for the "import" action.
type importRequest struct {
	Room    string        `cbor:"room"`
	Tickets []importEntry `cbor:"tickets"`
}

// importEntry is a single ticket in an import request. Unlike
// batchCreateEntry, the ID is caller-specified (preserved from export)
// and the content is the full TicketContent (preserving status,
// timestamps, assignee, notes, etc.).
type importEntry struct {
	ID      string               `cbor:"id"`
	Content ticket.TicketContent `cbor:"content"`
}

// importResponse is returned by the "import" action.
type importResponse struct {
	Room     string `json:"room"`
	Imported int    `json:"imported"`
}

// resolveGateRequest is the input for the "resolve-gate" action.
type resolveGateRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
	Gate   string `cbor:"gate"`
}

// updateGateRequest is the input for the "update-gate" action.
type updateGateRequest struct {
	Room        string `cbor:"room,omitempty"`
	Ticket      string `cbor:"ticket"`
	Gate        string `cbor:"gate"`
	Status      string `cbor:"status"`
	SatisfiedBy string `cbor:"satisfied_by,omitempty"`
}

// addNoteRequest is the input for the "add-note" action. Appends a
// new note to an existing ticket. The note ID and timestamps are
// assigned by the service; the caller provides only the body.
type addNoteRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
	Body   string `cbor:"body"`
}

// deferRequest is the input for the "defer" action. Exactly one of
// Until or For must be set. If the ticket already has a gate with
// ID "defer", its Target is updated. Otherwise a new timer gate is
// created.
type deferRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
	Until  string `cbor:"until,omitempty"`
	For    string `cbor:"for,omitempty"`
}

// setDispositionRequest is the input for the "set-disposition" action.
// Updates the calling reviewer's disposition on a ticket in review
// status. The caller (token.Subject) must be in the ticket's reviewer
// list.
type setDispositionRequest struct {
	Room        string `cbor:"room,omitempty"`
	Ticket      string `cbor:"ticket"`
	Disposition string `cbor:"disposition"`
}

// --- Mutation response types ---

// createResponse is returned by the "create" action.
type createResponse struct {
	ID   string `json:"id"`
	Room string `json:"room"`
}

// batchCreateResponse is returned by the "batch-create" action.
type batchCreateResponse struct {
	Room string            `json:"room"`
	Refs map[string]string `json:"refs"`
}

// mutationResponse is the common response for update, close, reopen,
// and gate operations. Returns the full updated content so the caller
// does not need a separate show call.
type mutationResponse struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content ticket.TicketContent `json:"content"`
}

// --- Mutation handlers ---

// handleCreate creates a new ticket in a room. The ticket starts with
// status "open" and the ID is derived from the room, timestamp, and
// title.
func (ts *TicketService) handleCreate(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/create"); err != nil {
		return nil, err
	}

	var request createRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	// Synthesize a recurring timer gate from convenience fields.
	if request.Schedule != "" && request.Interval != "" {
		return nil, errors.New("schedule and interval are mutually exclusive")
	}
	if request.Schedule != "" || request.Interval != "" {
		gate, err := ts.synthesizeRecurringGate(request.Schedule, request.Interval)
		if err != nil {
			return nil, err
		}
		request.Gates = append(request.Gates, gate)
	}

	// Synthesize a defer gate from convenience fields.
	if request.DeferUntil != "" && request.DeferFor != "" {
		return nil, errors.New("defer_until and defer_for are mutually exclusive")
	}
	if request.DeferUntil != "" || request.DeferFor != "" {
		gate, err := ts.synthesizeDeferGate(request.DeferUntil, request.DeferFor)
		if err != nil {
			return nil, err
		}
		request.Gates = append(request.Gates, gate)
	}

	roomID, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	// Enforce room-level type restrictions.
	if err := checkAllowedType(state.config, request.Type); err != nil {
		return nil, err
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     request.Title,
		Body:      request.Body,
		Status:    ticket.StatusOpen,
		Priority:  request.Priority,
		Type:      request.Type,
		Labels:    request.Labels,
		Affects:   request.Affects,
		Parent:    request.Parent,
		BlockedBy: request.BlockedBy,
		Gates:     request.Gates,
		Origin:    request.Origin,
		Pipeline:  request.Pipeline,
		Deadline:  request.Deadline,
		CreatedBy: token.Subject,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Auto-configure stewardship review gates from matching
	// declarations. This resolves principal patterns against room
	// membership and appends review gates before timestamp
	// enrichment so stewardship gates get CreatedAt populated by
	// the same loop as explicit gates.
	if len(content.Affects) > 0 {
		stewardship := ts.resolveStewardshipGates(content.Affects, content.Type, content.Priority)
		content.Gates = append(content.Gates, stewardship.gates...)
		if len(stewardship.reviewers) > 0 {
			if content.Review == nil {
				content.Review = &ticket.TicketReview{}
			}
			content.Review.Reviewers = append(content.Review.Reviewers, stewardship.reviewers...)
			content.Review.TierThresholds = stewardship.thresholds
		}
	}

	// Enrich gates with creation timestamps.
	for i := range content.Gates {
		if content.Gates[i].CreatedAt == "" {
			content.Gates[i].CreatedAt = now
		}
	}

	// Compute timer targets from Duration and Base. For base="created"
	// gates this sets Target = CreatedAt + Duration immediately. For
	// base="unblocked" gates, Target is only set if all blockers are
	// already closed; otherwise it stays empty until blockers clear.
	enrichTimerTargets(&content, state.index)

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	// Verify blocked_by tickets exist in this room.
	for _, blockerID := range content.BlockedBy {
		if _, exists := state.index.Get(blockerID); !exists {
			return nil, fmt.Errorf("blocked_by ticket %s not found in room", blockerID)
		}
	}

	// Verify parent exists if specified.
	if content.Parent != "" {
		if _, exists := state.index.Get(content.Parent); !exists {
			return nil, fmt.Errorf("parent ticket %s not found in room", content.Parent)
		}
	}

	ticketID := ts.generateTicketID(state, request.Type, request.Room, now, content.Title, nil)

	// Write to Matrix and update the local index so the creator sees
	// the result without a /sync round-trip. putWithEcho records the
	// event ID so the sync loop won't overwrite this with stale data.
	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	// Push any timer gates to the heap so the timer loop fires
	// them at the right time.
	ts.pushTimerGates(roomID, ticketID, &content)

	// Push a deadline monitoring entry if set.
	ts.pushDeadlineEntry(roomID, ticketID, &content)

	return createResponse{
		ID:   ticketID,
		Room: request.Room,
	}, nil
}

// handleUpdate applies a partial update to an existing ticket. Only
// fields present in the request are modified. Status transitions are
// validated and lifecycle fields (closed_at, assignee) are managed
// automatically.
func (ts *TicketService) handleUpdate(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request updateRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)

	// Apply non-lifecycle field updates.
	if request.Title != nil {
		content.Title = *request.Title
	}
	if request.Body != nil {
		content.Body = *request.Body
	}
	if request.Priority != nil {
		content.Priority = *request.Priority
	}
	if request.Type != nil {
		oldType := content.Type
		content.Type = *request.Type

		// Auto-clear pipeline content when changing away from pipeline
		// type. The CBOR request format uses nil to mean "not provided",
		// so there is no way for the caller to explicitly clear it.
		if oldType == "pipeline" && content.Type != "pipeline" {
			content.Pipeline = nil
		}

		// Enforce room-level type restrictions.
		if err := checkAllowedType(state.config, content.Type); err != nil {
			return nil, err
		}
	}
	if request.Labels != nil {
		content.Labels = *request.Labels
	}
	if request.Parent != nil {
		content.Parent = *request.Parent
	}
	if request.BlockedBy != nil {
		content.BlockedBy = *request.BlockedBy
	}
	if request.Deadline != nil {
		content.Deadline = *request.Deadline
	}
	if request.Review != nil {
		content.Review = request.Review
	}
	if request.Pipeline != nil {
		content.Pipeline = request.Pipeline
	}
	if request.Affects != nil {
		content.Affects = *request.Affects
	}

	// Re-resolve stewardship if Affects or Type changed. The old
	// stewardship gates are removed and replaced with gates
	// derived from the updated Affects and Type. Manual reviewers
	// (those not produced by stewardship resolution) are preserved.
	if request.Affects != nil || request.Type != nil {
		content.Gates = removeStewardshipGates(content.Gates)
		if len(content.Affects) > 0 {
			stewardship := ts.resolveStewardshipGates(content.Affects, content.Type, content.Priority)
			content.Gates = append(content.Gates, stewardship.gates...)
			mergeStewardshipReview(&content, stewardship.reviewers, stewardship.thresholds)
		} else {
			// Affects cleared — remove stewardship reviewers by
			// merging with an empty set.
			mergeStewardshipReview(&content, nil, nil)
		}

		// Enrich stewardship gate timestamps.
		for i := range content.Gates {
			if strings.HasPrefix(content.Gates[i].ID, "stewardship:") && content.Gates[i].CreatedAt == "" {
				content.Gates[i].CreatedAt = now
			}
		}
	}

	// Determine the proposed status and assignee, starting from
	// current values and applying any requested changes.
	proposedStatus := content.Status
	if request.Status != nil {
		proposedStatus = ticket.TicketStatus(*request.Status)
	}
	proposedAssignee := content.Assignee
	if request.Assignee != nil {
		proposedAssignee = *request.Assignee
	}

	// Validate status transition if the client explicitly sent a status
	// field. We check even when proposedStatus == content.Status because
	// in_progress → in_progress is a contention error that must be
	// caught.
	if request.Status != nil {
		if err := validateStatusTransition(content.Status, proposedStatus, content.Assignee); err != nil {
			return nil, err
		}

		// Closing and reopening require dedicated grants so that
		// operators can give workers ticket/update without letting
		// them close or reopen. Without these checks, a caller
		// could bypass handleClose's ticket/close check by calling
		// update with status:"closed".
		if proposedStatus == ticket.StatusClosed && content.Status != ticket.StatusClosed {
			if err := requireGrant(token, "ticket/close"); err != nil {
				return nil, err
			}
		}
		if content.Status == ticket.StatusClosed && proposedStatus != ticket.StatusClosed {
			if err := requireGrant(token, "ticket/reopen"); err != nil {
				return nil, err
			}
		}

		// Enforce review field when entering "review" status.
		// Review must be present (either from this request or
		// pre-populated) with at least one reviewer. The full
		// validation in Validate() catches this too, but a
		// specific error message here is more helpful.
		if proposedStatus == ticket.StatusReview && content.Status != ticket.StatusReview {
			if content.Review == nil || len(content.Review.Reviewers) == 0 {
				return nil, errors.New("reviewers are required when entering review status")
			}
		}

		if proposedStatus != content.Status {
			// Auto-clear assignee when leaving in_progress or
			// review for a non-active status. Assignee persists
			// across in_progress ↔ review transitions (the author
			// stays the same). Cleared when going to open, closed,
			// or blocked.
			if content.Status == ticket.StatusInProgress && proposedStatus != ticket.StatusInProgress && proposedStatus != ticket.StatusReview {
				proposedAssignee = ref.UserID{}
			}
			if content.Status == ticket.StatusReview && proposedStatus != ticket.StatusInProgress && proposedStatus != ticket.StatusReview {
				proposedAssignee = ref.UserID{}
			}

			// Auto-manage lifecycle fields for closed transitions.
			if proposedStatus == ticket.StatusClosed {
				content.ClosedAt = now
			}
			if content.Status == ticket.StatusClosed && proposedStatus != ticket.StatusClosed {
				content.ClosedAt = ""
				content.CloseReason = ""
			}
		}
	}

	// Enforce assignee/status coupling: in_progress requires an
	// assignee. Review MAY have an assignee (preserved from
	// in_progress, or set explicitly) but does not require one.
	// An assignee is only valid on in_progress or review tickets.
	if proposedStatus == ticket.StatusInProgress && proposedAssignee.IsZero() {
		return nil, errors.New("assignee is required when status is in_progress")
	}
	if !proposedAssignee.IsZero() && proposedStatus != ticket.StatusInProgress && proposedStatus != ticket.StatusReview {
		return nil, fmt.Errorf("assignee can only be set when status is in_progress or review (status is %q)", proposedStatus)
	}

	content.Status = proposedStatus
	content.Assignee = proposedAssignee

	// Check for dependency cycles if blocked_by changed.
	if request.BlockedBy != nil && len(content.BlockedBy) > 0 {
		if state.index.WouldCycle(ticketID, content.BlockedBy) {
			return nil, errors.New("blocked_by would create a dependency cycle")
		}
		for _, blockerID := range content.BlockedBy {
			if _, exists := state.index.Get(blockerID); !exists {
				return nil, fmt.Errorf("blocked_by ticket %s not found in room", blockerID)
			}
		}
	}

	// Verify parent exists if changed.
	if request.Parent != nil && content.Parent != "" {
		if _, exists := state.index.Get(content.Parent); !exists {
			return nil, fmt.Errorf("parent ticket %s not found in room", content.Parent)
		}
	}

	content.UpdatedAt = now

	// If this update closes a ticket with recurring timer gates,
	// re-arm instead of closing. Same logic as handleClose — the
	// close behavior should be consistent regardless of which
	// handler processes it. The update handler doesn't expose
	// EndRecurrence; use the dedicated close handler for that.
	if content.Status == ticket.StatusClosed && hasRecurringGates(&content) {
		rearmed, err := rearmRecurringGates(&content, ts.clock.Now())
		if err != nil {
			return nil, fmt.Errorf("re-arming recurring gates: %w", err)
		}
		if rearmed {
			content.Status = ticket.StatusOpen
			content.Assignee = ref.UserID{}
			content.ClosedAt = ""
			content.CloseReason = ""

			if err := content.Validate(); err != nil {
				return nil, fmt.Errorf("invalid ticket after re-arm: %w", err)
			}

			if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
				return nil, fmt.Errorf("writing re-armed ticket to Matrix: %w", err)
			}

			ts.pushTimerGates(roomID, ticketID, &content)

			return mutationResponse{
				ID:      ticketID,
				Room:    roomID.String(),
				Content: content,
			}, nil
		}
		// All recurring gates exhausted — fall through to normal write.
	}

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	// If this update closed the ticket, resolve timer targets for
	// dependents that may have become unblocked. Idempotent with the
	// sync loop's echo processing.
	if content.Status == ticket.StatusClosed {
		ts.resolveUnblockedTimerTargets(ctx, roomID, state, []string{ticketID})
	}

	// Push a deadline monitoring entry if the deadline was changed.
	// Stale entries from the old deadline are handled by lazy
	// deletion in fireExpiredTimersLocked.
	if request.Deadline != nil {
		ts.pushDeadlineEntry(roomID, ticketID, &content)
	}

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleClose transitions a ticket to "closed" with an optional reason.
// Validates that the current status allows closing and auto-clears the
// assignee if the ticket was in_progress.
//
// For tickets with recurring timer gates (Schedule or Interval set),
// close performs an atomic re-arm: the gate is reset to pending with a
// new Target for the next occurrence, and the ticket reopens as "open"
// instead of closing. The caller sees a normal mutation response
// containing the (now open, re-armed) ticket. If all recurring gates
// have exhausted their MaxOccurrences limit, the ticket closes
// normally.
//
// The EndRecurrence field on the request overrides re-arm behavior:
// when true, recurring gates are removed and the ticket closes
// permanently regardless of schedule.
func (ts *TicketService) handleClose(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/close"); err != nil {
		return nil, err
	}

	var request closeRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	if err := validateStatusTransition(content.Status, ticket.StatusClosed, content.Assignee); err != nil {
		return nil, err
	}

	now := ts.clock.Now()
	nowStr := now.UTC().Format(time.RFC3339)

	// Handle EndRecurrence: strip recurring gates before closing so
	// the ticket stays closed permanently.
	if request.EndRecurrence {
		stripRecurringGates(&content)
	}

	// Check for recurring timer gates that should re-arm instead of
	// letting the ticket close.
	if hasRecurringGates(&content) {
		rearmed, err := rearmRecurringGates(&content, now)
		if err != nil {
			return nil, fmt.Errorf("re-arming recurring gates: %w", err)
		}
		if rearmed {
			// Ticket reopens with re-armed gates instead of closing.
			content.Status = ticket.StatusOpen
			content.Assignee = ref.UserID{}
			content.UpdatedAt = nowStr
			// Do not set ClosedAt or CloseReason — the ticket is
			// not actually closing.

			if err := content.Validate(); err != nil {
				return nil, fmt.Errorf("invalid ticket after re-arm: %w", err)
			}

			if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
				return nil, fmt.Errorf("writing re-armed ticket to Matrix: %w", err)
			}

			// Push the re-armed timer gates to the heap so the
			// timer loop fires them at the new target time.
			ts.pushTimerGates(roomID, ticketID, &content)

			return mutationResponse{
				ID:      ticketID,
				Room:    roomID.String(),
				Content: content,
			}, nil
		}
		// All recurring gates exhausted — fall through to normal close.
	}

	// Normal close path.
	content.Status = ticket.StatusClosed
	content.ClosedAt = nowStr
	content.CloseReason = request.Reason
	content.Assignee = ref.UserID{}
	content.UpdatedAt = nowStr

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	// Resolve timer targets for tickets that depend on this one. The
	// closed ticket is already in the index (putWithEcho updates it
	// immediately), so dependents' AllBlockersClosed checks see the
	// current state. The sync loop will also process the echo, but
	// resolveUnblockedTimerTargets is idempotent (skips gates that
	// already have a Target).
	ts.resolveUnblockedTimerTargets(ctx, roomID, state, []string{ticketID})

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleReopen transitions a ticket from "closed" back to "open",
// clearing the close timestamp and reason.
func (ts *TicketService) handleReopen(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/reopen"); err != nil {
		return nil, err
	}

	var request reopenRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	if content.Status != ticket.StatusClosed {
		return nil, fmt.Errorf("cannot reopen: ticket status is %q, not closed", content.Status)
	}

	content.Status = ticket.StatusOpen
	content.ClosedAt = ""
	content.CloseReason = ""
	content.UpdatedAt = ts.clock.Now().UTC().Format(time.RFC3339)

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleBatchCreate creates multiple tickets with symbolic
// back-references resolved to real IDs. All tickets are validated
// before any are written; if any ticket is invalid, no state events
// are sent.
func (ts *TicketService) handleBatchCreate(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/create"); err != nil {
		return nil, err
	}

	var request batchCreateRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	roomID, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	if len(request.Tickets) == 0 {
		return nil, errors.New("missing required field: tickets")
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)

	type pendingTicket struct {
		id      string
		content ticket.TicketContent
	}
	tickets := make([]pendingTicket, len(request.Tickets))
	refToID := make(map[string]string, len(request.Tickets))
	batchIDs := make(map[string]struct{}, len(request.Tickets))

	// Phase 1: Generate IDs for all tickets and build the ref map.
	for i, entry := range request.Tickets {
		if entry.Ref == "" {
			return nil, fmt.Errorf("tickets[%d]: missing required field: ref", i)
		}
		if _, exists := refToID[entry.Ref]; exists {
			return nil, fmt.Errorf("tickets[%d]: duplicate ref %q", i, entry.Ref)
		}

		// Enforce room-level type restrictions.
		if err := checkAllowedType(state.config, entry.Type); err != nil {
			return nil, fmt.Errorf("tickets[%d]: %w", i, err)
		}

		ticketID := ts.generateTicketID(state, entry.Type, roomID.String(), now, entry.Title, batchIDs)
		batchIDs[ticketID] = struct{}{}
		refToID[entry.Ref] = ticketID

		content := ticket.TicketContent{
			Version:   ticket.TicketContentVersion,
			Title:     entry.Title,
			Body:      entry.Body,
			Status:    ticket.StatusOpen,
			Priority:  entry.Priority,
			Type:      entry.Type,
			Labels:    entry.Labels,
			Affects:   entry.Affects,
			Parent:    entry.Parent,
			BlockedBy: entry.BlockedBy,
			Gates:     entry.Gates,
			Origin:    entry.Origin,
			Pipeline:  entry.Pipeline,
			CreatedBy: token.Subject,
			CreatedAt: now,
			UpdatedAt: now,
		}

		// Auto-configure stewardship review gates.
		if len(entry.Affects) > 0 {
			stewardship := ts.resolveStewardshipGates(entry.Affects, entry.Type, entry.Priority)
			content.Gates = append(content.Gates, stewardship.gates...)
			if len(stewardship.reviewers) > 0 {
				if content.Review == nil {
					content.Review = &ticket.TicketReview{}
				}
				content.Review.Reviewers = append(content.Review.Reviewers, stewardship.reviewers...)
				content.Review.TierThresholds = stewardship.thresholds
			}
		}

		for j := range content.Gates {
			if content.Gates[j].CreatedAt == "" {
				content.Gates[j].CreatedAt = now
			}
		}

		tickets[i] = pendingTicket{id: ticketID, content: content}
	}

	// Phase 2: Resolve symbolic refs in blocked_by and parent fields.
	for i := range tickets {
		content := &tickets[i].content
		for j, ref := range content.BlockedBy {
			if realID, isRef := refToID[ref]; isRef {
				content.BlockedBy[j] = realID
			} else if _, exists := state.index.Get(ref); !exists {
				return nil, fmt.Errorf("tickets[%d]: blocked_by %q is neither a symbolic ref nor an existing ticket", i, ref)
			}
		}
		if content.Parent != "" {
			if realID, isRef := refToID[content.Parent]; isRef {
				content.Parent = realID
			} else if _, exists := state.index.Get(content.Parent); !exists {
				return nil, fmt.Errorf("tickets[%d]: parent %q is neither a symbolic ref nor an existing ticket", i, content.Parent)
			}
		}
	}

	// Phase 2.5: Compute timer targets. Needs real BlockedBy IDs (from
	// Phase 2) and the room index to check blocker status. Intra-batch
	// blockers are not yet in the index and won't be found by
	// AllBlockersClosed, so base="unblocked" gates blocked by
	// intra-batch tickets correctly leave Target empty.
	for i := range tickets {
		enrichTimerTargets(&tickets[i].content, state.index)
	}

	// Phase 3: Validate all tickets before writing any.
	for i := range tickets {
		if err := tickets[i].content.Validate(); err != nil {
			return nil, fmt.Errorf("tickets[%d]: %w", i, err)
		}
	}

	// Phase 4: Write all state events and update the index.
	for i := range tickets {
		if err := ts.putWithEcho(ctx, roomID, state, tickets[i].id, tickets[i].content); err != nil {
			return nil, fmt.Errorf("writing ticket %s to Matrix: %w", tickets[i].id, err)
		}
		ts.pushTimerGates(roomID, tickets[i].id, &tickets[i].content)
	}

	return batchCreateResponse{
		Room: request.Room,
		Refs: refToID,
	}, nil
}

// handleImport writes tickets with caller-specified IDs, preserving
// the full TicketContent from an export. This is the counterpart to
// the list query's JSONL output: export streams entries out, import
// writes them back with their original IDs.
//
// Unlike batch-create, import does not generate IDs or resolve symbolic
// refs. The caller provides the exact ID and content for each ticket.
// This makes import suitable for room archival restore, room splitting,
// and cross-room migration.
//
// Validation is all-or-nothing: all tickets are validated before any
// state events are written. Importing into the same room as the source
// is an upsert (Matrix state events are idempotent on type + state_key).
func (ts *TicketService) handleImport(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/import"); err != nil {
		return nil, err
	}

	var request importRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	roomID, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	if len(request.Tickets) == 0 {
		return nil, errors.New("missing required field: tickets")
	}

	// Collect all IDs in the import set so that blocked_by and parent
	// references can be validated against both the existing room index
	// and the incoming batch.
	importIDs := make(map[string]struct{}, len(request.Tickets))
	for i, entry := range request.Tickets {
		if entry.ID == "" {
			return nil, fmt.Errorf("tickets[%d]: missing required field: id", i)
		}
		if _, duplicate := importIDs[entry.ID]; duplicate {
			return nil, fmt.Errorf("tickets[%d]: duplicate id %q", i, entry.ID)
		}
		importIDs[entry.ID] = struct{}{}
	}

	// Phase 1: Validate all tickets.
	for i, entry := range request.Tickets {
		if err := entry.Content.Validate(); err != nil {
			return nil, fmt.Errorf("tickets[%d] (%s): %w", i, entry.ID, err)
		}

		// Enforce room-level type restrictions.
		if err := checkAllowedType(state.config, entry.Content.Type); err != nil {
			return nil, fmt.Errorf("tickets[%d] (%s): %w", i, entry.ID, err)
		}

		// Verify blocked_by references exist in either the import set
		// or the existing room index.
		for _, blockerID := range entry.Content.BlockedBy {
			if _, inImport := importIDs[blockerID]; !inImport {
				if _, inRoom := state.index.Get(blockerID); !inRoom {
					return nil, fmt.Errorf("tickets[%d] (%s): blocked_by %q not found in import set or room", i, entry.ID, blockerID)
				}
			}
		}

		// Verify parent reference if specified.
		if entry.Content.Parent != "" {
			if _, inImport := importIDs[entry.Content.Parent]; !inImport {
				if _, inRoom := state.index.Get(entry.Content.Parent); !inRoom {
					return nil, fmt.Errorf("tickets[%d] (%s): parent %q not found in import set or room", i, entry.ID, entry.Content.Parent)
				}
			}
		}
	}

	// Phase 2: Write all state events and update the index.
	for _, entry := range request.Tickets {
		if err := ts.putWithEcho(ctx, roomID, state, entry.ID, entry.Content); err != nil {
			return nil, fmt.Errorf("writing ticket %s to Matrix: %w", entry.ID, err)
		}
	}

	return importResponse{
		Room:     request.Room,
		Imported: len(request.Tickets),
	}, nil
}

// handleResolveGate manually satisfies a human-type gate on a ticket.
// Only "human" gates can be resolved manually — programmatic gates
// are satisfied by the sync loop or via the update-gate action.
func (ts *TicketService) handleResolveGate(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request resolveGateRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}
	if request.Gate == "" {
		return nil, errors.New("missing required field: gate")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	gateIndex := findGate(content.Gates, request.Gate)
	if gateIndex < 0 {
		return nil, fmt.Errorf("gate %q not found on ticket %s", request.Gate, ticketID)
	}

	gate := &content.Gates[gateIndex]
	if gate.Type != "human" {
		return nil, fmt.Errorf("gate %q is type %q: only human gates can be resolved manually; use update-gate for programmatic gates", gate.ID, gate.Type)
	}
	if gate.Status == "satisfied" {
		return nil, fmt.Errorf("gate %q is already satisfied", gate.ID)
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	gate.Status = "satisfied"
	gate.SatisfiedAt = now
	gate.SatisfiedBy = token.Subject.String()
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleUpdateGate updates a gate's status programmatically. Unlike
// resolve-gate (which is restricted to human gates), update-gate works
// on any gate type. This is the entry point for external systems (CI,
// pipelines) to report gate satisfaction.
func (ts *TicketService) handleUpdateGate(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request updateGateRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}
	if request.Gate == "" {
		return nil, errors.New("missing required field: gate")
	}
	if request.Status == "" {
		return nil, errors.New("missing required field: status")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	gateIndex := findGate(content.Gates, request.Gate)
	if gateIndex < 0 {
		return nil, fmt.Errorf("gate %q not found on ticket %s", request.Gate, ticketID)
	}

	gate := &content.Gates[gateIndex]
	now := ts.clock.Now().UTC().Format(time.RFC3339)

	gate.Status = request.Status
	if request.Status == "satisfied" {
		gate.SatisfiedAt = now
		if request.SatisfiedBy != "" {
			gate.SatisfiedBy = request.SatisfiedBy
		}
	}
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleSetDisposition updates the calling reviewer's disposition on a
// ticket in review status. The caller must be in the ticket's reviewer
// list. This is a dedicated action (not part of "update") because review
// disposition is a distinct permission: not everyone who can edit a
// ticket should be able to set review dispositions, and not every
// reviewer should be able to edit the ticket.
func (ts *TicketService) handleSetDisposition(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/review"); err != nil {
		return nil, err
	}

	var request setDispositionRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}
	if request.Disposition == "" {
		return nil, errors.New("missing required field: disposition")
	}
	if !ticket.IsValidDisposition(request.Disposition) {
		return nil, fmt.Errorf("invalid disposition %q: must be approved, changes_requested, or commented", request.Disposition)
	}
	// "pending" is valid in the schema but not as a set-disposition
	// action — you can't "un-review" by setting pending. That's the
	// initial state before any reviewer acts.
	if request.Disposition == "pending" {
		return nil, errors.New("cannot set disposition to pending: pending is the initial state before review")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	if content.Status != ticket.StatusReview {
		return nil, fmt.Errorf("ticket is not in review status (status is %q)", content.Status)
	}
	if content.Review == nil {
		return nil, errors.New("ticket has no review data")
	}

	// Find the caller in the reviewer list.
	callerID := token.Subject
	reviewerIndex := -1
	for i := range content.Review.Reviewers {
		if content.Review.Reviewers[i].UserID == callerID {
			reviewerIndex = i
			break
		}
	}
	if reviewerIndex < 0 {
		return nil, fmt.Errorf("caller %s is not in the reviewer list", callerID)
	}

	// Snapshot the review state before the disposition change so
	// we can detect newly-activated last_pending tiers.
	oldReview := snapshotReview(content.Review)

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	content.Review.Reviewers[reviewerIndex].Disposition = request.Disposition
	content.Review.Reviewers[reviewerIndex].UpdatedAt = now
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	// Check for escalation notifications after the successful write.
	ts.sendEscalationNotifications(ctx, roomID, ticketID, oldReview, content)

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleAddNote appends a note to an existing ticket. Notes are
// short annotations that travel with the ticket — step outcomes,
// warnings, references. The note ID is assigned sequentially
// ("n-1", "n-2", ...) and the author is taken from the token subject.
func (ts *TicketService) handleAddNote(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request addNoteRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}
	if request.Body == "" {
		return nil, errors.New("missing required field: body")
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)

	noteID := fmt.Sprintf("n-%d", len(content.Notes)+1)
	content.Notes = append(content.Notes, ticket.TicketNote{
		ID:        noteID,
		Author:    token.Subject,
		CreatedAt: now,
		Body:      request.Body,
	})
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}

// handleDefer sets or updates a "defer" timer gate on a ticket to
// delay its readiness until a specified time. If the ticket already
// has a gate with ID "defer", its Target is updated in-place.
// Otherwise a new pending timer gate is appended.
func (ts *TicketService) handleDefer(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request deferRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}
	if request.Until == "" && request.For == "" {
		return nil, errors.New("one of 'until' or 'for' is required")
	}
	if request.Until != "" && request.For != "" {
		return nil, errors.New("'until' and 'for' are mutually exclusive")
	}

	// Compute the absolute target time.
	now := ts.clock.Now()
	var target time.Time
	if request.Until != "" {
		parsed, err := time.Parse(time.RFC3339, request.Until)
		if err != nil {
			return nil, fmt.Errorf("invalid 'until' time: %w", err)
		}
		if !parsed.After(now) {
			return nil, fmt.Errorf("'until' time %s is not in the future", request.Until)
		}
		target = parsed
	} else {
		duration, err := time.ParseDuration(request.For)
		if err != nil {
			return nil, fmt.Errorf("invalid 'for' duration: %w", err)
		}
		if duration <= 0 {
			return nil, errors.New("'for' duration must be positive")
		}
		target = now.Add(duration)
	}

	roomID, state, ticketID, content, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	nowStr := now.UTC().Format(time.RFC3339)
	targetStr := target.UTC().Format(time.RFC3339)

	gateIndex := findGate(content.Gates, "defer")
	if gateIndex >= 0 {
		// Update existing defer gate's target and reset to pending.
		gate := &content.Gates[gateIndex]
		gate.Target = targetStr
		gate.Status = "pending"
		gate.SatisfiedAt = ""
		gate.SatisfiedBy = ""
	} else {
		// Create a new defer gate.
		content.Gates = append(content.Gates, ticket.TicketGate{
			ID:        "defer",
			Type:      "timer",
			Status:    "pending",
			Target:    targetStr,
			CreatedAt: nowStr,
		})
	}
	content.UpdatedAt = nowStr

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if err := ts.putWithEcho(ctx, roomID, state, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}

	// Push the defer gate to the timer heap so the timer loop fires
	// it at the right time.
	ts.pushTimerGates(roomID, ticketID, &content)

	return mutationResponse{
		ID:      ticketID,
		Room:    roomID.String(),
		Content: content,
	}, nil
}
