// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// SearchModel manages the detail pane's in-body text search state.
// Follows the same input-handling pattern as FilterModel: the caller
// routes keystrokes to HandleRune/HandleBackspace and reads the
// results via accessor methods.
type SearchModel struct {
	// Input is the current search query text.
	Input string

	// Active is true when the search bar has keyboard focus
	// (the user pressed / in detail mode to start typing).
	Active bool

	matches []searchMatch
	current int // Index of the currently highlighted match.
}

// searchMatch identifies a single occurrence of the search query
// in the body content.
type searchMatch struct {
	Line   int // 0-based line in the body content.
	Column int // Visible character column within the line.
}

// HandleRune appends a character to the search input. Returns true
// if the input changed.
func (search *SearchModel) HandleRune(character rune) bool {
	search.Input += string(character)
	return true
}

// HandleBackspace removes the last character from the search input.
// Returns true if the input changed.
func (search *SearchModel) HandleBackspace() bool {
	if len(search.Input) == 0 {
		return false
	}
	runes := []rune(search.Input)
	search.Input = string(runes[:len(runes)-1])
	return true
}

// Clear resets the search state: clears the query, deactivates the
// input, and removes all match data.
func (search *SearchModel) Clear() {
	search.Input = ""
	search.Active = false
	search.matches = nil
	search.current = 0
}

// SetMatches replaces the match list (called after highlighting
// recomputes matches). Clamps the current match index to the
// new list bounds.
func (search *SearchModel) SetMatches(matches []searchMatch) {
	search.matches = matches
	if search.current >= len(matches) {
		if len(matches) > 0 {
			search.current = len(matches) - 1
		} else {
			search.current = 0
		}
	}
}

// MatchCount returns the total number of matches.
func (search *SearchModel) MatchCount() int {
	return len(search.matches)
}

// CurrentIndex returns the 0-based index of the current match.
func (search *SearchModel) CurrentIndex() int {
	return search.current
}

// CurrentMatch returns the current match, or nil if no matches exist.
func (search *SearchModel) CurrentMatch() *searchMatch {
	if len(search.matches) == 0 {
		return nil
	}
	return &search.matches[search.current]
}

// NextMatch advances to the next match, wrapping around at the end.
func (search *SearchModel) NextMatch() {
	if len(search.matches) == 0 {
		return
	}
	search.current = (search.current + 1) % len(search.matches)
}

// PreviousMatch moves to the previous match, wrapping to the end.
func (search *SearchModel) PreviousMatch() {
	if len(search.matches) == 0 {
		return
	}
	search.current = (search.current - 1 + len(search.matches)) % len(search.matches)
}

// View renders the search bar. When active, shows " / query▎".
// When inactive with a query, shows " search: query (N/M)".
// When inactive with no query, returns empty string.
func (search *SearchModel) View(theme Theme, width int) string {
	if !search.Active && search.Input == "" {
		return ""
	}

	if search.Active {
		style := lipgloss.NewStyle().
			Foreground(theme.NormalText).
			Width(width)
		cursor := lipgloss.NewStyle().
			Foreground(theme.HeaderForeground).
			Bold(true).
			Render("▎")
		return style.Render(" / " + search.Input + cursor)
	}

	// Inactive with text — show match count.
	style := lipgloss.NewStyle().
		Foreground(theme.FaintText).
		Width(width)

	matchInfo := ""
	if len(search.matches) > 0 {
		matchInfo = lipgloss.NewStyle().
			Foreground(theme.NormalText).
			Render(strings.Repeat(" ", 1) +
				lipgloss.NewStyle().
					Foreground(theme.FaintText).
					Render("(") +
				lipgloss.NewStyle().
					Foreground(theme.HeaderForeground).
					Render(intToString(search.current+1)) +
				lipgloss.NewStyle().
					Foreground(theme.FaintText).
					Render("/"+intToString(len(search.matches))+")"))
	} else if search.Input != "" {
		matchInfo = lipgloss.NewStyle().
			Foreground(theme.FaintText).
			Render(" (no matches)")
	}

	return style.Render(" search: " + search.Input + matchInfo)
}

// intToString converts an int to its string representation. Avoids
// importing strconv for a single use.
func intToString(value int) string {
	if value == 0 {
		return "0"
	}

	negative := value < 0
	if negative {
		value = -value
	}

	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

// highlightSearchMatches applies search highlighting to an
// ANSI-decorated body string. For each case-insensitive occurrence
// of query in the visible text, wraps the corresponding bytes in
// highlight escape sequences. The highlight preserves existing
// foreground colors and formatting — only the background is changed.
//
// Returns the highlighted body and a list of match positions. The
// currentMatchIndex identifies which match gets a distinct "current"
// highlight; pass -1 to skip current highlighting.
func highlightSearchMatches(body string, query string, currentMatchIndex int, theme Theme) (string, []searchMatch) {
	if query == "" {
		return body, nil
	}

	queryRunes := []rune(strings.ToLower(query))
	queryRuneLength := len(queryRunes)

	lines := strings.Split(body, "\n")
	var matches []searchMatch
	var resultLines []string

	// ANSI escape sequences for background highlighting. Using raw
	// escapes rather than lipgloss because we're splicing into
	// existing ANSI-decorated text and need precise control.
	highlightOn := ansiBackground(theme.SearchHighlightBackground)
	currentOn := ansiBackground(theme.SearchCurrentBackground)
	backgroundOff := "\x1b[49m"

	matchIndex := 0
	for lineNumber, line := range lines {
		visibleText := ansi.Strip(line)
		visibleRunes := []rune(strings.ToLower(visibleText))

		// Find all occurrences of the query in this line's visible
		// text, working in rune positions so multi-byte characters
		// (e.g., status icons "●", "✓") are counted correctly.
		var lineMatches []visibleMatch
		searchFrom := 0
		for {
			index := runeSliceIndex(visibleRunes[searchFrom:], queryRunes)
			if index < 0 {
				break
			}
			startColumn := searchFrom + index
			lineMatches = append(lineMatches, visibleMatch{
				startColumn: startColumn,
				endColumn:   startColumn + queryRuneLength,
				matchIndex:  matchIndex,
			})
			matches = append(matches, searchMatch{
				Line:   lineNumber,
				Column: startColumn,
			})
			matchIndex++
			searchFrom = startColumn + queryRuneLength
		}

		if len(lineMatches) == 0 {
			resultLines = append(resultLines, line)
			continue
		}

		// Rebuild this line with highlight escapes spliced in at the
		// correct byte positions. Walk the raw line with
		// DecodeSequence to map visible character indices to byte
		// positions in the ANSI-decorated string.
		highlighted := highlightLine(line, lineMatches, currentMatchIndex, highlightOn, currentOn, backgroundOff)
		resultLines = append(resultLines, highlighted)
	}

	return strings.Join(resultLines, "\n"), matches
}

// visibleMatch tracks a match within a single line by rune position
// range. Rune positions correspond to indices in the ANSI-stripped
// visible text converted to []rune, which is the same unit used by
// runeSliceIndex when finding matches.
type visibleMatch struct {
	startColumn int // Inclusive rune position.
	endColumn   int // Exclusive rune position.
	matchIndex  int // Global match index (for current-match highlighting).
}

// highlightLine splices highlight ANSI sequences into a single line
// at the positions corresponding to the given rune-position match
// ranges. Walks the ANSI-decorated line with DecodeSequence and
// tracks rune position (not byte offset or cell width) so
// multi-byte characters like status icons are counted correctly.
func highlightLine(line string, lineMatches []visibleMatch, currentMatchIndex int, highlightOn, currentOn, backgroundOff string) string {
	var result strings.Builder
	result.Grow(len(line) + len(lineMatches)*20)

	runePosition := 0 // Current rune position in the visible text.
	matchPointer := 0 // Index into lineMatches.
	inHighlight := false

	var state byte
	remaining := line
	for len(remaining) > 0 {
		sequence, width, byteCount, newState := ansi.DecodeSequence(remaining, state, nil)
		state = newState

		if width == 0 {
			// Control sequence or escape — pass through unchanged.
			result.WriteString(sequence)
			remaining = remaining[byteCount:]
			continue
		}

		// This is a visible character (width > 0). Count the runes
		// it contributes to match against our rune-based positions.
		sequenceRuneCount := utf8.RuneCountInString(sequence)

		// End highlight if we've reached or passed the current
		// match's end column.
		if inHighlight && matchPointer < len(lineMatches) && runePosition >= lineMatches[matchPointer].endColumn {
			result.WriteString(backgroundOff)
			inHighlight = false
			matchPointer++
		}

		// Start highlight if we've reached the next match's start.
		if !inHighlight && matchPointer < len(lineMatches) && runePosition >= lineMatches[matchPointer].startColumn {
			if lineMatches[matchPointer].matchIndex == currentMatchIndex {
				result.WriteString(currentOn)
			} else {
				result.WriteString(highlightOn)
			}
			inHighlight = true
		}

		result.WriteString(sequence)
		runePosition += sequenceRuneCount
		remaining = remaining[byteCount:]
	}

	// Close any unclosed highlight at end of line.
	if inHighlight {
		result.WriteString(backgroundOff)
	}

	return result.String()
}

// runeSliceIndex returns the index of the first occurrence of needle
// in haystack, or -1 if not found. Both slices are compared
// element-wise. This is the rune-level equivalent of strings.Index.
func runeSliceIndex(haystack, needle []rune) int {
	needleLength := len(needle)
	if needleLength == 0 {
		return 0
	}
	limit := len(haystack) - needleLength
	for index := 0; index <= limit; index++ {
		match := true
		for offset := 0; offset < needleLength; offset++ {
			if haystack[index+offset] != needle[offset] {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}

// ansiBackground returns the ANSI escape sequence to set the
// background to a 256-color palette index. The lipgloss.Color is
// expected to be a numeric string (e.g., "58") — this function
// produces the raw "\x1b[48;5;Nm" escape.
func ansiBackground(color lipgloss.Color) string {
	return "\x1b[48;5;" + string(color) + "m"
}
