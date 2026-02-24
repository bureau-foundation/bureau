// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
)

// --- stewardship-list ---

// stewardshipListRequest requests all stewardship declarations,
// optionally scoped to a single room.
type stewardshipListRequest struct {
	Room string `cbor:"room,omitempty"`
}

// stewardshipListEntry is a single declaration in a stewardship-list
// response. Combines the declaration's room context with its content.
type stewardshipListEntry struct {
	RoomID           ref.RoomID                    `cbor:"room_id"`
	StateKey         string                        `cbor:"state_key"`
	ResourcePatterns []string                      `cbor:"resource_patterns"`
	GateTypes        []string                      `cbor:"gate_types,omitempty"`
	NotifyTypes      []string                      `cbor:"notify_types,omitempty"`
	OverlapPolicy    string                        `cbor:"overlap_policy,omitempty"`
	Description      string                        `cbor:"description,omitempty"`
	Tiers            []stewardship.StewardshipTier `cbor:"tiers"`
	DigestInterval   string                        `cbor:"digest_interval,omitempty"`
}

// handleStewardshipList returns all stewardship declarations in the
// index, optionally scoped to a single room. Results are sorted by
// room ID then state key for deterministic output.
func (ts *TicketService) handleStewardshipList(_ context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/stewardship-list"); err != nil {
		return nil, err
	}

	var request stewardshipListRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	var declarations []stewardshipindex.Declaration
	if request.Room != "" {
		roomID, err := ref.ParseRoomID(request.Room)
		if err != nil {
			return nil, fmt.Errorf("invalid room: %w", err)
		}
		declarations = ts.stewardshipIndex.DeclarationsInRoom(roomID)
	} else {
		declarations = ts.stewardshipIndex.All()
	}

	// Sort for deterministic output.
	sort.Slice(declarations, func(i, j int) bool {
		if declarations[i].RoomID != declarations[j].RoomID {
			return declarations[i].RoomID.String() < declarations[j].RoomID.String()
		}
		return declarations[i].StateKey < declarations[j].StateKey
	})

	entries := make([]stewardshipListEntry, len(declarations))
	for i, declaration := range declarations {
		entries[i] = stewardshipListEntry{
			RoomID:           declaration.RoomID,
			StateKey:         declaration.StateKey,
			ResourcePatterns: declaration.Content.ResourcePatterns,
			GateTypes:        declaration.Content.GateTypes,
			NotifyTypes:      declaration.Content.NotifyTypes,
			OverlapPolicy:    declaration.Content.OverlapPolicy,
			Description:      declaration.Content.Description,
			Tiers:            declaration.Content.Tiers,
			DigestInterval:   declaration.Content.DigestInterval,
		}
	}

	return entries, nil
}

// --- stewardship-resolve ---

// stewardshipResolveRequest is a dry-run resolution of stewardship
// declarations against given resource identifiers and ticket type.
type stewardshipResolveRequest struct {
	Affects    []string `cbor:"affects"`
	TicketType string   `cbor:"ticket_type"`
}

// stewardshipResolveResponse previews what gates and reviewers would
// be created for the given affects and ticket type.
type stewardshipResolveResponse struct {
	Gates      []ticket.TicketGate     `cbor:"gates"`
	Reviewers  []ticket.ReviewerEntry  `cbor:"reviewers"`
	Thresholds []ticket.TierThreshold  `cbor:"thresholds"`
	Matches    []stewardshipMatchEntry `cbor:"matches"`
}

// stewardshipMatchEntry describes a single declaration match for the
// resolve preview.
type stewardshipMatchEntry struct {
	RoomID          ref.RoomID `cbor:"room_id"`
	StateKey        string     `cbor:"state_key"`
	MatchedPattern  string     `cbor:"matched_pattern"`
	MatchedResource string     `cbor:"matched_resource"`
	OverlapPolicy   string     `cbor:"overlap_policy"`
	Description     string     `cbor:"description,omitempty"`
}

// handleStewardshipResolve performs a dry-run stewardship resolution.
// Given resource identifiers and a ticket type, it returns what review
// gates, reviewers, and thresholds would be created — without actually
// modifying any ticket. This lets agents and operators preview
// stewardship effects before creating or updating tickets.
func (ts *TicketService) handleStewardshipResolve(_ context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/stewardship-resolve"); err != nil {
		return nil, err
	}

	var request stewardshipResolveRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if len(request.Affects) == 0 {
		return nil, errors.New("affects is required and must be non-empty")
	}
	if request.TicketType == "" {
		return nil, errors.New("ticket_type is required")
	}

	// Build the match list before resolution (for the preview).
	rawMatches := ts.stewardshipIndex.Resolve(request.Affects)
	var matchEntries []stewardshipMatchEntry
	for _, match := range rawMatches {
		policy := match.Declaration.Content.OverlapPolicy
		if policy == "" {
			policy = "independent"
		}
		matchEntries = append(matchEntries, stewardshipMatchEntry{
			RoomID:          match.Declaration.RoomID,
			StateKey:        match.Declaration.StateKey,
			MatchedPattern:  match.MatchedPattern,
			MatchedResource: match.MatchedResource,
			OverlapPolicy:   policy,
			Description:     match.Declaration.Content.Description,
		})
	}

	// Resolve gates and reviewers using the same logic as create/update.
	result := ts.resolveStewardshipGates(request.Affects, request.TicketType)

	return stewardshipResolveResponse{
		Gates:      result.gates,
		Reviewers:  result.reviewers,
		Thresholds: result.thresholds,
		Matches:    matchEntries,
	}, nil
}

// --- stewardship-set ---

// stewardshipSetRequest writes a stewardship declaration to a room
// via a m.bureau.stewardship state event.
type stewardshipSetRequest struct {
	Room     string                         `cbor:"room"`
	StateKey string                         `cbor:"state_key"`
	Content  stewardship.StewardshipContent `cbor:"content"`
}

// stewardshipSetResponse confirms a successful stewardship write.
type stewardshipSetResponse struct {
	EventID ref.EventID `cbor:"event_id"`
}

// handleStewardshipSet writes a stewardship declaration to the
// specified room. The declaration is validated before writing. The
// state event arrives back through /sync, which updates the
// stewardship index — we don't update the index here to avoid
// double-processing.
func (ts *TicketService) handleStewardshipSet(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "ticket/stewardship-set"); err != nil {
		return nil, err
	}

	var request stewardshipSetRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Room == "" {
		return nil, errors.New("room is required")
	}
	if request.StateKey == "" {
		return nil, errors.New("state_key is required")
	}

	roomID, err := ref.ParseRoomID(request.Room)
	if err != nil {
		return nil, fmt.Errorf("invalid room: %w", err)
	}

	if err := request.Content.Validate(); err != nil {
		return nil, err
	}

	eventID, err := ts.writer.SendStateEvent(
		ctx,
		roomID,
		schema.EventTypeStewardship,
		request.StateKey,
		request.Content,
	)
	if err != nil {
		return nil, fmt.Errorf("writing stewardship event: %w", err)
	}

	return stewardshipSetResponse{EventID: eventID}, nil
}
