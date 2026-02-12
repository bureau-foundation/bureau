// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/service"
)

// registerActions registers all socket API actions on the server.
// Actions are grouped by category following the design in TICKETS.md.
func (ts *TicketService) registerActions(server *service.SocketServer) {
	// Health and diagnostics.
	server.Handle("status", ts.handleStatus)

	// Read-only query actions will be registered here as they are
	// implemented. The socket server returns "unknown action" for
	// unregistered actions, so clients get clear errors rather than
	// silent failures.
	//
	// Planned query actions:
	//   list, ready, blocked, show, children, grep, stats, deps
	//
	// Planned mutation actions (require authentication):
	//   create, update, close, reopen, batch-create,
	//   add-note, remove-note,
	//   add-gate, remove-gate, resolve-gate,
	//   attach, detach,
	//   import-jsonl, export-jsonl
}

// statusResponse is the response to the "status" action.
type statusResponse struct {
	// UptimeSeconds is how long the service has been running.
	UptimeSeconds float64 `json:"uptime_seconds"`

	// Rooms is the number of rooms with ticket management enabled.
	Rooms int `json:"rooms"`

	// TotalTickets is the total number of tickets across all rooms.
	TotalTickets int `json:"total_tickets"`

	// RoomDetails lists per-room ticket summaries.
	RoomDetails []roomSummary `json:"room_details"`
}

// handleStatus returns health and diagnostic information about the
// service. This action does not require authentication — it's a
// health check endpoint.
func (ts *TicketService) handleStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	uptime := ts.clock.Now().Sub(ts.startedAt)

	return statusResponse{
		UptimeSeconds: uptime.Seconds(),
		Rooms:         len(ts.rooms),
		TotalTickets:  ts.totalTickets(),
		RoomDetails:   ts.roomStats(),
	}, nil
}
