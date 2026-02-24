// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// Theme defines the color palette and visual properties for Bureau's
// terminal UIs. All colors use lipgloss ANSI 256-color codes for
// broad terminal compatibility.
//
// The fields cover both universal chrome (text, selection, borders)
// and semantic categories (priority, status) that recur across
// domains — tickets have priorities and statuses, pipelines have
// priorities and statuses, fleet machines have health statuses.
type Theme struct {
	// Text colors.
	NormalText lipgloss.Color
	FaintText  lipgloss.Color

	// Selected row.
	SelectedBackground lipgloss.Color
	SelectedForeground lipgloss.Color

	// Priority colors (indexed 0-4: critical, high, medium, low, backlog).
	PriorityColors [5]lipgloss.Color

	// Status colors.
	StatusOpen       lipgloss.Color
	StatusInProgress lipgloss.Color
	StatusReview     lipgloss.Color
	StatusBlocked    lipgloss.Color
	StatusClosed     lipgloss.Color

	// UI chrome.
	HeaderForeground lipgloss.Color
	BorderColor      lipgloss.Color
	HelpText         lipgloss.Color

	// Animation accents: background tint for recently-changed items.
	// HotAccentPut is used for created/updated items; HotAccentRemove
	// for items that left the view.
	HotAccentPut    lipgloss.Color
	HotAccentRemove lipgloss.Color

	// Search and filter match highlighting.
	SearchHighlightBackground lipgloss.Color // Background tint for matched characters.
	SearchCurrentBackground   lipgloss.Color // Background for the current search match.

	// Autolinked references.
	LinkForeground lipgloss.Color // Foreground color for inline reference links.

	// Hover tooltips.
	TooltipForeground lipgloss.Color // Text color inside tooltip boxes.
	TooltipBackground lipgloss.Color // Background color for tooltip boxes.
}

// PriorityColor returns the color for a priority level (0-4).
// Out-of-range values return NormalText.
func (theme Theme) PriorityColor(priority int) lipgloss.Color {
	if priority < 0 || priority >= len(theme.PriorityColors) {
		return theme.NormalText
	}
	return theme.PriorityColors[priority]
}

// StatusColor returns the color for a status string. Recognizes the
// five standard statuses (open, in_progress, review, blocked, closed)
// and returns FaintText for unknown values.
func (theme Theme) StatusColor(status string) lipgloss.Color {
	switch status {
	case string(ticket.StatusOpen):
		return theme.StatusOpen
	case string(ticket.StatusInProgress):
		return theme.StatusInProgress
	case string(ticket.StatusReview):
		return theme.StatusReview
	case string(ticket.StatusBlocked):
		return theme.StatusBlocked
	case string(ticket.StatusClosed):
		return theme.StatusClosed
	default:
		return theme.FaintText
	}
}

// DefaultTheme is the built-in dark-terminal color scheme. Designed for
// 256-color terminals with a dark background (the common case for
// development environments and tmux sessions).
var DefaultTheme = Theme{
	NormalText: lipgloss.Color("252"),
	FaintText:  lipgloss.Color("245"),

	SelectedBackground: lipgloss.Color("236"),
	SelectedForeground: lipgloss.Color("255"),

	PriorityColors: [5]lipgloss.Color{
		lipgloss.Color("196"), // P0 critical: bright red
		lipgloss.Color("208"), // P1 high: orange
		lipgloss.Color("75"),  // P2 medium: blue
		lipgloss.Color("245"), // P3 low: gray
		lipgloss.Color("240"), // P4 backlog: dim gray
	},

	StatusOpen:       lipgloss.Color("114"), // green
	StatusInProgress: lipgloss.Color("220"), // yellow/amber
	StatusReview:     lipgloss.Color("141"), // light purple
	StatusBlocked:    lipgloss.Color("196"), // red
	StatusClosed:     lipgloss.Color("245"), // gray

	HeaderForeground: lipgloss.Color("255"),
	BorderColor:      lipgloss.Color("240"),
	HelpText:         lipgloss.Color("241"),

	HotAccentPut:    lipgloss.Color("58"), // dark amber background tint
	HotAccentRemove: lipgloss.Color("52"), // dark red background tint

	SearchHighlightBackground: lipgloss.Color("58"),  // dark amber (matches HotAccentPut)
	SearchCurrentBackground:   lipgloss.Color("100"), // brighter amber for current match

	LinkForeground: lipgloss.Color("75"), // blue (informational, matches P2 priority)

	TooltipForeground: lipgloss.Color("252"), // same as NormalText
	TooltipBackground: lipgloss.Color("237"), // slightly lighter than terminal background
}
