// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// autolinkTestSource creates a source with known tickets for autolink tests.
func autolinkTestSource() *IndexSource {
	index := ticket.NewIndex()
	index.Put("tkt-001", schema.TicketContent{
		Title:    "First ticket",
		Status:   "open",
		Priority: 1,
	})
	index.Put("tkt-002", schema.TicketContent{
		Title:    "Second ticket",
		Status:   "closed",
		Priority: 2,
	})
	index.Put("epic-1", schema.TicketContent{
		Title:    "Epic one",
		Status:   "open",
		Priority: 0,
		Type:     "epic",
	})
	index.Put("tkt-3abc", schema.TicketContent{
		Title:    "Hex-suffix ticket",
		Status:   "in_progress",
		Priority: 2,
	})
	return NewIndexSource(index)
}

func TestDetectAutolinks_BasicDetection(testing *testing.T) {
	source := autolinkTestSource()
	body := "See tkt-001 for details."

	styled, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 1 {
		testing.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].TicketID != "tkt-001" {
		testing.Errorf("expected ticket ID tkt-001, got %s", targets[0].TicketID)
	}
	if targets[0].Line != 0 {
		testing.Errorf("expected line 0, got %d", targets[0].Line)
	}
	// "See " is 4 runes, "tkt-001" is 7 runes.
	if targets[0].StartX != 4 {
		testing.Errorf("expected StartX 4, got %d", targets[0].StartX)
	}
	if targets[0].EndX != 11 {
		testing.Errorf("expected EndX 11, got %d", targets[0].EndX)
	}

	// Visible text should be unchanged.
	if ansi.Strip(styled) != body {
		testing.Errorf("visible text changed:\ngot:  %q\nwant: %q", ansi.Strip(styled), body)
	}

	// Styled output should contain ANSI escapes (underline + color).
	if styled == body {
		testing.Error("expected ANSI escapes in styled output, got plain text")
	}
}

func TestDetectAutolinks_MultipleMatches(testing *testing.T) {
	source := autolinkTestSource()
	body := "Blocked by tkt-001 and tkt-002."

	_, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 2 {
		testing.Fatalf("expected 2 targets, got %d", len(targets))
	}
	if targets[0].TicketID != "tkt-001" {
		testing.Errorf("first target: expected tkt-001, got %s", targets[0].TicketID)
	}
	if targets[1].TicketID != "tkt-002" {
		testing.Errorf("second target: expected tkt-002, got %s", targets[1].TicketID)
	}
}

func TestDetectAutolinks_MultipleLines(testing *testing.T) {
	source := autolinkTestSource()
	body := "Line one with tkt-001.\nLine two with epic-1."

	_, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 2 {
		testing.Fatalf("expected 2 targets, got %d", len(targets))
	}
	if targets[0].Line != 0 {
		testing.Errorf("first target line: expected 0, got %d", targets[0].Line)
	}
	if targets[1].Line != 1 {
		testing.Errorf("second target line: expected 1, got %d", targets[1].Line)
	}
	if targets[1].TicketID != "epic-1" {
		testing.Errorf("second target ID: expected epic-1, got %s", targets[1].TicketID)
	}
}

func TestDetectAutolinks_UnknownTicketIgnored(testing *testing.T) {
	source := autolinkTestSource()
	body := "See tkt-999 for details."

	styled, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 0 {
		testing.Fatalf("expected 0 targets for unknown ticket, got %d", len(targets))
	}
	// No ANSI changes when nothing is autolinked.
	if styled != body {
		testing.Errorf("expected unchanged body for unknown ticket")
	}
}

func TestDetectAutolinks_SkipsExistingTargets(testing *testing.T) {
	source := autolinkTestSource()
	body := "tkt-001 is in the graph. Also see tkt-002."

	// Simulate an existing click target covering tkt-001 on line 0.
	existing := []BodyClickTarget{
		{Line: 0, TicketID: "tkt-001", StartX: 0, EndX: 7},
	}

	_, targets := detectAutolinks(body, existing, source, DefaultTheme)

	// Only tkt-002 should be autolinked; tkt-001 is covered by existing.
	if len(targets) != 1 {
		testing.Fatalf("expected 1 target (tkt-002 only), got %d", len(targets))
	}
	if targets[0].TicketID != "tkt-002" {
		testing.Errorf("expected tkt-002, got %s", targets[0].TicketID)
	}
}

func TestDetectAutolinks_EmptyBody(testing *testing.T) {
	source := autolinkTestSource()
	styled, targets := detectAutolinks("", nil, source, DefaultTheme)
	if styled != "" {
		testing.Error("expected empty string for empty body")
	}
	if len(targets) != 0 {
		testing.Error("expected no targets for empty body")
	}
}

func TestDetectAutolinks_NoTicketPatterns(testing *testing.T) {
	source := autolinkTestSource()
	body := "Just regular text without any ticket references."

	styled, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 0 {
		testing.Errorf("expected 0 targets, got %d", len(targets))
	}
	if styled != body {
		testing.Error("expected unchanged body when no patterns found")
	}
}

func TestDetectAutolinks_HexSuffixPattern(testing *testing.T) {
	source := autolinkTestSource()
	body := "Fix from tkt-3abc."

	_, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 1 {
		testing.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].TicketID != "tkt-3abc" {
		testing.Errorf("expected tkt-3abc, got %s", targets[0].TicketID)
	}
}

func TestDetectAutolinks_ANSIDecoratedBody(testing *testing.T) {
	source := autolinkTestSource()
	// Simulate a body with existing ANSI styling (e.g., from markdown render).
	body := "See \x1b[1mtkt-001\x1b[0m for details."

	styled, targets := detectAutolinks(body, nil, source, DefaultTheme)

	if len(targets) != 1 {
		testing.Fatalf("expected 1 target, got %d", len(targets))
	}

	// Visible text should be preserved.
	visibleOriginal := ansi.Strip(body)
	visibleStyled := ansi.Strip(styled)
	if visibleStyled != visibleOriginal {
		testing.Errorf("visible text changed:\ngot:  %q\nwant: %q", visibleStyled, visibleOriginal)
	}
}

func TestDetectAutolinks_PartialOverlap(testing *testing.T) {
	source := autolinkTestSource()
	// Existing target covers columns 0-20 on line 0 (full-line target
	// like a dependency list entry).
	body := "tkt-001 Fix connection pooling"
	existing := []BodyClickTarget{
		{Line: 0, TicketID: "tkt-001", StartX: 0, EndX: 30},
	}

	_, targets := detectAutolinks(body, existing, source, DefaultTheme)

	// tkt-001 overlaps with the existing full-line target; should be skipped.
	if len(targets) != 0 {
		testing.Fatalf("expected 0 targets (overlaps existing), got %d", len(targets))
	}
}

func TestDetectAutolinks_PreservesSearchCompatibility(testing *testing.T) {
	source := autolinkTestSource()
	body := "See tkt-001 for details."

	styled, _ := detectAutolinks(body, nil, source, DefaultTheme)

	// Search highlighting should still work on autolinked text.
	// highlightSearchMatches strips ANSI to find visible text, then
	// splices in highlight escapes — autolink escapes shouldn't interfere.
	highlighted, matches := highlightSearchMatches(styled, "tkt-001", 0, DefaultTheme)
	if len(matches) != 1 {
		testing.Fatalf("search found %d matches in autolinked text, expected 1", len(matches))
	}

	// The visible text should still contain the ticket ID.
	if !strings.Contains(ansi.Strip(highlighted), "tkt-001") {
		testing.Error("ticket ID lost after search highlighting on autolinked text")
	}
}

func TestOverlapsExistingTarget(testing *testing.T) {
	targets := []BodyClickTarget{
		{Line: 0, TicketID: "tkt-001", StartX: 5, EndX: 12},
		{Line: 2, TicketID: "tkt-002", StartX: 0, EndX: 7},
	}

	tests := []struct {
		name     string
		line     int
		start    int
		end      int
		expected bool
	}{
		{"exact match", 0, 5, 12, true},
		{"partial overlap left", 0, 3, 8, true},
		{"partial overlap right", 0, 10, 15, true},
		{"no overlap before", 0, 0, 5, false},
		{"no overlap after", 0, 12, 20, false},
		{"wrong line", 1, 5, 12, false},
		{"different line match", 2, 0, 7, true},
	}

	for _, test := range tests {
		result := overlapsExistingTarget(test.line, test.start, test.end, targets)
		if result != test.expected {
			testing.Errorf("%s: overlapsExistingTarget(%d, %d, %d) = %v, want %v",
				test.name, test.line, test.start, test.end, result, test.expected)
		}
	}
}
