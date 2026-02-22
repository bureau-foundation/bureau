// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	ticketschema "github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/tui"
)

// tooltipMaxWidth is the maximum width of the tooltip box in visible
// characters, including 1 character of padding on each side.
const tooltipMaxWidth = 50

// tooltipState holds the data for a visible hover tooltip.
type tooltipState struct {
	ticketID string
	content  ticketschema.TicketContent

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
func renderTooltip(ticketID string, content ticketschema.TicketContent, theme Theme, maxWidth int) []string {
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
	lines = append(lines, tui.PadOverlayLine(metaContent, innerWidth, maxWidth, backgroundStyle))

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
		lines = append(lines, tui.PadOverlayLine(titleStyle.Render(titleText), innerWidth, maxWidth, backgroundStyle))
	}

	// Lines 3+: description excerpt (up to 2 lines).
	if content.Body != "" {
		excerpt := tui.ExtractExcerpt(content.Body, innerWidth, 2)
		for _, excerptLine := range excerpt {
			styled := textStyle.Render(excerptLine)
			lines = append(lines, tui.PadOverlayLine(styled, innerWidth, maxWidth, backgroundStyle))
		}
	}

	return lines
}

// extractDescriptionExcerpt delegates to the shared TUI library.
func extractDescriptionExcerpt(body string, maxWidth, maxLines int) []string {
	return tui.ExtractExcerpt(body, maxWidth, maxLines)
}

// overlayBold delegates to the shared TUI library.
func overlayBold(view string, screenY, startX, endX int) string {
	return tui.OverlayBold(view, screenY, startX, endX)
}

// overlayTooltip delegates to the shared TUI library.
func overlayTooltip(view string, tooltipLines []string, anchorX, anchorY int) string {
	return tui.SpliceOverlay(view, tooltipLines, anchorX, anchorY)
}
