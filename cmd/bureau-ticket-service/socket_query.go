// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// --- Request types ---
//
// Each query action decodes its specific fields from the CBOR request.
// The "action" and "token" fields are handled by the socket server
// framework and are not included here.

// roomRequest is used by room-scoped actions that take only a room ID.
type roomRequest struct {
	Room string `cbor:"room"`
}

// listRequest is used by the "list" action for filtered queries.
type listRequest struct {
	Room     string `cbor:"room"`
	Status   string `cbor:"status,omitempty"`
	Priority *int   `cbor:"priority,omitempty"`
	Label    string `cbor:"label,omitempty"`
	Assignee string `cbor:"assignee,omitempty"`
	Type     string `cbor:"type,omitempty"`
	Parent   string `cbor:"parent,omitempty"`
}

// showRequest identifies a single ticket. Room provides the room
// context for the ticket lookup. If Ticket is a room-qualified
// reference (e.g., "iree/general/tkt-a3f9"), Room may be omitted.
// Used by show, deps, and epic-health handlers.
type showRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
}

// grepRequest contains the regex search pattern, optional room scope,
// and optional filter fields. Filter fields use the same semantics as
// listRequest: zero-value fields mean "no filter", all non-zero fields
// must match (AND semantics). The Status field supports synthetic
// values "active" (open + in_progress) and "ready" (open, all blockers
// closed, all gates satisfied).
type grepRequest struct {
	Pattern  string `cbor:"pattern"`
	Room     string `cbor:"room,omitempty"`
	Status   string `cbor:"status,omitempty"`
	Priority *int   `cbor:"priority,omitempty"`
	Label    string `cbor:"label,omitempty"`
	Assignee string `cbor:"assignee,omitempty"`
	Type     string `cbor:"type,omitempty"`
	Parent   string `cbor:"parent,omitempty"`
}

// searchRequest is the request body for the "search" action. Query is
// a natural language search string that may include ticket/artifact IDs
// for exact-match boosting and graph expansion.
type searchRequest struct {
	Query    string `cbor:"query"`
	Room     string `cbor:"room,omitempty"`
	Status   string `cbor:"status,omitempty"`
	Priority *int   `cbor:"priority,omitempty"`
	Label    string `cbor:"label,omitempty"`
	Assignee string `cbor:"assignee,omitempty"`
	Type     string `cbor:"type,omitempty"`
	Parent   string `cbor:"parent,omitempty"`
	Limit    int    `cbor:"limit,omitempty"`
}

// childrenRequest identifies the parent ticket and its room.
type childrenRequest struct {
	Room   string `cbor:"room,omitempty"`
	Ticket string `cbor:"ticket"`
}

// --- Response types ---
//
// Query responses use schema types directly (ticket.TicketContent,
// ticketindex.Entry, ticketindex.Stats) rather than defining parallel wire
// types. The CBOR library falls back to json struct tags when cbor
// tags are absent, so schema types serialize correctly over CBOR.
// This avoids a shadow schema that silently drifts when fields are
// added to the canonical types.
//
// Response-only types below add context that doesn't exist in the
// schema (room ID, computed dependency graph fields).

// entryWithRoom pairs a ticketindex.Entry with its room ID for cross-room
// query results. Room-scoped queries leave Room empty since the
// caller already knows which room they asked about.
type entryWithRoom struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content ticket.TicketContent `json:"content"`

	// Stewardship gate summary — populated when the ticket has
	// stewardship review gates (gates with "stewardship:" prefix).
	// Omitted when no stewardship gates exist.
	StewardshipGates     int `json:"stewardship_gates,omitempty"`
	StewardshipSatisfied int `json:"stewardship_satisfied,omitempty"`
}

// entriesFromIndex converts index entries to the wire format.
func entriesFromIndex(entries []ticketindex.Entry, room string) []entryWithRoom {
	result := make([]entryWithRoom, len(entries))
	for i, entry := range entries {
		gateCount, gateSatisfied := computeStewardshipSummary(entry.Content)
		result[i] = entryWithRoom{
			ID:                   entry.ID,
			Room:                 room,
			Content:              entry.Content,
			StewardshipGates:     gateCount,
			StewardshipSatisfied: gateSatisfied,
		}
	}
	return result
}

// searchEntryResponse pairs a ticket entry with its search relevance
// score for the "search" action response.
type searchEntryResponse struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content ticket.TicketContent `json:"content"`
	Score   float64              `json:"score"`
}

// showResponse is the full detail response for a single ticket.
// It embeds the schema content directly and adds computed fields
// from the dependency graph, child progress, scoring, and
// stewardship governance context.
type showResponse struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content ticket.TicketContent `json:"content"`

	// Computed fields from the dependency graph.
	Blocks      []string `json:"blocks,omitempty"`
	ChildTotal  int      `json:"child_total,omitempty"`
	ChildClosed int      `json:"child_closed,omitempty"`

	// Score holds the ranking dimensions for an open ticket. Nil
	// for closed tickets where scoring is not meaningful.
	Score *ticketindex.TicketScore `json:"score,omitempty"`

	// Stewardship holds the governance context for this ticket:
	// which declarations matched, and per-gate tier approval
	// progress. Nil when the ticket has no Affects or no matching
	// declarations.
	Stewardship *stewardshipContext `json:"stewardship,omitempty"`
}

// childrenResponse includes the children list and progress summary.
type childrenResponse struct {
	Parent      string          `json:"parent"`
	Children    []entryWithRoom `json:"children"`
	ChildTotal  int             `json:"child_total"`
	ChildClosed int             `json:"child_closed"`
}

// depsResponse is the transitive dependency closure.
type depsResponse struct {
	Ticket string   `json:"ticket"`
	Deps   []string `json:"deps"`
}

// rankedEntryResponse pairs a ticket entry with its computed score
// for the "ranked" action. Entries are sorted by composite score
// descending (highest-leverage first).
type rankedEntryResponse struct {
	ID      string                  `json:"id"`
	Room    string                  `json:"room,omitempty"`
	Content ticket.TicketContent    `json:"content"`
	Score   ticketindex.TicketScore `json:"score"`
}

// epicHealthResponse holds health metrics for an epic's children.
type epicHealthResponse struct {
	Ticket string                      `json:"ticket"`
	Health ticketindex.EpicHealthStats `json:"health"`
}

// upcomingGateEntry is a single upcoming timer gate with its ticket
// and room context. Entries are sorted by Target time ascending.
type upcomingGateEntry struct {
	// Gate metadata.
	GateID      string `json:"gate_id"`
	Target      string `json:"target"`
	Schedule    string `json:"schedule,omitempty"`
	Interval    string `json:"interval,omitempty"`
	FireCount   int    `json:"fire_count,omitempty"`
	LastFiredAt string `json:"last_fired_at,omitempty"`

	// Ticket context.
	TicketID string `json:"ticket_id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Assignee string `json:"assignee,omitempty"`
	Room     string `json:"room"`

	// Computed fields.
	UntilFire string `json:"until_fire"`
}

// --- Stewardship context types ---

// stewardshipContext describes the governance context for a ticket:
// which stewardship declarations matched its Affects field, and the
// per-gate tier approval progress for each stewardship review gate.
type stewardshipContext struct {
	// Declarations lists the stewardship declarations that matched
	// the ticket's Affects field. Reuses stewardshipMatchEntry from
	// the stewardship-resolve action for consistent wire format.
	Declarations []stewardshipMatchEntry `json:"declarations,omitempty"`

	// Gates summarizes the approval progress for each stewardship
	// review gate on the ticket. Each gate has per-tier counts of
	// total reviewers, approvals, and whether the tier is satisfied.
	Gates []stewardshipGateProgress `json:"gates,omitempty"`
}

// stewardshipGateProgress describes the approval progress for a
// single stewardship review gate.
type stewardshipGateProgress struct {
	GateID string                `json:"gate_id"`
	Status ticket.GateStatus     `json:"status"`
	Tiers  []tierApprovalSummary `json:"tiers,omitempty"`
}

// tierApprovalSummary describes the approval state of a single tier
// within a stewardship review gate.
type tierApprovalSummary struct {
	Tier      int  `json:"tier"`
	Total     int  `json:"total"`
	Approved  int  `json:"approved"`
	Threshold *int `json:"threshold,omitempty"`
	Satisfied bool `json:"satisfied"`
}

// computeStewardshipContext builds the full stewardship governance
// context for a ticket. Re-resolves Affects against the stewardship
// index (to show which declarations currently match) and computes
// per-gate tier approval status from the ticket's Review data.
func (ts *TicketService) computeStewardshipContext(roomID ref.RoomID, content ticket.TicketContent) *stewardshipContext {
	if len(content.Affects) == 0 {
		return nil
	}

	// Re-resolve declarations against current stewardship index.
	// Scoped to the ticket's room — stewardship declarations are
	// per-room state events, and a ticket's governance comes from
	// declarations in the same room.
	matches := ts.stewardshipIndex.ResolveForRoom(roomID, content.Affects)

	// Compute per-gate tier progress for stewardship review gates.
	var gateProgress []stewardshipGateProgress
	for _, gate := range content.Gates {
		if gate.Type != ticket.GateReview || !strings.HasPrefix(gate.ID, "stewardship:") {
			continue
		}
		progress := stewardshipGateProgress{
			GateID: gate.ID,
			Status: gate.Status,
		}
		if content.Review != nil {
			progress.Tiers = computeTierProgress(content.Review)
		}
		gateProgress = append(gateProgress, progress)
	}

	if len(matches) == 0 && len(gateProgress) == 0 {
		return nil
	}

	result := &stewardshipContext{
		Gates: gateProgress,
	}

	for _, match := range matches {
		result.Declarations = append(result.Declarations, stewardshipMatchEntry{
			RoomID:          match.Declaration.RoomID,
			StateKey:        match.Declaration.StateKey,
			OverlapPolicy:   string(match.Declaration.Content.OverlapPolicy),
			MatchedResource: match.MatchedResource,
			MatchedPattern:  match.MatchedPattern,
			Description:     match.Declaration.Content.Description,
		})
	}

	return result
}

// computeTierProgress builds per-tier approval summaries from the
// ticket's Review data. Groups reviewers by Tier, counts approvals,
// and checks each tier's threshold.
func computeTierProgress(review *ticket.TicketReview) []tierApprovalSummary {
	// Collect distinct tiers from thresholds and reviewers.
	tierSet := make(map[int]struct{})
	thresholdByTier := make(map[int]*int)
	for _, threshold := range review.TierThresholds {
		tierSet[threshold.Tier] = struct{}{}
		thresholdByTier[threshold.Tier] = threshold.Threshold
	}
	for _, reviewer := range review.Reviewers {
		tierSet[reviewer.Tier] = struct{}{}
	}

	// Sort tiers for deterministic output.
	tiers := make([]int, 0, len(tierSet))
	for tier := range tierSet {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	// Build per-tier counts.
	var summaries []tierApprovalSummary
	for _, tier := range tiers {
		total := 0
		approved := 0
		for _, reviewer := range review.Reviewers {
			if reviewer.Tier == tier {
				total++
				if reviewer.Disposition == ticket.DispositionApproved {
					approved++
				}
			}
		}
		threshold := thresholdByTier[tier]
		satisfied := false
		if total == 0 {
			satisfied = true
		} else if threshold == nil {
			satisfied = approved >= total
		} else {
			satisfied = approved >= *threshold
		}
		summaries = append(summaries, tierApprovalSummary{
			Tier:      tier,
			Total:     total,
			Approved:  approved,
			Threshold: threshold,
			Satisfied: satisfied,
		})
	}
	return summaries
}

// computeStewardshipSummary counts stewardship gates and how many
// are satisfied. Returns (gateCount, satisfiedCount).
func computeStewardshipSummary(content ticket.TicketContent) (int, int) {
	gateCount := 0
	satisfied := 0
	for _, gate := range content.Gates {
		if strings.HasPrefix(gate.ID, "stewardship:") {
			gateCount++
			if gate.Status == ticket.GateSatisfied {
				satisfied++
			}
		}
	}
	return gateCount, satisfied
}

// --- Query helpers ---

// parseFilterAssignee parses a string assignee from a query request into
// a ref.UserID for use in ticketindex.Filter. An empty string returns the
// zero value (no filter). A non-empty string must be a valid Matrix
// user ID.
func parseFilterAssignee(raw string) (ref.UserID, error) {
	if raw == "" {
		return ref.UserID{}, nil
	}
	return ref.ParseUserID(raw)
}

// --- Query handlers ---

// handleList returns tickets matching a filter within a room.
func (ts *TicketService) handleList(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionList); err != nil {
		return nil, err
	}

	var request listRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	_, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	assignee, err := parseFilterAssignee(request.Assignee)
	if err != nil {
		return nil, fmt.Errorf("invalid assignee filter: %w", err)
	}

	entries := state.index.List(ticketindex.Filter{
		Status:   request.Status,
		Priority: request.Priority,
		Label:    request.Label,
		Assignee: assignee,
		Type:     request.Type,
		Parent:   request.Parent,
	})

	return entriesFromIndex(entries, ""), nil
}

// roomQuery handles the common pattern for room-scoped queries that
// take only a room ID: check grant, decode request, look up room,
// call the query function. The queryFunc receives the room's index
// and returns the matching entries.
func (ts *TicketService) roomQuery(
	token *servicetoken.Token,
	raw []byte,
	grant string,
	queryFunc func(*ticketindex.Index) []ticketindex.Entry,
) (any, error) {
	if err := requireGrant(token, grant); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	_, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	return entriesFromIndex(queryFunc(state.index), ""), nil
}

// handleReady returns actionable tickets within a room: open tickets
// with all blockers closed and gates satisfied, plus in_progress
// tickets.
func (ts *TicketService) handleReady(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, ticket.ActionReady, (*ticketindex.Index).Ready)
}

// handleBlocked returns open tickets that cannot be started: they
// have at least one non-closed blocker or unsatisfied gate.
func (ts *TicketService) handleBlocked(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, ticket.ActionBlocked, (*ticketindex.Index).Blocked)
}

// handleRanked returns ready tickets sorted by composite score
// descending. This is the primary query for PM agents deciding
// what to assign next: highest-leverage, highest-urgency tickets
// appear first. The score breakdown is included so agents can
// explain their assignment decisions.
func (ts *TicketService) handleRanked(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionRanked); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	_, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	ranked := state.index.Ranked(ts.clock.Now(), ticketindex.DefaultRankWeights())
	result := make([]rankedEntryResponse, len(ranked))
	for i, entry := range ranked {
		result[i] = rankedEntryResponse{
			ID:      entry.ID,
			Content: entry.Content,
			Score:   entry.Score,
		}
	}

	return result, nil
}

// handleShow returns the full detail of a single ticket. The ticket
// is resolved via the room context in the request or a room-qualified
// ticket reference. The response includes computed fields from the
// dependency graph (blocks, child progress) alongside the schema
// content.
func (ts *TicketService) handleShow(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionShow); err != nil {
		return nil, err
	}

	var request showRequest
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

	blocks := state.index.Blocks(ticketID)
	childTotal, childClosed := state.index.ChildProgress(ticketID)

	// Compute score for non-closed tickets. Scoring a closed ticket
	// is meaningless (it cannot be assigned) and would produce
	// confusing results.
	var score *ticketindex.TicketScore
	if content.Status != ticket.StatusClosed {
		ticketScore := state.index.Score(ticketID, ts.clock.Now(), ticketindex.DefaultRankWeights())
		score = &ticketScore
	}

	return showResponse{
		ID:          ticketID,
		Room:        roomID.String(),
		Content:     content,
		Blocks:      blocks,
		ChildTotal:  childTotal,
		ChildClosed: childClosed,
		Score:       score,
		Stewardship: ts.computeStewardshipContext(roomID, content),
	}, nil
}

// handleChildren returns the direct children of a parent ticket and
// a progress summary.
func (ts *TicketService) handleChildren(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionChildren); err != nil {
		return nil, err
	}

	var request childrenRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	_, state, ticketID, _, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	children := state.index.Children(ticketID)
	childTotal, childClosed := state.index.ChildProgress(ticketID)

	return childrenResponse{
		Parent:      ticketID,
		Children:    entriesFromIndex(children, ""),
		ChildTotal:  childTotal,
		ChildClosed: childClosed,
	}, nil
}

// handleGrep searches tickets by regex across title, body, and notes,
// optionally filtered by status, priority, label, assignee, type, or
// parent. If a room is specified, searches only that room. Otherwise
// searches all rooms and includes the room ID in each result.
func (ts *TicketService) handleGrep(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionGrep); err != nil {
		return nil, err
	}

	var request grepRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Pattern == "" {
		return nil, errors.New("missing required field: pattern")
	}

	assignee, err := parseFilterAssignee(request.Assignee)
	if err != nil {
		return nil, fmt.Errorf("invalid assignee filter: %w", err)
	}

	filter := ticketindex.Filter{
		Status:   request.Status,
		Priority: request.Priority,
		Label:    request.Label,
		Assignee: assignee,
		Type:     request.Type,
		Parent:   request.Parent,
	}

	// Room-scoped grep.
	if request.Room != "" {
		_, state, err := ts.requireRoom(request.Room)
		if err != nil {
			return nil, err
		}
		entries, err := state.index.Grep(request.Pattern, filter)
		if err != nil {
			return nil, err
		}
		return entriesFromIndex(entries, ""), nil
	}

	// Cross-room grep — include room ID in each result.
	var allEntries []entryWithRoom
	for roomID, state := range ts.rooms {
		entries, err := state.index.Grep(request.Pattern, filter)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			gateCount, gateSatisfied := computeStewardshipSummary(entry.Content)
			allEntries = append(allEntries, entryWithRoom{
				ID:                   entry.ID,
				Room:                 roomID.String(),
				Content:              entry.Content,
				StewardshipGates:     gateCount,
				StewardshipSatisfied: gateSatisfied,
			})
		}
	}

	if allEntries == nil {
		allEntries = []entryWithRoom{}
	}

	return allEntries, nil
}

// handleSearch performs BM25-ranked full-text search with exact-match
// boosting and graph expansion. If a room is specified, searches only
// that room. Otherwise searches all rooms and merges results by score.
func (ts *TicketService) handleSearch(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionSearch); err != nil {
		return nil, err
	}

	var request searchRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Query == "" {
		return nil, errors.New("missing required field: query")
	}

	assignee, err := parseFilterAssignee(request.Assignee)
	if err != nil {
		return nil, fmt.Errorf("invalid assignee filter: %w", err)
	}

	filter := ticketindex.Filter{
		Status:   request.Status,
		Priority: request.Priority,
		Label:    request.Label,
		Assignee: assignee,
		Type:     request.Type,
		Parent:   request.Parent,
	}

	limit := request.Limit
	if limit <= 0 {
		limit = 50
	}

	// Room-scoped search.
	if request.Room != "" {
		_, state, err := ts.requireRoom(request.Room)
		if err != nil {
			return nil, err
		}
		results := state.index.Search(request.Query, filter, limit)
		entries := make([]searchEntryResponse, len(results))
		for i, result := range results {
			entries[i] = searchEntryResponse{
				ID:      result.ID,
				Content: result.Content,
				Score:   result.Score,
			}
		}
		return entries, nil
	}

	// Cross-room search: search each room with no limit, merge
	// all results, re-sort by score, then apply the global limit.
	var allResults []searchEntryResponse
	for roomID, state := range ts.rooms {
		results := state.index.Search(request.Query, filter, 0)
		for _, result := range results {
			allResults = append(allResults, searchEntryResponse{
				ID:      result.ID,
				Room:    roomID.String(),
				Content: result.Content,
				Score:   result.Score,
			})
		}
	}

	sort.Slice(allResults, func(i, j int) bool {
		if allResults[i].Score != allResults[j].Score {
			return allResults[i].Score > allResults[j].Score
		}
		if allResults[i].Content.Priority != allResults[j].Content.Priority {
			return allResults[i].Content.Priority < allResults[j].Content.Priority
		}
		return allResults[i].Content.CreatedAt < allResults[j].Content.CreatedAt
	})

	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	if allResults == nil {
		allResults = []searchEntryResponse{}
	}

	return allResults, nil
}

// handleStats returns aggregate counts for a room's tickets.
func (ts *TicketService) handleStats(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionStats); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	_, state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	return state.index.Stats(), nil
}

// handleDeps returns the transitive dependency closure for a ticket.
func (ts *TicketService) handleDeps(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionDeps); err != nil {
		return nil, err
	}

	var request showRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	_, state, ticketID, _, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	deps := state.index.Deps(ticketID)
	if deps == nil {
		deps = []string{}
	}

	return depsResponse{
		Ticket: ticketID,
		Deps:   deps,
	}, nil
}

// handleEpicHealth returns health metrics for an epic's children:
// parallelism width (ready children), completion progress, active
// fraction, and critical dependency depth. This helps PM agents
// understand whether an epic is stalling and where to focus.
func (ts *TicketService) handleEpicHealth(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionEpicHealth); err != nil {
		return nil, err
	}

	var request showRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	_, state, ticketID, _, err := ts.resolveTicket(request.Room, request.Ticket)
	if err != nil {
		return nil, err
	}

	health := state.index.EpicHealth(ticketID)

	return epicHealthResponse{
		Ticket: ticketID,
		Health: health,
	}, nil
}

// handleUpcomingGates returns pending timer gates across all rooms (or
// a single room if specified), sorted by target time ascending. Each
// entry includes the gate metadata, ticket context, and a computed
// time-until-fire duration.
func (ts *TicketService) handleUpcomingGates(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionUpcomingGates); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	now := ts.clock.Now()
	var entries []upcomingGateEntry

	for roomID, state := range ts.rooms {
		if request.Room != "" && roomID.String() != request.Room {
			continue
		}

		for _, pending := range state.index.PendingGates() {
			for i := range pending.Content.Gates {
				gate := &pending.Content.Gates[i]
				if gate.Type != ticket.GateTimer || gate.Status != ticket.GatePending || gate.Target == "" {
					continue
				}

				target, err := time.Parse(time.RFC3339, gate.Target)
				if err != nil {
					continue
				}

				untilFire := target.Sub(now)
				var untilFireStr string
				if untilFire <= 0 {
					untilFireStr = "overdue"
				} else {
					untilFireStr = formatDuration(untilFire)
				}

				entries = append(entries, upcomingGateEntry{
					GateID:      gate.ID,
					Target:      gate.Target,
					Schedule:    gate.Schedule,
					Interval:    gate.Interval,
					FireCount:   gate.FireCount,
					LastFiredAt: gate.LastFiredAt,
					TicketID:    pending.ID,
					Title:       pending.Content.Title,
					Status:      string(pending.Content.Status),
					Assignee:    pending.Content.Assignee.String(),
					Room:        roomID.String(),
					UntilFire:   untilFireStr,
				})
			}
		}
	}

	// Sort by target time ascending.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Target < entries[j].Target
	})

	return entries, nil
}

// formatDuration produces a human-readable relative time string like
// "2h 15m", "3d 4h", or "45s". Stops after two significant units to
// keep output concise.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	switch {
	case days > 0:
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		if seconds > 0 {
			return fmt.Sprintf("%dm %ds", minutes, seconds)
		}
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// --- List rooms ---

// roomInfo describes a single tracked room for the list-rooms
// response. Includes the room's canonical alias and ticket ID prefix
// so the viewer can display human-friendly room names and construct
// ticket references.
type roomInfo struct {
	RoomID string            `cbor:"room_id"`
	Alias  string            `cbor:"alias,omitempty"`
	Prefix string            `cbor:"prefix,omitempty"`
	Stats  ticketindex.Stats `cbor:"stats"`
}

// handleListRooms returns summary information for every tracked room.
// The viewer uses this for room selection when no --room flag is
// provided.
func (ts *TicketService) handleListRooms(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionListRooms); err != nil {
		return nil, err
	}

	rooms := make([]roomInfo, 0, len(ts.rooms))
	for roomID, state := range ts.rooms {
		prefix := state.config.Prefix
		if prefix == "" {
			prefix = "tkt"
		}
		rooms = append(rooms, roomInfo{
			RoomID: roomID.String(),
			Alias:  state.alias,
			Prefix: prefix,
			Stats:  state.index.Stats(),
		})
	}

	return rooms, nil
}

// --- List members ---

// memberInfo describes a single joined member of a tracked room,
// enriched with presence state from the ticket service's /sync loop.
// The TUI uses this to populate the assignee dropdown with
// availability indicators.
type memberInfo struct {
	UserID          ref.UserID `cbor:"user_id"`
	DisplayName     string     `cbor:"display_name"`
	Presence        string     `cbor:"presence"`
	StatusMsg       string     `cbor:"status_msg"`
	CurrentlyActive bool       `cbor:"currently_active"`
}

// presenceRank returns a sort key for presence states. Lower values
// sort first: online users appear before unavailable, which appear
// before offline or unknown.
func presenceRank(presence string) int {
	switch presence {
	case "online":
		return 0
	case "unavailable":
		return 1
	case "offline":
		return 2
	default:
		return 3
	}
}

// handleListMembers returns the joined members of a tracked room,
// enriched with presence state. The response is sorted by presence
// (online first, then unavailable, then offline/unknown), with
// alphabetical ordering by display name within each group.
//
// Membership data comes from the in-memory membership index
// (membersByRoom), maintained incrementally from m.room.member
// state events via /sync. This avoids synchronous HTTP calls to
// the homeserver while the read lock is held.
func (ts *TicketService) handleListMembers(_ context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, ticket.ActionListMembers); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	roomID, _, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	// Build member list from the in-memory membership index,
	// enriched with presence state. Only joined members are stored
	// in membersByRoom (leave/ban events remove them).
	members := ts.membersByRoom[roomID]
	result := make([]memberInfo, 0, len(members))
	for userID, member := range members {
		info := memberInfo{
			UserID:      userID,
			DisplayName: member.DisplayName,
		}
		if presence, exists := ts.presence[userID]; exists {
			info.Presence = presence.Presence
			info.StatusMsg = presence.StatusMsg
			info.CurrentlyActive = presence.CurrentlyActive
		}
		result = append(result, info)
	}

	// Sort by presence (online first), then alphabetical by display
	// name within each presence group.
	sort.Slice(result, func(i, j int) bool {
		rankI := presenceRank(result[i].Presence)
		rankJ := presenceRank(result[j].Presence)
		if rankI != rankJ {
			return rankI < rankJ
		}
		return result[i].DisplayName < result[j].DisplayName
	})

	return result, nil
}
