// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

func makeToken(actions []string, targets []string) *servicetoken.Token {
	grants := []servicetoken.Grant{{Actions: actions}}
	if len(targets) > 0 {
		grants[0].Targets = targets
	}

	serverName, _ := ref.ParseServerName("bureau.local")
	subject, _ := ref.ParseUserID("@test/fleet/prod/agent/test:" + serverName.String())
	fleet, _ := ref.ParseFleet("test/fleet/prod", serverName)
	machine, _ := ref.NewMachine(fleet, "test-machine")

	return &servicetoken.Token{
		Subject:  subject,
		Machine:  machine,
		Audience: "model",
		Grants:   grants,
		Project:  "test-project",
	}
}

func makeTokenMultiGrant(grants []servicetoken.Grant) *servicetoken.Token {
	serverName, _ := ref.ParseServerName("bureau.local")
	subject, _ := ref.ParseUserID("@test/fleet/prod/agent/test:" + serverName.String())
	fleet, _ := ref.ParseFleet("test/fleet/prod", serverName)
	machine, _ := ref.NewMachine(fleet, "test-machine")

	return &servicetoken.Token{
		Subject:  subject,
		Machine:  machine,
		Audience: "model",
		Grants:   grants,
		Project:  "test-project",
	}
}

// --- requireActionGrant ---

func TestRequireActionGrant_Allowed(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		action  string
	}{
		{"exact match", []string{"model/complete"}, "model/complete"},
		{"wildcard", []string{"model/**"}, "model/complete"},
		{"wildcard all", []string{"model/**"}, "model/embed"},
		{"multiple actions", []string{"model/complete", "model/embed"}, "model/embed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := makeToken(test.actions, nil)
			if err := requireActionGrant(token, test.action); err != nil {
				t.Errorf("requireActionGrant() = %v, want nil", err)
			}
		})
	}
}

func TestRequireActionGrant_Denied(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		action  string
	}{
		{"no matching action", []string{"model/embed"}, "model/complete"},
		{"different service", []string{"ticket/**"}, "model/complete"},
		{"empty grants", []string{}, "model/complete"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := makeToken(test.actions, nil)
			if err := requireActionGrant(token, test.action); err == nil {
				t.Error("requireActionGrant() = nil, want error")
			}
		})
	}
}

// --- requireModelGrant ---

func TestRequireModelGrant_NoTargets_Unrestricted(t *testing.T) {
	// Grants without target patterns mean unrestricted model access.
	// This is the backward-compatible behavior for existing tokens.
	token := makeToken([]string{"model/**"}, nil)

	if err := requireModelGrant(token, "model/complete", "codex"); err != nil {
		t.Errorf("requireModelGrant(codex) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "qwen3/32b"); err != nil {
		t.Errorf("requireModelGrant(qwen3/32b) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "anything"); err != nil {
		t.Errorf("requireModelGrant(anything) = %v, want nil", err)
	}
}

func TestRequireModelGrant_WildcardTarget(t *testing.T) {
	// Explicit wildcard target — matches everything.
	token := makeToken([]string{"model/**"}, []string{"*"})

	if err := requireModelGrant(token, "model/complete", "codex"); err != nil {
		t.Errorf("requireModelGrant(codex) = %v, want nil", err)
	}
}

func TestRequireModelGrant_ExactTarget(t *testing.T) {
	token := makeToken([]string{"model/complete"}, []string{"codex"})

	if err := requireModelGrant(token, "model/complete", "codex"); err != nil {
		t.Errorf("requireModelGrant(codex) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "sonnet"); err == nil {
		t.Error("requireModelGrant(sonnet) = nil, want access denied")
	}
}

func TestRequireModelGrant_GlobTarget(t *testing.T) {
	token := makeToken([]string{"model/complete"}, []string{"qwen3/*"})

	if err := requireModelGrant(token, "model/complete", "qwen3/32b"); err != nil {
		t.Errorf("requireModelGrant(qwen3/32b) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "qwen3/8b"); err != nil {
		t.Errorf("requireModelGrant(qwen3/8b) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "sonnet"); err == nil {
		t.Error("requireModelGrant(sonnet) = nil, want access denied")
	}
}

func TestRequireModelGrant_RecursiveGlobTarget(t *testing.T) {
	token := makeToken([]string{"model/**"}, []string{"tier/cheap/**"})

	if err := requireModelGrant(token, "model/complete", "tier/cheap/qwen3-8b"); err != nil {
		t.Errorf("requireModelGrant(tier/cheap/qwen3-8b) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "tier/premium/sonnet"); err == nil {
		t.Error("requireModelGrant(tier/premium/sonnet) = nil, want access denied")
	}
}

func TestRequireModelGrant_MultipleTargets(t *testing.T) {
	token := makeToken([]string{"model/complete"}, []string{"codex", "haiku", "qwen3/*"})

	allowed := []string{"codex", "haiku", "qwen3/8b", "qwen3/32b"}
	for _, alias := range allowed {
		if err := requireModelGrant(token, "model/complete", alias); err != nil {
			t.Errorf("requireModelGrant(%s) = %v, want nil", alias, err)
		}
	}

	denied := []string{"sonnet", "opus", "llama3/70b"}
	for _, alias := range denied {
		if err := requireModelGrant(token, "model/complete", alias); err == nil {
			t.Errorf("requireModelGrant(%s) = nil, want access denied", alias)
		}
	}
}

func TestRequireModelGrant_ActionDenied(t *testing.T) {
	// Even with a matching target, the action must be authorized.
	token := makeToken([]string{"model/embed"}, []string{"codex"})

	if err := requireModelGrant(token, "model/complete", "codex"); err == nil {
		t.Error("requireModelGrant() = nil, want action denied")
	}
}

func TestRequireModelGrant_MultipleGrants(t *testing.T) {
	// Multiple grants: one unrestricted for embed, one restricted
	// for complete. Complete should be restricted; embed should not.
	token := makeTokenMultiGrant([]servicetoken.Grant{
		{Actions: []string{"model/embed"}},
		{Actions: []string{"model/complete"}, Targets: []string{"codex"}},
	})

	// Complete is target-restricted.
	if err := requireModelGrant(token, "model/complete", "codex"); err != nil {
		t.Errorf("complete(codex) = %v, want nil", err)
	}
	if err := requireModelGrant(token, "model/complete", "sonnet"); err == nil {
		t.Error("complete(sonnet) = nil, want denied")
	}

	// Embed is unrestricted (no targets on the embed grant).
	if err := requireModelGrant(token, "model/embed", "anything"); err != nil {
		t.Errorf("embed(anything) = %v, want nil", err)
	}
}

func TestRequireModelGrant_MixedTargetedAndUntargeted(t *testing.T) {
	// Two grants for the same action: one without targets, one with.
	// The untargeted grant provides unrestricted access, so the
	// targeted grant's restrictions don't matter.
	token := makeTokenMultiGrant([]servicetoken.Grant{
		{Actions: []string{"model/complete"}},
		{Actions: []string{"model/complete"}, Targets: []string{"codex"}},
	})

	// The untargeted grant means unrestricted access.
	if err := requireModelGrant(token, "model/complete", "anything"); err != nil {
		t.Errorf("complete(anything) = %v, want nil (untargeted grant)", err)
	}
}
