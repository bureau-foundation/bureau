// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package relayauth

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Test fixtures for source rooms. Bureau naming convention:
//
//	#my_bureau/fleet/prod/workspace/feature-x:bureau.local
//	 └namespace┘└────fleet──────┘└──room────────────────┘
const testServer = "bureau.local"

func workspaceSource() SourceRoom {
	return SourceRoom{
		RoomID: ref.MustParseRoomID("!workspace1:bureau.local"),
		Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/prod/workspace/feature-x:" + testServer),
	}
}

func otherFleetSource() SourceRoom {
	return SourceRoom{
		RoomID: ref.MustParseRoomID("!workspace2:bureau.local"),
		Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/staging/workspace/hotfix:" + testServer),
	}
}

func otherNamespaceSource() SourceRoom {
	return SourceRoom{
		RoomID: ref.MustParseRoomID("!workspace3:bureau.local"),
		Alias:  ref.MustParseRoomAlias("#other_org/fleet/dev/workspace/test:" + testServer),
	}
}

func noAliasSource() SourceRoom {
	return SourceRoom{
		RoomID: ref.MustParseRoomID("!noalias:bureau.local"),
	}
}

func TestEvaluate_NilPolicy(t *testing.T) {
	result := Evaluate(nil, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("nil policy should deny")
	}
	if result.DenialReason == "" {
		t.Fatal("denial should have a reason")
	}
	if result.MatchedSource != nil {
		t.Fatal("denied result should have nil MatchedSource")
	}
}

func TestEvaluate_EmptySources(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("empty sources should deny")
	}
	if result.DenialReason == "" {
		t.Fatal("denial should have a reason")
	}
}

func TestEvaluate_FleetMember_Match(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if !result.Authorized {
		t.Fatalf("fleet member should match: %s", result.DenialReason)
	}
	if result.MatchedSource == nil {
		t.Fatal("authorized result should have MatchedSource")
	}
	if result.MatchedSource.Fleet != "my_bureau/fleet/prod" {
		t.Errorf("MatchedSource.Fleet = %q", result.MatchedSource.Fleet)
	}
}

func TestEvaluate_FleetMember_WrongFleet(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
		},
	}
	// staging fleet should not match prod fleet source.
	result := Evaluate(policy, otherFleetSource(), "resource_request")
	if result.Authorized {
		t.Fatal("wrong fleet should deny")
	}
}

func TestEvaluate_FleetMember_NoAlias(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
		},
	}
	result := Evaluate(policy, noAliasSource(), "resource_request")
	if result.Authorized {
		t.Fatal("no-alias room should not match fleet_member")
	}
}

func TestEvaluate_FleetMember_PrefixNotSubstring(t *testing.T) {
	// Ensure "my_bureau/fleet/production" does NOT match fleet
	// "my_bureau/fleet/prod" — the "/" delimiter prevents substring
	// collision.
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
		},
	}
	source := SourceRoom{
		RoomID: ref.MustParseRoomID("!ws:bureau.local"),
		Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/production/workspace/x:" + testServer),
	}
	result := Evaluate(policy, source, "resource_request")
	if result.Authorized {
		t.Fatal("production fleet should not match prod fleet (prefix boundary)")
	}
}

func TestEvaluate_Room_Match(t *testing.T) {
	source := workspaceSource()
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  source.RoomID,
			},
		},
	}
	result := Evaluate(policy, source, "resource_request")
	if !result.Authorized {
		t.Fatalf("exact room ID should match: %s", result.DenialReason)
	}
}

func TestEvaluate_Room_WrongRoom(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!different:bureau.local"),
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("different room ID should deny")
	}
}

func TestEvaluate_Room_NoAliasStillMatches(t *testing.T) {
	// Room match works without an alias — it uses room ID only.
	source := noAliasSource()
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  source.RoomID,
			},
		},
	}
	result := Evaluate(policy, source, "resource_request")
	if !result.Authorized {
		t.Fatalf("room match should work without alias: %s", result.DenialReason)
	}
}

func TestEvaluate_Namespace_Match(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if !result.Authorized {
		t.Fatalf("namespace should match: %s", result.DenialReason)
	}
}

func TestEvaluate_Namespace_WrongNamespace(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}
	result := Evaluate(policy, otherNamespaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("other_org namespace should not match my_bureau")
	}
}

func TestEvaluate_Namespace_NoAlias(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}
	result := Evaluate(policy, noAliasSource(), "resource_request")
	if result.Authorized {
		t.Fatal("no-alias room should not match namespace")
	}
}

func TestEvaluate_Namespace_PrefixNotSubstring(t *testing.T) {
	// "my_bureau_extended" should NOT match namespace "my_bureau".
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}
	source := SourceRoom{
		RoomID: ref.MustParseRoomID("!ws:bureau.local"),
		Alias:  ref.MustParseRoomAlias("#my_bureau_extended/fleet/dev/workspace/x:" + testServer),
	}
	result := Evaluate(policy, source, "resource_request")
	if result.Authorized {
		t.Fatal("extended namespace should not match (prefix boundary)")
	}
}

func TestEvaluate_AllowedTypes_Permitted(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!workspace1:bureau.local"),
			},
		},
		AllowedTypes: []string{"resource_request", "custom_type"},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if !result.Authorized {
		t.Fatalf("resource_request should be in allowed types: %s", result.DenialReason)
	}
}

func TestEvaluate_AllowedTypes_Denied(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!workspace1:bureau.local"),
			},
		},
		AllowedTypes: []string{"custom_type"},
	}
	// resource_request is not in allowed types — denied before
	// sources are even checked.
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("type not in allowed list should deny")
	}
	if result.MatchedSource != nil {
		t.Fatal("type denial should not populate MatchedSource")
	}
}

func TestEvaluate_AllowedTypes_EmptyPermitsAll(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!workspace1:bureau.local"),
			},
		},
		AllowedTypes: []string{},
	}
	result := Evaluate(policy, workspaceSource(), "any_type_at_all")
	if !result.Authorized {
		t.Fatalf("empty AllowedTypes should permit all: %s", result.DenialReason)
	}
}

func TestEvaluate_OutboundFilter_PassedThrough(t *testing.T) {
	filter := &schema.RelayFilter{
		Include: []string{"status", "close_reason", "result"},
	}
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!workspace1:bureau.local"),
			},
		},
		OutboundFilter: filter,
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if !result.Authorized {
		t.Fatalf("should authorize: %s", result.DenialReason)
	}
	if result.OutboundFilter == nil {
		t.Fatal("OutboundFilter should be passed through from policy")
	}
	if len(result.OutboundFilter.Include) != 3 {
		t.Errorf("OutboundFilter.Include length = %d, want 3", len(result.OutboundFilter.Include))
	}
}

func TestEvaluate_OutboundFilter_NilWhenPolicyHasNone(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!workspace1:bureau.local"),
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if !result.Authorized {
		t.Fatalf("should authorize: %s", result.DenialReason)
	}
	if result.OutboundFilter != nil {
		t.Fatal("OutboundFilter should be nil when policy has no filter")
	}
}

func TestEvaluate_MultipleSources_FirstMatchWins(t *testing.T) {
	source := workspaceSource()
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				// This won't match — different room ID.
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!other:bureau.local"),
			},
			{
				// This matches — fleet member.
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
			{
				// This also matches — namespace. But fleet should
				// win because it appears first.
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}
	result := Evaluate(policy, source, "resource_request")
	if !result.Authorized {
		t.Fatalf("should match second source: %s", result.DenialReason)
	}
	if result.MatchedSource.Match != schema.RelayMatchFleetMember {
		t.Errorf("MatchedSource.Match = %q, want fleet_member (first match)", result.MatchedSource.Match)
	}
}

func TestEvaluate_MultipleSources_NoneMatch(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  ref.MustParseRoomID("!nope:bureau.local"),
			},
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "other_org/fleet/prod",
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("no source should match")
	}
}

func TestEvaluate_UnknownMatchStrategy_FailsClosed(t *testing.T) {
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelaySourceMatch("future_strategy"),
			},
		},
	}
	result := Evaluate(policy, workspaceSource(), "resource_request")
	if result.Authorized {
		t.Fatal("unknown match strategy should fail closed")
	}
}

func TestEvaluate_FleetMember_MatchesMultipleRoomTypes(t *testing.T) {
	// Fleet member should match workspaces, service rooms, etc. —
	// anything under the fleet's naming hierarchy.
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchFleetMember,
				Fleet: "my_bureau/fleet/prod",
			},
		},
	}

	rooms := []SourceRoom{
		{
			RoomID: ref.MustParseRoomID("!ws:bureau.local"),
			Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/prod/workspace/feature:" + testServer),
		},
		{
			RoomID: ref.MustParseRoomID("!svc:bureau.local"),
			Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/prod/service:" + testServer),
		},
		{
			RoomID: ref.MustParseRoomID("!m:bureau.local"),
			Alias:  ref.MustParseRoomAlias("#my_bureau/fleet/prod/machine:" + testServer),
		},
	}

	for _, room := range rooms {
		result := Evaluate(policy, room, "resource_request")
		if !result.Authorized {
			t.Errorf("fleet member should match %s: %s",
				room.Alias.String(), result.DenialReason)
		}
	}
}

func TestEvaluate_Namespace_MatchesCrossFleet(t *testing.T) {
	// Namespace match should authorize rooms from any fleet within
	// the namespace.
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match:     schema.RelayMatchNamespace,
				Namespace: "my_bureau",
			},
		},
	}

	// Both prod and staging should match.
	rooms := []SourceRoom{
		workspaceSource(),  // my_bureau/fleet/prod/...
		otherFleetSource(), // my_bureau/fleet/staging/...
	}

	for _, room := range rooms {
		result := Evaluate(policy, room, "resource_request")
		if !result.Authorized {
			t.Errorf("namespace should match %s: %s",
				room.Alias.String(), result.DenialReason)
		}
	}
}

func TestEvaluate_TypeCheckBeforeSourceCheck(t *testing.T) {
	// Verify that type filtering happens before source evaluation.
	// Even if the source would match, a disallowed type is denied.
	source := workspaceSource()
	policy := &schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{
				Match: schema.RelayMatchRoom,
				Room:  source.RoomID,
			},
		},
		AllowedTypes: []string{"custom_only"},
	}
	result := Evaluate(policy, source, "resource_request")
	if result.Authorized {
		t.Fatal("type denial should take precedence over source match")
	}
	// The denial reason should mention the type, not the source.
	if result.DenialReason == "no relay source matched the origin room" {
		t.Error("denial reason suggests sources were checked, but type should have denied first")
	}
}
