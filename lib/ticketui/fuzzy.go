// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
)

// FuzzyResult holds the result of a single fuzzy match: the quality
// score and the rune-level positions of matched characters in the
// input string. A zero Score means no match.
type FuzzyResult struct {
	Score     int
	Positions []int // Rune indices of matched characters in the input.
}

// fuzzyMatch runs fzf's FuzzyMatchV2 algorithm on text against
// pattern. Matching is always case-insensitive: both text and pattern
// are lowercased before comparison. The pattern must be
// pre-lowercased by the caller for efficiency (avoids redundant
// lowering on each call). The returned positions are rune indices
// into the original (pre-lowered) text, so they can be used directly
// for highlighting.
//
// The slab parameter is an optional reusable memory pool. Pass nil
// for one-off matches; pass a shared slab when matching many strings
// in a loop (e.g., filtering a ticket list) to avoid per-call
// allocations.
func fuzzyMatch(text string, pattern []rune, slab *util.Slab) FuzzyResult {
	if len(pattern) == 0 {
		return FuzzyResult{}
	}

	// Pre-lowercase the text for reliable case-insensitive matching.
	// fzf's caseSensitive=false flag only partially folds case in its
	// ASCII fast path (trySkip), which misses matches when the input
	// has uppercase and the pattern is lowercase. Lowercasing the
	// input ourselves with caseSensitive=true avoids the issue
	// entirely. The returned rune positions still correspond to the
	// original text since lowercasing preserves character count.
	lowerText := strings.ToLower(text)
	chars := util.ToChars([]byte(lowerText))
	result, positions := algo.FuzzyMatchV2(
		true, // caseSensitive (both sides already lowered)
		true, // normalize (Unicode NFKD folding)
		true, // forward (left-to-right matching)
		&chars,
		pattern,
		true, // withPos (return character positions)
		slab,
	)

	if result.Score <= 0 {
		return FuzzyResult{}
	}

	var positionSlice []int
	if positions != nil {
		positionSlice = *positions
	}

	return FuzzyResult{
		Score:     result.Score,
		Positions: positionSlice,
	}
}
