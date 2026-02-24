// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
)

// --- resolveReviewersForTier tests ---

func TestResolveReviewersForTier(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	// Populate room with members.
	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"):       {DisplayName: "PM"},
		ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local"): {DisplayName: "Engineer"},
		ref.MustParseUserID("@bureau/admin:bureau.local"):         {DisplayName: "Admin"},
	}

	tier := stewardship.StewardshipTier{
		Principals: []string{"iree/amdgpu/*:bureau.local"},
	}

	reviewers := ts.resolveReviewersForTier(roomID, tier, 0)
	if len(reviewers) != 2 {
		t.Fatalf("expected 2 reviewers, got %d", len(reviewers))
	}

	// Verify all are pending with correct tier.
	for _, reviewer := range reviewers {
		if reviewer.Disposition != "pending" {
			t.Errorf("reviewer %s: disposition %q, want pending", reviewer.UserID, reviewer.Disposition)
		}
		if reviewer.Tier != 0 {
			t.Errorf("reviewer %s: tier %d, want 0", reviewer.UserID, reviewer.Tier)
		}
	}

	// Verify the matched user IDs.
	userIDs := reviewerUserIDs(reviewers)
	if !userIDs[ref.MustParseUserID("@iree/amdgpu/pm:bureau.local")] {
		t.Error("expected PM to match")
	}
	if !userIDs[ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local")] {
		t.Error("expected Engineer to match")
	}
	if userIDs[ref.MustParseUserID("@bureau/admin:bureau.local")] {
		t.Error("Admin should not match iree/amdgpu/* pattern")
	}
}

func TestResolveReviewersForTierNoMatch(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@bureau/admin:bureau.local"): {DisplayName: "Admin"},
	}

	tier := stewardship.StewardshipTier{
		Principals: []string{"iree/**:bureau.local"},
	}

	reviewers := ts.resolveReviewersForTier(roomID, tier, 0)
	if len(reviewers) != 0 {
		t.Fatalf("expected 0 reviewers for non-matching pattern, got %d", len(reviewers))
	}
}

func TestResolveReviewersForTierDeduplication(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"): {DisplayName: "PM"},
	}

	// Two patterns that both match the same user.
	tier := stewardship.StewardshipTier{
		Principals: []string{
			"iree/amdgpu/*:bureau.local",
			"iree/**/pm:bureau.local",
		},
	}

	reviewers := ts.resolveReviewersForTier(roomID, tier, 0)
	if len(reviewers) != 1 {
		t.Fatalf("expected 1 reviewer (deduplicated), got %d", len(reviewers))
	}
}

func TestResolveReviewersForTierEmptyRoom(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!empty-room:bureau.local")
	// Room not in membersByRoom at all.

	tier := stewardship.StewardshipTier{
		Principals: []string{"**:*"},
	}

	reviewers := ts.resolveReviewersForTier(roomID, tier, 0)
	if len(reviewers) != 0 {
		t.Fatalf("expected 0 reviewers for missing room, got %d", len(reviewers))
	}
}

func TestResolveReviewersForTierRemappedTier(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"): {DisplayName: "PM"},
	}

	tier := stewardship.StewardshipTier{
		Principals: []string{"iree/**:bureau.local"},
	}

	reviewers := ts.resolveReviewersForTier(roomID, tier, 5)
	if len(reviewers) != 1 {
		t.Fatalf("expected 1 reviewer, got %d", len(reviewers))
	}
	if reviewers[0].Tier != 5 {
		t.Errorf("tier: got %d, want 5", reviewers[0].Tier)
	}
}

// --- buildIndependentGate tests ---

func TestBuildIndependentGate(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"):       {DisplayName: "PM"},
		ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local"): {DisplayName: "Engineer"},
		ref.MustParseUserID("@bureau/admin:bureau.local"):         {DisplayName: "Admin"},
	}

	threshold := intPtr(1)
	declaration := stewardshipindex.Declaration{
		RoomID:   roomID,
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"fleet/gpu/**"},
			GateTypes:        []string{"task", "bug"},
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"iree/amdgpu/*:bureau.local"}, Threshold: threshold},
				{Principals: []string{"bureau/admin:bureau.local"}},
			},
		},
	}

	gate, reviewers, thresholds, nextOffset := ts.buildIndependentGate(declaration, 0)

	// Gate ID should encode the state key.
	if gate.ID != "stewardship:fleet/gpu" {
		t.Errorf("gate ID: got %q, want stewardship:fleet/gpu", gate.ID)
	}
	if gate.Type != "review" {
		t.Errorf("gate type: got %q, want review", gate.Type)
	}
	if gate.Status != "pending" {
		t.Errorf("gate status: got %q, want pending", gate.Status)
	}

	// Should resolve 3 reviewers (2 in tier 0, 1 in tier 1).
	if len(reviewers) != 3 {
		t.Fatalf("expected 3 reviewers, got %d", len(reviewers))
	}

	// Two tiers → two thresholds.
	if len(thresholds) != 2 {
		t.Fatalf("expected 2 thresholds, got %d", len(thresholds))
	}
	if thresholds[0].Tier != 0 || thresholds[0].Threshold == nil || *thresholds[0].Threshold != 1 {
		t.Errorf("threshold 0: got tier=%d threshold=%v", thresholds[0].Tier, thresholds[0].Threshold)
	}
	if thresholds[1].Tier != 1 || thresholds[1].Threshold != nil {
		t.Errorf("threshold 1: got tier=%d threshold=%v (want nil/all-must-approve)", thresholds[1].Tier, thresholds[1].Threshold)
	}

	// Next offset should be 2 (past tier 0 and 1).
	if nextOffset != 2 {
		t.Errorf("next offset: got %d, want 2", nextOffset)
	}
}

func TestBuildIndependentGateWithTierOffset(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}

	declaration := stewardshipindex.Declaration{
		RoomID:   roomID,
		StateKey: "workspace/lib",
		Content: stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"workspace/lib/**"},
			GateTypes:        []string{"feature"},
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"dev/*:bureau.local"}},
			},
		},
	}

	gate, reviewers, thresholds, nextOffset := ts.buildIndependentGate(declaration, 3)

	if gate.ID != "stewardship:workspace/lib" {
		t.Errorf("gate ID: got %q", gate.ID)
	}
	if len(reviewers) != 1 {
		t.Fatalf("expected 1 reviewer, got %d", len(reviewers))
	}
	if reviewers[0].Tier != 3 {
		t.Errorf("reviewer tier: got %d, want 3", reviewers[0].Tier)
	}
	if len(thresholds) != 1 || thresholds[0].Tier != 3 {
		t.Errorf("threshold tier: got %d, want 3", thresholds[0].Tier)
	}
	if nextOffset != 4 {
		t.Errorf("next offset: got %d, want 4", nextOffset)
	}
}

// --- buildCooperativeGate tests ---

func TestBuildCooperativeGate(t *testing.T) {
	ts := newTestService()
	roomA := testRoomID("!room-a:bureau.local")
	roomB := testRoomID("!room-b:bureau.local")

	ts.membersByRoom[roomA] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@team/alpha:bureau.local"): {DisplayName: "Alpha"},
	}
	ts.membersByRoom[roomB] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@team/beta:bureau.local"): {DisplayName: "Beta"},
	}

	threshold2 := intPtr(2)
	threshold1 := intPtr(1)
	declarations := []stewardshipindex.Declaration{
		{
			RoomID:   roomA,
			StateKey: "shared/a",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"shared/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					{Principals: []string{"team/*:bureau.local"}, Threshold: threshold2},
				},
			},
		},
		{
			RoomID:   roomB,
			StateKey: "shared/b",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"shared/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					{Principals: []string{"team/*:bureau.local"}, Threshold: threshold1},
				},
			},
		},
	}

	gate, reviewers, thresholds := ts.buildCooperativeGate(declarations, 0)

	if gate.ID != "stewardship:cooperative" {
		t.Errorf("gate ID: got %q, want stewardship:cooperative", gate.ID)
	}
	if gate.Type != "review" {
		t.Errorf("gate type: got %q, want review", gate.Type)
	}

	// Two reviewers from different rooms, merged into one tier.
	if len(reviewers) != 2 {
		t.Fatalf("expected 2 reviewers, got %d", len(reviewers))
	}

	// Max threshold for tier 0 should be 2 (max of 2 and 1).
	if len(thresholds) != 1 {
		t.Fatalf("expected 1 threshold, got %d", len(thresholds))
	}
	if thresholds[0].Threshold == nil || *thresholds[0].Threshold != 2 {
		t.Errorf("threshold: got %v, want 2", thresholds[0].Threshold)
	}
}

func TestBuildCooperativeGateDeduplicatesSameUser(t *testing.T) {
	ts := newTestService()
	room := testRoomID("!shared:bureau.local")

	ts.membersByRoom[room] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@shared/reviewer:bureau.local"): {DisplayName: "Reviewer"},
	}

	declarations := []stewardshipindex.Declaration{
		{
			RoomID:   room,
			StateKey: "res/a",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"res/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					{Principals: []string{"shared/*:bureau.local"}},
				},
			},
		},
		{
			RoomID:   room,
			StateKey: "res/b",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"res/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					{Principals: []string{"shared/*:bureau.local"}},
				},
			},
		},
	}

	_, reviewers, _ := ts.buildCooperativeGate(declarations, 0)
	if len(reviewers) != 1 {
		t.Fatalf("expected 1 reviewer (deduplicated), got %d", len(reviewers))
	}
}

func TestBuildCooperativeGateNilThresholdWins(t *testing.T) {
	ts := newTestService()
	room := testRoomID("!shared:bureau.local")

	ts.membersByRoom[room] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/one:bureau.local"): {DisplayName: "One"},
	}

	threshold1 := intPtr(1)
	declarations := []stewardshipindex.Declaration{
		{
			RoomID:   room,
			StateKey: "res/a",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"res/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					{Principals: []string{"dev/*:bureau.local"}, Threshold: threshold1},
				},
			},
		},
		{
			RoomID:   room,
			StateKey: "res/b",
			Content: stewardship.StewardshipContent{
				Version:          1,
				ResourcePatterns: []string{"res/**"},
				GateTypes:        []string{"task"},
				OverlapPolicy:    "cooperative",
				Tiers: []stewardship.StewardshipTier{
					// Nil threshold means "all must approve" — stricter.
					{Principals: []string{"dev/*:bureau.local"}},
				},
			},
		},
	}

	_, _, thresholds := ts.buildCooperativeGate(declarations, 0)
	if len(thresholds) != 1 {
		t.Fatalf("expected 1 threshold, got %d", len(thresholds))
	}
	// Nil wins because "all must approve" is stricter.
	if thresholds[0].Threshold != nil {
		t.Errorf("threshold should be nil (all must approve), got %d", *thresholds[0].Threshold)
	}
}

// --- resolveStewardshipGates tests ---

func TestResolveStewardshipGatesFiltersByGateTypes(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}

	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"bug"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Ticket type "task" is not in GateTypes ["bug"].
	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task", 2)
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for non-matching ticket type, got %d", len(result.gates))
	}

	// Ticket type "bug" matches.
	result = ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "bug", 2)
	if len(result.gates) != 1 {
		t.Fatalf("expected 1 gate for matching ticket type, got %d", len(result.gates))
	}
}

func TestResolveStewardshipGatesSkipsNotifyTypes(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}

	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		NotifyTypes:      []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// "task" is in NotifyTypes, not GateTypes — no gate produced.
	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task", 2)
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for notify-only type, got %d", len(result.gates))
	}
}

func TestResolveStewardshipGatesEmptyAffects(t *testing.T) {
	ts := newTestService()
	result := ts.resolveStewardshipGates(nil, "task", 2)
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for nil affects, got %d", len(result.gates))
	}
}

func TestResolveStewardshipGatesNoMatchingDeclarations(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Affects don't match any ResourcePatterns.
	result := ts.resolveStewardshipGates([]string{"workspace/lib/foo.go"}, "task", 2)
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for non-matching affects, got %d", len(result.gates))
	}
}

func TestResolveStewardshipGatesMixedOverlapPolicies(t *testing.T) {
	ts := newTestService()
	roomA := testRoomID("!room-a:bureau.local")
	roomB := testRoomID("!room-b:bureau.local")
	roomC := testRoomID("!room-c:bureau.local")

	ts.membersByRoom[roomA] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@team/alpha:bureau.local"): {DisplayName: "Alpha"},
	}
	ts.membersByRoom[roomB] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@team/beta:bureau.local"): {DisplayName: "Beta"},
	}
	ts.membersByRoom[roomC] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@team/gamma:bureau.local"): {DisplayName: "Gamma"},
	}

	// Independent declaration.
	ts.stewardshipIndex.Put(roomA, "shared/alpha", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"shared/**"},
		GateTypes:        []string{"task"},
		OverlapPolicy:    "independent",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"team/alpha:bureau.local"}},
		},
	})

	// Two cooperative declarations.
	ts.stewardshipIndex.Put(roomB, "shared/beta", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"shared/**"},
		GateTypes:        []string{"task"},
		OverlapPolicy:    "cooperative",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"team/beta:bureau.local"}},
		},
	})
	ts.stewardshipIndex.Put(roomC, "shared/gamma", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"shared/**"},
		GateTypes:        []string{"task"},
		OverlapPolicy:    "cooperative",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"team/gamma:bureau.local"}},
		},
	})

	result := ts.resolveStewardshipGates([]string{"shared/resource"}, "task", 2)

	// Should produce 2 gates: one independent + one cooperative.
	if len(result.gates) != 2 {
		t.Fatalf("expected 2 gates, got %d", len(result.gates))
	}

	// Sort gates by ID for deterministic checking.
	sort.Slice(result.gates, func(i, j int) bool {
		return result.gates[i].ID < result.gates[j].ID
	})

	if result.gates[0].ID != "stewardship:cooperative" {
		t.Errorf("gate 0 ID: got %q, want stewardship:cooperative", result.gates[0].ID)
	}
	if result.gates[1].ID != "stewardship:shared/alpha" {
		t.Errorf("gate 1 ID: got %q, want stewardship:shared/alpha", result.gates[1].ID)
	}

	// 3 total reviewers.
	if len(result.reviewers) != 3 {
		t.Errorf("expected 3 reviewers, got %d", len(result.reviewers))
	}
}

func TestResolveStewardshipGatesSkipsDeclarationWithNoMembers(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!steward-room:bureau.local")

	// Room has no members matching the principal pattern.
	ts.membersByRoom[roomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@unrelated/user:bureau.local"): {DisplayName: "User"},
	}

	ts.stewardshipIndex.Put(roomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"iree/**:bureau.local"}},
		},
	})

	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task", 2)
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates when no members match, got %d", len(result.gates))
	}
}

// --- removeStewardshipGates tests ---

func TestRemoveStewardshipGates(t *testing.T) {
	gates := []ticket.TicketGate{
		{ID: "ci-pass", Type: "pipeline", Status: "pending"},
		{ID: "stewardship:fleet/gpu", Type: "review", Status: "pending"},
		{ID: "lead-approval", Type: "human", Status: "pending"},
		{ID: "stewardship:cooperative", Type: "review", Status: "satisfied"},
	}

	filtered := removeStewardshipGates(gates)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 gates after removal, got %d", len(filtered))
	}
	if filtered[0].ID != "ci-pass" {
		t.Errorf("gate 0: got %q, want ci-pass", filtered[0].ID)
	}
	if filtered[1].ID != "lead-approval" {
		t.Errorf("gate 1: got %q, want lead-approval", filtered[1].ID)
	}
}

func TestRemoveStewardshipGatesNoStewardship(t *testing.T) {
	gates := []ticket.TicketGate{
		{ID: "ci-pass", Type: "pipeline", Status: "pending"},
	}

	filtered := removeStewardshipGates(gates)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 gate (no change), got %d", len(filtered))
	}
}

// --- mergeStewardshipReview tests ---

func TestMergeStewardshipReviewCreatesReview(t *testing.T) {
	content := ticket.TicketContent{}
	newReviewers := []ticket.ReviewerEntry{
		{UserID: ref.MustParseUserID("@dev/lead:bureau.local"), Disposition: "pending", Tier: 0},
	}
	thresholds := []ticket.TierThreshold{
		{Tier: 0},
	}

	mergeStewardshipReview(&content, newReviewers, thresholds)

	if content.Review == nil {
		t.Fatal("expected Review to be created")
	}
	if len(content.Review.Reviewers) != 1 {
		t.Fatalf("expected 1 reviewer, got %d", len(content.Review.Reviewers))
	}
	if len(content.Review.TierThresholds) != 1 {
		t.Fatalf("expected 1 threshold, got %d", len(content.Review.TierThresholds))
	}
}

func TestMergeStewardshipReviewPreservesManualReviewers(t *testing.T) {
	content := ticket.TicketContent{
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@manual/reviewer:bureau.local"), Disposition: "approved", Tier: 0},
			},
		},
	}

	newReviewers := []ticket.ReviewerEntry{
		{UserID: ref.MustParseUserID("@steward/lead:bureau.local"), Disposition: "pending", Tier: 0},
	}

	mergeStewardshipReview(&content, newReviewers, nil)

	if len(content.Review.Reviewers) != 2 {
		t.Fatalf("expected 2 reviewers (manual + stewardship), got %d", len(content.Review.Reviewers))
	}

	// Manual reviewer should be preserved with original disposition.
	found := false
	for _, reviewer := range content.Review.Reviewers {
		if reviewer.UserID == ref.MustParseUserID("@manual/reviewer:bureau.local") {
			found = true
			if reviewer.Disposition != "approved" {
				t.Errorf("manual reviewer disposition: got %q, want approved", reviewer.Disposition)
			}
		}
	}
	if !found {
		t.Error("manual reviewer not preserved")
	}
}

func TestMergeStewardshipReviewPreservesDisposition(t *testing.T) {
	content := ticket.TicketContent{
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@dev/lead:bureau.local"), Disposition: "approved", Tier: 0},
			},
		},
	}

	// Same user appears in new stewardship set (e.g., Affects changed
	// but same user still matched).
	newReviewers := []ticket.ReviewerEntry{
		{UserID: ref.MustParseUserID("@dev/lead:bureau.local"), Disposition: "pending", Tier: 1},
	}

	mergeStewardshipReview(&content, newReviewers, nil)

	if len(content.Review.Reviewers) != 1 {
		t.Fatalf("expected 1 reviewer (merged), got %d", len(content.Review.Reviewers))
	}

	// Disposition should be preserved from the existing entry.
	if content.Review.Reviewers[0].Disposition != "approved" {
		t.Errorf("disposition: got %q, want approved (preserved)", content.Review.Reviewers[0].Disposition)
	}
	// Tier should be updated to the new value.
	if content.Review.Reviewers[0].Tier != 1 {
		t.Errorf("tier: got %d, want 1 (from new resolution)", content.Review.Reviewers[0].Tier)
	}
}

func TestMergeStewardshipReviewEmptyNewSet(t *testing.T) {
	content := ticket.TicketContent{
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@manual/reviewer:bureau.local"), Disposition: "pending"},
			},
			TierThresholds: []ticket.TierThreshold{{Tier: 0}},
		},
	}

	mergeStewardshipReview(&content, nil, nil)

	// Manual reviewer should be preserved.
	if len(content.Review.Reviewers) != 1 {
		t.Fatalf("expected 1 reviewer (manual preserved), got %d", len(content.Review.Reviewers))
	}
	// TierThresholds replaced with nil.
	if content.Review.TierThresholds != nil {
		t.Errorf("expected nil TierThresholds, got %v", content.Review.TierThresholds)
	}
}

func TestMergeStewardshipReviewNilReviewEmptyNewSet(t *testing.T) {
	content := ticket.TicketContent{}
	mergeStewardshipReview(&content, nil, nil)
	if content.Review != nil {
		t.Error("expected Review to remain nil when no reviewers")
	}
}

// --- Integration with handleCreate ---

func TestHandleCreateWithStewardshipAffects(t *testing.T) {
	rooms := mutationRooms()
	env := testMutationServer(t, rooms)
	defer env.cleanup()

	// Set up stewardship declaration.
	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"feature"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "GPU allocation feature",
		"type":     "feature",
		"priority": 1,
		"affects":  []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Verify the ticket has a stewardship review gate.
	content, exists := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index", result.ID)
	}

	var stewardshipGate *ticket.TicketGate
	for i := range content.Gates {
		if content.Gates[i].ID == "stewardship:fleet/gpu" {
			stewardshipGate = &content.Gates[i]
			break
		}
	}
	if stewardshipGate == nil {
		t.Fatal("expected stewardship:fleet/gpu gate")
	}
	if stewardshipGate.Type != "review" {
		t.Errorf("gate type: got %q, want review", stewardshipGate.Type)
	}
	if stewardshipGate.Status != "pending" {
		t.Errorf("gate status: got %q, want pending", stewardshipGate.Status)
	}
	if stewardshipGate.CreatedAt == "" {
		t.Error("gate CreatedAt should be enriched")
	}

	// Verify reviewers.
	if content.Review == nil {
		t.Fatal("expected Review to be populated")
	}
	if len(content.Review.Reviewers) != 1 {
		t.Fatalf("expected 1 reviewer, got %d", len(content.Review.Reviewers))
	}
	if content.Review.Reviewers[0].UserID != ref.MustParseUserID("@dev/lead:bureau.local") {
		t.Errorf("reviewer: got %q", content.Review.Reviewers[0].UserID)
	}

	// Verify affects was stored.
	if len(content.Affects) != 1 || content.Affects[0] != "fleet/gpu/a100" {
		t.Errorf("affects: got %v", content.Affects)
	}
}

func TestHandleCreateWithAffectsNoMatchingDeclaration(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// No stewardship declarations configured.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "unstearded feature",
		"type":     "feature",
		"priority": 1,
		"affects":  []string{"unmatched/resource"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, _ := env.service.rooms[testRoomID("!room:bureau.local")].index.Get(result.ID)
	// No stewardship gates.
	for _, gate := range content.Gates {
		if gate.ID == "stewardship:fleet/gpu" {
			t.Error("unexpected stewardship gate")
		}
	}
	// Affects stored even without matching declarations.
	if len(content.Affects) != 1 {
		t.Errorf("affects should be stored: got %v", content.Affects)
	}
}

// --- Integration with handleUpdate ---

func TestHandleUpdateChangesAffects(t *testing.T) {
	rooms := mutationRooms()
	env := testMutationServer(t, rooms)
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// Update the open ticket to add affects.
	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"room":    "!room:bureau.local",
		"ticket":  "tkt-open",
		"affects": []string{"fleet/gpu/a100"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Verify the stewardship gate was added.
	var foundGate bool
	for _, gate := range result.Content.Gates {
		if gate.ID == "stewardship:fleet/gpu" {
			foundGate = true
			if gate.Type != "review" {
				t.Errorf("gate type: got %q", gate.Type)
			}
		}
	}
	if !foundGate {
		t.Fatal("expected stewardship:fleet/gpu gate after adding affects")
	}

	// Verify reviewer.
	if result.Content.Review == nil || len(result.Content.Review.Reviewers) != 1 {
		t.Fatal("expected 1 reviewer after adding affects")
	}
}

func TestHandleUpdateClearsAffects(t *testing.T) {
	rooms := mutationRooms()
	env := testMutationServer(t, rooms)
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"feature"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	// First create a ticket with affects.
	var createResult createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "stewardship test",
		"type":     "feature",
		"priority": 1,
		"affects":  []string{"fleet/gpu/a100"},
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Now update to clear affects.
	emptyAffects := []string{}
	var result mutationResponse
	err = env.client.Call(context.Background(), "update", map[string]any{
		"room":    "!room:bureau.local",
		"ticket":  createResult.ID,
		"affects": emptyAffects,
	}, &result)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Stewardship gates should be removed.
	for _, gate := range result.Content.Gates {
		if gate.ID == "stewardship:fleet/gpu" {
			t.Error("stewardship gate should be removed when affects cleared")
		}
	}
}

// --- Socket API: stewardship-list ---

func TestHandleStewardshipList(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Description:      "GPU fleet stewardship",
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})
	env.service.stewardshipIndex.Put(stewardRoomID, "workspace/lib", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"workspace/lib/**"},
		GateTypes:        []string{"feature"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"lib/*:bureau.local"}},
		},
	})

	var result []stewardshipListEntry
	err := env.client.Call(context.Background(), "stewardship-list", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 declarations, got %d", len(result))
	}

	// Results should be sorted by state key.
	if result[0].StateKey != "fleet/gpu" {
		t.Errorf("result[0].StateKey: got %q, want fleet/gpu", result[0].StateKey)
	}
	if result[1].StateKey != "workspace/lib" {
		t.Errorf("result[1].StateKey: got %q, want workspace/lib", result[1].StateKey)
	}
	if result[0].Description != "GPU fleet stewardship" {
		t.Errorf("result[0].Description: got %q", result[0].Description)
	}
}

func TestHandleStewardshipListByRoom(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	roomA := testRoomID("!room-a:bureau.local")
	roomB := testRoomID("!room-b:bureau.local")
	env.service.stewardshipIndex.Put(roomA, "res/a", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"res/a/**"},
		GateTypes:        []string{"task"},
		Tiers:            []stewardship.StewardshipTier{{Principals: []string{"dev/*:bureau.local"}}},
	})
	env.service.stewardshipIndex.Put(roomB, "res/b", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"res/b/**"},
		GateTypes:        []string{"task"},
		Tiers:            []stewardship.StewardshipTier{{Principals: []string{"dev/*:bureau.local"}}},
	})

	var result []stewardshipListEntry
	err := env.client.Call(context.Background(), "stewardship-list", map[string]any{
		"room": roomA.String(),
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 declaration in room A, got %d", len(result))
	}
	if result[0].StateKey != "res/a" {
		t.Errorf("state_key: got %q, want res/a", result[0].StateKey)
	}
}

func TestHandleStewardshipListEmpty(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result []stewardshipListEntry
	err := env.client.Call(context.Background(), "stewardship-list", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result != nil && len(result) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(result))
	}
}

// --- Socket API: stewardship-resolve ---

func TestHandleStewardshipResolve(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	stewardRoomID := testRoomID("!steward-room:bureau.local")
	env.service.membersByRoom[stewardRoomID] = map[ref.UserID]roomMember{
		ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
	}
	env.service.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}},
		},
	})

	var result stewardshipResolveResponse
	err := env.client.Call(context.Background(), "stewardship-resolve", map[string]any{
		"affects":     []string{"fleet/gpu/a100"},
		"ticket_type": "task",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Should produce a gate and reviewer.
	if len(result.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(result.Gates))
	}
	if result.Gates[0].ID != "stewardship:fleet/gpu" {
		t.Errorf("gate ID: got %q", result.Gates[0].ID)
	}
	if len(result.Reviewers) != 1 {
		t.Fatalf("expected 1 reviewer, got %d", len(result.Reviewers))
	}

	// Should also include match info.
	if len(result.Matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(result.Matches))
	}
	if result.Matches[0].MatchedResource != "fleet/gpu/a100" {
		t.Errorf("matched_resource: got %q", result.Matches[0].MatchedResource)
	}
	if result.Matches[0].MatchedPattern != "fleet/gpu/**" {
		t.Errorf("matched_pattern: got %q", result.Matches[0].MatchedPattern)
	}
}

func TestHandleStewardshipResolveNoMatch(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result stewardshipResolveResponse
	err := env.client.Call(context.Background(), "stewardship-resolve", map[string]any{
		"affects":     []string{"nonexistent/resource"},
		"ticket_type": "task",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Gates) != 0 {
		t.Errorf("expected 0 gates, got %d", len(result.Gates))
	}
	if len(result.Matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result.Matches))
	}
}

func TestHandleStewardshipResolveMissingAffects(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result stewardshipResolveResponse
	err := env.client.Call(context.Background(), "stewardship-resolve", map[string]any{
		"ticket_type": "task",
	}, &result)
	if err == nil {
		t.Fatal("expected error for missing affects")
	}
}

// --- Socket API: stewardship-set ---

func TestHandleStewardshipSet(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result stewardshipSetResponse
	err := env.client.Call(context.Background(), "stewardship-set", map[string]any{
		"room":      "!steward-room:bureau.local",
		"state_key": "fleet/gpu",
		"content": map[string]any{
			"version":           1,
			"resource_patterns": []string{"fleet/gpu/**"},
			"gate_types":        []string{"task"},
			"tiers": []map[string]any{
				{"principals": []string{"dev/*:bureau.local"}},
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// The fakeWriter should have received the state event.
	if result.EventID.IsZero() {
		t.Error("expected non-zero event ID")
	}
}

func TestHandleStewardshipSetInvalidContent(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result stewardshipSetResponse
	err := env.client.Call(context.Background(), "stewardship-set", map[string]any{
		"room":      "!steward-room:bureau.local",
		"state_key": "fleet/gpu",
		"content": map[string]any{
			"version": 0, // Invalid: version must be >= 1.
		},
	}, &result)
	if err == nil {
		t.Fatal("expected validation error for invalid content")
	}
}

func TestHandleStewardshipSetMissingRoom(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result stewardshipSetResponse
	err := env.client.Call(context.Background(), "stewardship-set", map[string]any{
		"state_key": "fleet/gpu",
		"content": map[string]any{
			"version":           1,
			"resource_patterns": []string{"fleet/gpu/**"},
			"gate_types":        []string{"task"},
			"tiers": []map[string]any{
				{"principals": []string{"dev/*:bureau.local"}},
			},
		},
	}, &result)
	if err == nil {
		t.Fatal("expected error for missing room")
	}
}

// --- Tier satisfaction ---

func TestTierSatisfiedAllApproved(t *testing.T) {
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: nil},
		},
	}
	if !tierSatisfied(review, 0) {
		t.Error("tier 0 should be satisfied when all reviewers approved")
	}
}

func TestTierSatisfiedThresholdMet(t *testing.T) {
	threshold := 1
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
		},
	}
	if !tierSatisfied(review, 0) {
		t.Error("tier 0 should be satisfied when threshold met (1 of 2 approved)")
	}
}

func TestTierSatisfiedNotMet(t *testing.T) {
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: nil}, // All must approve.
		},
	}
	if tierSatisfied(review, 0) {
		t.Error("tier 0 should not be satisfied when not all approved")
	}
}

func TestTierSatisfiedNoReviewers(t *testing.T) {
	review := &ticket.TicketReview{
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: nil},
		},
	}
	if !tierSatisfied(review, 0) {
		t.Error("tier with no reviewers should be vacuously satisfied")
	}
}

// --- Activated last_pending tiers ---

func TestActivatedLastPendingTiersBasic(t *testing.T) {
	// Tier 0 is immediate, tier 1 is last_pending.
	// After tier 0 becomes satisfied, tier 1 should activate.
	oldReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	newReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	activated := activatedLastPendingTiers(oldReview, newReview)
	if len(activated) != 1 || activated[0] != 1 {
		t.Errorf("expected [1], got %v", activated)
	}
}

func TestActivatedLastPendingTiersNotYet(t *testing.T) {
	// Tier 0 still pending — tier 1 (last_pending) should not activate.
	oldReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	newReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	activated := activatedLastPendingTiers(oldReview, newReview)
	if len(activated) != 0 {
		t.Errorf("expected no activations, got %v", activated)
	}
}

func TestActivatedLastPendingTiersAlreadyActivated(t *testing.T) {
	// Tier 0 was already satisfied before — tier 1 should not
	// activate again (was already activatable).
	oldReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	// New review is the same — no change.
	newReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	activated := activatedLastPendingTiers(oldReview, newReview)
	if len(activated) != 0 {
		t.Errorf("expected no activations (was already activatable), got %v", activated)
	}
}

func TestActivatedLastPendingTiersAlreadySatisfied(t *testing.T) {
	// Tier 1 is last_pending but already satisfied — no notification.
	newReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "approved", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
		},
	}

	activated := activatedLastPendingTiers(nil, newReview)
	if len(activated) != 0 {
		t.Errorf("expected no activations (tier already satisfied), got %v", activated)
	}
}

func TestActivatedLastPendingTiersNoThresholds(t *testing.T) {
	// No TierThresholds — legacy mode, no last_pending concept.
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
		},
	}

	activated := activatedLastPendingTiers(nil, review)
	if len(activated) != 0 {
		t.Errorf("expected no activations (no thresholds), got %v", activated)
	}
}

func TestActivatedLastPendingTiersNilReview(t *testing.T) {
	activated := activatedLastPendingTiers(nil, nil)
	if len(activated) != 0 {
		t.Errorf("expected no activations (nil review), got %v", activated)
	}
}

func TestActivatedLastPendingTiersThreeTiers(t *testing.T) {
	// Three tiers: 0 (immediate), 1 (last_pending), 2 (last_pending).
	// When tier 0 becomes satisfied, tier 1 should activate but not
	// tier 2 (tier 1 is still unsatisfied, so tier 2's "all earlier
	// tiers satisfied" check fails).
	oldReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 1},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 2},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
			{Tier: 2, Escalation: "last_pending"},
		},
	}

	newReview := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 1},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 2},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
			{Tier: 1, Escalation: "last_pending"},
			{Tier: 2, Escalation: "last_pending"},
		},
	}

	activated := activatedLastPendingTiers(oldReview, newReview)
	if len(activated) != 1 || activated[0] != 1 {
		t.Errorf("expected [1], got %v", activated)
	}
}

// --- P0 bypass ---

func TestResolveStewardshipGatesP0Bypass(t *testing.T) {
	stewardRoomID := testRoomID("!steward-room:bureau.local")
	ts := &TicketService{
		membersByRoom: map[ref.RoomID]map[ref.UserID]roomMember{
			stewardRoomID: {
				ref.MustParseUserID("@dev/lead:bureau.local"): {DisplayName: "Lead"},
			},
		},
		stewardshipIndex: stewardshipindex.NewIndex(),
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts.stewardshipIndex.Put(stewardRoomID, "fleet/gpu", stewardship.StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/gpu/**"},
		GateTypes:        []string{"task"},
		Tiers: []stewardship.StewardshipTier{
			{Principals: []string{"dev/*:bureau.local"}, Escalation: "immediate"},
			{Principals: []string{"dev/*:bureau.local"}, Escalation: "last_pending"},
		},
	})

	// P2: escalation preserved.
	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task", 2)
	if len(result.thresholds) != 2 {
		t.Fatalf("expected 2 thresholds, got %d", len(result.thresholds))
	}
	if result.thresholds[1].Escalation != "last_pending" {
		t.Errorf("P2: tier 1 escalation = %q, want last_pending", result.thresholds[1].Escalation)
	}

	// P0: all escalation overridden to "immediate".
	result = ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task", 0)
	if len(result.thresholds) != 2 {
		t.Fatalf("expected 2 thresholds, got %d", len(result.thresholds))
	}
	for i, threshold := range result.thresholds {
		if threshold.Escalation != "immediate" {
			t.Errorf("P0: tier %d escalation = %q, want immediate", i, threshold.Escalation)
		}
	}
}

// --- Escalation message formatting ---

func TestEscalationMessage(t *testing.T) {
	content := ticket.TicketContent{
		Title: "GPU fleet change",
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
				{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "approved", Tier: 0},
				{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
				{UserID: ref.MustParseUserID("@dave:bureau.local"), Disposition: "pending", Tier: 1},
			},
		},
	}

	message := escalationMessage("tkt-a3f9", content, 1)

	if !strings.Contains(message, "tkt-a3f9") {
		t.Error("message should contain ticket ID")
	}
	if !strings.Contains(message, "GPU fleet change") {
		t.Error("message should contain ticket title")
	}
	if !strings.Contains(message, "@alice:bureau.local") {
		t.Error("message should mention earlier tier approvers")
	}
	if !strings.Contains(message, "@charlie:bureau.local") {
		t.Error("message should mention activated tier reviewers")
	}
	if !strings.Contains(message, "Tier 1 review now needed") {
		t.Error("message should indicate which tier is needed")
	}
}

// --- Snapshot review ---

func TestSnapshotReviewIndependence(t *testing.T) {
	original := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Escalation: "immediate"},
		},
	}

	snapshot := snapshotReview(original)

	// Mutate the original.
	original.Reviewers[0].Disposition = "approved"

	// Snapshot should be unchanged.
	if snapshot.Reviewers[0].Disposition != "pending" {
		t.Error("snapshot should not be affected by original mutation")
	}
}

func TestSnapshotReviewNil(t *testing.T) {
	if snapshotReview(nil) != nil {
		t.Error("snapshotReview(nil) should return nil")
	}
}

// --- Escalation via set-disposition ---

func TestSetDispositionTriggersEscalation(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// The default test token subject is @bureau/fleet/prod/agent/tester:bureau.local.
	// Use that as the tier 0 reviewer so the token is authorized.
	roomID := testRoomID("!room:bureau.local")
	reviewer0 := ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local")
	reviewer1 := ref.MustParseUserID("@dev/senior:bureau.local")

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "GPU fleet change",
		Status:    "review",
		Type:      "task",
		Priority:  2,
		CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: reviewer0, Disposition: "pending", Tier: 0},
				{UserID: reviewer1, Disposition: "pending", Tier: 1},
			},
			TierThresholds: []ticket.TierThreshold{
				{Tier: 0, Escalation: "immediate"},
				{Tier: 1, Escalation: "last_pending"},
			},
		},
		Gates: []ticket.TicketGate{
			{ID: "stewardship:fleet/gpu", Type: "review", Status: "pending", CreatedAt: "2026-01-15T12:00:00Z"},
		},
	}
	env.service.rooms[roomID].index.Put("tkt-1", content)

	// Set disposition as the default test token subject (tier 0 approval).
	var result mutationResponse
	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"room":        roomID.String(),
		"ticket":      "tkt-1",
		"disposition": "approved",
	}, &result)
	if err != nil {
		t.Fatalf("set-disposition: %v", err)
	}

	// Verify escalation message was sent.
	env.messenger.mu.Lock()
	messages := env.messenger.messages
	env.messenger.mu.Unlock()

	if len(messages) != 1 {
		t.Fatalf("expected 1 escalation message, got %d", len(messages))
	}
	if messages[0].RoomID != roomID.String() {
		t.Errorf("message sent to %q, want %q", messages[0].RoomID, roomID.String())
	}
	if !strings.Contains(messages[0].Content.Body, "tkt-1") {
		t.Error("escalation message should contain ticket ID")
	}
	if !strings.Contains(messages[0].Content.Body, "@dev/senior:bureau.local") {
		t.Error("escalation message should mention tier 1 reviewer")
	}
}

func TestSetDispositionNoEscalationWhenNotLastPending(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Tier 0 has two reviewers, both immediate. No last_pending tiers.
	roomID := testRoomID("!room:bureau.local")
	reviewer0 := ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local")

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "Simple review",
		Status:    "review",
		Type:      "task",
		Priority:  2,
		CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: reviewer0, Disposition: "pending", Tier: 0},
				{UserID: ref.MustParseUserID("@dev/senior:bureau.local"), Disposition: "pending", Tier: 0},
			},
			TierThresholds: []ticket.TierThreshold{
				{Tier: 0, Escalation: "immediate"},
			},
		},
		Gates: []ticket.TicketGate{
			{ID: "stewardship:fleet/gpu", Type: "review", Status: "pending", CreatedAt: "2026-01-15T12:00:00Z"},
		},
	}
	env.service.rooms[roomID].index.Put("tkt-2", content)

	var result mutationResponse
	err := env.client.Call(context.Background(), "set-disposition", map[string]any{
		"room":        roomID.String(),
		"ticket":      "tkt-2",
		"disposition": "approved",
	}, &result)
	if err != nil {
		t.Fatalf("set-disposition: %v", err)
	}

	// No escalation messages — no last_pending tiers.
	env.messenger.mu.Lock()
	messageCount := len(env.messenger.messages)
	env.messenger.mu.Unlock()

	if messageCount != 0 {
		t.Errorf("expected 0 escalation messages, got %d", messageCount)
	}
}

// --- Escalation wired through stewardship resolution ---

func TestBuildIndependentGatePreservesEscalation(t *testing.T) {
	stewardRoomID := testRoomID("!steward-room:bureau.local")
	ts := &TicketService{
		membersByRoom: map[ref.RoomID]map[ref.UserID]roomMember{
			stewardRoomID: {
				ref.MustParseUserID("@dev/lead:bureau.local"):   {DisplayName: "Lead"},
				ref.MustParseUserID("@dev/senior:bureau.local"): {DisplayName: "Senior"},
			},
		},
		stewardshipIndex: stewardshipindex.NewIndex(),
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	declaration := stewardshipindex.Declaration{
		RoomID:   stewardRoomID,
		StateKey: "fleet/gpu",
		Content: stewardship.StewardshipContent{
			Version:          1,
			ResourcePatterns: []string{"fleet/gpu/**"},
			Tiers: []stewardship.StewardshipTier{
				{Principals: []string{"dev/lead:bureau.local"}, Escalation: "immediate"},
				{Principals: []string{"dev/senior:bureau.local"}, Escalation: "last_pending"},
			},
		},
	}

	_, _, thresholds, _ := ts.buildIndependentGate(declaration, 0)
	if len(thresholds) != 2 {
		t.Fatalf("expected 2 thresholds, got %d", len(thresholds))
	}
	if thresholds[0].Escalation != "immediate" {
		t.Errorf("tier 0 escalation = %q, want immediate", thresholds[0].Escalation)
	}
	if thresholds[1].Escalation != "last_pending" {
		t.Errorf("tier 1 escalation = %q, want last_pending", thresholds[1].Escalation)
	}
}

// --- Helpers ---

func intPtr(value int) *int {
	return &value
}

func reviewerUserIDs(reviewers []ticket.ReviewerEntry) map[ref.UserID]bool {
	result := make(map[ref.UserID]bool, len(reviewers))
	for _, reviewer := range reviewers {
		result[reviewer.UserID] = true
	}
	return result
}
