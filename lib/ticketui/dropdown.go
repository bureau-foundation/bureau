// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// DropdownOption is a single selectable item in a dropdown overlay.
type DropdownOption struct {
	Label string // Display text shown in the dropdown.
	Value string // Wire value sent to the service on selection.
}

// DropdownOverlay renders a floating menu anchored at a screen
// position. It captures all keyboard input when active (up/down to
// navigate, enter to select, escape to dismiss). The model owns the
// dropdown instance and routes input to it when FocusDropdown is set.
type DropdownOverlay struct {
	Options  []DropdownOption
	Cursor   int
	AnchorX  int    // Screen X coordinate of the dropdown's top-left corner.
	AnchorY  int    // Screen Y coordinate of the dropdown's top-left corner.
	Field    string // Which field this dropdown mutates ("status" or "priority").
	TicketID string // The ticket being mutated.
}

// dropdownSelectMsg is sent when the user selects a dropdown option.
// The model handles this message to dispatch the mutation.
type dropdownSelectMsg struct {
	field    string
	ticketID string
	value    string
}

// StatusTransitions returns the valid status transitions for a given
// current status. Each transition is a DropdownOption with the status
// value as both label and value.
func StatusTransitions(currentStatus string) []DropdownOption {
	switch currentStatus {
	case "open":
		return []DropdownOption{
			{Label: "IN_PROGRESS", Value: "in_progress"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "in_progress":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
			{Label: "BLOCKED", Value: "blocked"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "blocked":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
			{Label: "IN_PROGRESS", Value: "in_progress"},
			{Label: "CLOSED", Value: "closed"},
		}
	case "closed":
		return []DropdownOption{
			{Label: "OPEN", Value: "open"},
		}
	default:
		return nil
	}
}

// PriorityOptions returns dropdown options for priority selection,
// P0 through P4.
func PriorityOptions() []DropdownOption {
	return []DropdownOption{
		{Label: "P0", Value: "0"},
		{Label: "P1", Value: "1"},
		{Label: "P2", Value: "2"},
		{Label: "P3", Value: "3"},
		{Label: "P4", Value: "4"},
	}
}

// MoveUp moves the cursor up by one, wrapping to the bottom.
func (dropdown *DropdownOverlay) MoveUp() {
	dropdown.Cursor--
	if dropdown.Cursor < 0 {
		dropdown.Cursor = len(dropdown.Options) - 1
	}
}

// MoveDown moves the cursor down by one, wrapping to the top.
func (dropdown *DropdownOverlay) MoveDown() {
	dropdown.Cursor++
	if dropdown.Cursor >= len(dropdown.Options) {
		dropdown.Cursor = 0
	}
}

// Selected returns the currently highlighted option.
func (dropdown *DropdownOverlay) Selected() DropdownOption {
	return dropdown.Options[dropdown.Cursor]
}

// Render produces the dropdown lines for overlay splicing. Each line
// has the same visible width (including left/right padding) and a
// solid background for visual separation from the underlying content.
// The currently highlighted option uses a contrasting background.
func (dropdown *DropdownOverlay) Render(theme Theme) []string {
	// Compute the width: longest label + cursor marker + padding.
	maxLabelWidth := 0
	for _, option := range dropdown.Options {
		labelWidth := ansi.StringWidth(option.Label)
		if labelWidth > maxLabelWidth {
			maxLabelWidth = labelWidth
		}
	}
	// Layout: " > LABEL  " — 3 chars prefix (space + marker + space),
	// then label, then right padding to fill.
	innerWidth := 3 + maxLabelWidth
	totalWidth := innerWidth + 2 // 1 char padding on each side.

	backgroundStyle := lipgloss.NewStyle().
		Background(theme.TooltipBackground)
	selectedBackground := lipgloss.NewStyle().
		Background(theme.SelectedBackground).
		Foreground(theme.SelectedForeground)

	var lines []string
	for index, option := range dropdown.Options {
		marker := " "
		if index == dropdown.Cursor {
			marker = ">"
		}

		prefix := marker + " "
		content := prefix + option.Label
		contentWidth := ansi.StringWidth(content)
		rightPad := innerWidth - contentWidth
		if rightPad < 0 {
			rightPad = 0
		}
		paddedContent := content + strings.Repeat(" ", rightPad)

		var styledLine string
		if index == dropdown.Cursor {
			styledLine = selectedBackground.Render(" " + paddedContent + " ")
		} else {
			styledLine = backgroundStyle.Render(" " + paddedContent + " ")
		}

		// Ensure consistent visible width across all lines.
		lineWidth := ansi.StringWidth(styledLine)
		if lineWidth < totalWidth {
			if index == dropdown.Cursor {
				styledLine += selectedBackground.Render(strings.Repeat(" ", totalWidth-lineWidth))
			} else {
				styledLine += backgroundStyle.Render(strings.Repeat(" ", totalWidth-lineWidth))
			}
		}

		lines = append(lines, styledLine)
	}

	return lines
}
