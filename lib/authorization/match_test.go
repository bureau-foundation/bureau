// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func TestMatchAction(t *testing.T) {
	tests := []struct {
		pattern string
		action  string
		want    bool
	}{
		// Exact match.
		{observation.ActionObserve, observation.ActionObserve, true},
		{observation.ActionObserve, schema.ActionInterrupt, false},
		{observation.ActionReadWrite, observation.ActionReadWrite, true},
		{observation.ActionReadWrite, observation.ActionObserve, false},

		// Single-segment wildcard.
		{"ticket/*", ticket.ActionCreate, true},
		{"ticket/*", "ticket/assign", true},
		{"ticket/*", ticket.ActionClose, true},
		{"ticket/*", "ticket", false},
		{"ticket/*", "ticket/a/b", false},

		// Recursive wildcard.
		{ticket.ActionAll, ticket.ActionCreate, true},
		{ticket.ActionAll, "ticket/a/b", true},
		{observation.ActionAll, observation.ActionReadWrite, true},
		{observation.ActionAll, observation.ActionObserve, true},
		{schema.ActionCommandAll, "command/ticket/create", true},
		{schema.ActionCommandAll, "command", true},

		// Universal.
		{"**", "anything", true},
		{"**", "a/b/c/d", true},

		// Interior wildcard.
		{"grant/approve/*", "grant/approve/observe", true},
		{"grant/approve/**", "grant/approve/credential/provision", true},

		// No match.
		{observation.ActionObserve, observation.ActionReadWrite, false},
		{"ticket/*", fleet.ActionAssign, false},
	}

	for _, tt := range tests {
		got := MatchAction(tt.pattern, tt.action)
		if got != tt.want {
			t.Errorf("MatchAction(%q, %q) = %v, want %v", tt.pattern, tt.action, got, tt.want)
		}
	}
}

func TestGrantMatches(t *testing.T) {
	tests := []struct {
		name     string
		grant    schema.Grant
		action   string
		targetID string
		want     bool
	}{
		{
			name:     "self-service action matches",
			grant:    schema.Grant{Actions: []string{schema.ActionMatrixJoin}},
			action:   schema.ActionMatrixJoin,
			targetID: "",
			want:     true,
		},
		{
			name:     "self-service action no match",
			grant:    schema.Grant{Actions: []string{schema.ActionMatrixJoin}},
			action:   schema.ActionMatrixInvite,
			targetID: "",
			want:     false,
		},
		{
			name:     "cross-principal action matches",
			grant:    schema.Grant{Actions: []string{observation.ActionAll}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   observation.ActionReadWrite,
			targetID: "@bureau/dev/workspace/coder/0:bureau.local",
			want:     true,
		},
		{
			name:     "cross-principal target no match",
			grant:    schema.Grant{Actions: []string{observation.ActionAll}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   observation.ActionObserve,
			targetID: "@service/db/postgres:bureau.local",
			want:     false,
		},
		{
			name:     "cross-principal grant without targets rejects",
			grant:    schema.Grant{Actions: []string{observation.ActionAll}},
			action:   observation.ActionObserve,
			targetID: "@bureau/dev/coder/0:bureau.local",
			want:     false,
		},
		{
			name:     "targeted grant still matches self-service",
			grant:    schema.Grant{Actions: []string{observation.ActionAll}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   observation.ActionObserve,
			targetID: "",
			want:     true,
		},
		{
			name:     "wildcard action and target",
			grant:    schema.Grant{Actions: []string{"**"}, Targets: []string{"**:**"}},
			action:   fleet.ActionProvision,
			targetID: "@machine/gpu-01:bureau.local",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grantMatchesUserID(tt.grant, tt.action, tt.targetID, tt.targetID == "")
			if got != tt.want {
				t.Errorf("grantMatchesUserID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDenialMatches(t *testing.T) {
	tests := []struct {
		name     string
		denial   schema.Denial
		action   string
		targetID string
		want     bool
	}{
		{
			name:     "self-service denial matches",
			denial:   schema.Denial{Actions: []string{fleet.ActionAll}},
			action:   fleet.ActionAssign,
			targetID: "",
			want:     true,
		},
		{
			name:     "targeted denial matches",
			denial:   schema.Denial{Actions: []string{ticket.ActionClose}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   ticket.ActionClose,
			targetID: "@bureau/dev/workspace/coder/0:bureau.local",
			want:     true,
		},
		{
			name:     "targeted denial wrong target",
			denial:   schema.Denial{Actions: []string{ticket.ActionClose}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   ticket.ActionClose,
			targetID: "@iree/amdgpu/pm:bureau.local",
			want:     false,
		},
		{
			name:     "denial without targets does not match cross-principal",
			denial:   schema.Denial{Actions: []string{ticket.ActionClose}},
			action:   ticket.ActionClose,
			targetID: "@bureau/dev/workspace/coder/0:bureau.local",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := denialMatchesUserID(tt.denial, tt.action, tt.targetID, tt.targetID == "")
			if got != tt.want {
				t.Errorf("denialMatchesUserID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowanceMatches(t *testing.T) {
	tests := []struct {
		name      string
		allowance schema.Allowance
		action    string
		actorID   string
		want      bool
	}{
		{
			name:      "exact match",
			allowance: schema.Allowance{Actions: []string{observation.ActionObserve}, Actors: []string{"bureau/dev/pm:bureau.local"}},
			action:    observation.ActionObserve,
			actorID:   "@bureau/dev/pm:bureau.local",
			want:      true,
		},
		{
			name:      "glob actor match",
			allowance: schema.Allowance{Actions: []string{observation.ActionObserve}, Actors: []string{"iree/**:bureau.local"}},
			action:    observation.ActionObserve,
			actorID:   "@iree/amdgpu/pm:bureau.local",
			want:      true,
		},
		{
			name:      "wrong action",
			allowance: schema.Allowance{Actions: []string{observation.ActionObserve}, Actors: []string{"iree/**:bureau.local"}},
			action:    schema.ActionInterrupt,
			actorID:   "@iree/amdgpu/pm:bureau.local",
			want:      false,
		},
		{
			name:      "wrong actor",
			allowance: schema.Allowance{Actions: []string{observation.ActionObserve}, Actors: []string{"iree/**:bureau.local"}},
			action:    observation.ActionObserve,
			actorID:   "@bureau/dev/coder/0:bureau.local",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allowanceMatchesUserID(tt.allowance, tt.action, tt.actorID)
			if got != tt.want {
				t.Errorf("allowanceMatchesUserID() = %v, want %v", got, tt.want)
			}
		})
	}
}
