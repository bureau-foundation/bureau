// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestIndex_SetAndRemovePrincipal(t *testing.T) {
	index := NewIndex()

	policy := schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"bureau/dev/**"}},
		},
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"bureau-admin"}},
		},
	}

	index.SetPrincipal("bureau/dev/pm", policy)

	grants := index.Grants("bureau/dev/pm")
	if len(grants) != 1 {
		t.Fatalf("Grants length = %d, want 1", len(grants))
	}
	if grants[0].Actions[0] != "observe/**" {
		t.Errorf("grant action = %q, want observe/**", grants[0].Actions[0])
	}

	allowances := index.Allowances("bureau/dev/pm")
	if len(allowances) != 1 {
		t.Fatalf("Allowances length = %d, want 1", len(allowances))
	}

	index.RemovePrincipal("bureau/dev/pm")

	if grants := index.Grants("bureau/dev/pm"); grants != nil {
		t.Errorf("Grants after remove = %v, want nil", grants)
	}
	if allowances := index.Allowances("bureau/dev/pm"); allowances != nil {
		t.Errorf("Allowances after remove = %v, want nil", allowances)
	}
}

func TestIndex_SetPrincipalPreservesTemporalGrants(t *testing.T) {
	index := NewIndex()

	// Set initial policy.
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create"}},
		},
	})

	// Add a temporal grant.
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	ok := index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"observe"},
		Targets:   []string{"service/db/**"},
		ExpiresAt: future,
		Ticket:    "tkt-test",
	})
	if !ok {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Verify the agent now has 2 grants.
	grants := index.Grants("agent")
	if len(grants) != 2 {
		t.Fatalf("Grants after temporal add = %d, want 2", len(grants))
	}

	// Re-set the principal policy (simulating a config change).
	// The temporal grant should be preserved.
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
		},
	})

	grants = index.Grants("agent")
	if len(grants) != 2 {
		t.Fatalf("Grants after re-set = %d, want 2 (1 static + 1 temporal)", len(grants))
	}
}

func TestIndex_RemovePrincipalCleansTemporalGrants(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal("agent", schema.AuthorizationPolicy{})

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"observe"},
		Targets:   []string{"**"},
		ExpiresAt: future,
		Ticket:    "tkt-1",
	})

	index.RemovePrincipal("agent")

	// Re-add the principal: should have no temporal grants.
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"ticket/create"}}},
	})

	grants := index.Grants("agent")
	if len(grants) != 1 {
		t.Errorf("Grants after remove+re-add = %d, want 1 (no temporal)", len(grants))
	}
}

func TestIndex_AddTemporalGrant_RequiresExpiryAndTicket(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{})

	// No ExpiresAt: should fail.
	ok := index.AddTemporalGrant("agent", schema.Grant{
		Actions: []string{"observe"},
		Ticket:  "tkt-1",
	})
	if ok {
		t.Error("AddTemporalGrant without ExpiresAt should return false")
	}

	// No Ticket: should fail.
	ok = index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"observe"},
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if ok {
		t.Error("AddTemporalGrant without Ticket should return false")
	}

	// Invalid ExpiresAt: should fail.
	ok = index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"observe"},
		ExpiresAt: "not-a-date",
		Ticket:    "tkt-1",
	})
	if ok {
		t.Error("AddTemporalGrant with invalid ExpiresAt should return false")
	}
}

func TestIndex_RevokeTemporalGrant(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"ticket/create"}}},
	})

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"observe"},
		Targets:   []string{"service/db/**"},
		ExpiresAt: future,
		Ticket:    "tkt-revoke-me",
	})
	index.AddTemporalGrant("agent", schema.Grant{
		Actions:   []string{"interrupt"},
		Targets:   []string{"service/db/**"},
		ExpiresAt: future,
		Ticket:    "tkt-keep-me",
	})

	// Should have 3 grants now.
	if grants := index.Grants("agent"); len(grants) != 3 {
		t.Fatalf("before revoke: %d grants, want 3", len(grants))
	}

	removed := index.RevokeTemporalGrant("agent", "tkt-revoke-me")
	if removed != 1 {
		t.Errorf("RevokeTemporalGrant removed = %d, want 1", removed)
	}

	// Should have 2 grants now (static + tkt-keep-me).
	grants := index.Grants("agent")
	if len(grants) != 2 {
		t.Fatalf("after revoke: %d grants, want 2", len(grants))
	}

	// Verify the right one was removed.
	for _, grant := range grants {
		if grant.Ticket == "tkt-revoke-me" {
			t.Error("revoked grant still present")
		}
	}
}

func TestIndex_RevokeTemporalGrant_NonexistentTicket(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{})

	removed := index.RevokeTemporalGrant("agent", "tkt-does-not-exist")
	if removed != 0 {
		t.Errorf("RevokeTemporalGrant for nonexistent ticket = %d, want 0", removed)
	}
}

func TestIndex_SweepExpired(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal("agent-a", schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"ticket/create"}}},
	})
	index.SetPrincipal("agent-b", schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"ticket/create"}}},
	})

	// Add temporal grants with different expiry times.
	t1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	index.AddTemporalGrant("agent-a", schema.Grant{
		Actions: []string{"observe"}, Targets: []string{"**"},
		ExpiresAt: t1.Format(time.RFC3339), Ticket: "tkt-1",
	})
	index.AddTemporalGrant("agent-b", schema.Grant{
		Actions: []string{"observe"}, Targets: []string{"**"},
		ExpiresAt: t2.Format(time.RFC3339), Ticket: "tkt-2",
	})
	index.AddTemporalGrant("agent-a", schema.Grant{
		Actions: []string{"interrupt"}, Targets: []string{"**"},
		ExpiresAt: t3.Format(time.RFC3339), Ticket: "tkt-3",
	})

	// Sweep at 10:30: only tkt-1 expired.
	sweep1 := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	affected := index.SweepExpired(sweep1)
	if len(affected) != 1 || affected[0] != "agent-a" {
		t.Errorf("sweep at 10:30: affected = %v, want [agent-a]", affected)
	}

	// agent-a should have 2 grants (static + tkt-3).
	if grants := index.Grants("agent-a"); len(grants) != 2 {
		t.Errorf("agent-a after sweep: %d grants, want 2", len(grants))
	}

	// Sweep at 11:30: tkt-2 expired.
	sweep2 := time.Date(2026, 3, 1, 11, 30, 0, 0, time.UTC)
	affected = index.SweepExpired(sweep2)
	if len(affected) != 1 || affected[0] != "agent-b" {
		t.Errorf("sweep at 11:30: affected = %v, want [agent-b]", affected)
	}

	// agent-b should have 1 grant (static only).
	if grants := index.Grants("agent-b"); len(grants) != 1 {
		t.Errorf("agent-b after sweep: %d grants, want 1", len(grants))
	}

	// Sweep at 13:00: tkt-3 expired.
	sweep3 := time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)
	affected = index.SweepExpired(sweep3)
	if len(affected) != 1 || affected[0] != "agent-a" {
		t.Errorf("sweep at 13:00: affected = %v, want [agent-a]", affected)
	}

	// agent-a should have 1 grant (static only).
	if grants := index.Grants("agent-a"); len(grants) != 1 {
		t.Errorf("agent-a after final sweep: %d grants, want 1", len(grants))
	}
}

func TestIndex_SweepExpired_NoTemporalGrants(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{})

	affected := index.SweepExpired(time.Now())
	if affected != nil {
		t.Errorf("sweep with no temporal grants = %v, want nil", affected)
	}
}

func TestIndex_SweepExpired_NoneExpired(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{})

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	index.AddTemporalGrant("agent", schema.Grant{
		Actions: []string{"observe"}, Targets: []string{"**"},
		ExpiresAt: future, Ticket: "tkt-1",
	})

	affected := index.SweepExpired(time.Now())
	if affected != nil {
		t.Errorf("sweep with no expired grants = %v, want nil", affected)
	}
}

func TestIndex_SweepExpired_PreservesStaticGrants(t *testing.T) {
	index := NewIndex()

	// Set up a principal with a static grant that has an ExpiresAt
	// coincidentally matching the temporal grant's expiry. The sweep
	// should remove only the temporal grant, not the static one.
	expiry := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create"}},
			{Actions: []string{"observe"}, ExpiresAt: expiry.Format(time.RFC3339)},
		},
	})

	index.AddTemporalGrant("agent", schema.Grant{
		Actions: []string{"interrupt"}, Targets: []string{"**"},
		ExpiresAt: expiry.Format(time.RFC3339), Ticket: "tkt-sweep",
	})

	// Agent should have 3 grants: 2 static + 1 temporal.
	if grants := index.Grants("agent"); len(grants) != 3 {
		t.Fatalf("before sweep: %d grants, want 3", len(grants))
	}

	// Sweep: should remove only the temporal grant (matched by Ticket).
	after := expiry.Add(time.Second)
	affected := index.SweepExpired(after)
	if len(affected) != 1 || affected[0] != "agent" {
		t.Errorf("affected = %v, want [agent]", affected)
	}

	// Should have 2 grants remaining (both static).
	grants := index.Grants("agent")
	if len(grants) != 2 {
		t.Fatalf("after sweep: %d grants, want 2", len(grants))
	}

	// Verify the static grant with ExpiresAt was preserved.
	foundStaticWithExpiry := false
	for _, grant := range grants {
		if grant.ExpiresAt == expiry.Format(time.RFC3339) && len(grant.Actions) == 1 && grant.Actions[0] == "observe" {
			foundStaticWithExpiry = true
		}
	}
	if !foundStaticWithExpiry {
		t.Error("static grant with ExpiresAt was incorrectly removed by sweep")
	}
}

func TestIndex_GrantsCopy(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"observe"}}},
	})

	// Modifying the returned slice should not affect the index.
	grants := index.Grants("agent")
	grants[0].Actions[0] = "mutated"

	// Fetch again: should be the original value.
	grants2 := index.Grants("agent")
	if grants2[0].Actions[0] != "observe" {
		t.Errorf("Grants returned aliased slice: mutation visible")
	}
}

func TestIndex_AllowancesCopy(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal("agent", schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{{Actions: []string{"observe"}, Actors: []string{"admin"}}},
	})

	allowances := index.Allowances("agent")
	allowances[0].Actions[0] = "mutated"

	allowances2 := index.Allowances("agent")
	if allowances2[0].Actions[0] != "observe" {
		t.Errorf("Allowances returned aliased slice: mutation visible")
	}
}

func TestIndex_NilForUnknownPrincipal(t *testing.T) {
	index := NewIndex()

	if grants := index.Grants("nonexistent"); grants != nil {
		t.Errorf("Grants for unknown = %v, want nil", grants)
	}
	if allowances := index.Allowances("nonexistent"); allowances != nil {
		t.Errorf("Allowances for unknown = %v, want nil", allowances)
	}
}
