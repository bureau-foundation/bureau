// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

// authenticateObserver verifies a Matrix access token and returns the
// authenticated user ID. Returns a user-facing error message (no internal
// details) if authentication fails.
func (d *Daemon) authenticateObserver(ctx context.Context, token string) (ref.UserID, error) {
	if token == "" {
		return ref.UserID{}, fmt.Errorf("authentication required: provide an access token (run \"bureau login\" first)")
	}

	userIDStr, err := d.tokenVerifier.Verify(ctx, token)
	if err != nil {
		return ref.UserID{}, fmt.Errorf("authentication failed: invalid or expired token (run \"bureau login\" to refresh)")
	}

	userID, err := ref.ParseUserID(userIDStr)
	if err != nil {
		return ref.UserID{}, fmt.Errorf("authentication failed: verified token has invalid user ID %q", userIDStr)
	}

	return userID, nil
}

// observeAuthorization describes what an observer is allowed to do.
type observeAuthorization struct {
	// Allowed is true if the observer may observe the principal at all.
	Allowed bool

	// GrantedMode is "readwrite" or "readonly". When the observer requests
	// "readwrite" but lacks an "observe/read-write" allowance on the
	// target, the daemon downgrades to "readonly".
	GrantedMode string
}

// authorizeObserve checks whether an observer is permitted to observe a
// specific principal, and at what mode. The check uses the target
// principal's allowances in the authorization index — observation is a
// target-side authorization decision.
//
// Default-deny: if the target has no matching allowance, observation is
// rejected. The requestedMode is "readwrite" or "readonly"; the granted
// mode may be downgraded from readwrite to readonly based on allowances.
func (d *Daemon) authorizeObserve(observer ref.UserID, principal ref.UserID, requestedMode string) observeAuthorization {
	// All observation requires an "observe" allowance on the target.
	observeResult := authorization.TargetCheck(d.authorizationIndex, observer, observation.ActionObserve, principal)
	if !observeResult.Allowed {
		d.postAuditDeny(observer, observation.ActionObserve, principal,
			"daemon/observe", observeResult.Reason,
			observeResult.MatchedAllowance, observeResult.MatchedAllowanceDenial)
		return observeAuthorization{Allowed: false}
	}

	// Determine the granted mode.
	grantedMode := requestedMode
	if requestedMode == "readwrite" {
		rwResult := authorization.TargetCheck(d.authorizationIndex, observer, observation.ActionReadWrite, principal)
		if !rwResult.Allowed {
			grantedMode = "readonly"
		} else {
			// observe/read-write is a sensitive action — log the grant.
			d.postAuditAllow(observer, observation.ActionReadWrite, principal,
				"daemon/observe", rwResult.MatchedAllowance)
		}
	}

	return observeAuthorization{
		Allowed:     true,
		GrantedMode: grantedMode,
	}
}

// authorizeList checks whether an observer is permitted to see a specific
// principal in a list response. Uses the same authorization index check
// as authorizeObserve — if the target's allowances permit the observer
// for the "observe" action, the principal appears in the list.
func (d *Daemon) authorizeList(observer ref.UserID, principal ref.UserID) bool {
	return authorization.TargetAllows(d.authorizationIndex, observer, observation.ActionObserve, principal)
}
