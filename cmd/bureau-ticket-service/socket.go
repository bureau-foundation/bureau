// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// registerActions registers all socket API actions on the server.
// Actions are grouped by category following the design in tickets.md.
//
// The "status" action is unauthenticated (pure liveness check).
// All other actions use HandleAuth and require a valid service token.
func (ts *TicketService) registerActions(server *service.SocketServer) {
	// Liveness health check — no authentication required. Returns
	// only uptime; no room or ticket information is disclosed.
	server.Handle("status", ts.handleStatus)

	// Authenticated diagnostic action — returns the same information
	// that the old unauthenticated status action returned (room
	// counts, ticket counts, per-room summaries). Requires a valid
	// service token with any ticket/* grant.
	server.HandleAuth("info", ts.handleInfo)

	// Query actions — all authenticated, all read-only.
	server.HandleAuth("list", ts.handleList)
	server.HandleAuth("ready", ts.handleReady)
	server.HandleAuth("blocked", ts.handleBlocked)
	server.HandleAuth("ranked", ts.handleRanked)
	server.HandleAuth("show", ts.handleShow)
	server.HandleAuth("children", ts.handleChildren)
	server.HandleAuth("grep", ts.handleGrep)
	server.HandleAuth("stats", ts.handleStats)
	server.HandleAuth("deps", ts.handleDeps)
	server.HandleAuth("epic-health", ts.handleEpicHealth)

	// Mutation actions — all authenticated, all write to Matrix.
	server.HandleAuth("create", ts.handleCreate)
	server.HandleAuth("update", ts.handleUpdate)
	server.HandleAuth("close", ts.handleClose)
	server.HandleAuth("reopen", ts.handleReopen)
	server.HandleAuth("batch-create", ts.handleBatchCreate)
	server.HandleAuth("import", ts.handleImport)
	server.HandleAuth("resolve-gate", ts.handleResolveGate)
	server.HandleAuth("update-gate", ts.handleUpdateGate)
}

// statusResponse is the response to the "status" action. Contains
// only liveness information — no room IDs, ticket counts, or other
// data that could disclose what the service is tracking.
type statusResponse struct {
	// UptimeSeconds is how long the service has been running.
	UptimeSeconds float64 `cbor:"uptime_seconds"`
}

// handleStatus returns a minimal liveness response. This is the only
// unauthenticated action — it reveals nothing about the service's
// state beyond "I am alive."
func (ts *TicketService) handleStatus(ctx context.Context, raw []byte) (any, error) {
	uptime := ts.clock.Now().Sub(ts.startedAt)
	return statusResponse{
		UptimeSeconds: uptime.Seconds(),
	}, nil
}

// infoResponse is the response to the authenticated "info" action.
type infoResponse struct {
	// UptimeSeconds is how long the service has been running.
	UptimeSeconds float64 `cbor:"uptime_seconds"`

	// Rooms is the number of rooms with ticket management enabled.
	Rooms int `cbor:"rooms"`

	// TotalTickets is the total number of tickets across all rooms.
	TotalTickets int `cbor:"total_tickets"`

	// RoomDetails lists per-room ticket summaries.
	RoomDetails []roomSummary `cbor:"room_details"`
}

// handleInfo returns diagnostic information about the service. This
// action requires authentication — room IDs, ticket counts, and
// per-room summaries are sensitive information.
func (ts *TicketService) handleInfo(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	uptime := ts.clock.Now().Sub(ts.startedAt)

	return infoResponse{
		UptimeSeconds: uptime.Seconds(),
		Rooms:         len(ts.rooms),
		TotalTickets:  ts.totalTickets(),
		RoomDetails:   ts.roomStats(),
	}, nil
}

// --- Authorization helpers ---

// requireGrant checks that the token carries a grant for the given
// action pattern (e.g., "ticket/list"). Returns nil if authorized,
// or an error suitable for returning to the client.
func requireGrant(token *servicetoken.Token, action string) error {
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return fmt.Errorf("access denied: missing grant for %s", action)
	}
	return nil
}

// requireRoom validates the "room" field from a request and returns
// the corresponding room state. Returns an error if the room field
// is empty or the room is not tracked.
func (ts *TicketService) requireRoom(roomID string) (*roomState, error) {
	if roomID == "" {
		return nil, errors.New("missing required field: room")
	}
	state, exists := ts.rooms[roomID]
	if !exists {
		return nil, fmt.Errorf("room %s is not tracked by this service", roomID)
	}
	return state, nil
}

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

// showRequest identifies a single ticket.
type showRequest struct {
	Ticket string `cbor:"ticket"`
}

// grepRequest contains the regex search pattern and optional room scope.
type grepRequest struct {
	Pattern string `cbor:"pattern"`
	Room    string `cbor:"room,omitempty"`
}

// depsRequest identifies the ticket for dependency traversal.
type depsRequest struct {
	Ticket string `cbor:"ticket"`
}

// childrenRequest identifies the parent ticket.
type childrenRequest struct {
	Ticket string `cbor:"ticket"`
}

// --- Response types ---
//
// Query responses use schema types directly (schema.TicketContent,
// ticket.Entry, ticket.Stats) rather than defining parallel wire
// types. The CBOR library falls back to json struct tags when cbor
// tags are absent, so schema types serialize correctly over CBOR.
// This avoids a shadow schema that silently drifts when fields are
// added to the canonical types.
//
// Response-only types below add context that doesn't exist in the
// schema (room ID, computed dependency graph fields).

// entryWithRoom pairs a ticket.Entry with its room ID for cross-room
// query results. Room-scoped queries leave Room empty since the
// caller already knows which room they asked about.
type entryWithRoom struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content schema.TicketContent `json:"content"`
}

// entriesFromIndex converts index entries to the wire format.
func entriesFromIndex(entries []ticket.Entry, room string) []entryWithRoom {
	result := make([]entryWithRoom, len(entries))
	for i, entry := range entries {
		result[i] = entryWithRoom{
			ID:      entry.ID,
			Room:    room,
			Content: entry.Content,
		}
	}
	return result
}

// showResponse is the full detail response for a single ticket.
// It embeds the schema content directly and adds computed fields
// from the dependency graph, child progress, and scoring.
type showResponse struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content schema.TicketContent `json:"content"`

	// Computed fields from the dependency graph.
	Blocks      []string `json:"blocks,omitempty"`
	ChildTotal  int      `json:"child_total,omitempty"`
	ChildClosed int      `json:"child_closed,omitempty"`

	// Score holds the ranking dimensions for an open ticket. Nil
	// for closed tickets where scoring is not meaningful.
	Score *ticket.TicketScore `json:"score,omitempty"`
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
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content schema.TicketContent `json:"content"`
	Score   ticket.TicketScore   `json:"score"`
}

// epicHealthResponse holds health metrics for an epic's children.
type epicHealthResponse struct {
	Ticket string                 `json:"ticket"`
	Health ticket.EpicHealthStats `json:"health"`
}

// --- Query handlers ---

// handleList returns tickets matching a filter within a room.
func (ts *TicketService) handleList(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/list"); err != nil {
		return nil, err
	}

	var request listRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	entries := state.index.List(ticket.Filter{
		Status:   request.Status,
		Priority: request.Priority,
		Label:    request.Label,
		Assignee: request.Assignee,
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
	queryFunc func(*ticket.Index) []ticket.Entry,
) (any, error) {
	if err := requireGrant(token, grant); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	return entriesFromIndex(queryFunc(state.index), ""), nil
}

// handleReady returns actionable tickets within a room: open tickets
// with all blockers closed and gates satisfied, plus in_progress
// tickets.
func (ts *TicketService) handleReady(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, "ticket/ready", (*ticket.Index).Ready)
}

// handleBlocked returns open tickets that cannot be started: they
// have at least one non-closed blocker or unsatisfied gate.
func (ts *TicketService) handleBlocked(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, "ticket/blocked", (*ticket.Index).Blocked)
}

// handleRanked returns ready tickets sorted by composite score
// descending. This is the primary query for PM agents deciding
// what to assign next: highest-leverage, highest-urgency tickets
// appear first. The score breakdown is included so agents can
// explain their assignment decisions.
func (ts *TicketService) handleRanked(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/ranked"); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	ranked := state.index.Ranked(ts.clock.Now(), ticket.DefaultRankWeights())
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

// handleShow returns the full detail of a single ticket, looked up
// across all rooms. The response includes computed fields from the
// dependency graph (blocks, child progress) alongside the schema
// content.
func (ts *TicketService) handleShow(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/show"); err != nil {
		return nil, err
	}

	var request showRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	blocks := state.index.Blocks(request.Ticket)
	childTotal, childClosed := state.index.ChildProgress(request.Ticket)

	// Compute score for non-closed tickets. Scoring a closed ticket
	// is meaningless (it cannot be assigned) and would produce
	// confusing results.
	var score *ticket.TicketScore
	if content.Status != "closed" {
		ticketScore := state.index.Score(request.Ticket, ts.clock.Now(), ticket.DefaultRankWeights())
		score = &ticketScore
	}

	return showResponse{
		ID:          request.Ticket,
		Room:        roomID,
		Content:     content,
		Blocks:      blocks,
		ChildTotal:  childTotal,
		ChildClosed: childClosed,
		Score:       score,
	}, nil
}

// handleChildren returns the direct children of a parent ticket and
// a progress summary.
func (ts *TicketService) handleChildren(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/children"); err != nil {
		return nil, err
	}

	var request childrenRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	// Find which room the parent ticket lives in.
	_, state, _, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	children := state.index.Children(request.Ticket)
	childTotal, childClosed := state.index.ChildProgress(request.Ticket)

	return childrenResponse{
		Parent:      request.Ticket,
		Children:    entriesFromIndex(children, ""),
		ChildTotal:  childTotal,
		ChildClosed: childClosed,
	}, nil
}

// handleGrep searches tickets by regex across title, body, and notes.
// If a room is specified, searches only that room. Otherwise searches
// all rooms and includes the room ID in each result.
func (ts *TicketService) handleGrep(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/grep"); err != nil {
		return nil, err
	}

	var request grepRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Pattern == "" {
		return nil, errors.New("missing required field: pattern")
	}

	// Room-scoped grep.
	if request.Room != "" {
		state, err := ts.requireRoom(request.Room)
		if err != nil {
			return nil, err
		}
		entries, err := state.index.Grep(request.Pattern)
		if err != nil {
			return nil, err
		}
		return entriesFromIndex(entries, ""), nil
	}

	// Cross-room grep — include room ID in each result.
	var allEntries []entryWithRoom
	for roomID, state := range ts.rooms {
		entries, err := state.index.Grep(request.Pattern)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			allEntries = append(allEntries, entryWithRoom{
				ID:      entry.ID,
				Room:    roomID,
				Content: entry.Content,
			})
		}
	}

	if allEntries == nil {
		allEntries = []entryWithRoom{}
	}

	return allEntries, nil
}

// handleStats returns aggregate counts for a room's tickets.
func (ts *TicketService) handleStats(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/stats"); err != nil {
		return nil, err
	}

	var request roomRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	return state.index.Stats(), nil
}

// handleDeps returns the transitive dependency closure for a ticket.
func (ts *TicketService) handleDeps(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/deps"); err != nil {
		return nil, err
	}

	var request depsRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	// Find which room the ticket lives in.
	_, state, _, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	deps := state.index.Deps(request.Ticket)
	if deps == nil {
		deps = []string{}
	}

	return depsResponse{
		Ticket: request.Ticket,
		Deps:   deps,
	}, nil
}

// handleEpicHealth returns health metrics for an epic's children:
// parallelism width (ready children), completion progress, active
// fraction, and critical dependency depth. This helps PM agents
// understand whether an epic is stalling and where to focus.
func (ts *TicketService) handleEpicHealth(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/epic-health"); err != nil {
		return nil, err
	}

	var request showRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	_, state, _, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	health := state.index.EpicHealth(request.Ticket)

	return epicHealthResponse{
		Ticket: request.Ticket,
		Health: health,
	}, nil
}

// --- Matrix state writer ---

// matrixWriter is the subset of *messaging.Session needed for writing
// ticket state events to Matrix. Tests substitute a fake implementation.
type matrixWriter interface {
	SendStateEvent(ctx context.Context, roomID, eventType, stateKey string, content any) (string, error)
}

// --- Ticket ID generation ---

// ticketPrefix returns the ID prefix configured for a room, defaulting
// to "tkt" if no prefix is set.
func ticketPrefix(config *schema.TicketConfigContent) string {
	if config.Prefix != "" {
		return config.Prefix
	}
	return "tkt"
}

// generateTicketID produces a unique ticket ID by hashing the room ID,
// creation timestamp, and title, then truncating to the shortest prefix
// that avoids collision with existing tickets and the exclusion set.
// The exclusion set handles intra-batch collisions when generating
// multiple IDs in a single batch-create call; pass nil for single creates.
func (ts *TicketService) generateTicketID(state *roomState, roomID, timestamp, title string, exclude map[string]struct{}) string {
	prefix := ticketPrefix(state.config)
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

// --- Status transition validation ---

// validateStatusTransition checks whether a status transition is allowed.
// Returns nil if the transition is valid, or an error describing why it
// is rejected. The currentAssignee is included in contention error
// messages so the caller knows who owns the ticket.
//
// Allowed transitions:
//   - open → in_progress (caller must also provide assignee)
//   - open → closed (wontfix, duplicate)
//   - in_progress → open (unclaim)
//   - in_progress → closed (done)
//   - in_progress → blocked (agent hit blocker)
//   - in_progress → in_progress: REJECTED (contention)
//   - blocked → open (release back)
//   - blocked → in_progress (resume, caller must provide assignee)
//   - blocked → closed (cancelled while blocked)
//   - closed → open (reopen)
func validateStatusTransition(currentStatus, proposedStatus, currentAssignee string) error {
	if currentStatus == proposedStatus {
		if currentStatus == "in_progress" {
			return fmt.Errorf("ticket is already in_progress (assigned to %s)", currentAssignee)
		}
		return nil
	}

	switch currentStatus {
	case "open":
		switch proposedStatus {
		case "in_progress", "closed":
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case "in_progress":
		switch proposedStatus {
		case "open", "closed", "blocked":
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case "blocked":
		switch proposedStatus {
		case "open", "in_progress", "closed":
			return nil
		default:
			return fmt.Errorf("invalid status transition: %s → %s", currentStatus, proposedStatus)
		}
	case "closed":
		switch proposedStatus {
		case "open":
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
type createRequest struct {
	Room      string               `cbor:"room"`
	Title     string               `cbor:"title"`
	Body      string               `cbor:"body,omitempty"`
	Type      string               `cbor:"type"`
	Priority  int                  `cbor:"priority"`
	Labels    []string             `cbor:"labels,omitempty"`
	Parent    string               `cbor:"parent,omitempty"`
	BlockedBy []string             `cbor:"blocked_by,omitempty"`
	Gates     []schema.TicketGate  `cbor:"gates,omitempty"`
	Origin    *schema.TicketOrigin `cbor:"origin,omitempty"`
}

// updateRequest is the input for the "update" action. Pointer fields
// distinguish "not provided" (nil) from "set to zero value" (non-nil
// pointing to the zero value). Only non-nil fields are applied.
type updateRequest struct {
	Ticket    string    `cbor:"ticket"`
	Title     *string   `cbor:"title,omitempty"`
	Body      *string   `cbor:"body,omitempty"`
	Status    *string   `cbor:"status,omitempty"`
	Priority  *int      `cbor:"priority,omitempty"`
	Type      *string   `cbor:"type,omitempty"`
	Labels    *[]string `cbor:"labels,omitempty"`
	Assignee  *string   `cbor:"assignee,omitempty"`
	Parent    *string   `cbor:"parent,omitempty"`
	BlockedBy *[]string `cbor:"blocked_by,omitempty"`
}

// closeRequest is the input for the "close" action.
type closeRequest struct {
	Ticket string `cbor:"ticket"`
	Reason string `cbor:"reason,omitempty"`
}

// reopenRequest is the input for the "reopen" action.
type reopenRequest struct {
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
	Ref       string               `cbor:"ref"`
	Title     string               `cbor:"title"`
	Body      string               `cbor:"body,omitempty"`
	Type      string               `cbor:"type"`
	Priority  int                  `cbor:"priority"`
	Labels    []string             `cbor:"labels,omitempty"`
	Parent    string               `cbor:"parent,omitempty"`
	BlockedBy []string             `cbor:"blocked_by,omitempty"`
	Gates     []schema.TicketGate  `cbor:"gates,omitempty"`
	Origin    *schema.TicketOrigin `cbor:"origin,omitempty"`
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
	Content schema.TicketContent `cbor:"content"`
}

// importResponse is returned by the "import" action.
type importResponse struct {
	Room     string `json:"room"`
	Imported int    `json:"imported"`
}

// resolveGateRequest is the input for the "resolve-gate" action.
type resolveGateRequest struct {
	Ticket string `cbor:"ticket"`
	Gate   string `cbor:"gate"`
}

// updateGateRequest is the input for the "update-gate" action.
type updateGateRequest struct {
	Ticket      string `cbor:"ticket"`
	Gate        string `cbor:"gate"`
	Status      string `cbor:"status"`
	SatisfiedBy string `cbor:"satisfied_by,omitempty"`
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
	Content schema.TicketContent `json:"content"`
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

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	createdBy := "@" + token.Subject + ":" + ts.serverName

	content := schema.TicketContent{
		Version:   schema.TicketContentVersion,
		Title:     request.Title,
		Body:      request.Body,
		Status:    "open",
		Priority:  request.Priority,
		Type:      request.Type,
		Labels:    request.Labels,
		Parent:    request.Parent,
		BlockedBy: request.BlockedBy,
		Gates:     request.Gates,
		Origin:    request.Origin,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Enrich gates with creation timestamps.
	for i := range content.Gates {
		if content.Gates[i].CreatedAt == "" {
			content.Gates[i].CreatedAt = now
		}
	}

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

	ticketID := ts.generateTicketID(state, request.Room, now, content.Title, nil)

	// Write to Matrix, then update the local index so the creator
	// sees the result without a /sync round-trip.
	if _, err := ts.writer.SendStateEvent(ctx, request.Room, schema.EventTypeTicket, ticketID, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(ticketID, content)

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

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
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
		content.Type = *request.Type
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

	// Determine the proposed status and assignee, starting from
	// current values and applying any requested changes.
	proposedStatus := content.Status
	if request.Status != nil {
		proposedStatus = *request.Status
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

		if proposedStatus != content.Status {
			// Auto-clear assignee when leaving in_progress.
			if content.Status == "in_progress" && proposedStatus != "in_progress" {
				proposedAssignee = ""
			}

			// Auto-manage lifecycle fields for closed transitions.
			if proposedStatus == "closed" {
				content.ClosedAt = now
			}
			if content.Status == "closed" && proposedStatus != "closed" {
				content.ClosedAt = ""
				content.CloseReason = ""
			}
		}
	}

	// Enforce assignee/in_progress atomicity: in_progress requires
	// an assignee, and an assignee requires in_progress.
	if proposedStatus == "in_progress" && proposedAssignee == "" {
		return nil, errors.New("assignee is required when status is in_progress")
	}
	if proposedAssignee != "" && proposedStatus != "in_progress" {
		return nil, fmt.Errorf("assignee can only be set when status is in_progress (status is %q)", proposedStatus)
	}

	content.Status = proposedStatus
	content.Assignee = proposedAssignee

	// Check for dependency cycles if blocked_by changed.
	if request.BlockedBy != nil && len(content.BlockedBy) > 0 {
		if state.index.WouldCycle(request.Ticket, content.BlockedBy) {
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

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, request.Ticket, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(request.Ticket, content)

	return mutationResponse{
		ID:      request.Ticket,
		Room:    roomID,
		Content: content,
	}, nil
}

// handleClose transitions a ticket to "closed" with an optional reason.
// Validates that the current status allows closing and auto-clears the
// assignee if the ticket was in_progress.
func (ts *TicketService) handleClose(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request closeRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	if err := validateStatusTransition(content.Status, "closed", content.Assignee); err != nil {
		return nil, err
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	content.Status = "closed"
	content.ClosedAt = now
	content.CloseReason = request.Reason
	content.Assignee = ""
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, request.Ticket, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(request.Ticket, content)

	return mutationResponse{
		ID:      request.Ticket,
		Room:    roomID,
		Content: content,
	}, nil
}

// handleReopen transitions a ticket from "closed" back to "open",
// clearing the close timestamp and reason.
func (ts *TicketService) handleReopen(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/update"); err != nil {
		return nil, err
	}

	var request reopenRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}

	if request.Ticket == "" {
		return nil, errors.New("missing required field: ticket")
	}

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	if content.Status != "closed" {
		return nil, fmt.Errorf("cannot reopen: ticket status is %q, not closed", content.Status)
	}

	content.Status = "open"
	content.ClosedAt = ""
	content.CloseReason = ""
	content.UpdatedAt = ts.clock.Now().UTC().Format(time.RFC3339)

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, request.Ticket, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(request.Ticket, content)

	return mutationResponse{
		ID:      request.Ticket,
		Room:    roomID,
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

	state, err := ts.requireRoom(request.Room)
	if err != nil {
		return nil, err
	}

	if len(request.Tickets) == 0 {
		return nil, errors.New("missing required field: tickets")
	}

	now := ts.clock.Now().UTC().Format(time.RFC3339)
	createdBy := "@" + token.Subject + ":" + ts.serverName

	type pendingTicket struct {
		id      string
		content schema.TicketContent
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

		ticketID := ts.generateTicketID(state, request.Room, now, entry.Title, batchIDs)
		batchIDs[ticketID] = struct{}{}
		refToID[entry.Ref] = ticketID

		content := schema.TicketContent{
			Version:   schema.TicketContentVersion,
			Title:     entry.Title,
			Body:      entry.Body,
			Status:    "open",
			Priority:  entry.Priority,
			Type:      entry.Type,
			Labels:    entry.Labels,
			Parent:    entry.Parent,
			BlockedBy: entry.BlockedBy,
			Gates:     entry.Gates,
			Origin:    entry.Origin,
			CreatedBy: createdBy,
			CreatedAt: now,
			UpdatedAt: now,
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

	// Phase 3: Validate all tickets before writing any.
	for i := range tickets {
		if err := tickets[i].content.Validate(); err != nil {
			return nil, fmt.Errorf("tickets[%d]: %w", i, err)
		}
	}

	// Phase 4: Write all state events and update the index.
	for i := range tickets {
		if _, err := ts.writer.SendStateEvent(ctx, request.Room, schema.EventTypeTicket, tickets[i].id, tickets[i].content); err != nil {
			return nil, fmt.Errorf("writing ticket %s to Matrix: %w", tickets[i].id, err)
		}
		state.index.Put(tickets[i].id, tickets[i].content)
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

	state, err := ts.requireRoom(request.Room)
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
		if _, err := ts.writer.SendStateEvent(ctx, request.Room, schema.EventTypeTicket, entry.ID, entry.Content); err != nil {
			return nil, fmt.Errorf("writing ticket %s to Matrix: %w", entry.ID, err)
		}
		state.index.Put(entry.ID, entry.Content)
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

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	gateIndex := findGate(content.Gates, request.Gate)
	if gateIndex < 0 {
		return nil, fmt.Errorf("gate %q not found on ticket %s", request.Gate, request.Ticket)
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
	gate.SatisfiedBy = "@" + token.Subject + ":" + ts.serverName
	content.UpdatedAt = now

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, request.Ticket, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(request.Ticket, content)

	return mutationResponse{
		ID:      request.Ticket,
		Room:    roomID,
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

	roomID, state, content, found := ts.findTicket(request.Ticket)
	if !found {
		return nil, fmt.Errorf("ticket %s not found", request.Ticket)
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	gateIndex := findGate(content.Gates, request.Gate)
	if gateIndex < 0 {
		return nil, fmt.Errorf("gate %q not found on ticket %s", request.Gate, request.Ticket)
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

	if _, err := ts.writer.SendStateEvent(ctx, roomID, schema.EventTypeTicket, request.Ticket, content); err != nil {
		return nil, fmt.Errorf("writing ticket to Matrix: %w", err)
	}
	state.index.Put(request.Ticket, content)

	return mutationResponse{
		ID:      request.Ticket,
		Room:    roomID,
		Content: content,
	}, nil
}

// findGate returns the index of a gate with the given ID in the gates
// slice, or -1 if not found.
func findGate(gates []schema.TicketGate, gateID string) int {
	for i := range gates {
		if gates[i].ID == gateID {
			return i
		}
	}
	return -1
}
