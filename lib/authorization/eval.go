// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Decision is the outcome of an authorization check.
type Decision int

const (
	// Deny means the action is not permitted.
	Deny Decision = iota

	// Allow means the action is permitted.
	Allow
)

// String returns "allow" or "deny".
func (d Decision) String() string {
	if d == Allow {
		return "allow"
	}
	return "deny"
}

// DenyReason describes why an authorization check was denied.
type DenyReason int

const (
	// ReasonNoGrant means no grant matched the action and target.
	ReasonNoGrant DenyReason = iota

	// ReasonGrantExpired means a grant matched but was expired.
	ReasonGrantExpired

	// ReasonDenied means a matching denial overrode the grant.
	ReasonDenied

	// ReasonNoAllowance means no allowance on the target matched.
	ReasonNoAllowance

	// ReasonAllowanceDenied means a matching allowance denial overrode
	// the allowance.
	ReasonAllowanceDenied
)

// String returns a human-readable reason.
func (r DenyReason) String() string {
	switch r {
	case ReasonNoGrant:
		return "no matching grant"
	case ReasonGrantExpired:
		return "matching grant expired"
	case ReasonDenied:
		return "explicit denial"
	case ReasonNoAllowance:
		return "no matching allowance on target"
	case ReasonAllowanceDenied:
		return "explicit allowance denial on target"
	default:
		return "unknown"
	}
}

// Result describes the outcome of an authorization check, including
// the decision and a trace of which rules were evaluated. The trace
// supports debugging (bureau auth check) and audit logging.
type Result struct {
	// Decision is Allow or Deny.
	Decision Decision

	// Reason describes why the check was denied. Only meaningful when
	// Decision is Deny.
	Reason DenyReason

	// MatchedGrant is the grant that matched, if any. Nil when denied
	// at the grant stage.
	MatchedGrant *schema.Grant

	// MatchedDenial is the denial that fired, if any. Only set when
	// Reason is ReasonDenied.
	MatchedDenial *schema.Denial

	// MatchedAllowance is the allowance that matched, if any. Nil for
	// self-service actions or when denied at the allowance stage.
	MatchedAllowance *schema.Allowance

	// MatchedAllowanceDenial is the allowance denial that fired, if
	// any. Only set when Reason is ReasonAllowanceDenied.
	MatchedAllowanceDenial *schema.AllowanceDenial
}

// Authorized checks whether actor can perform action on target. Returns
// a Result containing the decision and the evaluation trace.
//
// Actor and target are full Matrix user IDs (ref.UserID). For
// self-service actions (no target), pass a zero-value ref.UserID.
// Grant/denial/allowance patterns use "localpart:server" format and
// are matched against the full user ID — this prevents cross-server
// identity confusion in federated deployments.
//
// Evaluation:
//  1. Find matching grant (action + target) → no match = DENY
//  2. Check grant expiry → expired = skip grant, try next
//  3. Check actor denials → match = DENY
//  4. For cross-principal: find matching allowance (action + actor)
//  5. Check allowance denials → match = DENY
//  6. All checks pass = ALLOW
func Authorized(index *Index, actor ref.UserID, action string, target ref.UserID) Result {
	return AuthorizedAt(index, actor, action, target, time.Now())
}

// AuthorizedAt is like Authorized but accepts an explicit time for
// grant expiry checks. This supports deterministic testing.
func AuthorizedAt(index *Index, actor ref.UserID, action string, target ref.UserID, now time.Time) Result {
	index.mu.RLock()
	defer index.mu.RUnlock()

	targetStr := target.String()
	selfService := target.IsZero()

	// Step 1-2: Find a non-expired matching grant.
	grants := index.grants[actor]
	var matchedGrant *schema.Grant
	for i := range grants {
		grant := &grants[i]
		if !grantMatchesUserID(*grant, action, targetStr, selfService) {
			continue
		}
		// Check expiry.
		if grant.ExpiresAt != "" {
			expiresAt, err := time.Parse(time.RFC3339, grant.ExpiresAt)
			if err != nil || !now.Before(expiresAt) {
				continue
			}
		}
		matchedGrant = grant
		break
	}

	if matchedGrant == nil {
		return Result{
			Decision: Deny,
			Reason:   ReasonNoGrant,
		}
	}

	// Step 3: Check actor denials.
	denials := index.denials[actor]
	for i := range denials {
		denial := &denials[i]
		if denialMatchesUserID(*denial, action, targetStr, selfService) {
			return Result{
				Decision:      Deny,
				Reason:        ReasonDenied,
				MatchedGrant:  matchedGrant,
				MatchedDenial: denial,
			}
		}
	}

	// Self-service actions: no target-side check needed.
	if selfService {
		return Result{
			Decision:     Allow,
			MatchedGrant: matchedGrant,
		}
	}

	// Step 4: Find matching allowance on the target.
	actorStr := actor.String()
	allowances := index.allowances[target]
	var matchedAllowance *schema.Allowance
	for i := range allowances {
		allowance := &allowances[i]
		if allowanceMatchesUserID(*allowance, action, actorStr) {
			matchedAllowance = allowance
			break
		}
	}

	if matchedAllowance == nil {
		return Result{
			Decision:     Deny,
			Reason:       ReasonNoAllowance,
			MatchedGrant: matchedGrant,
		}
	}

	// Step 5: Check target allowance denials.
	allowanceDenials := index.allowanceDenials[target]
	for i := range allowanceDenials {
		denial := &allowanceDenials[i]
		if allowanceDenialMatchesUserID(*denial, action, actorStr) {
			return Result{
				Decision:               Deny,
				Reason:                 ReasonAllowanceDenied,
				MatchedGrant:           matchedGrant,
				MatchedAllowance:       matchedAllowance,
				MatchedAllowanceDenial: denial,
			}
		}
	}

	// Step 6: All checks pass.
	return Result{
		Decision:         Allow,
		MatchedGrant:     matchedGrant,
		MatchedAllowance: matchedAllowance,
	}
}

// TargetResult describes the outcome of a target-side authorization
// check. Provides the deny reason and matched rules for audit logging.
type TargetResult struct {
	// Allowed is true if the target's allowances permit the action.
	Allowed bool

	// Reason describes why the check was denied. Only meaningful when
	// Allowed is false.
	Reason DenyReason

	// MatchedAllowance is the allowance that matched, if any.
	MatchedAllowance *schema.Allowance

	// MatchedAllowanceDenial is the allowance denial that fired, if
	// any. Only set when Reason is ReasonAllowanceDenied.
	MatchedAllowanceDenial *schema.AllowanceDenial
}

// TargetCheck checks whether a target's allowances permit an actor to
// perform an action, returning the full evaluation trace. This is the
// result-returning variant of TargetAllows — use it when the deny
// reason and matched rules are needed for audit logging.
func TargetCheck(index *Index, actor ref.UserID, action string, target ref.UserID) TargetResult {
	index.mu.RLock()
	defer index.mu.RUnlock()
	return targetCheckLocked(index, actor.String(), action, target)
}

// targetCheckLocked is the lock-free implementation of TargetCheck.
// Caller must hold index.mu.RLock.
func targetCheckLocked(index *Index, actorStr, action string, target ref.UserID) TargetResult {
	// Find a matching allowance on the target.
	allowances := index.allowances[target]
	var matchedAllowance *schema.Allowance
	for i := range allowances {
		if allowanceMatchesUserID(allowances[i], action, actorStr) {
			matchedAllowance = &allowances[i]
			break
		}
	}

	if matchedAllowance == nil {
		return TargetResult{
			Allowed: false,
			Reason:  ReasonNoAllowance,
		}
	}

	// Check target allowance denials.
	for i := range index.allowanceDenials[target] {
		denial := &index.allowanceDenials[target][i]
		if allowanceDenialMatchesUserID(*denial, action, actorStr) {
			return TargetResult{
				Allowed:                false,
				Reason:                 ReasonAllowanceDenied,
				MatchedAllowance:       matchedAllowance,
				MatchedAllowanceDenial: denial,
			}
		}
	}

	return TargetResult{
		Allowed:          true,
		MatchedAllowance: matchedAllowance,
	}
}

// TargetAllows checks whether a target's allowances permit an actor to
// perform an action. This evaluates only the target side of the
// authorization model: allowances and allowance denials.
//
// Used for authorization decisions where the relevant question is
// "does the target permit this actor?" rather than "does the actor have
// permission to act on this target?" Authentication establishes the
// actor's identity independently; the target's allowance policy
// controls access. Observation is the primary example: the observer
// authenticates via Matrix token verification, and the target
// principal's allowances determine who may observe.
//
// Returns false if the target has no matching allowance, or if a
// matching allowance denial overrides it. Use TargetCheck when the
// deny reason and matched rules are needed for audit logging.
func TargetAllows(index *Index, actor ref.UserID, action string, target ref.UserID) bool {
	return TargetCheck(index, actor, action, target).Allowed
}

// GrantsResult describes the outcome of a grants-only authorization
// check. Provides the matched grant for audit logging.
type GrantsResult struct {
	// Allowed is true if a matching non-expired grant was found.
	Allowed bool

	// MatchedGrant is the grant that matched, if any. Nil when denied.
	MatchedGrant *schema.Grant
}

// GrantsCheck checks whether a slice of grants authorizes a specific
// action on a specific target, returning the matched grant. This is
// the result-returning variant of GrantsAllow — use it when the
// matched grant is needed for audit logging.
//
// The target is a full Matrix user ID. For self-service actions (no
// target), pass a zero-value ref.UserID.
//
// Same matching semantics as GrantsAllow: no denials, no allowances.
func GrantsCheck(grants []schema.Grant, action string, target ref.UserID) GrantsResult {
	return GrantsCheckAt(grants, action, target, time.Now())
}

// GrantsCheckAt is like GrantsCheck but accepts an explicit time for
// expiry checks.
func GrantsCheckAt(grants []schema.Grant, action string, target ref.UserID, now time.Time) GrantsResult {
	targetStr := target.String()
	selfService := target.IsZero()
	for i := range grants {
		grant := &grants[i]
		if !grantMatchesUserID(*grant, action, targetStr, selfService) {
			continue
		}
		if grant.ExpiresAt != "" {
			expiresAt, err := time.Parse(time.RFC3339, grant.ExpiresAt)
			if err != nil || !now.Before(expiresAt) {
				continue
			}
		}
		return GrantsResult{Allowed: true, MatchedGrant: grant}
	}
	return GrantsResult{Allowed: false}
}

// GrantsAllow checks whether a slice of grants authorizes a specific
// action on a specific target. This is the same matching logic as
// Authorized() steps 1-2 but operates on a pre-resolved grant slice
// instead of a full index. Used by services that receive grants in
// a service token.
//
// Does not check denials or allowances — service tokens carry only
// subject-side grants, and the service is the target (not another
// principal). The daemon pre-filters grants to the service's namespace
// when minting the token, so the service only needs to check whether
// the embedded grants cover the requested operation.
//
// Use GrantsCheck when the matched grant is needed for audit logging.
func GrantsAllow(grants []schema.Grant, action string, target ref.UserID) bool {
	return GrantsCheck(grants, action, target).Allowed
}

// GrantsAllowAt is like GrantsAllow but accepts an explicit time for
// expiry checks.
func GrantsAllowAt(grants []schema.Grant, action string, target ref.UserID, now time.Time) bool {
	return GrantsCheckAt(grants, action, target, now).Allowed
}

// GrantsAllowServiceType checks whether a slice of grants authorizes
// discovering a service with the given type localpart. Unlike GrantsAllow
// which matches targets as full Matrix user IDs (localpart:server),
// this matches targets as localpart-level glob patterns.
//
// Service discovery targets are service TYPE localparts (e.g.,
// "service/stt/test"), not entity user IDs. The ServiceVisibility
// field on PrincipalAssignment defines localpart patterns like
// "service/stt/*" that match these type localparts. The proxy's
// HandleServiceDirectory uses this function to filter the service
// directory by the principal's grants.
func GrantsAllowServiceType(grants []schema.Grant, action, serviceLocalpart string) bool {
	for i := range grants {
		grant := &grants[i]
		if !matchAnyAction(grant.Actions, action) {
			continue
		}
		if len(grant.Targets) == 0 {
			continue
		}
		for _, target := range grant.Targets {
			if principal.MatchPattern(target, serviceLocalpart) {
				return true
			}
		}
	}
	return false
}
