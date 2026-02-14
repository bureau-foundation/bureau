// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

func TestFuzzyMatchBasic(t *testing.T) {
	result := fuzzyMatch("Fix connection pooling leak", []rune("pooling"), nil)
	if result.Score <= 0 {
		t.Fatal("expected positive score for substring match")
	}
	if len(result.Positions) == 0 {
		t.Fatal("expected non-empty match positions")
	}
}

func TestFuzzyMatchNonContiguous(t *testing.T) {
	// "plk" should match "pooling leak" — p from pooling, l from
	// pooling/leak, k from leak.
	result := fuzzyMatch("pooling leak", []rune("plk"), nil)
	if result.Score <= 0 {
		t.Fatal("expected positive score for non-contiguous fuzzy match")
	}
}

func TestFuzzyMatchNoMatch(t *testing.T) {
	result := fuzzyMatch("Fix connection pooling leak", []rune("xyz"), nil)
	if result.Score != 0 {
		t.Errorf("expected zero score for no match, got %d", result.Score)
	}
	if len(result.Positions) != 0 {
		t.Errorf("expected empty positions for no match, got %v", result.Positions)
	}
}

func TestFuzzyMatchCaseInsensitive(t *testing.T) {
	// Pattern is lowercase, text has uppercase "Pooling". Our wrapper
	// lowercases both sides, so this should match.
	result := fuzzyMatch("Fix Connection Pooling", []rune("pooling"), nil)
	if result.Score <= 0 {
		t.Fatalf("expected case-insensitive match, got score=%d", result.Score)
	}
}

func TestFuzzyMatchCaseInsensitiveAllCaps(t *testing.T) {
	// All-caps text with lowercase pattern.
	result := fuzzyMatch("MCP SERVER CONFIG", []rune("mcp"), nil)
	if result.Score <= 0 {
		t.Fatalf("expected match for 'mcp' in 'MCP SERVER CONFIG', got score=%d", result.Score)
	}
}

func TestFuzzyMatchEmptyPattern(t *testing.T) {
	result := fuzzyMatch("anything", []rune{}, nil)
	if result.Score != 0 {
		t.Errorf("expected zero score for empty pattern, got %d", result.Score)
	}
}

func TestApplyFuzzyEmptyFilter(t *testing.T) {
	source := testSource()
	entries := source.All().Entries

	filter := FilterModel{Input: ""}
	results := filter.ApplyFuzzy(entries, source)

	if len(results) != len(entries) {
		t.Errorf("empty filter should return all %d entries, got %d", len(entries), len(results))
	}

	for _, result := range results {
		if result.Score != 0 {
			t.Errorf("entry %s should have zero score with empty filter, got %d", result.Entry.ID, result.Score)
		}
		if len(result.TitlePositions) != 0 {
			t.Errorf("entry %s should have no title positions with empty filter", result.Entry.ID)
		}
	}
}

func TestApplyFuzzyMatchesSubstring(t *testing.T) {
	source := testSource()
	entries := source.All().Entries

	filter := FilterModel{Input: "pooling"}
	results := filter.ApplyFuzzy(entries, source)

	// testSource has one ticket with "pooling" in the title: tkt-001.
	found := false
	for _, result := range results {
		if result.Entry.ID == "tkt-001" {
			found = true
			if result.Score <= 0 {
				t.Error("expected positive score for matching entry")
			}
			if len(result.TitlePositions) == 0 {
				t.Error("expected title positions for matching entry")
			}
		}
	}
	if !found {
		t.Error("tkt-001 should appear in fuzzy results for 'pooling'")
	}
}

func TestApplyFuzzyNonContiguousMatch(t *testing.T) {
	source := testSource()
	entries := source.All().Entries

	// "cnpl" should match "connection pooling" via fuzzy matching.
	filter := FilterModel{Input: "cnpl"}
	results := filter.ApplyFuzzy(entries, source)

	found := false
	for _, result := range results {
		if result.Entry.ID == "tkt-001" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tkt-001 should match fuzzy query 'cnpl' against 'Fix connection pooling leak'")
	}
}

func TestApplyFuzzySortedByScore(t *testing.T) {
	// Create entries where one has a much better match than the other.
	index := ticket.NewIndex()
	index.Put("tkt-exact", schema.TicketContent{
		Title:  "pooling is great",
		Status: "open",
		Type:   "task",
	})
	index.Put("tkt-fuzzy", schema.TicketContent{
		Title:  "p-something o-other l-long i-inner n-nope g-gone",
		Status: "open",
		Type:   "task",
	})
	source := NewIndexSource(index)

	filter := FilterModel{Input: "pooling"}
	results := filter.ApplyFuzzy(source.All().Entries, source)

	if len(results) < 1 {
		t.Fatal("expected at least one result")
	}

	// The exact substring match should score higher than the scattered one.
	if results[0].Entry.ID != "tkt-exact" {
		t.Errorf("expected tkt-exact to be first (best score), got %s", results[0].Entry.ID)
	}
}

func TestApplyFuzzyTitlePositions(t *testing.T) {
	index := ticket.NewIndex()
	index.Put("tkt-001", schema.TicketContent{
		Title:  "hello world",
		Status: "open",
		Type:   "task",
	})
	source := NewIndexSource(index)

	filter := FilterModel{Input: "hw"}
	results := filter.ApplyFuzzy(source.All().Entries, source)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	positions := results[0].TitlePositions
	if len(positions) == 0 {
		t.Fatal("expected title match positions")
	}

	// The positions should be valid indices into "hello world".
	title := "hello world"
	for _, position := range positions {
		if position < 0 || position >= len([]rune(title)) {
			t.Errorf("position %d out of bounds for title %q", position, title)
		}
	}
}

func TestApplyFuzzyMatchesType(t *testing.T) {
	source := testSource()

	// "bug" should match entries with type "bug".
	filter := FilterModel{Input: "bug"}
	results := filter.ApplyFuzzy(source.All().Entries, source)

	bugCount := 0
	for _, result := range results {
		if result.Entry.Content.Type == "bug" {
			bugCount++
		}
	}

	// testSource has 2 bugs.
	if bugCount != 2 {
		t.Errorf("expected 2 bug entries in results, got %d", bugCount)
	}
}
