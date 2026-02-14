// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"time"

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
// For self-service actions (empty target), only the subject side
// (grants, denials) is checked — steps 4-5 are skipped.
//
// Evaluation:
//  1. Find matching grant (action + target) → no match = DENY
//  2. Check grant expiry → expired = skip grant, try next
//  3. Check actor denials → match = DENY
//  4. For cross-principal: find matching allowance (action + actor)
//  5. Check allowance denials → match = DENY
//  6. All checks pass = ALLOW
func Authorized(index *Index, actor, action, target string) Result {
	return AuthorizedAt(index, actor, action, target, time.Now())
}

// AuthorizedAt is like Authorized but accepts an explicit time for
// grant expiry checks. This supports deterministic testing.
func AuthorizedAt(index *Index, actor, action, target string, now time.Time) Result {
	index.mu.RLock()
	defer index.mu.RUnlock()

	// Step 1-2: Find a non-expired matching grant.
	grants := index.grants[actor]
	var matchedGrant *schema.Grant
	for i := range grants {
		grant := &grants[i]
		if !grantMatches(*grant, action, target) {
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
		if denialMatches(*denial, action, target) {
			return Result{
				Decision:      Deny,
				Reason:        ReasonDenied,
				MatchedGrant:  matchedGrant,
				MatchedDenial: denial,
			}
		}
	}

	// Self-service actions: no target-side check needed.
	if target == "" {
		return Result{
			Decision:     Allow,
			MatchedGrant: matchedGrant,
		}
	}

	// Step 4: Find matching allowance on the target.
	allowances := index.allowances[target]
	var matchedAllowance *schema.Allowance
	for i := range allowances {
		allowance := &allowances[i]
		if allowanceMatches(*allowance, action, actor) {
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
		if allowanceDenialMatches(*denial, action, actor) {
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
// matching allowance denial overrides it.
func TargetAllows(index *Index, actor, action, target string) bool {
	index.mu.RLock()
	defer index.mu.RUnlock()

	// Find a matching allowance on the target.
	allowances := index.allowances[target]
	matched := false
	for i := range allowances {
		if allowanceMatches(allowances[i], action, actor) {
			matched = true
			break
		}
	}

	if !matched {
		return false
	}

	// Check target allowance denials.
	for _, denial := range index.allowanceDenials[target] {
		if allowanceDenialMatches(denial, action, actor) {
			return false
		}
	}

	return true
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
func GrantsAllow(grants []schema.Grant, action, target string) bool {
	return GrantsAllowAt(grants, action, target, time.Now())
}

// GrantsAllowAt is like GrantsAllow but accepts an explicit time for
// expiry checks.
func GrantsAllowAt(grants []schema.Grant, action, target string, now time.Time) bool {
	for _, grant := range grants {
		if !grantMatches(grant, action, target) {
			continue
		}
		if grant.ExpiresAt != "" {
			expiresAt, err := time.Parse(time.RFC3339, grant.ExpiresAt)
			if err != nil || !now.Before(expiresAt) {
				continue
			}
		}
		return true
	}
	return false
}
