// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// requireActionGrant checks that the token carries a grant for the
// given action (e.g., "model/complete"). Returns nil if authorized,
// or an error suitable for returning to the client.
//
// This is the action-only check — it does not inspect model aliases.
// Use requireModelGrant for handlers that target a specific model.
func requireActionGrant(token *servicetoken.Token, action string) error {
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return fmt.Errorf("access denied: missing grant for %s", action)
	}
	return nil
}

// requireModelGrant checks that the token authorizes the given action
// on the given model alias. Two-level check:
//
//  1. The token must carry a grant whose action patterns match the
//     action. If no grant matches the action, access is denied
//     regardless of the alias.
//
//  2. If any action-matching grant has target patterns, the alias must
//     match at least one target pattern. If no action-matching grant
//     has target patterns, the agent has unrestricted model access
//     (backward compatible with tokens minted before target-scoped
//     grants were introduced).
//
// This means:
//   - {Actions: ["model/**"]}                           → any model
//   - {Actions: ["model/**"], Targets: ["*"]}           → any model (explicit)
//   - {Actions: ["model/complete"], Targets: ["codex"]} → only "codex"
//   - {Actions: ["model/complete"], Targets: ["qwen3/*"]} → any qwen3 alias
func requireModelGrant(token *servicetoken.Token, action, alias string) error {
	// Step 1: action check.
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return fmt.Errorf("access denied: missing grant for %s", action)
	}

	// Step 2: target check. Walk the grants to find action-matching
	// grants and classify them as targeted or untargeted.
	//
	// An untargeted grant (no Targets field) for this action means
	// unrestricted model access — it wins over any targeted grants.
	// A targeted grant restricts access to specific alias patterns.
	// Only when ALL action-matching grants are targeted does the
	// alias need to match one of them.
	hasTargetedGrant := false
	hasUntargetedGrant := false
	for _, grant := range token.Grants {
		if !principal.MatchAnyPattern(grant.Actions, action) {
			continue
		}
		if len(grant.Targets) == 0 {
			hasUntargetedGrant = true
			continue
		}
		hasTargetedGrant = true
		if principal.MatchAnyPattern(grant.Targets, alias) {
			return nil
		}
	}

	if hasUntargetedGrant {
		return nil
	}

	if hasTargetedGrant {
		return fmt.Errorf("access denied: model %q not permitted for %s", alias, action)
	}

	// No action-matching grant has target patterns — unrestricted
	// model access. (This path is only reachable if step 1 passed
	// via a different grant mechanism; included for completeness.)
	return nil
}
