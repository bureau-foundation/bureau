// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func uid(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("ParseUserID(%q): %v", raw, err)
	}
	return userID
}

func TestIndex_SetAndRemovePrincipal(t *testing.T) {
	index := NewIndex()

	policy := schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionObserveAll}, Targets: []string{"bureau/dev/**:bureau.local"}},
		},
		Allowances: []schema.Allowance{
			{Actions: []string{schema.ActionObserve}, Actors: []string{"bureau-admin:bureau.local"}},
		},
	}

	index.SetPrincipal(uid(t, "@bureau/dev/pm:bureau.local"), policy)

	grants := index.Grants(uid(t, "@bureau/dev/pm:bureau.local"))
	if len(grants) != 1 {
		t.Fatalf("Grants length = %d, want 1", len(grants))
	}
	if grants[0].Actions[0] != schema.ActionObserveAll {
		t.Errorf("grant action = %q, want %s", grants[0].Actions[0], schema.ActionObserveAll)
	}

	allowances := index.Allowances(uid(t, "@bureau/dev/pm:bureau.local"))
	if len(allowances) != 1 {
		t.Fatalf("Allowances length = %d, want 1", len(allowances))
	}

	index.RemovePrincipal(uid(t, "@bureau/dev/pm:bureau.local"))

	if grants := index.Grants(uid(t, "@bureau/dev/pm:bureau.local")); grants != nil {
		t.Errorf("Grants after remove = %v, want nil", grants)
	}
	if allowances := index.Allowances(uid(t, "@bureau/dev/pm:bureau.local")); allowances != nil {
		t.Errorf("Allowances after remove = %v, want nil", allowances)
	}
}

func TestIndex_SetPrincipalPreservesTemporalGrants(t *testing.T) {
	index := NewIndex()

	// Set initial policy.
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionTicketCreate}},
		},
	})

	// Add a temporal grant.
	ok := index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionObserve},
		Targets:   []string{"service/db/**:bureau.local"},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "tkt-test",
	})
	if !ok {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Verify the agent now has 2 grants.
	grants := index.Grants(uid(t, "@agent:bureau.local"))
	if len(grants) != 2 {
		t.Fatalf("Grants after temporal add = %d, want 2", len(grants))
	}

	// Re-set the principal policy (simulating a config change).
	// The temporal grant should be preserved.
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionTicketCreate, "ticket/assign"}},
		},
	})

	grants = index.Grants(uid(t, "@agent:bureau.local"))
	if len(grants) != 2 {
		t.Fatalf("Grants after re-set = %d, want 2 (1 static + 1 temporal)", len(grants))
	}
}

func TestIndex_RemovePrincipalCleansTemporalGrants(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{})

	index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionObserve},
		Targets:   []string{"**:**"},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "tkt-1",
	})

	index.RemovePrincipal(uid(t, "@agent:bureau.local"))

	// Re-add the principal: should have no temporal grants.
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{schema.ActionTicketCreate}}},
	})

	grants := index.Grants(uid(t, "@agent:bureau.local"))
	if len(grants) != 1 {
		t.Errorf("Grants after remove+re-add = %d, want 1 (no temporal)", len(grants))
	}
}

func TestIndex_AddTemporalGrant_RequiresExpiryAndTicket(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{})

	// No ExpiresAt: should fail.
	ok := index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionObserve},
		Ticket:  "tkt-1",
	})
	if ok {
		t.Error("AddTemporalGrant without ExpiresAt should return false")
	}

	// No Ticket: should fail.
	ok = index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionObserve},
		ExpiresAt: "2099-01-01T00:00:00Z",
	})
	if ok {
		t.Error("AddTemporalGrant without Ticket should return false")
	}

	// Invalid ExpiresAt: should fail.
	ok = index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionObserve},
		ExpiresAt: "not-a-date",
		Ticket:    "tkt-1",
	})
	if ok {
		t.Error("AddTemporalGrant with invalid ExpiresAt should return false")
	}
}

func TestIndex_RevokeTemporalGrant(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{schema.ActionTicketCreate}}},
	})

	index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionObserve},
		Targets:   []string{"service/db/**:bureau.local"},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "tkt-revoke-me",
	})
	index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions:   []string{schema.ActionInterrupt},
		Targets:   []string{"service/db/**:bureau.local"},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "tkt-keep-me",
	})

	// Should have 3 grants now.
	if grants := index.Grants(uid(t, "@agent:bureau.local")); len(grants) != 3 {
		t.Fatalf("before revoke: %d grants, want 3", len(grants))
	}

	removed := index.RevokeTemporalGrant(uid(t, "@agent:bureau.local"), "tkt-revoke-me")
	if removed != 1 {
		t.Errorf("RevokeTemporalGrant removed = %d, want 1", removed)
	}

	// Should have 2 grants now (static + tkt-keep-me).
	grants := index.Grants(uid(t, "@agent:bureau.local"))
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
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{})

	removed := index.RevokeTemporalGrant(uid(t, "@agent:bureau.local"), "tkt-does-not-exist")
	if removed != 0 {
		t.Errorf("RevokeTemporalGrant for nonexistent ticket = %d, want 0", removed)
	}
}

func TestIndex_SweepExpired(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent-a:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{schema.ActionTicketCreate}}},
	})
	index.SetPrincipal(uid(t, "@agent-b:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{schema.ActionTicketCreate}}},
	})

	// Add temporal grants with different expiry times.
	t1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	index.AddTemporalGrant(uid(t, "@agent-a:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionObserve}, Targets: []string{"**:**"},
		ExpiresAt: t1.Format(time.RFC3339), Ticket: "tkt-1",
	})
	index.AddTemporalGrant(uid(t, "@agent-b:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionObserve}, Targets: []string{"**:**"},
		ExpiresAt: t2.Format(time.RFC3339), Ticket: "tkt-2",
	})
	index.AddTemporalGrant(uid(t, "@agent-a:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionInterrupt}, Targets: []string{"**:**"},
		ExpiresAt: t3.Format(time.RFC3339), Ticket: "tkt-3",
	})

	// Sweep at 10:30: only tkt-1 expired.
	sweep1 := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	affected := index.SweepExpired(sweep1)
	if len(affected) != 1 || affected[0] != uid(t, "@agent-a:bureau.local") {
		t.Errorf("sweep at 10:30: affected = %v, want [@agent-a:bureau.local]", affected)
	}

	// agent-a should have 2 grants (static + tkt-3).
	if grants := index.Grants(uid(t, "@agent-a:bureau.local")); len(grants) != 2 {
		t.Errorf("agent-a after sweep: %d grants, want 2", len(grants))
	}

	// Sweep at 11:30: tkt-2 expired.
	sweep2 := time.Date(2026, 3, 1, 11, 30, 0, 0, time.UTC)
	affected = index.SweepExpired(sweep2)
	if len(affected) != 1 || affected[0] != uid(t, "@agent-b:bureau.local") {
		t.Errorf("sweep at 11:30: affected = %v, want [@agent-b:bureau.local]", affected)
	}

	// agent-b should have 1 grant (static only).
	if grants := index.Grants(uid(t, "@agent-b:bureau.local")); len(grants) != 1 {
		t.Errorf("agent-b after sweep: %d grants, want 1", len(grants))
	}

	// Sweep at 13:00: tkt-3 expired.
	sweep3 := time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)
	affected = index.SweepExpired(sweep3)
	if len(affected) != 1 || affected[0] != uid(t, "@agent-a:bureau.local") {
		t.Errorf("sweep at 13:00: affected = %v, want [@agent-a:bureau.local]", affected)
	}

	// agent-a should have 1 grant (static only).
	if grants := index.Grants(uid(t, "@agent-a:bureau.local")); len(grants) != 1 {
		t.Errorf("agent-a after final sweep: %d grants, want 1", len(grants))
	}
}

func TestIndex_SweepExpired_NoTemporalGrants(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{})

	sweepTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	affected := index.SweepExpired(sweepTime)
	if affected != nil {
		t.Errorf("sweep with no temporal grants = %v, want nil", affected)
	}
}

func TestIndex_SweepExpired_NoneExpired(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{})

	index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionObserve}, Targets: []string{"**:**"},
		ExpiresAt: "2099-01-01T00:00:00Z", Ticket: "tkt-1",
	})

	sweepTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	affected := index.SweepExpired(sweepTime)
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
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionTicketCreate}},
			{Actions: []string{schema.ActionObserve}, ExpiresAt: expiry.Format(time.RFC3339)},
		},
	})

	index.AddTemporalGrant(uid(t, "@agent:bureau.local"), schema.Grant{
		Actions: []string{schema.ActionInterrupt}, Targets: []string{"**:**"},
		ExpiresAt: expiry.Format(time.RFC3339), Ticket: "tkt-sweep",
	})

	// Agent should have 3 grants: 2 static + 1 temporal.
	if grants := index.Grants(uid(t, "@agent:bureau.local")); len(grants) != 3 {
		t.Fatalf("before sweep: %d grants, want 3", len(grants))
	}

	// Sweep: should remove only the temporal grant (matched by Ticket).
	after := expiry.Add(time.Second)
	affected := index.SweepExpired(after)
	if len(affected) != 1 || affected[0] != uid(t, "@agent:bureau.local") {
		t.Errorf("affected = %v, want [@agent:bureau.local]", affected)
	}

	// Should have 2 grants remaining (both static).
	grants := index.Grants(uid(t, "@agent:bureau.local"))
	if len(grants) != 2 {
		t.Fatalf("after sweep: %d grants, want 2", len(grants))
	}

	// Verify the static grant with ExpiresAt was preserved.
	foundStaticWithExpiry := false
	for _, grant := range grants {
		if grant.ExpiresAt == expiry.Format(time.RFC3339) && len(grant.Actions) == 1 && grant.Actions[0] == schema.ActionObserve {
			foundStaticWithExpiry = true
		}
	}
	if !foundStaticWithExpiry {
		t.Error("static grant with ExpiresAt was incorrectly removed by sweep")
	}
}

func TestIndex_GrantsCopy(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{schema.ActionObserve}}},
	})

	// Modifying the returned slice should not affect the index.
	grants := index.Grants(uid(t, "@agent:bureau.local"))
	grants[0].Actions[0] = "mutated"

	// Fetch again: should be the original value.
	grants2 := index.Grants(uid(t, "@agent:bureau.local"))
	if grants2[0].Actions[0] != schema.ActionObserve {
		t.Errorf("Grants returned aliased slice: mutation visible")
	}
}

func TestIndex_AllowancesCopy(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{{Actions: []string{schema.ActionObserve}, Actors: []string{"admin:bureau.local"}}},
	})

	allowances := index.Allowances(uid(t, "@agent:bureau.local"))
	allowances[0].Actions[0] = "mutated"

	allowances2 := index.Allowances(uid(t, "@agent:bureau.local"))
	if allowances2[0].Actions[0] != schema.ActionObserve {
		t.Errorf("Allowances returned aliased slice: mutation visible")
	}
}

func TestIndex_NilForUnknownPrincipal(t *testing.T) {
	index := NewIndex()

	if grants := index.Grants(uid(t, "@nonexistent:bureau.local")); grants != nil {
		t.Errorf("Grants for unknown = %v, want nil", grants)
	}
	if allowances := index.Allowances(uid(t, "@nonexistent:bureau.local")); allowances != nil {
		t.Errorf("Allowances for unknown = %v, want nil", allowances)
	}
	if denials := index.Denials(uid(t, "@nonexistent:bureau.local")); denials != nil {
		t.Errorf("Denials for unknown = %v, want nil", denials)
	}
	if allowanceDenials := index.AllowanceDenials(uid(t, "@nonexistent:bureau.local")); allowanceDenials != nil {
		t.Errorf("AllowanceDenials for unknown = %v, want nil", allowanceDenials)
	}
}

func TestIndex_DenialsAccessor(t *testing.T) {
	index := NewIndex()

	policy := schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionObserveAll}, Targets: []string{"**:**"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{schema.ActionObserveReadWrite}, Targets: []string{"secret/**:bureau.local"}},
			{Actions: []string{schema.ActionInterruptAll}},
		},
	}

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), policy)

	denials := index.Denials(uid(t, "@agent/alpha:bureau.local"))
	if len(denials) != 2 {
		t.Fatalf("Denials length = %d, want 2", len(denials))
	}
	if denials[0].Actions[0] != schema.ActionObserveReadWrite {
		t.Errorf("denial[0] action = %q, want %s", denials[0].Actions[0], schema.ActionObserveReadWrite)
	}
	if denials[1].Actions[0] != schema.ActionInterruptAll {
		t.Errorf("denial[1] action = %q, want %s", denials[1].Actions[0], schema.ActionInterruptAll)
	}

	// Verify deep copy: mutating the returned slice should not affect the index.
	denials[0].Actions[0] = "mutated"
	original := index.Denials(uid(t, "@agent/alpha:bureau.local"))
	if original[0].Actions[0] != schema.ActionObserveReadWrite {
		t.Errorf("mutation leaked into index: got %q", original[0].Actions[0])
	}
}

func TestIndex_AllowanceDenialsAccessor(t *testing.T) {
	index := NewIndex()

	policy := schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{schema.ActionObserveAll}, Actors: []string{"**:**"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{schema.ActionObserveReadWrite}, Actors: []string{"untrusted/**:bureau.local"}},
		},
	}

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), policy)

	allowanceDenials := index.AllowanceDenials(uid(t, "@agent/alpha:bureau.local"))
	if len(allowanceDenials) != 1 {
		t.Fatalf("AllowanceDenials length = %d, want 1", len(allowanceDenials))
	}
	if allowanceDenials[0].Actions[0] != schema.ActionObserveReadWrite {
		t.Errorf("allowance denial action = %q, want %s", allowanceDenials[0].Actions[0], schema.ActionObserveReadWrite)
	}
	if allowanceDenials[0].Actors[0] != "untrusted/**:bureau.local" {
		t.Errorf("allowance denial actor = %q, want untrusted/**:bureau.local", allowanceDenials[0].Actors[0])
	}

	// Verify deep copy.
	allowanceDenials[0].Actors[0] = "mutated"
	original := index.AllowanceDenials(uid(t, "@agent/alpha:bureau.local"))
	if original[0].Actors[0] != "untrusted/**:bureau.local" {
		t.Errorf("mutation leaked into index: got %q", original[0].Actors[0])
	}
}

func TestIndex_DenialsRemovedWithPrincipal(t *testing.T) {
	index := NewIndex()

	policy := schema.AuthorizationPolicy{
		Denials: []schema.Denial{
			{Actions: []string{schema.ActionObserveAll}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{schema.ActionObserveAll}, Actors: []string{"**:**"}},
		},
	}

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), policy)

	if denials := index.Denials(uid(t, "@agent/alpha:bureau.local")); len(denials) != 1 {
		t.Fatalf("Denials before remove = %d, want 1", len(denials))
	}
	if allowanceDenials := index.AllowanceDenials(uid(t, "@agent/alpha:bureau.local")); len(allowanceDenials) != 1 {
		t.Fatalf("AllowanceDenials before remove = %d, want 1", len(allowanceDenials))
	}

	index.RemovePrincipal(uid(t, "@agent/alpha:bureau.local"))

	if denials := index.Denials(uid(t, "@agent/alpha:bureau.local")); denials != nil {
		t.Errorf("Denials after remove = %v, want nil", denials)
	}
	if allowanceDenials := index.AllowanceDenials(uid(t, "@agent/alpha:bureau.local")); allowanceDenials != nil {
		t.Errorf("AllowanceDenials after remove = %v, want nil", allowanceDenials)
	}
}
