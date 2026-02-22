// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// NoteModal is a modal overlay for adding multi-line text input. It
// implements a simple text editor with cursor tracking, rendered as a
// centered overlay on top of the main view.
type NoteModal struct {
	// ContextLabel identifies what this note is being added to,
	// shown in the modal title (e.g., "tkt-001", "pip-042").
	ContextLabel string

	lines   [][]rune // Each line is a slice of runes.
	cursorY int      // Current line index.
	cursorX int      // Cursor position within the current line.
	theme   Theme
}

// NewNoteModal creates a NoteModal for adding a note in the given
// context. The textarea starts empty and focused.
func NewNoteModal(contextLabel string, theme Theme) NoteModal {
	return NoteModal{
		ContextLabel: contextLabel,
		lines:        [][]rune{{}},
		cursorY:      0,
		cursorX:      0,
		theme:        theme,
	}
}

// Value returns the current text content of the note.
func (modal NoteModal) Value() string {
	var parts []string
	for _, line := range modal.lines {
		parts = append(parts, string(line))
	}
	return strings.Join(parts, "\n")
}

// Update processes a key message for the note modal's text editor.
func (modal *NoteModal) Update(message tea.KeyMsg) {
	switch message.Type {
	case tea.KeyRunes, tea.KeySpace:
		for _, character := range message.Runes {
			modal.insertRune(character)
		}

	case tea.KeyEnter:
		// Split the current line at the cursor.
		line := modal.lines[modal.cursorY]
		before := make([]rune, modal.cursorX)
		copy(before, line[:modal.cursorX])
		after := make([]rune, len(line)-modal.cursorX)
		copy(after, line[modal.cursorX:])

		modal.lines[modal.cursorY] = before
		// Insert a new line after the current one.
		newLines := make([][]rune, len(modal.lines)+1)
		copy(newLines, modal.lines[:modal.cursorY+1])
		newLines[modal.cursorY+1] = after
		copy(newLines[modal.cursorY+2:], modal.lines[modal.cursorY+1:])
		modal.lines = newLines
		modal.cursorY++
		modal.cursorX = 0

	case tea.KeyBackspace:
		if modal.cursorX > 0 {
			line := modal.lines[modal.cursorY]
			modal.lines[modal.cursorY] = append(line[:modal.cursorX-1], line[modal.cursorX:]...)
			modal.cursorX--
		} else if modal.cursorY > 0 {
			// Merge with previous line.
			previousLine := modal.lines[modal.cursorY-1]
			currentLine := modal.lines[modal.cursorY]
			modal.cursorX = len(previousLine)
			modal.lines[modal.cursorY-1] = append(previousLine, currentLine...)
			modal.lines = append(modal.lines[:modal.cursorY], modal.lines[modal.cursorY+1:]...)
			modal.cursorY--
		}

	case tea.KeyDelete:
		line := modal.lines[modal.cursorY]
		if modal.cursorX < len(line) {
			modal.lines[modal.cursorY] = append(line[:modal.cursorX], line[modal.cursorX+1:]...)
		} else if modal.cursorY < len(modal.lines)-1 {
			// Merge with next line.
			nextLine := modal.lines[modal.cursorY+1]
			modal.lines[modal.cursorY] = append(line, nextLine...)
			modal.lines = append(modal.lines[:modal.cursorY+1], modal.lines[modal.cursorY+2:]...)
		}

	case tea.KeyLeft:
		if modal.cursorX > 0 {
			modal.cursorX--
		} else if modal.cursorY > 0 {
			modal.cursorY--
			modal.cursorX = len(modal.lines[modal.cursorY])
		}

	case tea.KeyRight:
		line := modal.lines[modal.cursorY]
		if modal.cursorX < len(line) {
			modal.cursorX++
		} else if modal.cursorY < len(modal.lines)-1 {
			modal.cursorY++
			modal.cursorX = 0
		}

	case tea.KeyUp:
		if modal.cursorY > 0 {
			modal.cursorY--
			if modal.cursorX > len(modal.lines[modal.cursorY]) {
				modal.cursorX = len(modal.lines[modal.cursorY])
			}
		}

	case tea.KeyDown:
		if modal.cursorY < len(modal.lines)-1 {
			modal.cursorY++
			if modal.cursorX > len(modal.lines[modal.cursorY]) {
				modal.cursorX = len(modal.lines[modal.cursorY])
			}
		}

	case tea.KeyHome, tea.KeyCtrlA:
		modal.cursorX = 0

	case tea.KeyEnd, tea.KeyCtrlE:
		modal.cursorX = len(modal.lines[modal.cursorY])
	}
}

// insertRune inserts a single rune at the cursor position.
func (modal *NoteModal) insertRune(character rune) {
	line := modal.lines[modal.cursorY]
	newLine := make([]rune, len(line)+1)
	copy(newLine, line[:modal.cursorX])
	newLine[modal.cursorX] = character
	copy(newLine[modal.cursorX+1:], line[modal.cursorX:])
	modal.lines[modal.cursorY] = newLine
	modal.cursorX++
}

// Modal chrome overhead: 2 columns border + 2 columns padding = 4
// columns horizontal; 2 lines border + 1 title + 1 footer = 4 lines
// vertical. The inner text area gets the remainder.
const (
	noteModalChromeWidth  = 4
	noteModalChromeHeight = 4
	// Minimum inner text area: 30 columns wide, 5 lines tall. Below
	// this the editor is too cramped to be useful.
	noteModalMinInnerWidth  = 30
	noteModalMinInnerHeight = 5
	// Margin between the modal edge and the screen edge, so the user
	// can see the underlying view isn't gone. 2 lines/columns on each
	// side when there's room; collapses to 0 on very small screens.
	noteModalMargin = 2
)

// Render produces the modal overlay lines for splicing onto the view.
// Returns the rendered lines and the anchor position (top-left corner
// in screen coordinates).
func (modal NoteModal) Render(screenWidth, screenHeight int) ([]string, int, int) {
	// Size the modal to fill the screen minus a margin, but never
	// smaller than the minimum inner area plus chrome. On very small
	// screens the margin shrinks to zero before the inner area does.
	modalWidth := screenWidth - noteModalMargin*2
	modalHeight := screenHeight - noteModalMargin*2

	minWidth := noteModalMinInnerWidth + noteModalChromeWidth
	minHeight := noteModalMinInnerHeight + noteModalChromeHeight
	if modalWidth < minWidth {
		modalWidth = minWidth
	}
	if modalHeight < minHeight {
		modalHeight = minHeight
	}
	// Clamp to screen bounds so the overlay doesn't extend past the
	// terminal edges even when the minimum exceeds the screen.
	if modalWidth > screenWidth {
		modalWidth = screenWidth
	}
	if modalHeight > screenHeight {
		modalHeight = screenHeight
	}

	innerWidth := modalWidth - noteModalChromeWidth
	innerHeight := modalHeight - noteModalChromeHeight

	bgStyle := lipgloss.NewStyle().
		Background(modal.theme.TooltipBackground)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(modal.theme.HeaderForeground).
		Background(modal.theme.TooltipBackground)

	footerStyle := lipgloss.NewStyle().
		Foreground(modal.theme.FaintText).
		Background(modal.theme.TooltipBackground)

	cursorStyle := lipgloss.NewStyle().
		Reverse(true)

	textStyle := lipgloss.NewStyle().
		Foreground(modal.theme.NormalText).
		Background(modal.theme.TooltipBackground)

	// Build title line.
	title := titleStyle.Render("Add Note to " + modal.ContextLabel)
	titleWidth := ansi.StringWidth(title)
	if titleWidth < innerWidth {
		title += bgStyle.Render(strings.Repeat(" ", innerWidth-titleWidth))
	}

	// Build footer line.
	footer := footerStyle.Render("Ctrl+D submit  Esc cancel")
	footerWidth := ansi.StringWidth(footer)
	if footerWidth < innerWidth {
		footer += bgStyle.Render(strings.Repeat(" ", innerWidth-footerWidth))
	}

	// Build text area lines with cursor.
	var textLines []string
	// Scroll the view if the cursor is past the visible area.
	scrollOffset := 0
	if modal.cursorY >= innerHeight {
		scrollOffset = modal.cursorY - innerHeight + 1
	}

	for lineIndex := scrollOffset; lineIndex < scrollOffset+innerHeight; lineIndex++ {
		var renderedLine string
		if lineIndex < len(modal.lines) {
			line := modal.lines[lineIndex]
			if lineIndex == modal.cursorY {
				// Render with cursor.
				if modal.cursorX >= len(line) {
					renderedLine = textStyle.Render(string(line)) + cursorStyle.Render(" ")
				} else {
					before := textStyle.Render(string(line[:modal.cursorX]))
					atCursor := cursorStyle.Render(string(line[modal.cursorX : modal.cursorX+1]))
					after := textStyle.Render(string(line[modal.cursorX+1:]))
					renderedLine = before + atCursor + after
				}
			} else {
				renderedLine = textStyle.Render(string(line))
			}
		}

		// Pad to inner width.
		lineWidth := ansi.StringWidth(renderedLine)
		if lineWidth < innerWidth {
			renderedLine += bgStyle.Render(strings.Repeat(" ", innerWidth-lineWidth))
		}
		textLines = append(textLines, renderedLine)
	}

	// Assemble the modal content inside a border.
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(modal.theme.BorderColor).
		Background(modal.theme.TooltipBackground)

	inner := title + "\n" + strings.Join(textLines, "\n") + "\n" + footer
	rendered := borderStyle.Render(inner)

	// Split into lines and compute anchor for centering.
	resultLines := strings.Split(rendered, "\n")
	renderedWidth := 0
	if len(resultLines) > 0 {
		renderedWidth = ansi.StringWidth(resultLines[0])
	}

	anchorX := (screenWidth - renderedWidth) / 2
	anchorY := (screenHeight - len(resultLines)) / 2
	if anchorX < 0 {
		anchorX = 0
	}
	if anchorY < 0 {
		anchorY = 0
	}

	return resultLines, anchorX, anchorY
}
