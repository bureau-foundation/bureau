// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

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
	server.HandleAuth("show", ts.handleShow)
	server.HandleAuth("children", ts.handleChildren)
	server.HandleAuth("grep", ts.handleGrep)
	server.HandleAuth("stats", ts.handleStats)
	server.HandleAuth("deps", ts.handleDeps)
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
// from the dependency graph and child progress.
type showResponse struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content schema.TicketContent `json:"content"`

	// Computed fields from the dependency graph.
	Blocks      []string `json:"blocks,omitempty"`
	ChildTotal  int      `json:"child_total,omitempty"`
	ChildClosed int      `json:"child_closed,omitempty"`
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

// handleReady returns open tickets with no open blockers and all
// gates satisfied within a room.
func (ts *TicketService) handleReady(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, "ticket/ready", (*ticket.Index).Ready)
}

// handleBlocked returns open tickets that cannot be started: they
// have at least one non-closed blocker or unsatisfied gate.
func (ts *TicketService) handleBlocked(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	return ts.roomQuery(token, raw, "ticket/blocked", (*ticket.Index).Blocked)
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

	return showResponse{
		ID:          request.Ticket,
		Room:        roomID,
		Content:     content,
		Blocks:      blocks,
		ChildTotal:  childTotal,
		ChildClosed: childClosed,
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
