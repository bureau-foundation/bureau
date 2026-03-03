// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"slices"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// Rule selects models from a catalog based on ID patterns and metadata
// constraints. All conditions are AND — a model must satisfy every
// non-zero field to match.
type Rule struct {
	// Match is a glob pattern on the model ID. Uses Bureau's
	// hierarchical pattern syntax: "*" matches a single segment,
	// "**" matches across segments, "?" matches one character.
	// Examples: "anthropic/*", "qwenlm/qwen3*", "**".
	// Empty string matches nothing.
	Match string

	// Exclude is a list of glob patterns. Models matching any
	// exclude pattern are rejected even if they match the Match
	// pattern. Common uses: ["*preview*", "*:free", "*beta*"].
	Exclude []string

	// MinContextLength filters out models with a context window
	// shorter than this value (in tokens). Zero means no minimum.
	MinContextLength int

	// MaxInputPriceMicrodollarsPerMtok filters out models whose
	// input pricing exceeds this value. Zero means no maximum.
	MaxInputPriceMicrodollarsPerMtok int64

	// RequiredParameters filters out models that don't support all
	// listed parameters. Example: ["tools"] requires tool use
	// support. Empty means no parameter requirements.
	RequiredParameters []string
}

// Matches reports whether a catalog entry satisfies all conditions
// in the rule.
func (rule *Rule) Matches(entry CatalogEntry) bool {
	if rule.Match == "" {
		return false
	}

	// Positive match.
	if !principal.MatchPattern(rule.Match, entry.ID) {
		return false
	}

	// Exclusion patterns.
	for _, exclude := range rule.Exclude {
		if principal.MatchPattern(exclude, entry.ID) {
			return false
		}
	}

	// Context length filter.
	if rule.MinContextLength > 0 && entry.ContextLength < rule.MinContextLength {
		return false
	}

	// Price filter.
	if rule.MaxInputPriceMicrodollarsPerMtok > 0 && entry.InputPriceMicrodollarsPerMtok > rule.MaxInputPriceMicrodollarsPerMtok {
		return false
	}

	// Required parameters filter.
	for _, required := range rule.RequiredParameters {
		if !slices.Contains(entry.SupportedParameters, required) {
			return false
		}
	}

	return true
}

// ApplyRules filters a catalog to entries matching any of the given
// rules. Each entry is tested against all rules — an entry appears
// in the result if at least one rule matches it. Duplicate entries
// (matching multiple rules) appear only once.
func ApplyRules(entries []CatalogEntry, rules []Rule) []CatalogEntry {
	if len(rules) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var result []CatalogEntry

	for _, entry := range entries {
		if seen[entry.ID] {
			continue
		}
		for ruleIndex := range rules {
			if rules[ruleIndex].Matches(entry) {
				result = append(result, entry)
				seen[entry.ID] = true
				break
			}
		}
	}

	return result
}
