// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
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
func typeIcon(ticketType ticket.TicketType) string {
	switch ticketType {
	case ticket.TypeBug:
		return "🐛"
	case ticket.TypeTask:
		return "📋"
	case ticket.TypeFeature:
		return "✨"
	case ticket.TypeEpic:
		return "🎯"
	case ticket.TypeChore:
		return "🔧"
	case ticket.TypeDocs:
		return "📝"
	case ticket.TypeQuestion:
		return "❓"
	case ticket.TypePipeline:
		return "🚀"
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
func (renderer ListRenderer) RenderRow(entry ticketindex.Entry, selected bool, score *ticketindex.TicketScore, matchPositions []int) string {
	// Title width uses worst-case left portion (with status icon)
	// so truncation is consistent across rows.
	titleWidth := renderer.width - maxLeftWidth - columnWidthID
	if titleWidth < 0 {
		titleWidth = 0
	}

	// Reserve space for the unblock indicator when present.
	hasUnblock := score != nil && score.UnblockCount > 0
	if hasUnblock {
		titleWidth -= unblockIndicatorWidth
		if titleWidth < 0 {
			titleWidth = 0
		}
	}

	// Build the title portion with optional labels suffix.
	titleText := entry.Content.Title
	labelsText := ""
	if len(entry.Content.Labels) > 0 {
		labelsText = " [" + strings.Join(entry.Content.Labels, ", ") + "]"
	}

	// Truncate title+labels to fit the available width. When the
	// available width is zero or negative (panel narrower than the
	// fixed columns), show nothing rather than overflowing.
	combined := titleText + labelsText
	if titleWidth <= 0 {
		combined = ""
	} else if lipgloss.Width(combined) > titleWidth {
		// Truncate to fit, preferring to show the title over labels.
		if lipgloss.Width(titleText) >= titleWidth-1 {
			combined = truncateString(titleText, titleWidth-1) + "…"
		} else {
			combined = titleText + truncateString(labelsText, titleWidth-lipgloss.Width(titleText)-1) + "…"
		}
	}

	// Build the unblock indicator separately — it contains ANSI
	// styling and must not be included in the string that
	// highlightTitle iterates over, or match positions (which index
	// into the plain title) will land on escape code bytes and
	// corrupt the output.
	unblockSuffix := ""
	if hasUnblock {
		unblockSuffix = renderer.renderUnblockIndicator(score.UnblockCount, selected)
	}

	// Determine the display priority color. When the ticket has a
	// borrowed priority more urgent (lower number) than its own,
	// use the borrowed priority's color to visually escalate it.
	displayPriorityColor := entry.Content.Priority
	if score != nil && score.BorrowedPriority >= 0 && score.BorrowedPriority < entry.Content.Priority {
		displayPriorityColor = score.BorrowedPriority
	}

	if selected {
		return renderer.renderSelectedRow(entry, combined, unblockSuffix, matchPositions)
	}
	return renderer.renderNormalRow(entry, combined, unblockSuffix, displayPriorityColor, matchPositions)
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
// in the original title that should be highlighted. The unblockSuffix
// is pre-styled ANSI text appended after highlighting to avoid
// corrupting escape sequences during character-level match rendering.
func (renderer ListRenderer) renderNormalRow(entry ticketindex.Entry, titleLabels string, unblockSuffix string, displayPriorityColor int, matchPositions []int) string {
	priority := entry.Content.Priority
	status := string(entry.Content.Status)

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
		titleRendered +
		unblockSuffix

	return lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// renderSelectedRow renders the selected row with a highlight
// background. All text uses the selected foreground color.
// matchPositions contains rune indices in the original title that
// should be highlighted (with bold+underline on the selection bg).
// The unblockSuffix is pre-rendered text appended after highlighting.
func (renderer ListRenderer) renderSelectedRow(entry ticketindex.Entry, titleLabels string, unblockSuffix string, matchPositions []int) string {
	baseStyle := lipgloss.NewStyle().
		Background(renderer.theme.SelectedBackground).
		Foreground(renderer.theme.SelectedForeground)

	typePriorityText := typeIcon(entry.Content.Type) + fmt.Sprintf("P%d", entry.Content.Priority)

	icon := statusIconString(string(entry.Content.Status))
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
		titleRendered +
		unblockSuffix

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
	if availableForTitle <= 0 {
		title = ""
	} else if lipgloss.Width(title) > availableForTitle {
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

// RenderPipelineRow renders a pipeline ticket as a formatted list row.
// Shows the pipeline reference as the primary identifier rather than
// the ticket title, with a pipeline-specific status suffix (step
// progress, conclusion, or schedule time). Falls back to RenderRow
// if the ticket has no pipeline content.
//
// Row layout:
//
//	🚀P2 ● pip-3vk5  deploy/staging  Deploy to staging  [4/10 clone-repo]
//	└──emoji+pri+status+id──────────┘ └──pipeline ref──┘ └──title (faint)┘ └──suffix──┘
func (renderer ListRenderer) RenderPipelineRow(entry ticketindex.Entry, selected bool, matchPositions []int) string {
	if entry.Content.Pipeline == nil {
		return renderer.RenderRow(entry, selected, nil, matchPositions)
	}

	pipeline := entry.Content.Pipeline
	suffix := pipelineSuffix(entry.Content)
	suffixWidth := lipgloss.Width(suffix)

	// Available width after the left portion (indent+emoji+priority+
	// status icon+space) and ID column.
	contentWidth := renderer.width - maxLeftWidth - columnWidthID
	if contentWidth < 0 {
		contentWidth = 0
	}

	// Reserve space for the suffix with a space separator.
	textWidth := contentWidth
	if suffix != "" {
		textWidth = contentWidth - suffixWidth - 1
		if textWidth < 0 {
			textWidth = 0
		}
	}

	// Build the text: pipeline ref + title. Pipeline ref gets priority
	// space; the title is shown in faint text if room permits.
	pipelineRef := pipeline.PipelineRef
	title := entry.Content.Title

	if selected {
		return renderer.renderSelectedPipelineRow(entry, pipelineRef, title, suffix, textWidth, contentWidth)
	}
	return renderer.renderNormalPipelineRow(entry, pipelineRef, title, suffix, textWidth, contentWidth)
}

// renderNormalPipelineRow renders a pipeline row with per-component colors.
func (renderer ListRenderer) renderNormalPipelineRow(
	entry ticketindex.Entry,
	pipelineRef, title, suffix string,
	textWidth, contentWidth int,
) string {
	priority := entry.Content.Priority
	status := string(entry.Content.Status)

	typePriorityStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.PriorityColor(priority)).
		Bold(priority <= 1)

	idStyle := lipgloss.NewStyle().
		Width(columnWidthID).
		Foreground(renderer.theme.StatusColor(status))

	refStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText)

	titleStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	suffixStyle := renderer.pipelineSuffixStyle(entry.Content)

	typePriorityText := typeIcon(entry.Content.Type) + fmt.Sprintf("P%d", priority)

	icon := statusIconString(status)
	var statusPart string
	if icon != "" {
		statusPart = " " + lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(status)).
			Render(icon)
	} else {
		statusPart = "  "
	}

	// Compose the text portion: ref + title, truncated to fit.
	textPart := composePipelineText(pipelineRef, title, textWidth, refStyle, titleStyle)

	// Pad and append suffix.
	textRendered := textPart
	textRenderedWidth := lipgloss.Width(textPart)
	if suffix != "" {
		gap := contentWidth - textRenderedWidth - lipgloss.Width(suffix)
		if gap < 1 {
			gap = 1
		}
		textRendered += strings.Repeat(" ", gap) + suffixStyle.Render(suffix)
	}

	row := " " +
		typePriorityStyle.Render(typePriorityText) +
		statusPart +
		" " +
		idStyle.Render(entry.ID) +
		textRendered

	return lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// renderSelectedPipelineRow renders the selected pipeline row with
// highlight background.
func (renderer ListRenderer) renderSelectedPipelineRow(
	entry ticketindex.Entry,
	pipelineRef, title, suffix string,
	textWidth, contentWidth int,
) string {
	baseStyle := lipgloss.NewStyle().
		Background(renderer.theme.SelectedBackground).
		Foreground(renderer.theme.SelectedForeground)

	typePriorityText := typeIcon(entry.Content.Type) + fmt.Sprintf("P%d", entry.Content.Priority)

	icon := statusIconString(string(entry.Content.Status))
	var statusPart string
	if icon != "" {
		statusPart = " " + baseStyle.Render(icon)
	} else {
		statusPart = "  "
	}

	// Compose the text: ref + title using the base style for both.
	textPart := composePipelineText(pipelineRef, title, textWidth, baseStyle, baseStyle)

	textRendered := textPart
	textRenderedWidth := lipgloss.Width(textPart)
	if suffix != "" {
		gap := contentWidth - textRenderedWidth - lipgloss.Width(suffix)
		if gap < 1 {
			gap = 1
		}
		textRendered += strings.Repeat(" ", gap) + baseStyle.Render(suffix)
	}

	row := " " +
		baseStyle.Bold(true).Render(typePriorityText) +
		statusPart +
		" " +
		baseStyle.Width(columnWidthID).Bold(false).Render(entry.ID) +
		textRendered

	return baseStyle.Width(renderer.width).MaxWidth(renderer.width).Render(row)
}

// composePipelineText combines the pipeline ref and title into a
// single text portion, truncating to fit the available width. The
// pipeline ref gets priority; the title is appended in faint style
// only if space remains.
func composePipelineText(pipelineRef, title string, maxWidth int, refStyle, titleStyle lipgloss.Style) string {
	if maxWidth <= 0 {
		return ""
	}

	refWidth := lipgloss.Width(pipelineRef)

	if refWidth >= maxWidth {
		return refStyle.Render(truncateString(pipelineRef, maxWidth-1) + "…")
	}

	result := refStyle.Render(pipelineRef)

	// Append title if there's room (need at least 2 chars: space + 1 char).
	remaining := maxWidth - refWidth
	if remaining >= 4 && title != "" {
		separator := "  "
		titleSpace := remaining - len(separator)
		if lipgloss.Width(title) > titleSpace {
			title = truncateString(title, titleSpace-1) + "…"
		}
		result += titleStyle.Render(separator + title)
	}

	return result
}

// pipelineSuffix returns the right-side status indicator for a
// pipeline ticket row, based on its execution state.
func pipelineSuffix(content ticket.TicketContent) string {
	pipeline := content.Pipeline
	if pipeline == nil {
		return ""
	}

	switch content.Status {
	case ticket.StatusInProgress:
		if pipeline.TotalSteps > 0 && pipeline.CurrentStepName != "" {
			return fmt.Sprintf("[%d/%d %s]", pipeline.CurrentStep, pipeline.TotalSteps, pipeline.CurrentStepName)
		}
		if pipeline.TotalSteps > 0 {
			return fmt.Sprintf("[%d/%d]", pipeline.CurrentStep, pipeline.TotalSteps)
		}
		return ""

	case ticket.StatusClosed:
		switch pipeline.Conclusion {
		case "success":
			return "✓ success"
		case "failure":
			return "✗ failure"
		case "aborted":
			return "✗ aborted"
		case "cancelled":
			return "✗ cancelled"
		default:
			return ""
		}

	default:
		target := firstPendingTimerGateTarget(content)
		if target != "" {
			return "⏲ " + compactTimestamp(target)
		}
		return ""
	}
}

// pipelineSuffixStyle returns the styling for a pipeline suffix
// based on the ticket's state and conclusion.
func (renderer ListRenderer) pipelineSuffixStyle(content ticket.TicketContent) lipgloss.Style {
	pipeline := content.Pipeline
	if pipeline == nil {
		return lipgloss.NewStyle().Foreground(renderer.theme.FaintText)
	}

	switch content.Status {
	case ticket.StatusInProgress:
		return lipgloss.NewStyle().Foreground(renderer.theme.StatusInProgress)
	case ticket.StatusClosed:
		switch pipeline.Conclusion {
		case "success":
			return lipgloss.NewStyle().Foreground(renderer.theme.StatusOpen).Bold(true)
		case "failure", "aborted", "cancelled":
			return lipgloss.NewStyle().Foreground(renderer.theme.PriorityColor(0)).Bold(true)
		}
	}

	return lipgloss.NewStyle().Foreground(renderer.theme.FaintText)
}

// compactTimestamp converts an RFC 3339 timestamp to a compact
// human-readable format like "Feb 22 03:00". Falls back to the
// original string if parsing fails.
func compactTimestamp(rfc3339 string) string {
	parsed, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return parsed.Format("Jan 2 15:04")
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
