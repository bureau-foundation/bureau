// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/ticket"
)

// Column widths for the list table. The title column fills remaining
// space; all others are fixed.
const (
	columnWidthID = 11 // short ID + space (e.g. "tkt-xxxxx ")

	// maxLeftWidth is the worst-case width of the left portion before
	// the ID column: 1 (indent) + 4 (emoji + "P1") + 3 (" ● ") = 8.
	// Open tickets have no status icon so their left portion is only
	// 5 (indent + emoji + priority), but title truncation uses worst
	// case for consistency across rows.
	maxLeftWidth = 8
)

// typeIcon returns a double-width emoji representing the ticket type.
// Each icon is visually distinct so the type is recognizable at a
// glance without reading text.
func typeIcon(ticketType string) string {
	switch ticketType {
	case "bug":
		return "🐛"
	case "task":
		return "📋"
	case "feature":
		return "✨"
	case "epic":
		return "🎯"
	case "chore":
		return "🔧"
	case "docs":
		return "📝"
	case "question":
		return "❓"
	default:
		return "  " // 2 spaces to match emoji width
	}
}

// ListRenderer handles the table-style rendering of ticket entries
// within a given width. Includes both flat ticket rows and grouped
// epic headers for the ready view.
type ListRenderer struct {
	theme Theme
	width int
}

// NewListRenderer creates a ListRenderer for the given width.
func NewListRenderer(theme Theme, width int) ListRenderer {
	return ListRenderer{theme: theme, width: width}
}

// unblockIndicatorWidth is the fixed width reserved for the unblock
// count suffix when a ticket has UnblockCount > 0. Format: " ↑NN".
const unblockIndicatorWidth = 4

// RenderRow renders a single ticket entry as a formatted table row.
// The selected flag controls whether the row gets highlight styling.
// The optional score parameter adds leverage and urgency indicators
// when non-nil (used on the Ready tab; nil on Blocked/All tabs).
// The matchPositions parameter contains rune indices in the title
// that matched the current fuzzy filter query; when non-nil, those
// characters are highlighted with the search highlight background.
//
// Row layout: indent + emoji + priority [+ " " + status icon] + " " + ID + title [+ " ↑N"]
//
// When the ticket has an active status (in_progress, blocked, closed),
// the icon appears between the priority and ID with surrounding spaces.
// Open tickets omit the icon entirely, keeping the gap tight:
//
//	🐛P0 ● tkt-3vk5  Fix connection pooling leak [infra]
//	📋P2 tkt-2ccg    Implement retry backoff [transport] ↑3
func (renderer ListRenderer) RenderRow(entry ticket.Entry, selected bool, score *ticket.TicketScore, matchPositions []int) string {
	// Title width uses worst-case left portion (with status icon)
	// so truncation is consistent across rows.
	titleWidth := renderer.width - maxLeftWidth - columnWidthID
	if titleWidth < 10 {
		titleWidth = 10
	}

	// Reserve space for the unblock indicator when present.
	hasUnblock := score != nil && score.UnblockCount > 0
	if hasUnblock {
		titleWidth -= unblockIndicatorWidth
	}

	// Build the title portion with optional labels suffix.
	titleText := entry.Content.Title
	labelsText := ""
	if len(entry.Content.Labels) > 0 {
		labelsText = " [" + strings.Join(entry.Content.Labels, ", ") + "]"
	}

	// Truncate title+labels to fit the available width.
	combined := titleText + labelsText
	if lipgloss.Width(combined) > titleWidth {
		// Truncate to fit, preferring to show the title over labels.
		if lipgloss.Width(titleText) >= titleWidth-1 {
			combined = truncateString(titleText, titleWidth-1) + "…"
		} else {
			combined = titleText + truncateString(labelsText, titleWidth-lipgloss.Width(titleText)-1) + "…"
		}
	}

	// Append unblock indicator after the title+labels.
	if hasUnblock {
		combined += renderer.renderUnblockIndicator(score.UnblockCount, selected)
	}

	// Determine the display priority color. When the ticket has a
	// borrowed priority more urgent (lower number) than its own,
	// use the borrowed priority's color to visually escalate it.
	displayPriorityColor := entry.Content.Priority
	if score != nil && score.BorrowedPriority >= 0 && score.BorrowedPriority < entry.Content.Priority {
		displayPriorityColor = score.BorrowedPriority
	}

	if selected {
		return renderer.renderSelectedRow(entry, combined, matchPositions)
	}
	return renderer.renderNormalRow(entry, combined, displayPriorityColor, matchPositions)
}

// renderUnblockIndicator returns a styled " ↑N" suffix for the title
// showing how many tickets this one unblocks. The color varies by
// count to draw attention proportionally: 1-2 subtle, 3-5 warm, 6+ hot.
func (renderer ListRenderer) renderUnblockIndicator(count int, selected bool) string {
	if selected {
		// Selected rows use uniform foreground; indicator rendered
		// in the same style as the rest of the row.
		return fmt.Sprintf(" ↑%d", count)
	}
	color := renderer.theme.FaintText
	switch {
	case count >= 6:
		color = renderer.theme.PriorityColor(0) // critical color
	case count >= 3:
		color = renderer.theme.PriorityColor(1) // high color
	}
	return " " + lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("↑%d", count))
}

// renderNormalRow renders a row with per-component foreground colors.
// No background color (default terminal background).
// displayPriorityColor controls the color of the priority badge; it
// may differ from the ticket's own priority when borrowed priority
// escalates the visual urgency. matchPositions contains rune indices
// in the original title that should be highlighted.
func (renderer ListRenderer) renderNormalRow(entry ticket.Entry, titleLabels string, displayPriorityColor int, matchPositions []int) string {
	priority := entry.Content.Priority
	status := entry.Content.Status

	typePriorityStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.PriorityColor(displayPriorityColor)).
		Bold(displayPriorityColor <= 1)

	idStyle := lipgloss.NewStyle().
		Width(columnWidthID).
		Foreground(renderer.theme.StatusColor(status))

	titleStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText)

	typePriorityText := typeIcon(entry.Content.Type) + fmt.Sprintf("P%d", priority)

	// Status icon: always 2 chars wide (" ●" or "  ") so the ID
	// column aligns regardless of status. On the ready tab most
	// tickets are open (blank icon), but All/Blocked tabs mix
	// statuses and need consistent alignment.
	icon := statusIconString(status)
	var statusPart string
	if icon != "" {
		statusPart = " " + lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(status)).
			Render(icon)
	} else {
		statusPart = "  "
	}

	var titleRendered string
	if len(matchPositions) > 0 {
		highlightStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.NormalText).
			Background(renderer.theme.SearchHighlightBackground)
		titleRendered = highlightTitle(titleLabels, entry.Content.Title, matchPositions, titleStyle, highlightStyle)
	} else {
		titleRendered = titleStyle.Render(titleLabels)
	}

	row := " " +
		typePriorityStyle.Render(typePriorityText) +
		statusPart +
		" " +
		idStyle.Render(entry.ID) +
		titleRendered

	return lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// renderSelectedRow renders the selected row with a highlight
// background. All text uses the selected foreground color.
// matchPositions contains rune indices in the original title that
// should be highlighted (with bold+underline on the selection bg).
func (renderer ListRenderer) renderSelectedRow(entry ticket.Entry, titleLabels string, matchPositions []int) string {
	baseStyle := lipgloss.NewStyle().
		Background(renderer.theme.SelectedBackground).
		Foreground(renderer.theme.SelectedForeground)

	typePriorityText := typeIcon(entry.Content.Type) + fmt.Sprintf("P%d", entry.Content.Priority)

	icon := statusIconString(entry.Content.Status)
	var statusPart string
	if icon != "" {
		statusPart = " " + baseStyle.Render(icon)
	} else {
		statusPart = "  "
	}

	var titleRendered string
	if len(matchPositions) > 0 {
		// On a selected row the background is already the selection
		// color, so a different background tint would be subtle.
		// Use bold+underline to make matches pop against the
		// selection highlight.
		highlightStyle := baseStyle.Bold(true).Underline(true)
		titleRendered = highlightTitle(titleLabels, entry.Content.Title, matchPositions, baseStyle, highlightStyle)
	} else {
		titleRendered = baseStyle.Render(titleLabels)
	}

	row := " " +
		baseStyle.Bold(true).Render(typePriorityText) +
		statusPart +
		" " +
		baseStyle.Width(columnWidthID).Bold(false).Render(entry.ID) +
		titleRendered

	return baseStyle.Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// RenderGroupHeader renders an epic group header row. The header shows
// a collapse/expand indicator (▼/▶), the epic title, and a progress
// count like "(3/6)".
func (renderer ListRenderer) RenderGroupHeader(item ListItem, width int, selected bool) string {
	color := renderer.theme.HeaderForeground
	indicator := "▼"
	if item.Collapsed {
		color = renderer.theme.FaintText
		indicator = "▶"
	}

	headerStyle := lipgloss.NewStyle().
		Foreground(color).
		Bold(true).
		Width(width).
		MaxWidth(width)

	if selected {
		headerStyle = headerStyle.
			Background(renderer.theme.SelectedBackground).
			Foreground(renderer.theme.SelectedForeground)
	}

	progress := ""
	if item.EpicTotal > 0 {
		progress = fmt.Sprintf("  (%d/%d)", item.EpicDone, item.EpicTotal)
		// Append parallelism width and critical depth from epic health
		// when available and non-trivial.
		if item.EpicHealth != nil {
			if item.EpicHealth.ReadyChildren > 0 {
				progress += fmt.Sprintf(" rdy:%d", item.EpicHealth.ReadyChildren)
			}
			if item.EpicHealth.CriticalDepth > 0 {
				progress += fmt.Sprintf(" dep:%d", item.EpicHealth.CriticalDepth)
			}
		}
	}

	// Truncate the title so the row never wraps. Reserve space for
	// the prefix (" ▼ ") and the progress suffix.
	prefix := " " + indicator + " "
	prefixWidth := lipgloss.Width(prefix)
	progressWidth := lipgloss.Width(progress)
	availableForTitle := width - prefixWidth - progressWidth
	title := item.EpicTitle
	if availableForTitle > 0 && lipgloss.Width(title) > availableForTitle {
		title = truncateString(title, availableForTitle-1) + "…"
	}

	return headerStyle.Render(prefix + title + progress)
}

// highlightTitle renders a title+labels string with character-level
// highlighting at the given rune positions. Positions index into the
// original title text (before labels were appended). Characters at
// matched positions use highlightStyle; all others use baseStyle.
// Consecutive runs of same-style characters are batched into a single
// Render call to keep ANSI output compact.
func highlightTitle(titleLabels string, originalTitle string, positions []int, baseStyle, highlightStyle lipgloss.Style) string {
	if len(positions) == 0 {
		return baseStyle.Render(titleLabels)
	}

	// Build a set of matched rune indices for O(1) lookup.
	positionSet := make(map[int]bool, len(positions))
	for _, position := range positions {
		positionSet[position] = true
	}

	titleRunes := []rune(originalTitle)
	titleLabelsRunes := []rune(titleLabels)
	titleLength := len(titleRunes)

	var result strings.Builder
	runStart := 0
	isHighlighted := runStart < titleLength && positionSet[0]

	for index := 1; index <= len(titleLabelsRunes); index++ {
		// Characters past the title length (i.e., the labels suffix)
		// are never highlighted.
		currentHighlighted := index < titleLength && positionSet[index]
		if currentHighlighted != isHighlighted || index == len(titleLabelsRunes) {
			chunk := string(titleLabelsRunes[runStart:index])
			if isHighlighted {
				result.WriteString(highlightStyle.Render(chunk))
			} else {
				result.WriteString(baseStyle.Render(chunk))
			}
			runStart = index
			isHighlighted = currentHighlighted
		}
	}

	return result.String()
}

// truncateString truncates a string to maxWidth visual characters.
// Handles multi-byte characters correctly via lipgloss width
// measurement.
func truncateString(text string, maxWidth int) string {
	if lipgloss.Width(text) <= maxWidth {
		return text
	}
	// Truncate by runes until we fit.
	runes := []rune(text)
	for length := len(runes) - 1; length >= 0; length-- {
		candidate := string(runes[:length])
		if lipgloss.Width(candidate) <= maxWidth {
			return candidate
		}
	}
	return ""
}
