// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

func TestFilterMatchesTitle(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "pooling"}

	entry := ticketindex.Entry{
		ID: "tkt-001",
		Content: ticket.TicketContent{
			Title: "Fix connection pooling leak",
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter 'pooling' should match title 'Fix connection pooling leak'")
	}
}

func TestFilterMatchesLabel(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "transport"}

	entry := ticketindex.Entry{
		ID: "tkt-002",
		Content: ticket.TicketContent{
			Title:  "Implement retry backoff",
			Labels: []string{"transport"},
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter 'transport' should match label")
	}
}

func TestFilterMatchesID(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "tkt-003"}

	entry := ticketindex.Entry{
		ID: "tkt-003",
		Content: ticket.TicketContent{
			Title: "Update CI pipeline config",
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter 'tkt-003' should match ticket ID")
	}
}

func TestFilterMatchesAssignee(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "iree"}

	entry := ticketindex.Entry{
		ID: "tkt-001",
		Content: ticket.TicketContent{
			Title:    "Fix connection pooling leak",
			Assignee: ref.MustParseUserID("@iree/pm:bureau.local"),
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter 'iree' should match assignee '@iree/pm:bureau.local'")
	}
}

func TestFilterMatchesType(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "bug"}

	entry := ticketindex.Entry{
		ID: "tkt-001",
		Content: ticket.TicketContent{
			Title: "Fix connection pooling leak",
			Type:  ticket.TypeBug,
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter 'bug' should match type")
	}
}

func TestFilterCaseInsensitive(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "POOLING"}

	entry := ticketindex.Entry{
		ID: "tkt-001",
		Content: ticket.TicketContent{
			Title: "Fix connection pooling leak",
		},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("filter should be case-insensitive")
	}
}

func TestFilterNoMatch(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: "xyz-nonexistent"}

	entry := ticketindex.Entry{
		ID: "tkt-001",
		Content: ticket.TicketContent{
			Title: "Fix connection pooling leak",
			Type:  ticket.TypeBug,
		},
	}

	if filter.MatchesEntry(entry, source) {
		t.Error("filter 'xyz-nonexistent' should not match anything")
	}
}

func TestFilterEmptyMatchesAll(t *testing.T) {
	source := testSource()
	filter := FilterModel{Input: ""}

	entry := ticketindex.Entry{
		ID:      "tkt-001",
		Content: ticket.TicketContent{Title: "Anything"},
	}

	if !filter.MatchesEntry(entry, source) {
		t.Error("empty filter should match everything")
	}
}

func TestFilterApply(t *testing.T) {
	source := testSource()
	entries := source.All().Entries

	filter := FilterModel{Input: "bug"}
	result := filter.Apply(entries, source)

	// testSource has 2 bugs: tkt-001 and tkt-004.
	if len(result) != 2 {
		t.Errorf("filter 'bug' should match 2 entries, got %d", len(result))
	}

	for _, entry := range result {
		if entry.Content.Type != ticket.TypeBug {
			t.Errorf("filtered entry %s has type %q, expected %q", entry.ID, entry.Content.Type, ticket.TypeBug)
		}
	}
}

func TestFilterHandleRune(t *testing.T) {
	filter := FilterModel{}
	filter.HandleRune('a')
	filter.HandleRune('b')
	if filter.Input != "ab" {
		t.Errorf("expected 'ab', got %q", filter.Input)
	}
}

func TestFilterHandleBackspace(t *testing.T) {
	filter := FilterModel{Input: "abc"}
	changed := filter.HandleBackspace()
	if !changed {
		t.Error("backspace should return true when there's text")
	}
	if filter.Input != "ab" {
		t.Errorf("expected 'ab' after backspace, got %q", filter.Input)
	}

	// Backspace on empty.
	filter.Input = ""
	changed = filter.HandleBackspace()
	if changed {
		t.Error("backspace on empty should return false")
	}
}

func TestFilterClear(t *testing.T) {
	filter := FilterModel{Input: "test", Active: true}
	filter.Clear()
	if filter.Input != "" {
		t.Errorf("expected empty input after clear, got %q", filter.Input)
	}
	if filter.Active {
		t.Error("filter should be inactive after clear")
	}
}
