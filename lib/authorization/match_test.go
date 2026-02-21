// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestMatchAction(t *testing.T) {
	tests := []struct {
		pattern string
		action  string
		want    bool
	}{
		// Exact match.
		{"observe", "observe", true},
		{"observe", "interrupt", false},
		{"observe/read-write", "observe/read-write", true},
		{"observe/read-write", "observe", false},

		// Single-segment wildcard.
		{"ticket/*", "ticket/create", true},
		{"ticket/*", "ticket/assign", true},
		{"ticket/*", "ticket/close", true},
		{"ticket/*", "ticket", false},
		{"ticket/*", "ticket/a/b", false},

		// Recursive wildcard.
		{"ticket/**", "ticket/create", true},
		{"ticket/**", "ticket/a/b", true},
		{"observe/**", "observe/read-write", true},
		{"observe/**", "observe", true},
		{"command/**", "command/ticket/create", true},
		{"command/**", "command", true},

		// Universal.
		{"**", "anything", true},
		{"**", "a/b/c/d", true},

		// Interior wildcard.
		{"grant/approve/*", "grant/approve/observe", true},
		{"grant/approve/**", "grant/approve/credential/provision", true},

		// No match.
		{"observe", "observe/read-write", false},
		{"ticket/*", "fleet/assign", false},
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
			grant:    schema.Grant{Actions: []string{"matrix/join"}},
			action:   "matrix/join",
			targetID: "",
			want:     true,
		},
		{
			name:     "self-service action no match",
			grant:    schema.Grant{Actions: []string{"matrix/join"}},
			action:   "matrix/invite",
			targetID: "",
			want:     false,
		},
		{
			name:     "cross-principal action matches",
			grant:    schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   "observe/read-write",
			targetID: "@bureau/dev/workspace/coder/0:bureau.local",
			want:     true,
		},
		{
			name:     "cross-principal target no match",
			grant:    schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   "observe",
			targetID: "@service/db/postgres:bureau.local",
			want:     false,
		},
		{
			name:     "cross-principal grant without targets rejects",
			grant:    schema.Grant{Actions: []string{"observe/**"}},
			action:   "observe",
			targetID: "@bureau/dev/coder/0:bureau.local",
			want:     false,
		},
		{
			name:     "targeted grant still matches self-service",
			grant:    schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   "observe",
			targetID: "",
			want:     true,
		},
		{
			name:     "wildcard action and target",
			grant:    schema.Grant{Actions: []string{"**"}, Targets: []string{"**:**"}},
			action:   "fleet/provision",
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
			denial:   schema.Denial{Actions: []string{"fleet/**"}},
			action:   "fleet/assign",
			targetID: "",
			want:     true,
		},
		{
			name:     "targeted denial matches",
			denial:   schema.Denial{Actions: []string{"ticket/close"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   "ticket/close",
			targetID: "@bureau/dev/workspace/coder/0:bureau.local",
			want:     true,
		},
		{
			name:     "targeted denial wrong target",
			denial:   schema.Denial{Actions: []string{"ticket/close"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			action:   "ticket/close",
			targetID: "@iree/amdgpu/pm:bureau.local",
			want:     false,
		},
		{
			name:     "denial without targets does not match cross-principal",
			denial:   schema.Denial{Actions: []string{"ticket/close"}},
			action:   "ticket/close",
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
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"bureau/dev/pm:bureau.local"}},
			action:    "observe",
			actorID:   "@bureau/dev/pm:bureau.local",
			want:      true,
		},
		{
			name:      "glob actor match",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**:bureau.local"}},
			action:    "observe",
			actorID:   "@iree/amdgpu/pm:bureau.local",
			want:      true,
		},
		{
			name:      "wrong action",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**:bureau.local"}},
			action:    "interrupt",
			actorID:   "@iree/amdgpu/pm:bureau.local",
			want:      false,
		},
		{
			name:      "wrong actor",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**:bureau.local"}},
			action:    "observe",
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
