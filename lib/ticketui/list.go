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
	columnWidthID = 11 // "bd-xxxxx" + space (typical beads ID)

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

// RenderRow renders a single ticket entry as a formatted table row.
// The selected flag controls whether the row gets highlight styling.
//
// Row layout: indent + emoji + priority [+ " " + status icon] + " " + ID + title
//
// When the ticket has an active status (in_progress, blocked, closed),
// the icon appears between the priority and ID with surrounding spaces.
// Open tickets omit the icon entirely, keeping the gap tight:
//
//	🐛P0 ● bd-3vk5  Fix connection pooling leak [infra]
//	📋P2 bd-2ccg    Implement retry backoff [transport]
func (renderer ListRenderer) RenderRow(entry ticket.Entry, selected bool) string {
	// Title width uses worst-case left portion (with status icon)
	// so truncation is consistent across rows.
	titleWidth := renderer.width - maxLeftWidth - columnWidthID
	if titleWidth < 10 {
		titleWidth = 10
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

	if selected {
		return renderer.renderSelectedRow(entry, combined)
	}
	return renderer.renderNormalRow(entry, combined)
}

// renderNormalRow renders a row with per-component foreground colors.
// No background color (default terminal background).
func (renderer ListRenderer) renderNormalRow(entry ticket.Entry, titleLabels string) string {
	priority := entry.Content.Priority
	status := entry.Content.Status

	typePriorityStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.PriorityColor(priority)).
		Bold(priority <= 1)

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

	row := " " +
		typePriorityStyle.Render(typePriorityText) +
		statusPart +
		" " +
		idStyle.Render(entry.ID) +
		titleStyle.Render(titleLabels)

	return lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// renderSelectedRow renders the selected row with a highlight
// background. All text uses the selected foreground color.
func (renderer ListRenderer) renderSelectedRow(entry ticket.Entry, titleLabels string) string {
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

	row := " " +
		baseStyle.Bold(true).Render(typePriorityText) +
		statusPart +
		" " +
		baseStyle.Width(columnWidthID).Bold(false).Render(entry.ID) +
		baseStyle.Render(titleLabels)

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
