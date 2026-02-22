// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// SpliceOverlay replaces a rectangular region of a rendered view with
// overlay content. The overlay lines are placed starting at (anchorX,
// anchorY) in screen coordinates. Uses ANSI-aware truncation so escape
// sequences in the original view are preserved on both sides of the
// overlay.
func SpliceOverlay(view string, overlayLines []string, anchorX, anchorY int) string {
	if len(overlayLines) == 0 {
		return view
	}

	viewLines := strings.Split(view, "\n")
	overlayWidth := ansi.StringWidth(overlayLines[0])

	for index, overlayLine := range overlayLines {
		viewLineIndex := anchorY + index
		if viewLineIndex < 0 || viewLineIndex >= len(viewLines) {
			continue
		}

		viewLine := viewLines[viewLineIndex]
		viewLineWidth := ansi.StringWidth(viewLine)

		// Build: prefix + reset + overlay + reset + suffix.
		var result strings.Builder

		// Prefix: everything before the overlay anchor.
		if anchorX > 0 {
			prefix := ansi.Truncate(viewLine, anchorX, "")
			result.WriteString(prefix)
		}
		result.WriteString("\x1b[0m")
		result.WriteString(overlayLine)
		result.WriteString("\x1b[0m")

		// Suffix: everything after the overlay region.
		suffixStart := anchorX + overlayWidth
		if suffixStart < viewLineWidth {
			suffix := ansi.TruncateLeft(viewLine, suffixStart, "")
			result.WriteString(suffix)
		}

		viewLines[viewLineIndex] = result.String()
	}

	return strings.Join(viewLines, "\n")
}

// OverlayBold applies bold ANSI escapes to a specific character range
// on a specific line of the view. Used to highlight a hovered link
// target while an overlay is visible.
//
// Because lipgloss wraps each styled segment with a full SGR reset
// (\x1b[0m), a single bold-on at the range start would be killed by
// the first reset within the range. To maintain bold across multiple
// styled segments, we re-assert \x1b[1m after every ANSI escape
// sequence encountered while inside the bold range.
func OverlayBold(view string, screenY, startX, endX int) string {
	if startX >= endX {
		return view
	}

	viewLines := strings.Split(view, "\n")
	if screenY < 0 || screenY >= len(viewLines) {
		return view
	}

	line := viewLines[screenY]
	lineWidth := ansi.StringWidth(line)
	if startX >= lineWidth {
		return view
	}

	var result strings.Builder
	result.Grow(len(line) + 40)

	columnPosition := 0
	inBold := false

	var state byte
	remaining := line
	for len(remaining) > 0 {
		sequence, displayWidth, byteCount, newState := ansi.DecodeSequence(remaining, state, nil)
		state = newState

		if displayWidth == 0 {
			// Control sequence or escape — pass through unchanged.
			result.WriteString(sequence)
			// Re-assert bold after every escape within the bold
			// range. Any of these escapes could be a full SGR
			// reset (\x1b[0m) that kills the bold attribute.
			if inBold {
				result.WriteString("\x1b[1m")
			}
			remaining = remaining[byteCount:]
			continue
		}

		// End bold if we've reached the end column.
		if inBold && columnPosition >= endX {
			result.WriteString("\x1b[22m")
			inBold = false
		}

		// Start bold if we've reached the start column.
		if !inBold && columnPosition >= startX && columnPosition < endX {
			result.WriteString("\x1b[1m")
			inBold = true
		}

		result.WriteString(sequence)
		columnPosition += displayWidth
		remaining = remaining[byteCount:]
	}

	// Close bold if it was still open at end of line.
	if inBold {
		result.WriteString("\x1b[22m")
	}

	viewLines[screenY] = result.String()
	return strings.Join(viewLines, "\n")
}

// PadOverlayLine takes styled content for the inner area and pads it
// to the full width with background-colored spaces. Returns
// " content  " with background applied to the padding.
func PadOverlayLine(styledContent string, innerWidth, totalWidth int, backgroundStyle lipgloss.Style) string {
	contentWidth := ansi.StringWidth(styledContent)
	rightPad := innerWidth - contentWidth
	if rightPad < 0 {
		rightPad = 0
	}
	return backgroundStyle.Render(" ") +
		styledContent +
		backgroundStyle.Render(strings.Repeat(" ", rightPad+1))
}

// ExtractExcerpt returns the first maxLines non-blank lines of a
// body text, each truncated to maxWidth. Blank lines and leading
// whitespace-only lines are skipped.
func ExtractExcerpt(body string, maxWidth, maxLines int) []string {
	bodyLines := strings.Split(body, "\n")
	var result []string
	for _, line := range bodyLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if ansi.StringWidth(trimmed) > maxWidth {
			trimmed = ansi.Truncate(trimmed, maxWidth-1, "…")
		}
		result = append(result, trimmed)
		if len(result) >= maxLines {
			break
		}
	}
	return result
}
