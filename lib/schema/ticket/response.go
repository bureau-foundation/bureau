// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import "github.com/bureau-foundation/bureau/lib/ref"

// TicketEntry is a ticket ID paired with its content and optional
// room ID. Used by list, ready, blocked, and grep responses.
type TicketEntry struct {
	ID      string        `json:"id"             desc:"ticket identifier"`
	Room    string        `json:"room,omitempty"  desc:"room ID"`
	Content TicketContent `json:"content"         desc:"ticket content"`

	// Stewardship gate summary — populated when the ticket has
	// stewardship review gates (gates with "stewardship:" prefix).
	// Omitted when no stewardship gates exist.
	StewardshipGates     int `json:"stewardship_gates,omitempty"     desc:"total stewardship review gates"`
	StewardshipSatisfied int `json:"stewardship_satisfied,omitempty" desc:"satisfied stewardship review gates"`
}

// ShowResponse is the full detail response for a single ticket,
// including computed dependency graph fields, scoring, and
// stewardship governance context.
type ShowResponse struct {
	ID      string        `json:"id"      desc:"ticket identifier"`
	Room    string        `json:"room"    desc:"room ID"`
	Content TicketContent `json:"content" desc:"ticket content"`

	// Computed fields from the dependency graph.
	Blocks      []string `json:"blocks,omitempty"       desc:"IDs of tickets blocked by this one"`
	ChildTotal  int      `json:"child_total,omitempty"  desc:"total child ticket count"`
	ChildClosed int      `json:"child_closed,omitempty" desc:"closed child ticket count"`

	// Score holds the ranking dimensions for an open ticket. Nil
	// for closed tickets where scoring is not meaningful.
	Score *TicketScore `json:"score,omitempty" desc:"computed ranking score"`

	// Stewardship holds the governance context for this ticket:
	// which declarations matched, and per-gate tier approval
	// progress. Nil when the ticket has no Affects or no matching
	// declarations.
	Stewardship *StewardshipContext `json:"stewardship,omitempty" desc:"stewardship governance context"`
}

// SearchEntry pairs a ticket entry with its search relevance score.
// Score combines BM25 text relevance with exact-match and
// graph-proximity boosting.
type SearchEntry struct {
	ID      string        `json:"id"              desc:"ticket identifier"`
	Room    string        `json:"room,omitempty"   desc:"room ID"`
	Content TicketContent `json:"content"          desc:"ticket content"`
	Score   float64       `json:"score"            desc:"search relevance score"`
}

// RankedEntry pairs a ticket with its composite score. Entries are
// sorted by score descending (highest-leverage first).
type RankedEntry struct {
	ID      string        `json:"id"             desc:"ticket identifier"`
	Room    string        `json:"room,omitempty"  desc:"room ID"`
	Content TicketContent `json:"content"         desc:"ticket content"`
	Score   TicketScore   `json:"score"           desc:"composite ranking score"`
}

// ChildrenResponse includes the children list and progress summary.
type ChildrenResponse struct {
	Parent      string        `json:"parent"       desc:"parent ticket ID"`
	Children    []TicketEntry `json:"children"     desc:"child ticket entries"`
	ChildTotal  int           `json:"child_total"  desc:"total child count"`
	ChildClosed int           `json:"child_closed" desc:"closed child count"`
}

// DepsResponse is the transitive dependency closure for a ticket.
type DepsResponse struct {
	Ticket string   `json:"ticket" desc:"ticket ID"`
	Deps   []string `json:"deps"   desc:"transitive dependency IDs"`
}

// EpicHealthResponse holds health metrics for an epic's children.
type EpicHealthResponse struct {
	Ticket string          `json:"ticket" desc:"epic ticket ID"`
	Health EpicHealthStats `json:"health" desc:"epic health statistics"`
}

// UpcomingGateEntry is a single upcoming timer gate with its ticket
// and room context. Entries are sorted by target time ascending.
type UpcomingGateEntry struct {
	// Gate metadata.
	GateID      string `json:"gate_id"                   desc:"gate identifier"`
	Target      string `json:"target"                    desc:"absolute fire time (RFC 3339)"`
	Schedule    string `json:"schedule,omitempty"         desc:"cron expression for recurring gates"`
	Interval    string `json:"interval,omitempty"         desc:"Go duration for recurring gates"`
	FireCount   int    `json:"fire_count,omitempty"       desc:"number of times this gate has fired"`
	LastFiredAt string `json:"last_fired_at,omitempty"    desc:"last fire time (RFC 3339)"`

	// Ticket context.
	TicketID string `json:"ticket_id"                  desc:"ticket identifier"`
	Title    string `json:"title"                      desc:"ticket title"`
	Status   string `json:"status"                     desc:"ticket status"`
	Assignee string `json:"assignee,omitempty"          desc:"ticket assignee"`
	Room     string `json:"room"                       desc:"room ID"`

	// Computed fields.
	UntilFire string `json:"until_fire"                 desc:"human-readable time until fire"`
}

// --- Mutation response types ---

// CreateResponse is returned by the "create" action.
type CreateResponse struct {
	ID   string `json:"id"   desc:"created ticket ID"`
	Room string `json:"room" desc:"target room ID"`
}

// BatchCreateResponse is returned by the "batch-create" action.
type BatchCreateResponse struct {
	Room string            `json:"room" desc:"target room ID"`
	Refs map[string]string `json:"refs" desc:"mapping of ref labels to ticket IDs"`
}

// ImportResponse is returned by the "import" action.
type ImportResponse struct {
	Room     string `json:"room"     desc:"target room ID"`
	Imported int    `json:"imported" desc:"number of tickets imported"`
}

// MutationResponse is the common response for update, close, reopen,
// and gate operations. Returns the full updated content.
type MutationResponse struct {
	ID      string        `json:"id"      desc:"mutated ticket ID"`
	Room    string        `json:"room"    desc:"room ID"`
	Content TicketContent `json:"content" desc:"updated ticket content"`
}

// --- Stewardship context types (embedded in ShowResponse) ---

// StewardshipMatchEntry describes a single declaration match. Used
// in both ShowResponse.Stewardship.Declarations and the stewardship
// resolve response (in lib/schema/stewardship).
type StewardshipMatchEntry struct {
	RoomID          ref.RoomID `json:"room_id"           desc:"room containing the declaration"`
	StateKey        string     `json:"state_key"         desc:"declaration state key"`
	MatchedPattern  string     `json:"matched_pattern"   desc:"glob pattern that matched"`
	MatchedResource string     `json:"matched_resource"  desc:"resource identifier that matched"`
	OverlapPolicy   string     `json:"overlap_policy"    desc:"overlap resolution policy"`
	Description     string     `json:"description,omitempty" desc:"human-readable description"`
}

// StewardshipContext describes the governance context for a ticket:
// which stewardship declarations matched its Affects field, and the
// per-gate tier approval progress for each stewardship review gate.
type StewardshipContext struct {
	// Declarations lists the stewardship declarations that matched
	// the ticket's Affects field.
	Declarations []StewardshipMatchEntry `json:"declarations,omitempty" desc:"matched stewardship declarations"`

	// Gates summarizes the approval progress for each stewardship
	// review gate on the ticket.
	Gates []StewardshipGateProgress `json:"gates,omitempty" desc:"per-gate approval progress"`
}

// StewardshipGateProgress describes the approval progress for a
// single stewardship review gate.
type StewardshipGateProgress struct {
	GateID string                `json:"gate_id" desc:"gate identifier"`
	Status GateStatus            `json:"status"  desc:"gate status"`
	Tiers  []TierApprovalSummary `json:"tiers,omitempty" desc:"per-tier approval state"`
}

// TierApprovalSummary describes the approval state of a single tier
// within a stewardship review gate.
type TierApprovalSummary struct {
	Tier      int  `json:"tier"                desc:"tier number"`
	Total     int  `json:"total"               desc:"total reviewers in tier"`
	Approved  int  `json:"approved"            desc:"approved reviewers in tier"`
	Threshold *int `json:"threshold,omitempty" desc:"approval threshold (nil = unanimous)"`
	Satisfied bool `json:"satisfied"           desc:"whether this tier is satisfied"`
}

// --- Info/diagnostic response types ---

// InfoResponse is the authenticated diagnostic response from the
// ticket service "info" action.
type InfoResponse struct {
	UptimeSeconds float64       `json:"uptime_seconds" desc:"service uptime in seconds"`
	Rooms         int           `json:"rooms"          desc:"number of tracked rooms"`
	TotalTickets  int           `json:"total_tickets"  desc:"total ticket count across all rooms"`
	RoomDetails   []RoomSummary `json:"room_details"   desc:"per-room summary details"`
}

// RoomSummary is a per-room summary in the info response. Shows
// ticket counts broken down by status and priority.
type RoomSummary struct {
	RoomID     ref.RoomID     `json:"room_id"      desc:"Matrix room ID"`
	Tickets    int            `json:"tickets"      desc:"ticket count in room"`
	ByStatus   map[string]int `json:"by_status"    desc:"ticket count by status"`
	ByPriority map[int]int    `json:"by_priority"  desc:"ticket count by priority level"`
}

// RoomInfo describes a single tracked room for the list-rooms
// response. Includes the room's canonical alias, ticket ID prefix,
// and aggregate statistics.
type RoomInfo struct {
	RoomID string `json:"room_id"          desc:"Matrix room ID"`
	Alias  string `json:"alias,omitempty"  desc:"room canonical alias"`
	Prefix string `json:"prefix,omitempty" desc:"ticket ID prefix"`

	// Stats embeds the room's aggregate ticket counts. Uses a
	// generic map to avoid importing ticketindex (which would
	// create an import cycle). The stats shape matches
	// ticketindex.Stats: total, by_status, by_priority, by_type.
	Total      int            `json:"total"       desc:"total ticket count"`
	ByStatus   map[string]int `json:"by_status"   desc:"ticket count by status"`
	ByPriority map[int]int    `json:"by_priority" desc:"ticket count by priority level"`
	ByType     map[string]int `json:"by_type"     desc:"ticket count by type"`
}

// MemberInfo describes a joined member of a tracked room, enriched
// with presence state from the ticket service's /sync loop.
type MemberInfo struct {
	UserID          ref.UserID `json:"user_id"          desc:"Matrix user ID"`
	DisplayName     string     `json:"display_name"     desc:"display name"`
	Presence        string     `json:"presence"         desc:"presence state (online, unavailable, offline)"`
	StatusMsg       string     `json:"status_msg"       desc:"custom status message"`
	CurrentlyActive bool       `json:"currently_active" desc:"whether currently active"`
}
