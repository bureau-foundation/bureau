// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package stewardship

import (
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// StewardshipListEntry is a single declaration in a stewardship-list
// response. Combines the declaration's room context with its content.
type StewardshipListEntry struct {
	RoomID           ref.RoomID          `json:"room_id"            desc:"room containing the declaration"`
	StateKey         string              `json:"state_key"          desc:"declaration state key"`
	ResourcePatterns []string            `json:"resource_patterns"  desc:"glob patterns for resource matching"`
	GateTypes        []ticket.TicketType `json:"gate_types,omitempty"   desc:"ticket types that trigger gates"`
	NotifyTypes      []ticket.TicketType `json:"notify_types,omitempty" desc:"ticket types that trigger notifications"`
	OverlapPolicy    string              `json:"overlap_policy,omitempty" desc:"overlap resolution policy"`
	Description      string              `json:"description,omitempty"   desc:"human-readable description"`
	Tiers            []StewardshipTier   `json:"tiers"              desc:"review tier configuration"`
	DigestInterval   string              `json:"digest_interval,omitempty" desc:"notification digest interval"`
}

// StewardshipResolveResponse previews what gates and reviewers would
// be created for given resources and ticket type.
type StewardshipResolveResponse struct {
	Gates      []ticket.TicketGate            `json:"gates"      desc:"review gates that would be created"`
	Reviewers  []ticket.ReviewerEntry         `json:"reviewers"  desc:"reviewers that would be assigned"`
	Thresholds []ticket.TierThreshold         `json:"thresholds" desc:"per-tier approval thresholds"`
	Matches    []ticket.StewardshipMatchEntry `json:"matches"    desc:"matched declarations"`
}

// StewardshipSetResponse confirms a successful stewardship write.
type StewardshipSetResponse struct {
	EventID ref.EventID `json:"event_id" desc:"Matrix event ID of the written state event"`
}
