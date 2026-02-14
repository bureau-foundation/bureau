// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"
)

func TestSearchModelHandleRune(t *testing.T) {
	var search SearchModel
	search.HandleRune('h')
	search.HandleRune('i')
	if search.Input != "hi" {
		t.Errorf("expected input 'hi', got %q", search.Input)
	}
}

func TestSearchModelHandleBackspace(t *testing.T) {
	var search SearchModel
	search.Input = "abc"

	if !search.HandleBackspace() {
		t.Fatal("expected HandleBackspace to return true when input is non-empty")
	}
	if search.Input != "ab" {
		t.Errorf("expected input 'ab' after backspace, got %q", search.Input)
	}

	// Backspace on empty input returns false.
	search.Input = ""
	if search.HandleBackspace() {
		t.Fatal("expected HandleBackspace to return false on empty input")
	}
}

func TestSearchModelClear(t *testing.T) {
	search := SearchModel{
		Input:  "query",
		Active: true,
	}
	search.SetMatches([]searchMatch{{Line: 1, Column: 0}})

	search.Clear()

	if search.Input != "" {
		t.Errorf("expected empty input after Clear, got %q", search.Input)
	}
	if search.Active {
		t.Error("expected Active=false after Clear")
	}
	if search.MatchCount() != 0 {
		t.Errorf("expected 0 matches after Clear, got %d", search.MatchCount())
	}
}

func TestSearchModelMatchNavigation(t *testing.T) {
	var search SearchModel
	search.SetMatches([]searchMatch{
		{Line: 0, Column: 0},
		{Line: 2, Column: 5},
		{Line: 4, Column: 10},
	})

	if search.CurrentIndex() != 0 {
		t.Errorf("expected current index 0, got %d", search.CurrentIndex())
	}

	search.NextMatch()
	if search.CurrentIndex() != 1 {
		t.Errorf("expected current index 1 after NextMatch, got %d", search.CurrentIndex())
	}

	search.NextMatch()
	search.NextMatch() // Wraps around.
	if search.CurrentIndex() != 0 {
		t.Errorf("expected current index 0 after wrap, got %d", search.CurrentIndex())
	}

	search.PreviousMatch() // Wraps backward.
	if search.CurrentIndex() != 2 {
		t.Errorf("expected current index 2 after backward wrap, got %d", search.CurrentIndex())
	}
}

func TestSearchModelMatchNavigationEmpty(t *testing.T) {
	var search SearchModel
	// NextMatch/PreviousMatch should be no-ops with no matches.
	search.NextMatch()
	search.PreviousMatch()
	if search.CurrentMatch() != nil {
		t.Error("expected nil CurrentMatch with no matches")
	}
}

func TestHighlightSearchMatchesPlainText(t *testing.T) {
	body := "hello world\nfoo bar hello\nbaz"
	highlighted, matches := highlightSearchMatches(body, "hello", -1, DefaultTheme)

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	if matches[0].Line != 0 || matches[0].Column != 0 {
		t.Errorf("match 0: expected line=0, column=0, got line=%d, column=%d",
			matches[0].Line, matches[0].Column)
	}
	if matches[1].Line != 1 || matches[1].Column != 8 {
		t.Errorf("match 1: expected line=1, column=8, got line=%d, column=%d",
			matches[1].Line, matches[1].Column)
	}

	// The highlighted output should contain ANSI escape sequences.
	if !strings.Contains(highlighted, "\x1b[48;5;") {
		t.Error("expected ANSI background escape in highlighted output")
	}

	// The non-matching line should be unchanged.
	highlightedLines := strings.Split(highlighted, "\n")
	if highlightedLines[2] != "baz" {
		t.Errorf("expected unmodified line 'baz', got %q", highlightedLines[2])
	}
}

func TestHighlightSearchMatchesCaseInsensitive(t *testing.T) {
	body := "Hello WORLD"
	_, matches := highlightSearchMatches(body, "hello", -1, DefaultTheme)

	if len(matches) != 1 {
		t.Fatalf("expected 1 case-insensitive match, got %d", len(matches))
	}
	if matches[0].Column != 0 {
		t.Errorf("expected match at column 0, got %d", matches[0].Column)
	}
}

func TestHighlightSearchMatchesAnsiPreserved(t *testing.T) {
	// Simulate ANSI-decorated text (red foreground "hello" then reset).
	body := "\x1b[31mhello\x1b[0m world"
	highlighted, matches := highlightSearchMatches(body, "hello", 0, DefaultTheme)

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// The original red foreground escape should still be present.
	if !strings.Contains(highlighted, "\x1b[31m") {
		t.Error("expected original ANSI foreground to be preserved")
	}

	// The current-match background should use SearchCurrentBackground.
	currentEscape := "\x1b[48;5;" + string(DefaultTheme.SearchCurrentBackground) + "m"
	if !strings.Contains(highlighted, currentEscape) {
		t.Errorf("expected current-match background %q in output", currentEscape)
	}
}

func TestHighlightSearchMatchesMultiplePerLine(t *testing.T) {
	body := "abcabcabc"
	_, matches := highlightSearchMatches(body, "abc", -1, DefaultTheme)

	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(matches))
	}

	if matches[0].Column != 0 || matches[1].Column != 3 || matches[2].Column != 6 {
		t.Errorf("unexpected match columns: %d, %d, %d",
			matches[0].Column, matches[1].Column, matches[2].Column)
	}
}

func TestHighlightSearchMatchesEmptyQuery(t *testing.T) {
	body := "hello world"
	highlighted, matches := highlightSearchMatches(body, "", -1, DefaultTheme)

	if highlighted != body {
		t.Errorf("expected body unchanged with empty query, got %q", highlighted)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches with empty query, got %d", len(matches))
	}
}

func TestHighlightSearchMatchesNoMatch(t *testing.T) {
	body := "hello world"
	highlighted, matches := highlightSearchMatches(body, "xyz", -1, DefaultTheme)

	if highlighted != body {
		t.Errorf("expected body unchanged with no matches, got %q", highlighted)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}
}

func TestHighlightSearchMatchesCurrentVsNormal(t *testing.T) {
	body := "abc abc"
	// Mark match index 1 as current.
	highlighted, matches := highlightSearchMatches(body, "abc", 1, DefaultTheme)

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	normalEscape := "\x1b[48;5;" + string(DefaultTheme.SearchHighlightBackground) + "m"
	currentEscape := "\x1b[48;5;" + string(DefaultTheme.SearchCurrentBackground) + "m"

	if !strings.Contains(highlighted, normalEscape) {
		t.Error("expected normal highlight escape in output")
	}
	if !strings.Contains(highlighted, currentEscape) {
		t.Error("expected current-match highlight escape in output")
	}
}

func TestHighlightSearchMatchesMultiBytePrefix(t *testing.T) {
	// Status icons like "●" are 3 bytes in UTF-8 but 1 rune.
	// Highlighting must use rune positions, not byte offsets,
	// so the highlight lands on the correct characters.
	body := "● tkt-3o5.3 some title"
	highlighted, matches := highlightSearchMatches(body, "tkt-", -1, DefaultTheme)

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Column != 2 {
		t.Errorf("expected match at rune column 2, got %d", matches[0].Column)
	}

	// Verify the highlight wraps "tkt-" and not some shifted substring.
	// Strip the highlight escapes and confirm the raw text is unchanged.
	stripped := ansiStripForTest(highlighted)
	if stripped != body {
		t.Errorf("stripping highlights should recover original text, got %q", stripped)
	}

	// The highlight background should appear BEFORE the "t" of "tkt-",
	// not shifted right.
	highlightEscape := "\x1b[48;5;" + string(DefaultTheme.SearchHighlightBackground) + "m"
	highlightIndex := strings.Index(highlighted, highlightEscape)
	if highlightIndex < 0 {
		t.Fatal("expected highlight escape in output")
	}
	// After the highlight escape, the next visible chars should be "tkt-".
	afterHighlight := highlighted[highlightIndex+len(highlightEscape):]
	if !strings.HasPrefix(afterHighlight, "tkt-") {
		t.Errorf("expected 'tkt-' immediately after highlight escape, got %q",
			afterHighlight[:min(10, len(afterHighlight))])
	}
}

func TestHighlightSearchMatchesAnsiDecoratedMultiByte(t *testing.T) {
	// ANSI-decorated line with a multi-byte status icon, as rendered
	// by the dependency list: colored icon + space + colored ID.
	body := "\x1b[33m●\x1b[0m \x1b[245mtkt-3o5.3\x1b[0m Fix something"
	highlighted, matches := highlightSearchMatches(body, "tkt-", -1, DefaultTheme)

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// Verify "tkt-" is highlighted (background escape appears right before it).
	highlightEscape := "\x1b[48;5;" + string(DefaultTheme.SearchHighlightBackground) + "m"
	highlightIndex := strings.Index(highlighted, highlightEscape)
	if highlightIndex < 0 {
		t.Fatal("expected highlight escape in output")
	}
	afterHighlight := highlighted[highlightIndex+len(highlightEscape):]
	// The next visible text after the highlight start should begin with
	// the ANSI-decorated "t" — either directly "t" or via an existing
	// ANSI sequence then "t". Strip any ANSI prefix to check.
	visibleAfter := ansiStripForTest(afterHighlight)
	if !strings.HasPrefix(visibleAfter, "tkt-") {
		t.Errorf("expected visible text 'tkt-' after highlight, got %q",
			visibleAfter[:min(10, len(visibleAfter))])
	}
}

// ansiStripForTest removes ANSI escape sequences from a string.
// Uses a simple state machine sufficient for test assertions.
func ansiStripForTest(input string) string {
	var result strings.Builder
	inEscape := false
	for _, character := range input {
		if inEscape {
			if (character >= 'A' && character <= 'Z') ||
				(character >= 'a' && character <= 'z') {
				inEscape = false
			}
			continue
		}
		if character == '\x1b' {
			inEscape = true
			continue
		}
		result.WriteRune(character)
	}
	return result.String()
}

func TestSearchModelSetMatchesClampsIndex(t *testing.T) {
	var search SearchModel
	search.SetMatches([]searchMatch{
		{Line: 0, Column: 0},
		{Line: 1, Column: 0},
		{Line: 2, Column: 0},
	})
	search.NextMatch()
	search.NextMatch() // current = 2

	// Now reduce matches to just 1.
	search.SetMatches([]searchMatch{
		{Line: 0, Column: 0},
	})

	if search.CurrentIndex() != 0 {
		t.Errorf("expected current index clamped to 0, got %d", search.CurrentIndex())
	}
}
