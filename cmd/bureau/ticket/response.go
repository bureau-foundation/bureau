// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// Response types for decoding CBOR responses from the ticket service
// socket. These mirror the types defined in the service's socket.go
// (which lives in package main and cannot be imported). Field names
// and json tags must match the service's encoding — the CBOR codec
// falls back to json tags when cbor tags are absent.

// ticketEntry is a ticket ID paired with its content and optional
// room ID. Used by list, ready, blocked, and grep responses.
type ticketEntry struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content schema.TicketContent `json:"content"`
}

// showResult is the full detail response for a single ticket,
// including computed dependency graph fields and scoring.
type showResult struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content schema.TicketContent `json:"content"`

	Blocks      []string            `json:"blocks,omitempty"`
	ChildTotal  int                 `json:"child_total,omitempty"`
	ChildClosed int                 `json:"child_closed,omitempty"`
	Score       *ticket.TicketScore `json:"score,omitempty"`
}

// rankedEntry pairs a ticket with its composite score for the
// "ranked" action. Entries are sorted by score descending.
type rankedEntry struct {
	ID      string               `json:"id"`
	Room    string               `json:"room,omitempty"`
	Content schema.TicketContent `json:"content"`
	Score   ticket.TicketScore   `json:"score"`
}

// childrenResult includes the children list and progress summary.
type childrenResult struct {
	Parent      string        `json:"parent"`
	Children    []ticketEntry `json:"children"`
	ChildTotal  int           `json:"child_total"`
	ChildClosed int           `json:"child_closed"`
}

// depsResult is the transitive dependency closure for a ticket.
type depsResult struct {
	Ticket string   `json:"ticket"`
	Deps   []string `json:"deps"`
}

// epicHealthResult holds health metrics for an epic's children.
type epicHealthResult struct {
	Ticket string                 `json:"ticket"`
	Health ticket.EpicHealthStats `json:"health"`
}

// serviceInfo is the authenticated diagnostic response from "info".
type serviceInfo struct {
	UptimeSeconds float64      `json:"uptime_seconds"`
	Rooms         int          `json:"rooms"`
	TotalTickets  int          `json:"total_tickets"`
	RoomDetails   []roomDetail `json:"room_details"`
}

// roomDetail is a per-room summary in the info response.
type roomDetail struct {
	RoomID     string         `json:"room_id"`
	Tickets    int            `json:"tickets"`
	ByStatus   map[string]int `json:"by_status"`
	ByPriority map[int]int    `json:"by_priority"`
}

// createResult is returned by the "create" action.
type createResult struct {
	ID   string `json:"id"`
	Room string `json:"room"`
}

// batchCreateResult is returned by the "batch-create" action.
type batchCreateResult struct {
	Room string            `json:"room"`
	Refs map[string]string `json:"refs"`
}

// importResult is returned by the "import" action.
type importResult struct {
	Room     string `json:"room"`
	Imported int    `json:"imported"`
}

// mutationResult is the common response for update, close, reopen,
// and gate operations. Returns the full updated content.
type mutationResult struct {
	ID      string               `json:"id"`
	Room    string               `json:"room"`
	Content schema.TicketContent `json:"content"`
}
