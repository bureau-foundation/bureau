// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/tui"
)

// Re-export search types from the shared TUI library so that
// existing code within this package can refer to them unqualified.

// SearchModel manages in-body text search state for a scrollable
// content pane.
type SearchModel = tui.SearchModel

// searchMatch identifies a single occurrence of the search query
// in body content.
type searchMatch = tui.SearchMatch

// visibleMatch tracks a match within a single line by rune position.
type visibleMatch = tui.VisibleMatch

// highlightSearchMatches delegates to the shared TUI library.
func highlightSearchMatches(body string, query string, currentMatchIndex int, theme Theme) (string, []searchMatch) {
	return tui.HighlightSearchMatches(body, query, currentMatchIndex, theme)
}

// highlightLine delegates to the shared TUI library.
func highlightLine(line string, lineMatches []visibleMatch, currentMatchIndex int, highlightOn, currentOn, backgroundOff string) string {
	return tui.HighlightLine(line, lineMatches, currentMatchIndex, highlightOn, currentOn, backgroundOff)
}

// runeSliceIndex delegates to the shared TUI library.
func runeSliceIndex(haystack, needle []rune) int {
	return tui.RuneSliceIndex(haystack, needle)
}

// ansiBackground delegates to the shared TUI library.
func ansiBackground(color lipgloss.Color) string {
	return tui.ANSIBackground(color)
}

// intToString delegates to the shared TUI library.
func intToString(value int) string {
	return tui.IntToString(value)
}
