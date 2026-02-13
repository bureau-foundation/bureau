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

// matchAnyTarget checks whether a target matches any pattern in the
// list. Returns true on the first match.
func matchAnyTarget(patterns []string, target string) bool {
	return principal.MatchAnyPattern(patterns, target)
}

// matchAnyActor checks whether an actor matches any pattern in the
// list. Returns true on the first match.
func matchAnyActor(patterns []string, actor string) bool {
	return principal.MatchAnyPattern(patterns, actor)
}

// grantMatches checks whether a grant covers a specific action on a
// specific target. For self-service actions (empty target), only the
// action patterns are checked and the grant must also have empty
// targets. For cross-principal actions, both action and target patterns
// must match.
func grantMatches(grant schema.Grant, action, target string) bool {
	if !matchAnyAction(grant.Actions, action) {
		return false
	}

	if target == "" {
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
	return matchAnyTarget(grant.Targets, target)
}

// denialMatches checks whether a denial covers a specific action on a
// specific target.
func denialMatches(denial schema.Denial, action, target string) bool {
	if !matchAnyAction(denial.Actions, action) {
		return false
	}

	if target == "" {
		return true
	}

	if len(denial.Targets) == 0 {
		return false
	}
	return matchAnyTarget(denial.Targets, target)
}

// allowanceMatches checks whether an allowance covers a specific action
// from a specific actor.
func allowanceMatches(allowance schema.Allowance, action, actor string) bool {
	if !matchAnyAction(allowance.Actions, action) {
		return false
	}
	return matchAnyActor(allowance.Actors, actor)
}

// allowanceDenialMatches checks whether an allowance denial covers a
// specific action from a specific actor.
func allowanceDenialMatches(denial schema.AllowanceDenial, action, actor string) bool {
	if !matchAnyAction(denial.Actions, action) {
		return false
	}
	return matchAnyActor(denial.Actors, actor)
}
