// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// authenticateObserver verifies a Matrix access token and returns the
// authenticated user ID. Returns a user-facing error message (no internal
// details) if authentication fails.
func (d *Daemon) authenticateObserver(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("authentication required: provide an access token (run \"bureau login\" first)")
	}

	userID, err := d.tokenVerifier.Verify(ctx, token)
	if err != nil {
		return "", fmt.Errorf("authentication failed: invalid or expired token (run \"bureau login\" to refresh)")
	}

	return userID, nil
}

// observeAuthorization describes what an observer is allowed to do.
type observeAuthorization struct {
	// Allowed is true if the observer may observe the principal at all.
	Allowed bool

	// GrantedMode is "readwrite" or "readonly". When the observer requests
	// "readwrite" but is only in AllowedObservers (not ReadWriteObservers),
	// the daemon downgrades to "readonly".
	GrantedMode string
}

// authorizeObserve checks whether an observer is permitted to observe a
// specific principal, and at what mode. The check uses the principal's
// ObservePolicy from MachineConfig, falling back to the machine-level
// DefaultObservePolicy.
//
// Default-deny: if no policy exists at either level, observation is rejected.
// The requestedMode is "readwrite" or "readonly"; the granted mode may be
// downgraded from readwrite to readonly based on policy.
func (d *Daemon) authorizeObserve(observerUserID, principalLocalpart, requestedMode string) observeAuthorization {
	if d.lastConfig == nil {
		return observeAuthorization{Allowed: false}
	}

	policy := d.findObservePolicy(principalLocalpart)
	if policy == nil {
		return observeAuthorization{Allowed: false}
	}

	observerLocalpart, err := principal.LocalpartFromMatrixID(observerUserID)
	if err != nil {
		// The observer's Matrix user ID doesn't follow Bureau naming
		// conventions (e.g., "@admin:bureau.local" without a hierarchical
		// localpart). Fall back to matching against the full user ID.
		// This allows policies to use patterns like "@admin:bureau.local".
		observerLocalpart = observerUserID
	}

	// Check if the observer matches any AllowedObservers pattern.
	if !principal.MatchAnyPattern(policy.AllowedObservers, observerLocalpart) {
		return observeAuthorization{Allowed: false}
	}

	// Observer is allowed. Determine the granted mode.
	grantedMode := requestedMode
	if requestedMode == "readwrite" {
		if !principal.MatchAnyPattern(policy.ReadWriteObservers, observerLocalpart) {
			grantedMode = "readonly"
		}
	}

	return observeAuthorization{
		Allowed:     true,
		GrantedMode: grantedMode,
	}
}

// authorizeList checks whether an observer is permitted to see a specific
// principal in a list response. Uses the same ObservePolicy lookup as
// authorizeObserve — if the observer would be allowed to observe the
// principal (in any mode), it appears in the list.
func (d *Daemon) authorizeList(observerUserID, principalLocalpart string) bool {
	authz := d.authorizeObserve(observerUserID, principalLocalpart, "readonly")
	return authz.Allowed
}

// findObservePolicy looks up the ObservePolicy for a principal. Checks the
// principal's PrincipalAssignment first, then falls back to the machine-level
// DefaultObservePolicy. Returns nil if no policy exists (default-deny).
func (d *Daemon) findObservePolicy(principalLocalpart string) *schema.ObservePolicy {
	for _, assignment := range d.lastConfig.Principals {
		if assignment.Localpart == principalLocalpart {
			if assignment.ObservePolicy != nil {
				return assignment.ObservePolicy
			}
			break
		}
	}
	return d.lastConfig.DefaultObservePolicy
}
