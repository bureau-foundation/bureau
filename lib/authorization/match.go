// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package authorization

import (
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// MatchAction checks whether a concrete action matches a glob pattern.
// Actions use the same "/" hierarchy and glob semantics as localparts:
//
//	"observe"        matches "observe" exactly
//	"observe/*"      matches "observe/read-write" but not "observe/foo/bar"
//	"observe/**"     matches "observe/read-write", "observe/foo/bar", etc.
//	"ticket/**"      matches "ticket/create", "ticket/assign", etc.
//	"**"             matches any action
//
// This delegates to principal.MatchPattern, which implements the full
// glob semantics for hierarchical "/"-separated strings.
func MatchAction(pattern, action string) bool {
	return principal.MatchPattern(pattern, action)
}

// matchAnyAction checks whether an action matches any pattern in the
// list. Returns true on the first match.
func matchAnyAction(patterns []string, action string) bool {
	return principal.MatchAnyPattern(patterns, action)
}

// matchAnyTarget checks whether a target user ID matches any user ID
// pattern in the list. Returns true on the first match. Patterns use
// the "localpart_pattern:server_pattern" format — bare localpart
// patterns are rejected to prevent cross-server grants.
func matchAnyTarget(patterns []string, target string) bool {
	return principal.MatchAnyUserID(patterns, target)
}

// matchAnyActor checks whether an actor user ID matches any user ID
// pattern in the list. Returns true on the first match. Patterns use
// the "localpart_pattern:server_pattern" format.
func matchAnyActor(patterns []string, actor string) bool {
	return principal.MatchAnyUserID(patterns, actor)
}

// grantMatchesUserID checks whether a grant covers a specific action on
// a specific target user ID. For self-service actions (selfService=true),
// only the action patterns are checked. For cross-principal actions, both
// action and target user ID patterns must match.
func grantMatchesUserID(grant schema.Grant, action, targetStr string, selfService bool) bool {
	if !matchAnyAction(grant.Actions, action) {
		return false
	}

	if selfService {
		// Self-service action: grant applies regardless of targets.
		// A grant with targets also matches self-service actions
		// (the targets field restricts cross-principal use, not
		// self-service use).
		return true
	}

	// Cross-principal action: targets must be present and match.
	if len(grant.Targets) == 0 {
		return false
	}
	return matchAnyTarget(grant.Targets, targetStr)
}

// denialMatchesUserID checks whether a denial covers a specific action on
// a specific target user ID.
func denialMatchesUserID(denial schema.Denial, action, targetStr string, selfService bool) bool {
	if !matchAnyAction(denial.Actions, action) {
		return false
	}

	if selfService {
		return true
	}

	if len(denial.Targets) == 0 {
		return false
	}
	return matchAnyTarget(denial.Targets, targetStr)
}

// allowanceMatchesUserID checks whether an allowance covers a specific
// action from a specific actor user ID.
func allowanceMatchesUserID(allowance schema.Allowance, action, actorStr string) bool {
	if !matchAnyAction(allowance.Actions, action) {
		return false
	}
	return matchAnyActor(allowance.Actors, actorStr)
}

// allowanceDenialMatchesUserID checks whether an allowance denial covers
// a specific action from a specific actor user ID.
func allowanceDenialMatchesUserID(denial schema.AllowanceDenial, action, actorStr string) bool {
	if !matchAnyAction(denial.Actions, action) {
		return false
	}
	return matchAnyActor(denial.Actors, actorStr)
}
