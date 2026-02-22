// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	ticketschema "github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func TestRenderTooltip_BasicContent(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "Fix connection pooling leak",
		Status:   "open",
		Priority: 1,
		Body:     "The connection pool is leaking under high load.",
	}

	lines := renderTooltip("tkt-001", content, DefaultTheme, tooltipMaxWidth)

	if len(lines) < 3 {
		testing.Fatalf("expected at least 3 lines (meta + title + desc), got %d", len(lines))
	}

	// All lines should have the same visible width.
	firstWidth := ansi.StringWidth(lines[0])
	for index, line := range lines {
		width := ansi.StringWidth(line)
		if width != firstWidth {
			testing.Errorf("line %d width %d != first line width %d", index, width, firstWidth)
		}
	}

	// Line 1 should contain status and ticket ID.
	meta := ansi.Strip(lines[0])
	if !strings.Contains(meta, "OPEN") {
		testing.Errorf("meta line missing status: %q", meta)
	}
	if !strings.Contains(meta, "P1") {
		testing.Errorf("meta line missing priority: %q", meta)
	}
	if !strings.Contains(meta, "tkt-001") {
		testing.Errorf("meta line missing ticket ID: %q", meta)
	}

	// Line 2 should contain the title.
	titleLine := ansi.Strip(lines[1])
	if !strings.Contains(titleLine, "Fix connection pooling leak") {
		testing.Errorf("title line missing title: %q", titleLine)
	}

	// Line 3 should contain description excerpt.
	descLine := ansi.Strip(lines[2])
	if !strings.Contains(descLine, "connection pool") {
		testing.Errorf("desc line missing excerpt: %q", descLine)
	}
}

func TestRenderTooltip_NoDescription(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "Short task",
		Status:   "closed",
		Priority: 3,
	}

	lines := renderTooltip("tkt-002", content, DefaultTheme, tooltipMaxWidth)

	// Should have meta + title = 2 lines, no description.
	if len(lines) != 2 {
		testing.Fatalf("expected 2 lines (no description), got %d", len(lines))
	}
}

func TestRenderTooltip_NoTitle(testing *testing.T) {
	content := ticketschema.TicketContent{
		Status:   "open",
		Priority: 0,
	}

	lines := renderTooltip("tkt-003", content, DefaultTheme, tooltipMaxWidth)

	// Should have meta only = 1 line.
	if len(lines) != 1 {
		testing.Fatalf("expected 1 line (no title, no description), got %d", len(lines))
	}
}

func TestRenderTooltip_LongTitleTruncated(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "This is a very long title that definitely exceeds the maximum tooltip width and should be truncated with an ellipsis character",
		Status:   "open",
		Priority: 2,
	}

	lines := renderTooltip("tkt-004", content, DefaultTheme, tooltipMaxWidth)

	// Title line should fit within tooltip width.
	titleWidth := ansi.StringWidth(lines[1])
	if titleWidth > tooltipMaxWidth {
		testing.Errorf("title line width %d exceeds max %d", titleWidth, tooltipMaxWidth)
	}
}

func TestRenderTooltip_MultilineDescription(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "Task",
		Status:   "open",
		Priority: 2,
		Body:     "First line of description.\nSecond line.\nThird line should not appear.",
	}

	lines := renderTooltip("tkt-005", content, DefaultTheme, tooltipMaxWidth)

	// meta + title + 2 desc lines = 4 lines max.
	if len(lines) > 4 {
		testing.Fatalf("expected at most 4 lines, got %d", len(lines))
	}

	// Third description line should not appear.
	allText := ""
	for _, line := range lines {
		allText += ansi.Strip(line) + "\n"
	}
	if strings.Contains(allText, "Third line") {
		testing.Error("third description line should be excluded")
	}
}

func TestRenderTooltip_DescriptionSkipsBlankLines(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "Task",
		Status:   "open",
		Priority: 2,
		Body:     "\n\n  \nActual content here.",
	}

	lines := renderTooltip("tkt-006", content, DefaultTheme, tooltipMaxWidth)

	// Should show the non-blank line, not empty lines.
	found := false
	for _, line := range lines {
		if strings.Contains(ansi.Strip(line), "Actual content") {
			found = true
		}
	}
	if !found {
		testing.Error("expected description content after skipping blank lines")
	}
}

func TestRenderTooltip_ConsistentWidth(testing *testing.T) {
	content := ticketschema.TicketContent{
		Title:    "Short",
		Status:   "in_progress",
		Priority: 0,
		Body:     "Short desc.",
	}

	lines := renderTooltip("tkt-007", content, DefaultTheme, tooltipMaxWidth)

	// Every line should be exactly the same visible width.
	widths := make(map[int]bool)
	for _, line := range lines {
		widths[ansi.StringWidth(line)] = true
	}
	if len(widths) != 1 {
		testing.Errorf("tooltip lines have inconsistent widths: %v", widths)
	}
}

func TestOverlayTooltip_BasicOverlay(testing *testing.T) {
	// Create a simple 5-line view, each line 20 chars.
	viewLines := []string{
		"aaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccc",
		"dddddddddddddddddddd",
		"eeeeeeeeeeeeeeeeeeee",
	}
	view := strings.Join(viewLines, "\n")

	tooltipLines := []string{
		"XXXX",
		"YYYY",
	}

	result := overlayTooltip(view, tooltipLines, 5, 1)
	resultViewLines := strings.Split(result, "\n")

	// Line 0 unchanged.
	if ansi.Strip(resultViewLines[0]) != "aaaaaaaaaaaaaaaaaaaa" {
		testing.Errorf("line 0 should be unchanged: %q", ansi.Strip(resultViewLines[0]))
	}

	// Line 1 should have tooltip spliced at position 5.
	stripped1 := ansi.Strip(resultViewLines[1])
	if !strings.Contains(stripped1, "XXXX") {
		testing.Errorf("line 1 missing tooltip content: %q", stripped1)
	}
	// Prefix "bbbbb" should be preserved.
	if !strings.HasPrefix(stripped1, "bbbbb") {
		testing.Errorf("line 1 missing prefix: %q", stripped1)
	}

	// Line 2 should have second tooltip line.
	stripped2 := ansi.Strip(resultViewLines[2])
	if !strings.Contains(stripped2, "YYYY") {
		testing.Errorf("line 2 missing tooltip content: %q", stripped2)
	}

	// Lines 3-4 unchanged.
	if !strings.HasPrefix(ansi.Strip(resultViewLines[3]), "d") {
		testing.Errorf("line 3 should be unchanged: %q", ansi.Strip(resultViewLines[3]))
	}
}

func TestOverlayTooltip_AtEdge(testing *testing.T) {
	view := "aaaaaaaaaa\nbbbbbbbbbb"
	tooltip := []string{"XX"}

	// Overlay at right edge.
	result := overlayTooltip(view, tooltip, 8, 0)
	stripped := ansi.Strip(strings.Split(result, "\n")[0])
	if !strings.Contains(stripped, "XX") {
		testing.Errorf("overlay at edge missing content: %q", stripped)
	}
}

func TestOverlayTooltip_OutOfBounds(testing *testing.T) {
	view := "aaaa\nbbbb"
	tooltip := []string{"XX"}

	// Overlay beyond view bounds — should not panic.
	result := overlayTooltip(view, tooltip, 0, 10)
	if result != view {
		testing.Error("overlay out of bounds should return view unchanged")
	}
}

func TestOverlayTooltip_EmptyTooltip(testing *testing.T) {
	view := "aaaa\nbbbb"
	result := overlayTooltip(view, nil, 0, 0)
	if result != view {
		testing.Error("empty tooltip should return view unchanged")
	}
}

func TestOverlayTooltip_PreservesANSI(testing *testing.T) {
	// View with ANSI styling.
	view := "\x1b[31mred text here\x1b[0m\nnormal line here"
	tooltip := []string{"TIP"}

	result := overlayTooltip(view, tooltip, 4, 0)
	lines := strings.Split(result, "\n")

	// Tooltip should be in the output.
	if !strings.Contains(ansi.Strip(lines[0]), "TIP") {
		testing.Errorf("tooltip not found in ANSI line: %q", ansi.Strip(lines[0]))
	}

	// Second line should be unchanged.
	if ansi.Strip(lines[1]) != "normal line here" {
		testing.Errorf("second line changed: %q", ansi.Strip(lines[1]))
	}
}

func TestOverlayBold_BasicBold(testing *testing.T) {
	view := "hello world\nsecond line"
	result := overlayBold(view, 0, 6, 11)
	lines := strings.Split(result, "\n")

	// The bolded region should contain "world".
	stripped := ansi.Strip(lines[0])
	if stripped != "hello world" {
		testing.Errorf("visible text should be unchanged: %q", stripped)
	}

	// The raw output should contain bold-on and bold-off sequences.
	if !strings.Contains(lines[0], "\x1b[1m") {
		testing.Error("missing bold-on escape")
	}
	if !strings.Contains(lines[0], "\x1b[22m") {
		testing.Error("missing bold-off escape")
	}

	// Second line should be unchanged.
	if lines[1] != "second line" {
		testing.Errorf("second line should be unchanged: %q", lines[1])
	}
}

func TestOverlayBold_OutOfBounds(testing *testing.T) {
	view := "hello\nworld"

	// Row out of bounds — should return unchanged.
	result := overlayBold(view, 5, 0, 3)
	if result != view {
		testing.Error("out-of-bounds row should return view unchanged")
	}

	// Start beyond line width — should return unchanged.
	result = overlayBold(view, 0, 100, 200)
	if result != view {
		testing.Error("start beyond line width should return view unchanged")
	}

	// Empty range — should return unchanged.
	result = overlayBold(view, 0, 3, 3)
	if result != view {
		testing.Error("empty range should return view unchanged")
	}
}

func TestOverlayBold_WithExistingANSI(testing *testing.T) {
	// Line with existing color escapes.
	view := "\x1b[31mred text\x1b[0m normal"
	result := overlayBold(view, 0, 0, 3)

	// Visible text should be preserved.
	stripped := ansi.Strip(result)
	if stripped != "red text normal" {
		testing.Errorf("visible text should be unchanged: %q", stripped)
	}

	// Bold sequences should be present.
	if !strings.Contains(result, "\x1b[1m") {
		testing.Error("missing bold-on escape in ANSI input")
	}
}

func TestOverlayBold_SurvivesResetsWithinRange(testing *testing.T) {
	// Simulate a dependency list entry where each styled segment ends
	// with \x1b[0m (full reset). Bold must persist across all segments.
	//
	// Layout: icon + " " + priority + " " + ticketID
	// Each segment is wrapped by lipgloss: \x1b[color]text\x1b[0m
	line := "\x1b[38;5;248m●\x1b[0m \x1b[38;5;214mP2\x1b[0m \x1b[38;5;248mtkt-1234\x1b[0m title"
	// Visible: "● P2 tkt-1234 title" (18 chars)
	// Bold the entire entry: positions 0 through 12 ("● P2 tkt-1234")
	result := overlayBold(line, 0, 0, 13)

	// Visible text preserved.
	stripped := ansi.Strip(result)
	if stripped != "● P2 tkt-1234 title" {
		testing.Errorf("visible text changed: %q", stripped)
	}

	// Bold must be re-asserted after each reset within the range.
	// Count the number of \x1b[1m injections — there should be
	// multiple (at least after each \x1b[0m within the range).
	boldCount := strings.Count(result, "\x1b[1m")
	if boldCount < 3 {
		testing.Errorf("expected bold to be re-asserted multiple times, got %d bold-on sequences", boldCount)
	}

	// The last bold-related sequence before "title" should be
	// \x1b[22m (bold off), not \x1b[1m.
	titleByteIndex := strings.Index(result, "title")
	if titleByteIndex < 0 {
		testing.Fatal("'title' not found in result")
	}
	beforeTitle := result[:titleByteIndex]
	lastBoldOn := strings.LastIndex(beforeTitle, "\x1b[1m")
	lastBoldOff := strings.LastIndex(beforeTitle, "\x1b[22m")
	if lastBoldOff < lastBoldOn {
		testing.Error("bold should be turned off before the text outside the range")
	}
}

func TestOverlayBold_MiddleOfStyledLine(testing *testing.T) {
	// Bold a region in the middle of a line with multiple styled segments.
	line := "aaa\x1b[32mbbb\x1b[0m\x1b[33mccc\x1b[0mddd"
	// Visible: "aaabbbcccddd" (12 chars)
	// Bold positions 3-6 = "bbb"
	result := overlayBold(line, 0, 3, 6)

	stripped := ansi.Strip(result)
	if stripped != "aaabbbcccddd" {
		testing.Errorf("visible text changed: %q", stripped)
	}

	if !strings.Contains(result, "\x1b[1m") {
		testing.Error("bold-on missing")
	}
	if !strings.Contains(result, "\x1b[22m") {
		testing.Error("bold-off missing")
	}
}

func TestExtractDescriptionExcerpt(testing *testing.T) {
	tests := []struct {
		name     string
		body     string
		maxWidth int
		maxLines int
		expected int // Expected number of lines.
	}{
		{"empty", "", 40, 2, 0},
		{"single line", "Hello world", 40, 2, 1},
		{"two lines", "Line one\nLine two", 40, 2, 2},
		{"skips blanks", "\n\nContent", 40, 2, 1},
		{"caps at maxLines", "A\nB\nC\nD", 40, 2, 2},
		{"whitespace only lines skipped", "  \n\t\nReal content", 40, 2, 1},
	}

	for _, test := range tests {
		result := extractDescriptionExcerpt(test.body, test.maxWidth, test.maxLines)
		if len(result) != test.expected {
			testing.Errorf("%s: expected %d lines, got %d: %v",
				test.name, test.expected, len(result), result)
		}
	}
}
