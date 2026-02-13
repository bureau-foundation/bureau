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
		{"observe/readwrite", "observe/readwrite", true},
		{"observe/readwrite", "observe", false},

		// Single-segment wildcard.
		{"ticket/*", "ticket/create", true},
		{"ticket/*", "ticket/assign", true},
		{"ticket/*", "ticket/close", true},
		{"ticket/*", "ticket", false},
		{"ticket/*", "ticket/a/b", false},

		// Recursive wildcard.
		{"ticket/**", "ticket/create", true},
		{"ticket/**", "ticket/a/b", true},
		{"observe/**", "observe/readwrite", true},
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
		{"observe", "observe/readwrite", false},
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
		name   string
		grant  schema.Grant
		action string
		target string
		want   bool
	}{
		{
			name:   "self-service action matches",
			grant:  schema.Grant{Actions: []string{"matrix/join"}},
			action: "matrix/join",
			target: "",
			want:   true,
		},
		{
			name:   "self-service action no match",
			grant:  schema.Grant{Actions: []string{"matrix/join"}},
			action: "matrix/invite",
			target: "",
			want:   false,
		},
		{
			name:   "cross-principal action matches",
			grant:  schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**"}},
			action: "observe/readwrite",
			target: "bureau/dev/workspace/coder/0",
			want:   true,
		},
		{
			name:   "cross-principal target no match",
			grant:  schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**"}},
			action: "observe",
			target: "service/db/postgres",
			want:   false,
		},
		{
			name:   "cross-principal grant without targets rejects",
			grant:  schema.Grant{Actions: []string{"observe/**"}},
			action: "observe",
			target: "bureau/dev/coder/0",
			want:   false,
		},
		{
			name:   "targeted grant still matches self-service",
			grant:  schema.Grant{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**"}},
			action: "observe",
			target: "",
			want:   true,
		},
		{
			name:   "wildcard action and target",
			grant:  schema.Grant{Actions: []string{"**"}, Targets: []string{"**"}},
			action: "fleet/provision",
			target: "machine/gpu-01",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grantMatches(tt.grant, tt.action, tt.target)
			if got != tt.want {
				t.Errorf("grantMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDenialMatches(t *testing.T) {
	tests := []struct {
		name   string
		denial schema.Denial
		action string
		target string
		want   bool
	}{
		{
			name:   "self-service denial matches",
			denial: schema.Denial{Actions: []string{"fleet/**"}},
			action: "fleet/assign",
			target: "",
			want:   true,
		},
		{
			name:   "targeted denial matches",
			denial: schema.Denial{Actions: []string{"ticket/close"}, Targets: []string{"bureau/dev/**"}},
			action: "ticket/close",
			target: "bureau/dev/workspace/coder/0",
			want:   true,
		},
		{
			name:   "targeted denial wrong target",
			denial: schema.Denial{Actions: []string{"ticket/close"}, Targets: []string{"bureau/dev/**"}},
			action: "ticket/close",
			target: "iree/amdgpu/pm",
			want:   false,
		},
		{
			name:   "denial without targets does not match cross-principal",
			denial: schema.Denial{Actions: []string{"ticket/close"}},
			action: "ticket/close",
			target: "bureau/dev/workspace/coder/0",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := denialMatches(tt.denial, tt.action, tt.target)
			if got != tt.want {
				t.Errorf("denialMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowanceMatches(t *testing.T) {
	tests := []struct {
		name      string
		allowance schema.Allowance
		action    string
		actor     string
		want      bool
	}{
		{
			name:      "exact match",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"bureau/dev/pm"}},
			action:    "observe",
			actor:     "bureau/dev/pm",
			want:      true,
		},
		{
			name:      "glob actor match",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**"}},
			action:    "observe",
			actor:     "iree/amdgpu/pm",
			want:      true,
		},
		{
			name:      "wrong action",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**"}},
			action:    "interrupt",
			actor:     "iree/amdgpu/pm",
			want:      false,
		},
		{
			name:      "wrong actor",
			allowance: schema.Allowance{Actions: []string{"observe"}, Actors: []string{"iree/**"}},
			action:    "observe",
			actor:     "bureau/dev/coder/0",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allowanceMatches(tt.allowance, tt.action, tt.actor)
			if got != tt.want {
				t.Errorf("allowanceMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}
