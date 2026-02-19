// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// autolinkPattern matches candidate ticket IDs in visible text. The
// pattern requires a letter, optional word characters, a dash, then
// more word characters — catching patterns like "tkt-001", "epic-1",
// "tkt-2abc". The Source.Get check filters out false positives.
var autolinkPattern = regexp.MustCompile(`[a-zA-Z]\w*-\w+`)

// detectAutolinks scans the rendered body for ticket ID references
// and returns the body with link styling applied plus additional
// click targets for the detected references. existingTargets are
// checked to avoid double-linking IDs already present as graph or
// dependency entries.
func detectAutolinks(body string, existingTargets []BodyClickTarget, source Source, theme Theme) (string, []BodyClickTarget) {
	if body == "" {
		return body, nil
	}

	lines := strings.Split(body, "\n")
	var additionalTargets []BodyClickTarget
	var resultLines []string

	// ANSI escapes for link styling: foreground color + underline.
	linkOn := ansiForeground(theme.LinkForeground) + "\x1b[4m"
	linkOff := "\x1b[24m\x1b[39m" // underline off + default foreground

	for lineNumber, line := range lines {
		visibleText := ansi.Strip(line)

		// Find all candidate ticket IDs in this line's visible text.
		matches := autolinkPattern.FindAllStringIndex(visibleText, -1)
		if len(matches) == 0 {
			resultLines = append(resultLines, line)
			continue
		}

		// Convert byte indices to rune indices for ANSI-aware splicing.
		visibleRunes := []rune(visibleText)
		var lineAutolinks []autolinkMatch

		for _, match := range matches {
			// Convert byte offsets to rune offsets.
			startRune := utf8.RuneCountInString(visibleText[:match[0]])
			endRune := utf8.RuneCountInString(visibleText[:match[1]])
			candidate := string(visibleRunes[startRune:endRune])

			// Verify the candidate is an actual ticket in the source.
			if _, exists := source.Get(candidate); !exists {
				continue
			}

			// Skip if this position overlaps with an existing click target.
			if overlapsExistingTarget(lineNumber, startRune, endRune, existingTargets) {
				continue
			}

			lineAutolinks = append(lineAutolinks, autolinkMatch{
				ticketID:    candidate,
				startColumn: startRune,
				endColumn:   endRune,
			})

			additionalTargets = append(additionalTargets, BodyClickTarget{
				Line:     lineNumber,
				TicketID: candidate,
				StartX:   startRune,
				EndX:     endRune,
			})
		}

		if len(lineAutolinks) == 0 {
			resultLines = append(resultLines, line)
			continue
		}

		// Apply link styling via ANSI splicing — same technique as
		// highlightLine in search.go.
		styled := styleAutolinks(line, lineAutolinks, linkOn, linkOff)
		resultLines = append(resultLines, styled)
	}

	return strings.Join(resultLines, "\n"), additionalTargets
}

// autolinkMatch tracks a detected autolink within a single line.
type autolinkMatch struct {
	ticketID    string
	startColumn int // Inclusive rune position in visible text.
	endColumn   int // Exclusive rune position in visible text.
}

// overlapsExistingTarget returns true if the given rune range on
// lineNumber falls within any existing click target.
func overlapsExistingTarget(lineNumber, startRune, endRune int, targets []BodyClickTarget) bool {
	for _, target := range targets {
		if target.Line != lineNumber {
			continue
		}
		// Overlap: the ranges [startRune, endRune) and
		// [target.StartX, target.EndX) intersect.
		if startRune < target.EndX && endRune > target.StartX {
			return true
		}
	}
	return false
}

// styleAutolinks splices link ANSI escapes into a line at the rune
// positions of detected autolinks. Walks the ANSI-decorated line
// with DecodeSequence, tracking rune position to insert escape
// sequences at the correct byte boundaries.
func styleAutolinks(line string, autolinks []autolinkMatch, linkOn, linkOff string) string {
	var result strings.Builder
	result.Grow(len(line) + len(autolinks)*30)

	runePosition := 0
	matchPointer := 0
	inLink := false

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

		sequenceRuneCount := utf8.RuneCountInString(sequence)

		// End link styling if we've reached the current match's end.
		if inLink && matchPointer < len(autolinks) && runePosition >= autolinks[matchPointer].endColumn {
			result.WriteString(linkOff)
			inLink = false
			matchPointer++
		}

		// Start link styling if we've reached the next match's start.
		if !inLink && matchPointer < len(autolinks) && runePosition >= autolinks[matchPointer].startColumn {
			result.WriteString(linkOn)
			inLink = true
		}

		result.WriteString(sequence)
		runePosition += sequenceRuneCount
		remaining = remaining[byteCount:]
	}

	// Close any unclosed link at end of line.
	if inLink {
		result.WriteString(linkOff)
	}

	return result.String()
}

// ansiForeground returns the ANSI escape sequence to set the
// foreground to a 256-color palette index.
func ansiForeground(color lipgloss.Color) string {
	return "\x1b[38;5;" + string(color) + "m"
}
