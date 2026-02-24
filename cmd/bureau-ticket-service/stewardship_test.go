// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sort"
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
	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task")
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for non-matching ticket type, got %d", len(result.gates))
	}

	// Ticket type "bug" matches.
	result = ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "bug")
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
	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task")
	if len(result.gates) != 0 {
		t.Fatalf("expected 0 gates for notify-only type, got %d", len(result.gates))
	}
}

func TestResolveStewardshipGatesEmptyAffects(t *testing.T) {
	ts := newTestService()
	result := ts.resolveStewardshipGates(nil, "task")
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
	result := ts.resolveStewardshipGates([]string{"workspace/lib/foo.go"}, "task")
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

	result := ts.resolveStewardshipGates([]string{"shared/resource"}, "task")

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

	result := ts.resolveStewardshipGates([]string{"fleet/gpu/a100"}, "task")
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
