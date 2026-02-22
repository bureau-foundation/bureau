// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// detailHeaderLines is the fixed number of lines consumed by the
// detail pane header (metadata + title). This is constant so the
// scrollable body never shifts vertically when switching tickets.
//
// Layout:
//
//	Line 1: STATUS  P1  type  ticket-id  [labels]      ↑3  →P0  5d
//	Line 2: timestamps / assignee (condensed)
//	Line 3: title line 1
//	Line 4: title line 2 (or blank)
//	Line 5: separator
//
// Line 1 includes right-aligned signal indicators (unblock count,
// borrowed priority, staleness) when a score is provided and when
// there is enough horizontal space after the metadata and labels.
const detailHeaderLines = 5

// BodyClickTarget maps a line in the rendered body to a navigable
// ticket ID. Used by the model to handle mouse clicks on dependency,
// child, and parent entries in the detail pane.
type BodyClickTarget struct {
	Line     int // 0-based line number in the body content.
	TicketID string
	StartX   int // Inclusive start column of the clickable region.
	EndX     int // Exclusive end column of the clickable region.
}

// HeaderClickTarget maps a region in the fixed header to a clickable
// field. Used by the model to handle mouse clicks on status, priority,
// and title for inline mutation.
type HeaderClickTarget struct {
	Line   int    // 0-based line in the header (0-4).
	StartX int    // Inclusive start column of the clickable region.
	EndX   int    // Exclusive end column of the clickable region.
	Field  string // "status", "priority", or "title".
}

// HeaderRenderResult carries the rendered header string alongside
// click targets for interactive fields.
type HeaderRenderResult struct {
	Rendered string
	Targets  []HeaderClickTarget
}

// DetailRenderer builds the content for the detail pane. Produces a
// fixed header (rendered outside the viewport) and scrollable body
// (set into the viewport).
type DetailRenderer struct {
	theme Theme
	width int
}

// NewDetailRenderer creates a DetailRenderer for the given width.
func NewDetailRenderer(theme Theme, width int) DetailRenderer {
	return DetailRenderer{theme: theme, width: width}
}

// RenderHeader produces the fixed header lines for a ticket. Always
// exactly [detailHeaderLines] lines tall regardless of content. When
// a score is provided, signal indicators (↑N, →P0, Nd) are
// right-aligned on Line 1. Returns a HeaderRenderResult with the
// rendered string and click targets for status, priority, and title.
func (renderer DetailRenderer) RenderHeader(entry ticketindex.Entry, score *ticketindex.TicketScore) HeaderRenderResult {
	content := entry.Content

	// Line 1: STATUS  P1  type  id  [labels]  ...signals
	line1, statusEnd, priorityStart, priorityEnd := renderer.renderMetaLine(entry.ID, content, score)

	// Line 2: timestamps + assignee, condensed onto one line.
	line2 := renderer.renderTimestampLine(content)

	// Lines 3-4: title, hard-capped at 2 lines.
	titleLine1, titleLine2 := renderer.renderTitleLines(content.Title)

	// Line 5: separator.
	separatorStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.BorderColor).
		Width(renderer.width)
	separator := separatorStyle.Render(strings.Repeat("─", renderer.width))

	rendered := strings.Join([]string{line1, line2, titleLine1, titleLine2, separator}, "\n")

	// Build header click targets for interactive fields.
	var targets []HeaderClickTarget

	// Status: line 0, columns 0..statusEnd.
	targets = append(targets, HeaderClickTarget{
		Line:   0,
		StartX: 0,
		EndX:   statusEnd,
		Field:  "status",
	})

	// Priority: line 0, columns priorityStart..priorityEnd.
	targets = append(targets, HeaderClickTarget{
		Line:   0,
		StartX: priorityStart,
		EndX:   priorityEnd,
		Field:  "priority",
	})

	// Title: lines 2-3, full width.
	if content.Title != "" {
		targets = append(targets, HeaderClickTarget{
			Line:   2,
			StartX: 0,
			EndX:   renderer.width,
			Field:  "title",
		})
		targets = append(targets, HeaderClickTarget{
			Line:   3,
			StartX: 0,
			EndX:   renderer.width,
			Field:  "title",
		})
	}

	return HeaderRenderResult{
		Rendered: rendered,
		Targets:  targets,
	}
}

// RenderBody produces the scrollable body content for a ticket and a
// set of click targets mapping body line numbers to navigable ticket
// IDs. Layout order: dependency graph, children, parent context,
// close reason, description, notes. The now parameter drives
// borrowed-priority computation for graph node indicators.
func (renderer DetailRenderer) RenderBody(source Source, entry ticketindex.Entry, now time.Time) (string, []BodyClickTarget) {
	var allTargets []BodyClickTarget
	var sections []string
	totalLines := 0 // Running count of lines produced so far.

	// addSection appends a rendered section and adjusts click target
	// line numbers to absolute positions in the final body. Sections
	// are joined with "\n\n" (one blank line between them), so each
	// section after the first starts one line later than the previous
	// section's last line.
	addSection := func(section string, targets []BodyClickTarget) {
		sectionStart := totalLines
		if len(sections) > 0 {
			sectionStart++ // Blank line from "\n\n" join.
		}
		for _, target := range targets {
			allTargets = append(allTargets, BodyClickTarget{
				Line:     sectionStart + target.Line,
				TicketID: target.TicketID,
				StartX:   target.StartX,
				EndX:     target.EndX,
			})
		}
		sectionLineCount := strings.Count(section, "\n") + 1
		totalLines = sectionStart + sectionLineCount
		sections = append(sections, section)
	}

	// Dependency graph: horizontal DAG with blocked-by on the left,
	// the current ticket in the center, and blocks on the right.
	blockers := source.Blocks(entry.ID)
	if len(entry.Content.BlockedBy) > 0 || len(blockers) > 0 {
		depGraph := NewDependencyGraph(renderer.theme, renderer.width)
		graphString, graphTargets := depGraph.Render(entry.ID, entry.Content.BlockedBy, blockers, source, now)
		if graphString != "" {
			addSection(graphString, graphTargets)
		}
	}

	// Dependency lists with titles: blocked-by and blocks rendered
	// as a tight block (joined with "\n", not "\n\n") so the titled
	// entries are scannable alongside the graph above.
	var depString string
	var depTargets []BodyClickTarget
	if len(entry.Content.BlockedBy) > 0 {
		blockedByString, blockedByTargets := renderer.renderDependencies("Blocked By", entry.Content.BlockedBy, source)
		depString = blockedByString
		depTargets = blockedByTargets
	}
	if len(blockers) > 0 {
		blocksString, blocksTargets := renderer.renderDependencies("Blocks", blockers, source)
		if depString != "" {
			offset := strings.Count(depString, "\n") + 1
			for _, target := range blocksTargets {
				depTargets = append(depTargets, BodyClickTarget{
					Line:     target.Line + offset,
					TicketID: target.TicketID,
					StartX:   target.StartX,
					EndX:     target.EndX,
				})
			}
			depString += "\n" + blocksString
		} else {
			depString = blocksString
			depTargets = blocksTargets
		}
	}
	if depString != "" {
		addSection(depString, depTargets)
	}

	// Pipeline-specific sections: execution progress, variables,
	// and schedule info. Shown for pipeline tickets between the
	// dependency graph and the structural sections (children, parent).
	if entry.Content.Type == "pipeline" {
		executionSection := renderer.renderPipelineExecution(entry.Content)
		if executionSection != "" {
			addSection(executionSection, nil)
		}

		if entry.Content.Pipeline != nil && len(entry.Content.Pipeline.Variables) > 0 {
			variablesSection := renderer.renderPipelineVariables(entry.Content.Pipeline.Variables)
			addSection(variablesSection, nil)
		}

		scheduleSection := renderer.renderPipelineSchedule(entry.Content)
		if scheduleSection != "" {
			addSection(scheduleSection, nil)
		}
	}

	// Children (for epics).
	children := source.Children(entry.ID)
	if len(children) > 0 {
		childString, childTargets := renderer.renderChildren(entry.ID, children, source)
		addSection(childString, childTargets)
	}

	// Parent epic context.
	if entry.Content.Parent != "" {
		parentContent, exists := source.Get(entry.Content.Parent)
		if exists {
			parentString, parentTargets := renderer.renderParentContext(entry.Content.Parent, parentContent, source)
			addSection(parentString, parentTargets)
		}
	}

	// Close reason.
	if entry.Content.CloseReason != "" {
		addSection(renderer.renderMarkdownSection("Close Reason", entry.Content.CloseReason), nil)
	}

	// Body / description.
	if entry.Content.Body != "" {
		addSection(renderer.renderMarkdownSection("Description", entry.Content.Body), nil)
	}

	// Notes.
	if len(entry.Content.Notes) > 0 {
		addSection(renderer.renderNotes(entry.Content.Notes), nil)
	}

	if len(sections) == 0 {
		return "", nil
	}
	return strings.Join(sections, "\n\n"), allTargets
}

// renderMetaLine builds the first header line: status, priority, type,
// ID, labels, and right-aligned signal indicators. The score parameter
// is optional; when nil no indicators are shown.
//
// Returns the rendered line plus the column positions of the status and
// priority text for click target computation:
//   - statusEnd: exclusive end column of the status text
//   - priorityStart: inclusive start column of the priority text
//   - priorityEnd: exclusive end column of the priority text
func (renderer DetailRenderer) renderMetaLine(ticketID string, content ticket.TicketContent, score *ticketindex.TicketScore) (line string, statusEnd, priorityStart, priorityEnd int) {
	statusStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.StatusColor(content.Status)).
		Bold(true)

	priorityStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.PriorityColor(content.Priority)).
		Bold(content.Priority <= 1)

	typeStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	idStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	// Track visible character positions for click targets.
	statusText := strings.ToUpper(content.Status)
	statusEnd = len(statusText) // Status text is ASCII, so len == visible width.

	priorityText := fmt.Sprintf("P%d", content.Priority)
	priorityStart = statusEnd + 2 // "  " gap after status.
	priorityEnd = priorityStart + len(priorityText)

	leftPortion := statusStyle.Render(statusText) + "  " +
		priorityStyle.Render(priorityText) + "  " +
		typeStyle.Render(content.Type) + "  " +
		idStyle.Render(ticketID)

	// Append labels inline if they fit.
	if len(content.Labels) > 0 {
		labelText := renderer.renderLabels(content.Labels)
		leftPortion += "  " + labelText
	}

	// Right-align signal indicators if there is enough space.
	signals := renderer.renderSignalIndicators(score, content.Priority)
	if signals != "" {
		leftWidth := lipgloss.Width(leftPortion)
		signalsWidth := lipgloss.Width(signals)
		gap := renderer.width - leftWidth - signalsWidth
		if gap >= 2 {
			leftPortion += strings.Repeat(" ", gap) + signals
		}
	}

	line = lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(leftPortion)
	return line, statusEnd, priorityStart, priorityEnd
}

// renderSignalIndicators builds compact right-aligned signal
// indicators for the header meta line. Format: "↑3  →P0  5d".
// Returns empty string when score is nil or no signals are
// worth showing.
//
// Indicator styling matches the list view for visual consistency:
//   - ↑N: unblock count. Faint for 1-2, P1 color for 3-5, P0 color for 6+.
//   - →P0: borrowed priority, only when more urgent than own. Uses
//     the borrowed priority's color.
//   - Nd: days since ready, only when > 0. Faint text.
func (renderer DetailRenderer) renderSignalIndicators(score *ticketindex.TicketScore, ownPriority int) string {
	if score == nil {
		return ""
	}

	var indicators []string

	if score.UnblockCount > 0 {
		color := renderer.theme.FaintText
		switch {
		case score.UnblockCount >= 6:
			color = renderer.theme.PriorityColor(0)
		case score.UnblockCount >= 3:
			color = renderer.theme.PriorityColor(1)
		}
		style := lipgloss.NewStyle().Foreground(color)
		indicators = append(indicators, style.Render(fmt.Sprintf("↑%d", score.UnblockCount)))
	}

	if score.BorrowedPriority >= 0 && score.BorrowedPriority < ownPriority {
		style := lipgloss.NewStyle().
			Foreground(renderer.theme.PriorityColor(score.BorrowedPriority)).
			Bold(true)
		indicators = append(indicators, style.Render(fmt.Sprintf("→P%d", score.BorrowedPriority)))
	}

	if score.DaysSinceReady > 0 {
		style := lipgloss.NewStyle().Foreground(renderer.theme.FaintText)
		indicators = append(indicators, style.Render(fmt.Sprintf("%dd", score.DaysSinceReady)))
	}

	if len(indicators) == 0 {
		return ""
	}

	return strings.Join(indicators, "  ")
}

// renderTimestampLine builds the second header line: timestamps and
// assignee condensed onto a single line. Timestamps are shortened to
// just the date (YYYY-MM-DD) to avoid wrapping.
func (renderer DetailRenderer) renderTimestampLine(content ticket.TicketContent) string {
	metaStyle := lipgloss.NewStyle().Foreground(renderer.theme.FaintText)

	var parts []string
	if !content.CreatedBy.IsZero() {
		parts = append(parts, content.CreatedBy.String())
	}
	if content.CreatedAt != "" {
		parts = append(parts, shortenTimestamp(content.CreatedAt))
	}
	if content.UpdatedAt != "" && content.UpdatedAt != content.CreatedAt {
		parts = append(parts, "upd "+shortenTimestamp(content.UpdatedAt))
	}
	if content.ClosedAt != "" {
		parts = append(parts, "closed "+shortenTimestamp(content.ClosedAt))
	}
	if !content.Assignee.IsZero() {
		parts = append(parts, content.Assignee.String())
	}

	line := strings.Join(parts, "  ")
	return metaStyle.Render(lipgloss.NewStyle().Width(renderer.width).MaxWidth(renderer.width).Render(line))
}

// shortenTimestamp extracts just the date from an ISO 8601 timestamp
// (e.g., "2026-02-12T22:32:27Z" → "2026-02-12"). Returns the input
// unchanged if it doesn't contain a 'T' separator.
func shortenTimestamp(timestamp string) string {
	if index := strings.IndexByte(timestamp, 'T'); index > 0 {
		return timestamp[:index]
	}
	return timestamp
}

// renderTitleLines renders the ticket title into exactly 2 lines.
// Long titles are truncated with an ellipsis at the end of line 2.
// Short titles leave line 2 blank.
func (renderer DetailRenderer) renderTitleLines(title string) (string, string) {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.HeaderForeground)

	blankLine := lipgloss.NewStyle().Width(renderer.width).Render("")

	if title == "" {
		return blankLine, blankLine
	}

	runes := []rune(title)

	// Find where the first line breaks.
	firstLineEnd := findLineBreak(runes, renderer.width)

	if firstLineEnd >= len(runes) {
		// Title fits on one line.
		return titleStyle.Width(renderer.width).Render(title), blankLine
	}

	// First line: up to the break point.
	line1 := titleStyle.Width(renderer.width).Render(string(runes[:firstLineEnd]))

	// Second line: remainder, truncated if needed.
	remainder := runes[firstLineEnd:]
	// Skip a leading space from the word-break.
	if len(remainder) > 0 && remainder[0] == ' ' {
		remainder = remainder[1:]
	}

	remainderString := string(remainder)
	if lipgloss.Width(remainderString) > renderer.width {
		remainderString = truncateString(remainderString, renderer.width-1) + "…"
	}

	line2 := titleStyle.Width(renderer.width).Render(remainderString)
	return line1, line2
}

// findLineBreak returns the rune index where the first line should
// end, preferring to break at a word boundary.
func findLineBreak(runes []rune, maxWidth int) int {
	lastSpace := -1
	for index := range runes {
		if lipgloss.Width(string(runes[:index+1])) > maxWidth {
			if lastSpace > 0 {
				return lastSpace
			}
			return index
		}
		if runes[index] == ' ' {
			lastSpace = index
		}
	}
	return len(runes)
}

// renderLabels renders label pills as bracketed colored text.
func (renderer DetailRenderer) renderLabels(labels []string) string {
	var parts []string
	for _, label := range labels {
		// Deterministic color from label name hash.
		color := labelColor(label)
		style := lipgloss.NewStyle().Foreground(color)
		parts = append(parts, style.Render("["+label+"]"))
	}
	return strings.Join(parts, " ")
}

// labelColor returns a deterministic ANSI color for a label name.
// Uses a simple hash to spread labels across the 256-color palette
// (avoiding the first 16 ANSI colors which vary by terminal theme).
func labelColor(label string) lipgloss.Color {
	hash := uint32(0)
	for _, character := range label {
		hash = hash*31 + uint32(character)
	}
	// Map to range 17-231 (avoiding 0-16 terminal theme colors and
	// 232-255 grayscale which would be hard to distinguish).
	colorIndex := 17 + (hash % 215)
	return lipgloss.Color(fmt.Sprintf("%d", colorIndex))
}

// renderSection renders a titled section with plain body text.
func (renderer DetailRenderer) renderSection(title, body string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	bodyStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText).
		Width(renderer.width)

	return headerStyle.Render(title) + "\n" + bodyStyle.Render(body)
}

// renderMarkdownSection renders a titled section with markdown body.
// Falls back to plain text wrapping if rendering fails.
func (renderer DetailRenderer) renderMarkdownSection(title, body string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	rendered, err := renderer.renderMarkdown(body)
	if err != nil {
		// Fall back to plain text wrapping on rendering failure.
		return renderer.renderSection(title, body)
	}

	// Trim trailing newlines so section spacing is controlled by the caller.
	rendered = strings.TrimRight(rendered, "\n")

	return headerStyle.Render(title) + "\n" + rendered
}

// renderMarkdown renders markdown text as styled terminal output with
// word wrap sized to the detail pane width. Soft line breaks within
// paragraphs become spaces so hard-wrapped source text reflows
// correctly at any terminal width.
func (renderer DetailRenderer) renderMarkdown(text string) (string, error) {
	return renderTerminalMarkdown(text, renderer.theme, renderer.width), nil
}

// renderParentContext renders the parent epic with progress bar.
// The parent entry line is clickable for navigation. Returns the
// rendered section and a click target for the parent line.
func (renderer DetailRenderer) renderParentContext(parentID string, parent ticket.TicketContent, source Source) (string, []BodyClickTarget) {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	contentStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	total, closed := source.ChildProgress(parentID)
	progress := ""
	if total > 0 {
		progress = fmt.Sprintf(" (%d/%d)", closed, total)
		// Simple text progress bar.
		barWidth := 20
		if total > 0 {
			filled := barWidth * closed / total
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
			progress += " " + bar
		}
	}

	contentText := parentID + " " + parent.Title + progress
	if lipgloss.Width(contentText) > renderer.width {
		contentText = truncateString(contentText, renderer.width-1) + "…"
	}

	target := BodyClickTarget{
		Line:     1, // Line 0 is the "Epic" header.
		TicketID: parentID,
		EndX:     renderer.width,
	}

	return headerStyle.Render("Epic") + "\n" +
		contentStyle.Render(contentText), []BodyClickTarget{target}
}

// renderDependencies renders a titled list of dependency ticket IDs
// with their status icons and titles. Each entry is truncated to one
// line. Returns the rendered section and click targets with line
// offsets relative to the section (line 0 is the header).
func (renderer DetailRenderer) renderDependencies(title string, ticketIDs []string, source Source) (string, []BodyClickTarget) {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	var lines []string
	var targets []BodyClickTarget

	for _, ticketID := range ticketIDs {
		content, exists := source.Get(ticketID)
		if !exists {
			plainText := ticketID + " (not in index)"
			if lipgloss.Width(plainText) > renderer.width {
				plainText = truncateString(plainText, renderer.width-1) + "…"
			}
			lines = append(lines, lipgloss.NewStyle().
				Foreground(renderer.theme.FaintText).
				Render(plainText))
			continue
		}

		statusStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(content.Status))
		priorityStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.PriorityColor(content.Priority))
		idStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(content.Status))
		titleStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.FaintText)

		icon := statusIconString(content.Status)
		if icon == "" {
			icon = " " // Reserve the column so IDs align.
		}

		priorityText := fmt.Sprintf("P%d", content.Priority)
		prefix := statusStyle.Render(icon) + " " + priorityStyle.Render(priorityText) + " " + idStyle.Render(ticketID) + " "
		prefixWidth := lipgloss.Width(prefix)

		titleText := content.Title
		availableForTitle := renderer.width - prefixWidth
		if availableForTitle > 0 && lipgloss.Width(titleText) > availableForTitle {
			titleText = truncateString(titleText, availableForTitle-1) + "…"
		}

		targets = append(targets, BodyClickTarget{
			Line:     len(lines) + 1, // +1 for the header line.
			TicketID: ticketID,
			EndX:     renderer.width,
		})
		lines = append(lines, prefix+titleStyle.Render(titleText))
	}

	return headerStyle.Render(title) + "\n" + strings.Join(lines, "\n"), targets
}

// renderChildren renders the children of an epic with progress. Each
// entry is truncated to one line. Returns the rendered section and
// click targets with line offsets relative to the section.
func (renderer DetailRenderer) renderChildren(parentID string, children []ticketindex.Entry, source Source) (string, []BodyClickTarget) {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	total, closed := source.ChildProgress(parentID)
	header := fmt.Sprintf("Children (%d/%d)", closed, total)

	var lines []string
	var targets []BodyClickTarget

	for _, child := range children {
		statusStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(child.Content.Status))
		priorityStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.PriorityColor(child.Content.Priority))
		idStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.StatusColor(child.Content.Status))
		titleStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.FaintText)

		icon := statusIconString(child.Content.Status)
		if icon == "" {
			icon = " " // Reserve the column so IDs align.
		}

		priorityText := fmt.Sprintf("P%d", child.Content.Priority)
		prefix := statusStyle.Render(icon) + " " + priorityStyle.Render(priorityText) + " " + idStyle.Render(child.ID) + " "
		prefixWidth := lipgloss.Width(prefix)

		titleText := child.Content.Title
		availableForTitle := renderer.width - prefixWidth
		if availableForTitle > 0 && lipgloss.Width(titleText) > availableForTitle {
			titleText = truncateString(titleText, availableForTitle-1) + "…"
		}

		targets = append(targets, BodyClickTarget{
			Line:     len(lines) + 1, // +1 for the header line.
			TicketID: child.ID,
			EndX:     renderer.width,
		})
		lines = append(lines, prefix+titleStyle.Render(titleText))
	}

	return headerStyle.Render(header) + "\n" + strings.Join(lines, "\n"), targets
}

// renderNotes renders ticket notes with author and timestamp.
func (renderer DetailRenderer) renderNotes(notes []ticket.TicketNote) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)

	var lines []string
	for _, note := range notes {
		metaStyle := lipgloss.NewStyle().
			Foreground(renderer.theme.FaintText)
		meta := metaStyle.Render(fmt.Sprintf("%s at %s", note.Author, note.CreatedAt))
		body := renderTerminalMarkdown(note.Body, renderer.theme, renderer.width)
		lines = append(lines, meta+"\n"+body)
	}

	return headerStyle.Render("Notes") + "\n" + strings.Join(lines, "\n")
}

// renderPipelineExecution renders the pipeline execution section:
// pipeline reference, progress bar (for active pipelines), and
// conclusion badge (for completed pipelines).
func (renderer DetailRenderer) renderPipelineExecution(content ticket.TicketContent) string {
	pipeline := content.Pipeline
	if pipeline == nil {
		return ""
	}

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)
	contentStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText)
	labelStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	var lines []string

	lines = append(lines, labelStyle.Render("ref: ")+contentStyle.Render(pipeline.PipelineRef))

	// Progress bar for active pipelines.
	if content.Status == "in_progress" && pipeline.TotalSteps > 0 {
		barWidth := 20
		filled := barWidth * pipeline.CurrentStep / pipeline.TotalSteps
		if filled > barWidth {
			filled = barWidth
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

		stepLabel := ""
		if pipeline.CurrentStepName != "" {
			stepLabel = ": " + pipeline.CurrentStepName
		}

		progressText := fmt.Sprintf("%s %d/%d%s",
			bar, pipeline.CurrentStep, pipeline.TotalSteps, stepLabel)
		lines = append(lines, contentStyle.Render(progressText))
	}

	// Conclusion badge for completed pipelines.
	if pipeline.Conclusion != "" {
		conclusionRendered := renderer.conclusionStyle(pipeline.Conclusion).Render(pipeline.Conclusion)
		lines = append(lines, labelStyle.Render("conclusion: ")+conclusionRendered)
	}

	return headerStyle.Render("Pipeline Execution") + "\n" + strings.Join(lines, "\n")
}

// renderPipelineVariables renders a sorted key=value table of
// resolved pipeline variables.
func (renderer DetailRenderer) renderPipelineVariables(variables map[string]string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)
	keyStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)
	valueStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText)

	keys := make([]string, 0, len(variables))
	for variableName := range variables {
		keys = append(keys, variableName)
	}
	slices.Sort(keys)

	var lines []string
	for _, variableName := range keys {
		line := keyStyle.Render(variableName+": ") + valueStyle.Render(variables[variableName])
		if lipgloss.Width(line) > renderer.width {
			line = truncateString(line, renderer.width-1) + "…"
		}
		lines = append(lines, line)
	}

	return headerStyle.Render("Variables") + "\n" + strings.Join(lines, "\n")
}

// renderPipelineSchedule renders timer gate details for pipeline
// tickets that have timer-based scheduling. Shows the gate status,
// target time, schedule or interval expression, and fire count.
func (renderer DetailRenderer) renderPipelineSchedule(content ticket.TicketContent) string {
	var timerGates []ticket.TicketGate
	for index := range content.Gates {
		if content.Gates[index].Type == "timer" {
			timerGates = append(timerGates, content.Gates[index])
		}
	}
	if len(timerGates) == 0 {
		return ""
	}

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(renderer.theme.NormalText)
	contentStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.NormalText)
	labelStyle := lipgloss.NewStyle().
		Foreground(renderer.theme.FaintText)

	var lines []string
	for _, gate := range timerGates {
		// Gate ID and status.
		statusIndicator := "⏲ pending"
		statusColor := renderer.theme.StatusInProgress
		if gate.Status == "satisfied" {
			statusIndicator = "✓ satisfied"
			statusColor = renderer.theme.StatusOpen
		}
		statusStyle := lipgloss.NewStyle().Foreground(statusColor)

		gateLabel := gate.ID
		if gate.Description != "" {
			gateLabel += ": " + gate.Description
		}
		lines = append(lines, statusStyle.Render(statusIndicator)+"  "+contentStyle.Render(gateLabel))

		if gate.Target != "" {
			lines = append(lines, labelStyle.Render("  target: ")+contentStyle.Render(gate.Target))
		}
		if gate.Schedule != "" {
			lines = append(lines, labelStyle.Render("  schedule: ")+contentStyle.Render(gate.Schedule))
		}
		if gate.Interval != "" {
			lines = append(lines, labelStyle.Render("  interval: ")+contentStyle.Render(gate.Interval))
		}
		if gate.IsRecurring() {
			recurrence := fmt.Sprintf("fired %d times", gate.FireCount)
			if gate.MaxOccurrences > 0 {
				recurrence += fmt.Sprintf(" (max %d)", gate.MaxOccurrences)
			}
			lines = append(lines, labelStyle.Render("  ")+contentStyle.Render(recurrence))
		}
	}

	return headerStyle.Render("Schedule") + "\n" + strings.Join(lines, "\n")
}

// conclusionStyle returns a lipgloss style for rendering a pipeline
// conclusion badge. Success is green, failure is red/bold, and
// aborted/cancelled use the high-priority color.
func (renderer DetailRenderer) conclusionStyle(conclusion string) lipgloss.Style {
	switch conclusion {
	case "success":
		return lipgloss.NewStyle().Foreground(renderer.theme.StatusOpen).Bold(true)
	case "failure":
		return lipgloss.NewStyle().Foreground(renderer.theme.PriorityColor(0)).Bold(true)
	case "aborted", "cancelled":
		return lipgloss.NewStyle().Foreground(renderer.theme.PriorityColor(1))
	default:
		return lipgloss.NewStyle().Foreground(renderer.theme.FaintText)
	}
}

// DetailPane wraps a bubbles viewport for scrollable detail content.
// The pane has a fixed header (metadata + title, [detailHeaderLines]
// tall) rendered above the viewport, and a scrollable body below.
type DetailPane struct {
	viewport viewport.Model
	theme    Theme
	width    int
	height   int

	// Retained for re-rendering on resize. source and entry are set
	// by SetContent and cleared by Clear. When hasEntry is true,
	// SetSize re-renders the content at the new width so markdown
	// word wrap adapts to splitter changes.
	hasEntry bool
	source   Source
	entry    ticketindex.Entry

	// Pre-rendered header string, set by SetContent and rerender.
	header string

	// renderTime is the time snapshot used for scoring computation
	// in the header. Set by SetContent, reused by rerender so resize
	// doesn't change the displayed signal indicators.
	renderTime time.Time

	// clickTargets maps body line numbers to navigable ticket IDs.
	// Set by SetContent and rerender; used by the model to handle
	// mouse clicks on dependency, child, and parent entries.
	clickTargets []BodyClickTarget

	// headerClickTargets maps regions in the fixed header to
	// interactive fields (status, priority, title). Set by
	// SetContent and rerender; used by the model to handle
	// mutation triggers via mouse clicks on header elements.
	headerClickTargets []HeaderClickTarget

	// Search state for in-body text search.
	search SearchModel

	// rawBody holds the rendered body before search highlighting,
	// so highlighting can be reapplied when the query changes
	// without re-rendering the entire body from markdown.
	rawBody string
}

// NewDetailPane creates an empty detail pane.
func NewDetailPane(theme Theme) DetailPane {
	return DetailPane{
		theme: theme,
	}
}

// bodyHeight returns the number of lines available for the scrollable
// viewport body (total height minus the fixed header, and minus the
// search bar when visible).
func (pane DetailPane) bodyHeight() int {
	result := pane.height - detailHeaderLines
	if pane.searchBarVisible() {
		result--
	}
	if result < 1 {
		result = 1
	}
	return result
}

// contentWidth returns the usable width for text content (total width
// minus the left padding column and right scrollbar column).
func (pane DetailPane) contentWidth() int {
	return pane.width - 2
}

// SetSize updates the detail pane dimensions. If the width changed and
// there is content displayed, the content is re-rendered at the new
// width so markdown wrapping stays correct.
func (pane *DetailPane) SetSize(width, height int) {
	previousWidth := pane.width
	pane.width = width
	pane.height = height
	pane.viewport.Width = pane.contentWidth()
	pane.viewport.Height = pane.bodyHeight()

	if pane.hasEntry && width != previousWidth {
		pane.rerender()
	}
}

// SetContent updates the detail pane with rendered content for a ticket.
// The now parameter drives scoring computation for the header signal
// indicators and is stored for consistent re-rendering on resize.
//
// When the displayed ticket changes (different entry ID), any active
// search is cleared because the search was against the previous
// ticket's body content.
func (pane *DetailPane) SetContent(source Source, entry ticketindex.Entry, now time.Time) {
	if entry.ID != pane.entry.ID {
		pane.search.Clear()
		// Viewport height may change when the search bar disappears.
		pane.viewport.Height = pane.bodyHeight()
	}

	pane.hasEntry = true
	pane.source = source
	pane.entry = entry
	pane.renderTime = now

	contentWidth := pane.contentWidth()
	renderer := NewDetailRenderer(pane.theme, contentWidth)
	score := pane.computeScore(source, entry, now)
	headerResult := renderer.RenderHeader(entry, score)
	pane.header = headerResult.Rendered
	pane.headerClickTargets = headerResult.Targets
	body, clickTargets := renderer.RenderBody(source, entry, now)

	// Wrap body to contentWidth so no line exceeds the viewport width.
	// Dependency/child/parent lines are already truncated by the
	// renderer, but markdown sections could have long lines that need
	// wrapping. This wrapping happens after click target line numbers
	// are computed, but deps/children/parent come before markdown
	// sections in the body, so their line numbers remain stable.
	body = lipgloss.NewStyle().Width(contentWidth).Render(body)

	// Detect and style inline ticket ID references. This runs after
	// width constraint so character positions are final. Existing
	// click targets (graph nodes, dep lists) are passed in to avoid
	// double-styling IDs that already have visual treatment.
	autolinkBody, autolinkTargets := detectAutolinks(body, clickTargets, source, pane.theme)
	body = autolinkBody
	clickTargets = append(clickTargets, autolinkTargets...)

	pane.clickTargets = clickTargets
	pane.rawBody = body
	pane.applySearchHighlighting()
	pane.viewport.GotoTop()
}

// Clear removes the detail pane content.
func (pane *DetailPane) Clear() {
	pane.hasEntry = false
	pane.source = nil
	pane.entry = ticketindex.Entry{}
	pane.header = ""
	pane.clickTargets = nil
	pane.headerClickTargets = nil
	pane.rawBody = ""
	pane.search.Clear()
	pane.viewport.SetContent("")
}

// rerender regenerates the content at the current width, preserving
// the scroll position as closely as possible.
func (pane *DetailPane) rerender() {
	previousOffset := pane.viewport.YOffset

	contentWidth := pane.contentWidth()
	renderer := NewDetailRenderer(pane.theme, contentWidth)
	score := pane.computeScore(pane.source, pane.entry, pane.renderTime)
	headerResult := renderer.RenderHeader(pane.entry, score)
	pane.header = headerResult.Rendered
	pane.headerClickTargets = headerResult.Targets
	body, clickTargets := renderer.RenderBody(pane.source, pane.entry, pane.renderTime)

	// Same width constraint as SetContent: ensure no line exceeds the
	// viewport width after re-rendering at the new width.
	body = lipgloss.NewStyle().Width(contentWidth).Render(body)

	// Detect and style autolinks (same as SetContent).
	autolinkBody, autolinkTargets := detectAutolinks(body, clickTargets, pane.source, pane.theme)
	body = autolinkBody
	clickTargets = append(clickTargets, autolinkTargets...)

	pane.clickTargets = clickTargets
	pane.rawBody = body
	pane.applySearchHighlighting()

	// Restore scroll position, clamped to the new content height.
	maxOffset := pane.viewport.TotalLineCount() - pane.viewport.Height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if previousOffset > maxOffset {
		previousOffset = maxOffset
	}
	pane.viewport.SetYOffset(previousOffset)
}

// computeScore returns a TicketScore pointer for the given ticket, or
// nil for closed tickets where scoring is not meaningful.
func (pane *DetailPane) computeScore(source Source, entry ticketindex.Entry, now time.Time) *ticketindex.TicketScore {
	if entry.Content.Status == "closed" {
		return nil
	}
	score := source.Score(entry.ID, now)
	return &score
}

// searchBarVisible returns whether the search bar should be displayed.
func (pane DetailPane) searchBarVisible() bool {
	return pane.search.Active || pane.search.Input != ""
}

// View renders the detail pane as a docked panel with a fixed header,
// scrollable body, optional search bar, left padding, and a right
// scrollbar.
func (pane DetailPane) View(focused bool) string {
	contentWidth := pane.contentWidth()

	if !pane.hasEntry {
		emptyStyle := lipgloss.NewStyle().
			Foreground(pane.theme.FaintText)

		contentStyle := lipgloss.NewStyle().
			PaddingLeft(1).
			Width(pane.width - 1).
			Height(pane.height)

		content := contentStyle.Render(
			lipgloss.Place(
				contentWidth, pane.height,
				lipgloss.Center, lipgloss.Center,
				emptyStyle.Render("Select a ticket to view details"),
			),
		)

		scrollbar := renderScrollbar(
			pane.theme, pane.height,
			0, pane.height, 0,
			focused,
		)
		return lipgloss.JoinHorizontal(lipgloss.Top, content, scrollbar)
	}

	// Build the content column as exactly pane.height lines.
	// Fixed header (detailHeaderLines) + scrollable body (remainder)
	// + optional search bar (1 line when visible).
	paddingStyle := lipgloss.NewStyle().
		PaddingLeft(1).
		Width(pane.width - 1)

	headerView := paddingStyle.Height(detailHeaderLines).Render(pane.header)

	bodyHeight := pane.bodyHeight()
	searchBarView := ""
	if pane.searchBarVisible() {
		searchBarView = "\n" + paddingStyle.Height(1).Render(
			pane.search.View(pane.theme, contentWidth))
	}

	bodyView := paddingStyle.Height(bodyHeight).Render(pane.viewport.View())
	content := headerView + "\n" + bodyView + searchBarView

	// Scrollbar: blank column for the header rows, actual scrollbar
	// for the body rows. This way the scrollbar only covers the
	// region it actually scrolls.
	headerColumn := lipgloss.NewStyle().
		Width(1).
		Height(detailHeaderLines).
		Render("")
	totalLines := pane.viewport.TotalLineCount()
	scrollbarHeight := bodyHeight
	if pane.searchBarVisible() {
		scrollbarHeight++
	}
	bodyScrollbar := renderScrollbar(
		pane.theme, scrollbarHeight,
		totalLines, pane.viewport.Height, pane.viewport.YOffset,
		focused,
	)
	scrollColumn := headerColumn + "\n" + bodyScrollbar

	return lipgloss.JoinHorizontal(lipgloss.Top, content, scrollColumn)
}

// ClickTarget returns the ticket ID at the given viewport-relative
// position in the body area, or empty string if the position is not
// a clickable entry. viewportY is relative to the top of the viewport
// (scroll offset is added internally). relativeX is the X position
// within the content area, used to match against each target's
// horizontal span.
func (pane DetailPane) ClickTarget(viewportY, relativeX int) string {
	bodyLine := pane.viewport.YOffset + viewportY
	for _, target := range pane.clickTargets {
		if target.Line == bodyLine && relativeX >= target.StartX && relativeX < target.EndX {
			return target.TicketID
		}
	}
	return ""
}

// HeaderTarget returns the field name at the given header-relative
// position, or empty string if the position is not a clickable
// header region. headerLine is 0-based within the header (0-4).
// relativeX is the X position within the content area (after left
// padding).
func (pane DetailPane) HeaderTarget(headerLine, relativeX int) string {
	for _, target := range pane.headerClickTargets {
		if target.Line == headerLine && relativeX >= target.StartX && relativeX < target.EndX {
			return target.Field
		}
	}
	return ""
}

// HoverTarget returns the full BodyClickTarget at the given
// viewport-relative position, or nil if the position is not a
// clickable entry. Used by the hover tooltip system to get the
// target's column bounds for bold highlighting.
func (pane DetailPane) HoverTarget(viewportY, relativeX int) *BodyClickTarget {
	bodyLine := pane.viewport.YOffset + viewportY
	for index := range pane.clickTargets {
		target := &pane.clickTargets[index]
		if target.Line == bodyLine && relativeX >= target.StartX && relativeX < target.EndX {
			return target
		}
	}
	return nil
}

// applySearchHighlighting sets the viewport content to the rawBody
// with search matches highlighted (or the raw body unchanged when
// there's no active search query). Called after any change to the
// rawBody or search query.
func (pane *DetailPane) applySearchHighlighting() {
	if pane.search.Input == "" {
		pane.viewport.SetContent(pane.rawBody)
		pane.search.SetMatches(nil)
		return
	}

	currentIndex := -1
	if pane.search.MatchCount() > 0 {
		currentIndex = pane.search.CurrentIndex()
	}
	highlighted, matches := highlightSearchMatches(
		pane.rawBody, pane.search.Input, currentIndex, pane.theme)
	pane.search.SetMatches(matches)
	pane.viewport.SetContent(highlighted)
}

// ScrollToCurrentMatch scrolls the viewport so the current search
// match is visible, centered vertically when possible.
func (pane *DetailPane) ScrollToCurrentMatch() {
	match := pane.search.CurrentMatch()
	if match == nil {
		return
	}

	// Center the match line in the viewport.
	targetOffset := match.Line - pane.viewport.Height/2
	if targetOffset < 0 {
		targetOffset = 0
	}
	maxOffset := pane.viewport.TotalLineCount() - pane.viewport.Height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if targetOffset > maxOffset {
		targetOffset = maxOffset
	}
	pane.viewport.SetYOffset(targetOffset)
}

// ScrollUp scrolls the detail pane up by half a page.
func (pane *DetailPane) ScrollUp() {
	pane.viewport.HalfViewUp()
}

// ScrollDown scrolls the detail pane down by half a page.
func (pane *DetailPane) ScrollDown() {
	pane.viewport.HalfViewDown()
}
