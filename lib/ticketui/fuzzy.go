// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"github.com/junegunn/fzf/src/util"

	"github.com/bureau-foundation/bureau/lib/tui"
)

// FuzzyResult re-exports the shared fuzzy match result type.
type FuzzyResult = tui.FuzzyResult

// fuzzyMatch delegates to the shared TUI library's fuzzy matcher.
func fuzzyMatch(text string, pattern []rune, slab *util.Slab) FuzzyResult {
	return tui.FuzzyMatch(text, pattern, slab)
}
