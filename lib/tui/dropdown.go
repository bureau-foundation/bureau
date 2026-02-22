// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

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
// dropdown instance and routes input to it when focus is set.
type DropdownOverlay struct {
	Options []DropdownOption
	Cursor  int
	AnchorX int    // Screen X coordinate of the dropdown's top-left corner.
	AnchorY int    // Screen Y coordinate of the dropdown's top-left corner.
	Field   string // Which field this dropdown mutates (e.g., "status", "priority", "assignee").
	ItemID  string // The item being mutated.
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
