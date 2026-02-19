// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// tooltipMaxWidth is the maximum width of the tooltip box in visible
// characters, including 1 character of padding on each side.
const tooltipMaxWidth = 50

// tooltipState holds the data for a visible hover tooltip.
type tooltipState struct {
	ticketID string
	content  schema.TicketContent

	// Screen coordinates where the tooltip box is anchored (top-left).
	anchorX int
	anchorY int

	// Screen coordinates of the hovered link target, used to apply
	// bold styling to the link text while the tooltip is visible.
	hoverScreenY  int
	hoverScreenX0 int // Inclusive start column.
	hoverScreenX1 int // Exclusive end column.
}

// renderTooltip produces a styled tooltip box for a ticket. Returns a
// slice of lines, each exactly the same visible width, with the
// theme's tooltip background applied. The solid background provides
// visual separation from the underlying content without needing
// box-drawing border characters.
//
// Layout:
//
//	STATUS  P{n}  ticket-id
//	title (truncated to fit)
//	description line 1...
//	description line 2...
func renderTooltip(ticketID string, content schema.TicketContent, theme Theme, maxWidth int) []string {
	if maxWidth < 10 {
		maxWidth = 10
	}

	// Inner width: maxWidth minus 2 characters of left/right padding.
	innerWidth := maxWidth - 2

	backgroundStyle := lipgloss.NewStyle().
		Background(theme.TooltipBackground)

	textStyle := lipgloss.NewStyle().
		Foreground(theme.TooltipForeground).
		Background(theme.TooltipBackground)

	// Line 1: status + priority + ticket ID.
	statusStyle := lipgloss.NewStyle().
		Foreground(theme.StatusColor(content.Status)).
		Background(theme.TooltipBackground).
		Bold(true)
	priorityStyle := lipgloss.NewStyle().
		Foreground(theme.PriorityColor(content.Priority)).
		Background(theme.TooltipBackground)

	statusText := strings.ToUpper(content.Status)
	priorityText := fmt.Sprintf("P%d", content.Priority)
	separator := backgroundStyle.Render("  ")
	metaContent := statusStyle.Render(statusText) + separator +
		priorityStyle.Render(priorityText) + separator +
		textStyle.Render(ticketID)

	var lines []string
	lines = append(lines, padTooltipLine(metaContent, innerWidth, maxWidth, backgroundStyle))

	// Line 2: title.
	titleText := content.Title
	if titleText != "" {
		titleStyle := lipgloss.NewStyle().
			Foreground(theme.TooltipForeground).
			Background(theme.TooltipBackground).
			Bold(true)
		if ansi.StringWidth(titleText) > innerWidth {
			titleText = ansi.Truncate(titleText, innerWidth-1, "…")
		}
		lines = append(lines, padTooltipLine(titleStyle.Render(titleText), innerWidth, maxWidth, backgroundStyle))
	}

	// Lines 3+: description excerpt (up to 2 lines).
	if content.Body != "" {
		excerpt := extractDescriptionExcerpt(content.Body, innerWidth, 2)
		for _, excerptLine := range excerpt {
			styled := textStyle.Render(excerptLine)
			lines = append(lines, padTooltipLine(styled, innerWidth, maxWidth, backgroundStyle))
		}
	}

	return lines
}

// padTooltipLine takes styled content for the inner area and pads it
// to the full tooltip width with background-colored spaces. Returns
// " content  " with background applied to the padding.
func padTooltipLine(styledContent string, innerWidth, totalWidth int, backgroundStyle lipgloss.Style) string {
	contentWidth := ansi.StringWidth(styledContent)
	rightPad := innerWidth - contentWidth
	if rightPad < 0 {
		rightPad = 0
	}
	return backgroundStyle.Render(" ") +
		styledContent +
		backgroundStyle.Render(strings.Repeat(" ", rightPad+1))
}

// extractDescriptionExcerpt returns the first maxLines lines of a
// description body, each truncated to maxWidth. Blank lines and
// leading whitespace-only lines are skipped.
func extractDescriptionExcerpt(body string, maxWidth, maxLines int) []string {
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

// overlayBold applies bold ANSI escapes to a specific character range
// on a specific line of the view. Used to highlight the hovered link
// target while a tooltip is visible.
//
// This uses the same ansi.DecodeSequence walker pattern as highlightLine
// (search.go) and styleAutolinks (autolink.go): walk each token in the
// line, pass ANSI escapes through unchanged, and inject bold-on/bold-off
// at the visible character boundaries.
//
// Because lipgloss wraps each styled segment with a full SGR reset
// (\x1b[0m), a single bold-on at the range start would be killed by
// the first reset within the range. To maintain bold across multiple
// styled segments, we re-assert \x1b[1m after every ANSI escape
// sequence encountered while inside the bold range.
func overlayBold(view string, screenY, startX, endX int) string {
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

// overlayTooltip splices a tooltip box onto a rendered view string.
// The tooltip lines replace the characters at the anchor position in
// the view. Uses ANSI-aware truncation and slicing so escape
// sequences in the original view are preserved on both sides of the
// overlay.
func overlayTooltip(view string, tooltipLines []string, anchorX, anchorY int) string {
	if len(tooltipLines) == 0 {
		return view
	}

	viewLines := strings.Split(view, "\n")
	tooltipWidth := ansi.StringWidth(tooltipLines[0])

	for index, tooltipLine := range tooltipLines {
		viewLineIndex := anchorY + index
		if viewLineIndex < 0 || viewLineIndex >= len(viewLines) {
			continue
		}

		viewLine := viewLines[viewLineIndex]
		viewLineWidth := ansi.StringWidth(viewLine)

		// Build: prefix + reset + tooltip + reset + suffix.
		var result strings.Builder

		// Prefix: everything before the tooltip anchor.
		if anchorX > 0 {
			prefix := ansi.Truncate(viewLine, anchorX, "")
			result.WriteString(prefix)
		}
		result.WriteString("\x1b[0m")
		result.WriteString(tooltipLine)
		result.WriteString("\x1b[0m")

		// Suffix: everything after the tooltip region.
		suffixStart := anchorX + tooltipWidth
		if suffixStart < viewLineWidth {
			suffix := ansi.TruncateLeft(viewLine, suffixStart, "")
			result.WriteString(suffix)
		}

		viewLines[viewLineIndex] = result.String()
	}

	return strings.Join(viewLines, "\n")
}
