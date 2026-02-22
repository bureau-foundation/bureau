// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// withReadLock wraps an authenticated handler with a read lock on
// the service's shared state. Use for query handlers that only read
// the rooms map and ticket indexes.
func (ts *TicketService) withReadLock(handler service.AuthActionFunc) service.AuthActionFunc {
	return func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		ts.mu.RLock()
		defer ts.mu.RUnlock()
		return handler(ctx, token, raw)
	}
}

// withWriteLock wraps an authenticated handler with a write lock on
// the service's shared state. Use for mutation handlers that modify
// ticket indexes or the rooms map. The write lock is held for the
// entire handler including the Matrix SendStateEvent call to prevent
// read-modify-write races between concurrent mutations.
func (ts *TicketService) withWriteLock(handler service.AuthActionFunc) service.AuthActionFunc {
	return func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		return handler(ctx, token, raw)
	}
}

// registerActions registers all socket API actions on the server.
// Actions are grouped by category following the design in tickets.md.
//
// The "status" action is unauthenticated (pure liveness check) and
// does not access shared state, so it needs no locking. All other
// actions use HandleAuth and are wrapped with withReadLock (query)
// or withWriteLock (mutation) to serialize access to rooms and
// ticket indexes.
func (ts *TicketService) registerActions(server *service.SocketServer) {
	// Liveness health check — no authentication required, no shared
	// state access. Only reads immutable fields (clock, startedAt).
	server.Handle("status", ts.handleStatus)

	// Authenticated diagnostic action — reads rooms and indexes.
	server.HandleAuth("info", ts.withReadLock(ts.handleInfo))

	// Query actions — read lock, all read-only.
	server.HandleAuth("list-rooms", ts.withReadLock(ts.handleListRooms))
	server.HandleAuth("list", ts.withReadLock(ts.handleList))
	server.HandleAuth("ready", ts.withReadLock(ts.handleReady))
	server.HandleAuth("blocked", ts.withReadLock(ts.handleBlocked))
	server.HandleAuth("ranked", ts.withReadLock(ts.handleRanked))
	server.HandleAuth("show", ts.withReadLock(ts.handleShow))
	server.HandleAuth("children", ts.withReadLock(ts.handleChildren))
	server.HandleAuth("grep", ts.withReadLock(ts.handleGrep))
	server.HandleAuth("search", ts.withReadLock(ts.handleSearch))
	server.HandleAuth("stats", ts.withReadLock(ts.handleStats))
	server.HandleAuth("deps", ts.withReadLock(ts.handleDeps))
	server.HandleAuth("epic-health", ts.withReadLock(ts.handleEpicHealth))
	server.HandleAuth("upcoming-gates", ts.withReadLock(ts.handleUpcomingGates))

	// Mutation actions — write lock, all write to Matrix and update
	// the local index.
	server.HandleAuth("create", ts.withWriteLock(ts.handleCreate))
	server.HandleAuth("update", ts.withWriteLock(ts.handleUpdate))
	server.HandleAuth("close", ts.withWriteLock(ts.handleClose))
	server.HandleAuth("reopen", ts.withWriteLock(ts.handleReopen))
	server.HandleAuth("batch-create", ts.withWriteLock(ts.handleBatchCreate))
	server.HandleAuth("import", ts.withWriteLock(ts.handleImport))
	server.HandleAuth("add-note", ts.withWriteLock(ts.handleAddNote))
	server.HandleAuth("resolve-gate", ts.withWriteLock(ts.handleResolveGate))
	server.HandleAuth("update-gate", ts.withWriteLock(ts.handleUpdateGate))
	server.HandleAuth("defer", ts.withWriteLock(ts.handleDefer))

	// Stream actions — long-lived authenticated connections. The
	// handler takes ownership of the connection after the initial
	// CBOR handshake and manages its own locking.
	server.HandleAuthStream("subscribe", ts.handleSubscribe)
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

// requireRoom validates the "room" field from a request, parses it as
// a ref.RoomID, and returns the corresponding room state and parsed ID.
// Returns an error if the room field is empty, unparseable, or the room
// is not tracked.
func (ts *TicketService) requireRoom(roomIDString string) (ref.RoomID, *roomState, error) {
	if roomIDString == "" {
		return ref.RoomID{}, nil, errors.New("missing required field: room")
	}
	roomID, err := ref.ParseRoomID(roomIDString)
	if err != nil {
		return ref.RoomID{}, nil, fmt.Errorf("invalid room ID %q: %w", roomIDString, err)
	}
	state, exists := ts.rooms[roomID]
	if !exists {
		return ref.RoomID{}, nil, fmt.Errorf("room %s is not tracked by this service", roomID)
	}
	return roomID, state, nil
}

// --- Ticket resolution ---

// resolveTicket resolves a ticket reference to a room ID, room state,
// bare ticket ID, and ticket content. The ticket reference may be:
//
//   - A bare ticket ID ("tkt-a3f9") with room context in requestRoom
//   - A room-qualified reference ("iree/general/tkt-a3f9") with the
//     room alias localpart embedded in the reference
//
// If requestRoom is a room ID (starts with "!"), it is used directly.
// If requestRoom is a room alias localpart, it is matched against
// tracked rooms' canonical aliases. If both the ticket reference and
// requestRoom carry room context, they must agree.
func (ts *TicketService) resolveTicket(requestRoom, ticketRefStr string) (ref.RoomID, *roomState, string, ticket.TicketContent, error) {
	ticketRef := ticketindex.ParseTicketRef(ticketRefStr)

	// Determine room context: from the reference, from the request, or both.
	roomID, err := ts.resolveRoomContext(requestRoom, ticketRef)
	if err != nil {
		return ref.RoomID{}, nil, "", ticket.TicketContent{}, err
	}

	state, exists := ts.rooms[roomID]
	if !exists {
		return ref.RoomID{}, nil, "", ticket.TicketContent{}, fmt.Errorf("room %s is not tracked by this service", roomID)
	}

	content, exists := state.index.Get(ticketRef.Ticket)
	if !exists {
		return ref.RoomID{}, nil, "", ticket.TicketContent{}, fmt.Errorf("ticket %s not found in room %s", ticketRef.Ticket, roomID)
	}

	return roomID, state, ticketRef.Ticket, content, nil
}

// resolveRoomContext determines the room ID from a combination of the
// explicit Room field on the request and the room qualifier embedded
// in a ticket reference. Returns an error if no room context is
// available or if both sources disagree.
func (ts *TicketService) resolveRoomContext(requestRoom string, ticketRef ticketindex.TicketRef) (ref.RoomID, error) {
	refRoomID, refFound := ts.resolveRoomRef(ticketRef)
	requestRoomID, requestFound := ts.resolveRoomString(requestRoom)

	switch {
	case refFound && requestFound:
		if refRoomID != requestRoomID {
			return ref.RoomID{}, fmt.Errorf("room mismatch: ticket reference implies %s but request specifies %s", refRoomID, requestRoomID)
		}
		return refRoomID, nil
	case refFound:
		return refRoomID, nil
	case requestFound:
		return requestRoomID, nil
	default:
		return ref.RoomID{}, errors.New("ticket reference requires room context: use a room-qualified ticket reference (e.g., iree/general/tkt-a3f9) or provide room in the request")
	}
}

// resolveRoomRef resolves the room qualifier from a parsed ticket
// reference to a room ID. Returns empty string if the reference is
// bare or the room cannot be resolved.
func (ts *TicketService) resolveRoomRef(ticketRef ticketindex.TicketRef) (ref.RoomID, bool) {
	if ticketRef.IsBare() {
		return ref.RoomID{}, false
	}
	var serverName ref.ServerName
	if ticketRef.ServerName != "" {
		var err error
		serverName, err = ref.ParseServerName(ticketRef.ServerName)
		if err != nil {
			return ref.RoomID{}, false
		}
	}
	return ts.matchRoomAlias(ticketRef.RoomLocalpart, serverName)
}

// resolveRoomString resolves a room string (from the Room field on a
// request) to a room ID. The string may be a room ID (starts with
// "!"), a room alias localpart, or empty.
func (ts *TicketService) resolveRoomString(room string) (ref.RoomID, bool) {
	if room == "" {
		return ref.RoomID{}, false
	}
	// Room IDs start with "!" and are used directly.
	if len(room) > 0 && room[0] == '!' {
		roomID, err := ref.ParseRoomID(room)
		if err != nil {
			return ref.RoomID{}, false
		}
		return roomID, true
	}
	// Otherwise treat as an alias localpart on this server.
	return ts.matchRoomAlias(room, ref.ServerName{})
}

// matchRoomAlias finds the room ID for a room alias localpart among
// tracked rooms. If serverName is zero, matches against this
// server's name. Returns zero RoomID if no match is found.
func (ts *TicketService) matchRoomAlias(localpart string, serverName ref.ServerName) (ref.RoomID, bool) {
	if serverName.IsZero() {
		serverName = ts.service.Server()
	}
	// Build the expected canonical alias: "#localpart:server"
	expectedAlias := schema.FullRoomAlias(localpart, serverName)
	for roomID, state := range ts.rooms {
		if state.alias == expectedAlias {
			return roomID, true
		}
	}
	return ref.RoomID{}, false
}

// --- Matrix state writer ---

// matrixWriter is the subset of *messaging.DirectSession needed for writing
// ticket state events to Matrix. Tests substitute a fake implementation.
type matrixWriter interface {
	SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error)
}

// findGate returns the index of a gate with the given ID in the gates
// slice, or -1 if not found.
func findGate(gates []ticket.TicketGate, gateID string) int {
	for i := range gates {
		if gates[i].ID == gateID {
			return i
		}
	}
	return -1
}
