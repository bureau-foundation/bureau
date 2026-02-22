// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// RenderScrollbar produces a single-column scrollbar of the given height.
// The thumb indicates the visible region within the total content.
//
// The scrollbar is always fully rendered: track + thumb. When content fits
// within the visible area the thumb spans the entire height. The thumb
// uses the accent color when focused, and a dim color when unfocused.
func RenderScrollbar(theme Theme, height, totalItems, visibleItems, scrollOffset int, focused bool) string {
	if height <= 0 {
		return ""
	}

	trackColor := theme.BorderColor
	thumbColor := theme.BorderColor
	if focused {
		thumbColor = theme.StatusInProgress
	}

	trackStyle := lipgloss.NewStyle().Foreground(trackColor)
	thumbStyle := lipgloss.NewStyle().Foreground(thumbColor)

	lines := make([]string, height)

	// Content fits — thumb spans the full height.
	if totalItems <= visibleItems || totalItems <= 0 {
		for index := range lines {
			lines[index] = thumbStyle.Render("┃")
		}
		return strings.Join(lines, "\n")
	}

	// Thumb size: proportional to visible/total, minimum 1 row.
	thumbSize := height * visibleItems / totalItems
	if thumbSize < 1 {
		thumbSize = 1
	}

	// Thumb position: proportional to scroll offset within scrollable range.
	scrollableRange := totalItems - visibleItems
	trackRange := height - thumbSize
	thumbOffset := 0
	if scrollableRange > 0 && trackRange > 0 {
		thumbOffset = scrollOffset * trackRange / scrollableRange
	}
	if thumbOffset+thumbSize > height {
		thumbOffset = height - thumbSize
	}

	for index := range lines {
		if index >= thumbOffset && index < thumbOffset+thumbSize {
			lines[index] = thumbStyle.Render("┃")
		} else {
			lines[index] = trackStyle.Render("│")
		}
	}

	return strings.Join(lines, "\n")
}
