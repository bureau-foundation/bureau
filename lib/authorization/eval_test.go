// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// setupIndex creates a test index with a standard scenario:
//
//   - PM can observe and interrupt all dev agents
//   - PM can join rooms and discover services (self-service)
//   - Coder can create and assign tickets (self-service)
//   - Coder has a denial for fleet operations
//   - Dev agents allow observation from PM and admin
//   - Dev agents deny observation from secret project agents
func setupIndex(t *testing.T) *Index {
	t.Helper()
	index := NewIndex()

	// PM grants and denials.
	index.SetPrincipal(uid(t, "@bureau/dev/pm:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe/**", "interrupt"}, Targets: []string{"bureau/dev/**:bureau.local"}},
			{Actions: []string{"matrix/join", "matrix/invite"}},
			{Actions: []string{"service/discover"}, Targets: []string{"service/**:bureau.local"}},
		},
	})

	// Coder grants and denials.
	index.SetPrincipal(uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
			{Actions: []string{"command/ticket/*", "command/artifact/*"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{"fleet/**"}},
		},
		// Coder allows observation from PM and admin.
		Allowances: []schema.Allowance{
			{Actions: []string{"observe", "observe/read-write"}, Actors: []string{"bureau/dev/pm:bureau.local", "bureau-admin:bureau.local"}},
			{Actions: []string{"observe"}, Actors: []string{"bureau/dev/**:bureau.local"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe/**"}, Actors: []string{"bureau/dev/secret/**:bureau.local"}},
		},
	})

	// Operator grants (full access).
	index.SetPrincipal(uid(t, "@bureau-admin:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"**"}, Targets: []string{"**:**"}},
		},
	})

	return index
}

func TestAuthorized_SelfServiceGrant(t *testing.T) {
	index := setupIndex(t)

	// PM can join rooms (self-service, no target).
	result := Authorized(index, uid(t, "@bureau/dev/pm:bureau.local"), "matrix/join", ref.UserID{})
	if result.Decision != Allow {
		t.Errorf("PM matrix/join: got %v (%v), want allow", result.Decision, result.Reason)
	}

	// PM can discover services (self-service).
	result = Authorized(index, uid(t, "@bureau/dev/pm:bureau.local"), "matrix/invite", ref.UserID{})
	if result.Decision != Allow {
		t.Errorf("PM matrix/invite: got %v (%v), want allow", result.Decision, result.Reason)
	}
}

func TestAuthorized_SelfServiceNoGrant(t *testing.T) {
	index := setupIndex(t)

	// Coder cannot join rooms (no grant).
	result := Authorized(index, uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), "matrix/join", ref.UserID{})
	if result.Decision != Deny {
		t.Errorf("coder matrix/join: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonNoGrant {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoGrant)
	}
}

func TestAuthorized_CrossPrincipal_FullChain(t *testing.T) {
	index := setupIndex(t)

	// PM observes coder: PM has grant + coder has allowance → allow.
	result := Authorized(index, uid(t, "@bureau/dev/pm:bureau.local"), "observe", uid(t, "@bureau/dev/workspace/coder/0:bureau.local"))
	if result.Decision != Allow {
		t.Errorf("PM observe coder: got %v (%v), want allow", result.Decision, result.Reason)
	}
	if result.MatchedGrant == nil {
		t.Error("MatchedGrant is nil")
	}
	if result.MatchedAllowance == nil {
		t.Error("MatchedAllowance is nil")
	}
}

func TestAuthorized_CrossPrincipal_ReadWriteUpgrade(t *testing.T) {
	index := setupIndex(t)

	// PM observes coder with readwrite: PM has observe/** grant, coder
	// allows observe/read-write from PM → allow.
	result := Authorized(index, uid(t, "@bureau/dev/pm:bureau.local"), "observe/read-write", uid(t, "@bureau/dev/workspace/coder/0:bureau.local"))
	if result.Decision != Allow {
		t.Errorf("PM observe/read-write coder: got %v (%v), want allow", result.Decision, result.Reason)
	}
}

func TestAuthorized_CrossPrincipal_NoAllowance(t *testing.T) {
	index := setupIndex(t)

	// Set up a dev principal with no allowances. The PM has a grant
	// targeting bureau/dev/**, so the grant matches — but the target
	// has no allowance for the PM.
	index.SetPrincipal(uid(t, "@bureau/dev/workspace/worker/0:bureau.local"), schema.AuthorizationPolicy{})

	result := Authorized(index, uid(t, "@bureau/dev/pm:bureau.local"), "observe", uid(t, "@bureau/dev/workspace/worker/0:bureau.local"))
	if result.Decision != Deny {
		t.Errorf("PM observe worker with no allowances: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonNoAllowance {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoAllowance)
	}
}

func TestAuthorized_SubjectDenial(t *testing.T) {
	index := setupIndex(t)

	// Coder tries fleet operation: has no grant anyway, but also has a
	// denial. Should deny at the grant stage.
	result := Authorized(index, uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), "fleet/assign", ref.UserID{})
	if result.Decision != Deny {
		t.Errorf("coder fleet/assign: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonNoGrant {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoGrant)
	}
}

func TestAuthorized_SubjectDenialOverridesGrant(t *testing.T) {
	index := setupIndex(t)

	// Give coder a fleet grant, but the denial should override it.
	index.SetPrincipal(uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"fleet/**"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{"fleet/**"}},
		},
	})

	result := Authorized(index, uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), "fleet/assign", ref.UserID{})
	if result.Decision != Deny {
		t.Errorf("coder fleet/assign (denied): got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonDenied {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonDenied)
	}
	if result.MatchedDenial == nil {
		t.Error("MatchedDenial is nil")
	}
}

func TestAuthorized_AllowanceDenial(t *testing.T) {
	index := setupIndex(t)

	// A secret project agent has a grant for observe, but the coder's
	// allowance denial blocks observation from secret agents.
	index.SetPrincipal(uid(t, "@bureau/dev/secret/agent/0:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe"}, Targets: []string{"bureau/dev/**:bureau.local"}},
		},
	})

	result := Authorized(index, uid(t, "@bureau/dev/secret/agent/0:bureau.local"), "observe", uid(t, "@bureau/dev/workspace/coder/0:bureau.local"))
	if result.Decision != Deny {
		t.Errorf("secret agent observe coder: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonAllowanceDenied {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonAllowanceDenied)
	}
	if result.MatchedAllowanceDenial == nil {
		t.Error("MatchedAllowanceDenial is nil")
	}
}

func TestAuthorized_UnknownPrincipal(t *testing.T) {
	index := setupIndex(t)

	// A principal not in the index has no grants → deny.
	result := Authorized(index, uid(t, "@unknown/agent:bureau.local"), "observe", uid(t, "@bureau/dev/workspace/coder/0:bureau.local"))
	if result.Decision != Deny {
		t.Errorf("unknown agent: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonNoGrant {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoGrant)
	}
}

func TestAuthorized_OperatorFullAccess(t *testing.T) {
	index := setupIndex(t)

	// Operator has ** grant on ** targets. But the target also needs
	// an allowance for the operator.
	index.SetPrincipal(uid(t, "@bureau/dev/workspace/coder/0:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"**"}, Actors: []string{"bureau-admin:bureau.local"}},
		},
	})

	result := Authorized(index, uid(t, "@bureau-admin:bureau.local"), "interrupt/terminate", uid(t, "@bureau/dev/workspace/coder/0:bureau.local"))
	if result.Decision != Allow {
		t.Errorf("admin interrupt/terminate coder: got %v (%v), want allow", result.Decision, result.Reason)
	}
}

func TestAuthorized_ExpiredGrant(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe"}, Targets: []string{"**:**"}, ExpiresAt: "2020-01-01T00:00:00Z"},
		},
	})
	index.SetPrincipal(uid(t, "@target:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"agent:bureau.local"}},
		},
	})

	checkTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	result := AuthorizedAt(index, uid(t, "@agent:bureau.local"), "observe", uid(t, "@target:bureau.local"), checkTime)
	if result.Decision != Deny {
		t.Errorf("expired grant: got %v, want deny", result.Decision)
	}
	if result.Reason != ReasonNoGrant {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoGrant)
	}
}

func TestAuthorized_FutureGrantValid(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe"}, Targets: []string{"**:**"}, ExpiresAt: "2099-01-01T00:00:00Z"},
		},
	})
	index.SetPrincipal(uid(t, "@target:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"agent:bureau.local"}},
		},
	})

	checkTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	result := AuthorizedAt(index, uid(t, "@agent:bureau.local"), "observe", uid(t, "@target:bureau.local"), checkTime)
	if result.Decision != Allow {
		t.Errorf("future grant: got %v (%v), want allow", result.Decision, result.Reason)
	}
}

func TestAuthorizedAt_Deterministic(t *testing.T) {
	index := NewIndex()

	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	index.SetPrincipal(uid(t, "@agent:bureau.local"), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"observe"}, Targets: []string{"**:**"}, ExpiresAt: expiresAt.Format(time.RFC3339)},
		},
	})
	index.SetPrincipal(uid(t, "@target:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"agent:bureau.local"}},
		},
	})

	// Before expiry: allowed.
	before := expiresAt.Add(-time.Second)
	result := AuthorizedAt(index, uid(t, "@agent:bureau.local"), "observe", uid(t, "@target:bureau.local"), before)
	if result.Decision != Allow {
		t.Errorf("before expiry: got %v (%v), want allow", result.Decision, result.Reason)
	}

	// At expiry: denied (not strictly before).
	result = AuthorizedAt(index, uid(t, "@agent:bureau.local"), "observe", uid(t, "@target:bureau.local"), expiresAt)
	if result.Decision != Deny {
		t.Errorf("at expiry: got %v, want deny", result.Decision)
	}

	// After expiry: denied.
	after := expiresAt.Add(time.Second)
	result = AuthorizedAt(index, uid(t, "@agent:bureau.local"), "observe", uid(t, "@target:bureau.local"), after)
	if result.Decision != Deny {
		t.Errorf("after expiry: got %v, want deny", result.Decision)
	}
}

func TestTargetAllows_BasicAllow(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
		},
	})

	if !TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/alice should be allowed to observe agent/alpha")
	}
}

func TestTargetAllows_NoAllowance(t *testing.T) {
	index := NewIndex()

	// Target has no allowances at all.
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{})

	if TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("should deny when target has no allowances")
	}
}

func TestTargetAllows_ActorDoesNotMatch(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/alice:bureau.local"}},
		},
	})

	if TargetAllows(index, uid(t, "@ops/bob:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/bob should not match allowance for ops/alice")
	}
}

func TestTargetAllows_ActionDoesNotMatch(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
		},
	})

	if TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe/read-write", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("observe/read-write should not match allowance for observe")
	}
}

func TestTargetAllows_AllowanceDenialOverrides(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe"}, Actors: []string{"ops/untrusted:bureau.local"}},
		},
	})

	// ops/alice passes: matches allowance, no denial.
	if !TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/alice should be allowed")
	}

	// ops/untrusted blocked: matches allowance but also matches denial.
	if TargetAllows(index, uid(t, "@ops/untrusted:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/untrusted should be denied by allowance denial")
	}
}

func TestTargetAllows_GlobPatterns(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/*:bureau.local"}},
		},
	})

	// ops/alice matches ops/* (single segment).
	if !TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/alice should match ops/*")
	}

	// ops/team/lead does NOT match ops/* (multi-segment).
	if TargetAllows(index, uid(t, "@ops/team/lead:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/team/lead should not match ops/* (single segment only)")
	}
}

func TestTargetAllows_TargetNotInIndex(t *testing.T) {
	index := NewIndex()

	// Target not in index at all — should deny.
	if TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@unknown/principal:bureau.local")) {
		t.Error("should deny when target is not in the index")
	}
}

func TestTargetAllows_MultipleAllowances(t *testing.T) {
	index := NewIndex()

	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"ops/alice:bureau.local"}},
		},
	})

	// ops/alice gets observe (first allowance).
	if !TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/alice should be allowed to observe")
	}

	// ops/alice gets observe/read-write (second allowance).
	if !TargetAllows(index, uid(t, "@ops/alice:bureau.local"), "observe/read-write", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/alice should be allowed observe/read-write")
	}

	// ops/bob gets observe but not observe/read-write.
	if !TargetAllows(index, uid(t, "@ops/bob:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/bob should be allowed to observe")
	}
	if TargetAllows(index, uid(t, "@ops/bob:bureau.local"), "observe/read-write", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("ops/bob should not be allowed observe/read-write")
	}
}

func TestTargetAllows_ActorNotInIndex(t *testing.T) {
	index := NewIndex()

	// The actor does not need to be in the index — TargetAllows only
	// checks the target's allowances and the actor string as a match
	// parameter. This is the key difference from Authorized(): external
	// actors (humans, cross-machine principals) work without index entries.
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ben:bureau.local"}},
		},
	})

	if !TargetAllows(index, uid(t, "@ben:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local")) {
		t.Error("external actor 'ben' should be allowed by target's allowance")
	}
}

func TestGrantsAllow(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create", "ticket/assign"}},
		{Actions: []string{"observe"}, Targets: []string{"bureau/dev/**:bureau.local"}},
	}

	tests := []struct {
		action string
		target ref.UserID
		want   bool
	}{
		{"ticket/create", ref.UserID{}, true},
		{"ticket/assign", ref.UserID{}, true},
		{"ticket/close", ref.UserID{}, false},
		{"observe", uid(t, "@bureau/dev/coder/0:bureau.local"), true},
		{"observe", uid(t, "@iree/agent:bureau.local"), false},
		{"observe", ref.UserID{}, true}, // self-service check on targeted grant
	}

	for _, tt := range tests {
		got := GrantsAllow(grants, tt.action, tt.target)
		if got != tt.want {
			t.Errorf("GrantsAllow(%q, %v) = %v, want %v", tt.action, tt.target, got, tt.want)
		}
	}
}

func TestGrantsAllow_ExpiredGrant(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create"}, ExpiresAt: "2020-01-01T00:00:00Z"},
	}

	checkTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if GrantsAllowAt(grants, "ticket/create", ref.UserID{}, checkTime) {
		t.Error("expired grant should not match")
	}
}

func TestGrantsAllowAt_Deterministic(t *testing.T) {
	expiresAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	grants := []schema.Grant{
		{Actions: []string{"ticket/create"}, ExpiresAt: expiresAt.Format(time.RFC3339)},
	}

	before := expiresAt.Add(-time.Second)
	if !GrantsAllowAt(grants, "ticket/create", ref.UserID{}, before) {
		t.Error("before expiry should match")
	}

	if GrantsAllowAt(grants, "ticket/create", ref.UserID{}, expiresAt) {
		t.Error("at expiry should not match")
	}
}

func TestDecisionString(t *testing.T) {
	if Allow.String() != "allow" {
		t.Errorf("Allow.String() = %q, want allow", Allow.String())
	}
	if Deny.String() != "deny" {
		t.Errorf("Deny.String() = %q, want deny", Deny.String())
	}
}

func TestDenyReasonString(t *testing.T) {
	reasons := []struct {
		reason DenyReason
		want   string
	}{
		{ReasonNoGrant, "no matching grant"},
		{ReasonGrantExpired, "matching grant expired"},
		{ReasonDenied, "explicit denial"},
		{ReasonNoAllowance, "no matching allowance on target"},
		{ReasonAllowanceDenied, "explicit allowance denial on target"},
	}

	for _, tt := range reasons {
		if got := tt.reason.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestTargetCheck_Allow(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
		},
	})

	result := TargetCheck(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local"))
	if !result.Allowed {
		t.Errorf("got denied (%v), want allowed", result.Reason)
	}
	if result.MatchedAllowance == nil {
		t.Error("MatchedAllowance is nil on allow")
	}
}

func TestTargetCheck_NoAllowance(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{})

	result := TargetCheck(index, uid(t, "@ops/alice:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local"))
	if result.Allowed {
		t.Error("got allowed, want denied")
	}
	if result.Reason != ReasonNoAllowance {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonNoAllowance)
	}
}

func TestTargetCheck_AllowanceDenial(t *testing.T) {
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe"}, Actors: []string{"ops/untrusted:bureau.local"}},
		},
	})

	result := TargetCheck(index, uid(t, "@ops/untrusted:bureau.local"), "observe", uid(t, "@agent/alpha:bureau.local"))
	if result.Allowed {
		t.Error("got allowed, want denied")
	}
	if result.Reason != ReasonAllowanceDenied {
		t.Errorf("reason = %v, want %v", result.Reason, ReasonAllowanceDenied)
	}
	if result.MatchedAllowance == nil {
		t.Error("MatchedAllowance is nil (should show what was overridden)")
	}
	if result.MatchedAllowanceDenial == nil {
		t.Error("MatchedAllowanceDenial is nil")
	}
}

func TestTargetCheck_ConsistentWithTargetAllows(t *testing.T) {
	// Verify TargetCheck and TargetAllows always agree.
	index := NewIndex()
	index.SetPrincipal(uid(t, "@agent/alpha:bureau.local"), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:bureau.local"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"ops/alice:bureau.local"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe"}, Actors: []string{"ops/untrusted:bureau.local"}},
		},
	})

	cases := []struct {
		actor  ref.UserID
		action string
	}{
		{uid(t, "@ops/alice:bureau.local"), "observe"},
		{uid(t, "@ops/alice:bureau.local"), "observe/read-write"},
		{uid(t, "@ops/bob:bureau.local"), "observe"},
		{uid(t, "@ops/bob:bureau.local"), "observe/read-write"},
		{uid(t, "@ops/untrusted:bureau.local"), "observe"},
		{uid(t, "@dev/coder:bureau.local"), "observe"},
	}

	target := uid(t, "@agent/alpha:bureau.local")
	for _, tt := range cases {
		boolResult := TargetAllows(index, tt.actor, tt.action, target)
		checkResult := TargetCheck(index, tt.actor, tt.action, target)
		if boolResult != checkResult.Allowed {
			t.Errorf("TargetAllows(%v, %q) = %v but TargetCheck.Allowed = %v",
				tt.actor, tt.action, boolResult, checkResult.Allowed)
		}
	}
}

func TestGrantsCheck_Allow(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create", "ticket/assign"}},
		{Actions: []string{"observe"}, Targets: []string{"bureau/dev/**:bureau.local"}},
	}

	result := GrantsCheck(grants, "ticket/create", ref.UserID{})
	if !result.Allowed {
		t.Error("got denied, want allowed")
	}
	if result.MatchedGrant == nil {
		t.Error("MatchedGrant is nil on allow")
	}
}

func TestGrantsCheck_Deny(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create"}},
	}

	result := GrantsCheck(grants, "fleet/assign", ref.UserID{})
	if result.Allowed {
		t.Error("got allowed, want denied")
	}
	if result.MatchedGrant != nil {
		t.Error("MatchedGrant should be nil on deny")
	}
}

func TestGrantsCheck_ConsistentWithGrantsAllow(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create", "ticket/assign"}},
		{Actions: []string{"observe"}, Targets: []string{"bureau/dev/**:bureau.local"}},
	}

	cases := []struct {
		action string
		target ref.UserID
	}{
		{"ticket/create", ref.UserID{}},
		{"ticket/close", ref.UserID{}},
		{"observe", uid(t, "@bureau/dev/coder/0:bureau.local")},
		{"observe", uid(t, "@iree/agent:bureau.local")},
		{"fleet/assign", ref.UserID{}},
	}

	for _, tt := range cases {
		boolResult := GrantsAllow(grants, tt.action, tt.target)
		checkResult := GrantsCheck(grants, tt.action, tt.target)
		if boolResult != checkResult.Allowed {
			t.Errorf("GrantsAllow(%q, %v) = %v but GrantsCheck.Allowed = %v",
				tt.action, tt.target, boolResult, checkResult.Allowed)
		}
	}
}

func TestGrantsCheckAt_Expired(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"ticket/create"}, ExpiresAt: "2020-01-01T00:00:00Z"},
	}

	checkTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	result := GrantsCheckAt(grants, "ticket/create", ref.UserID{}, checkTime)
	if result.Allowed {
		t.Error("expired grant should not match")
	}
}
