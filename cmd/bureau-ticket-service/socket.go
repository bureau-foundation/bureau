// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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
