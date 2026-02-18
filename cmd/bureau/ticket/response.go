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
	ID      string               `json:"id"             desc:"ticket identifier"`
	Room    string               `json:"room,omitempty"  desc:"room ID"`
	Content schema.TicketContent `json:"content"         desc:"ticket content"`
}

// showResult is the full detail response for a single ticket,
// including computed dependency graph fields and scoring.
type showResult struct {
	ID      string               `json:"id"      desc:"ticket identifier"`
	Room    string               `json:"room"    desc:"room ID"`
	Content schema.TicketContent `json:"content" desc:"ticket content"`

	Blocks      []string            `json:"blocks,omitempty"       desc:"IDs of tickets blocked by this one"`
	ChildTotal  int                 `json:"child_total,omitempty"  desc:"total child ticket count"`
	ChildClosed int                 `json:"child_closed,omitempty" desc:"closed child ticket count"`
	Score       *ticket.TicketScore `json:"score,omitempty"        desc:"computed ranking score"`
}

// rankedEntry pairs a ticket with its composite score for the
// "ranked" action. Entries are sorted by score descending.
type rankedEntry struct {
	ID      string               `json:"id"             desc:"ticket identifier"`
	Room    string               `json:"room,omitempty"  desc:"room ID"`
	Content schema.TicketContent `json:"content"         desc:"ticket content"`
	Score   ticket.TicketScore   `json:"score"           desc:"composite ranking score"`
}

// childrenResult includes the children list and progress summary.
type childrenResult struct {
	Parent      string        `json:"parent"       desc:"parent ticket ID"`
	Children    []ticketEntry `json:"children"     desc:"child ticket entries"`
	ChildTotal  int           `json:"child_total"  desc:"total child count"`
	ChildClosed int           `json:"child_closed" desc:"closed child count"`
}

// depsResult is the transitive dependency closure for a ticket.
type depsResult struct {
	Ticket string   `json:"ticket" desc:"ticket ID"`
	Deps   []string `json:"deps"   desc:"transitive dependency IDs"`
}

// epicHealthResult holds health metrics for an epic's children.
type epicHealthResult struct {
	Ticket string                 `json:"ticket" desc:"epic ticket ID"`
	Health ticket.EpicHealthStats `json:"health" desc:"epic health statistics"`
}

// serviceInfo is the authenticated diagnostic response from "info".
type serviceInfo struct {
	UptimeSeconds float64      `json:"uptime_seconds" desc:"service uptime in seconds"`
	Rooms         int          `json:"rooms"          desc:"number of tracked rooms"`
	TotalTickets  int          `json:"total_tickets"  desc:"total ticket count across all rooms"`
	RoomDetails   []roomDetail `json:"room_details"   desc:"per-room summary details"`
}

// roomDetail is a per-room summary in the info response.
type roomDetail struct {
	RoomID     string         `json:"room_id"      desc:"Matrix room ID"`
	Tickets    int            `json:"tickets"      desc:"ticket count in room"`
	ByStatus   map[string]int `json:"by_status"    desc:"ticket count by status"`
	ByPriority map[int]int    `json:"by_priority"  desc:"ticket count by priority level"`
}

// createResult is returned by the "create" action.
type createResult struct {
	ID   string `json:"id"   desc:"created ticket ID"`
	Room string `json:"room" desc:"target room ID"`
}

// batchCreateResult is returned by the "batch-create" action.
type batchCreateResult struct {
	Room string            `json:"room" desc:"target room ID"`
	Refs map[string]string `json:"refs" desc:"mapping of ref labels to ticket IDs"`
}

// importResult is returned by the "import" action.
type importResult struct {
	Room     string `json:"room"     desc:"target room ID"`
	Imported int    `json:"imported" desc:"number of tickets imported"`
}

// mutationResult is the common response for update, close, reopen,
// and gate operations. Returns the full updated content.
type mutationResult struct {
	ID      string               `json:"id"      desc:"mutated ticket ID"`
	Room    string               `json:"room"    desc:"room ID"`
	Content schema.TicketContent `json:"content" desc:"updated ticket content"`
}

// upcomingGateResult is a single upcoming timer gate with its ticket
// and room context. Mirrors the service's upcomingGateEntry type.
type upcomingGateResult struct {
	GateID      string `json:"gate_id"                   desc:"gate identifier"`
	Target      string `json:"target"                    desc:"absolute fire time (RFC 3339)"`
	Schedule    string `json:"schedule,omitempty"         desc:"cron expression for recurring gates"`
	Interval    string `json:"interval,omitempty"         desc:"Go duration for recurring gates"`
	FireCount   int    `json:"fire_count,omitempty"       desc:"number of times this gate has fired"`
	LastFiredAt string `json:"last_fired_at,omitempty"    desc:"last fire time (RFC 3339)"`
	TicketID    string `json:"ticket_id"                  desc:"ticket identifier"`
	Title       string `json:"title"                      desc:"ticket title"`
	Status      string `json:"status"                     desc:"ticket status"`
	Assignee    string `json:"assignee,omitempty"          desc:"ticket assignee"`
	Room        string `json:"room"                       desc:"room ID"`
	UntilFire   string `json:"until_fire"                 desc:"human-readable time until fire"`
}
