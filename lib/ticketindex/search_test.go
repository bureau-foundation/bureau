// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketindex

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

func TestSearchBasicTextMatch(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Fix authentication token refresh",
		Body:   "The auth token is not being refreshed properly.",
		Status: "open",
	})
	index.Put("tkt-bb00", ticket.TicketContent{
		Title:  "Add pagination to list view",
		Body:   "Users need pagination when there are many items.",
		Status: "open",
	})
	index.Put("tkt-cc00", ticket.TicketContent{
		Title:  "Update deployment scripts",
		Body:   "The authentication middleware needs updating in deploy.",
		Status: "open",
	})

	results := index.Search("authentication", Filter{}, 0)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// tkt-aa00 has "authentication" in both title (weight 5) and body
	// (weight 2), so it should rank higher than tkt-cc00 which has it
	// only in body (weight 2).
	if results[0].ID != "tkt-aa00" {
		t.Errorf("expected tkt-aa00 first (title match), got %s", results[0].ID)
	}

	// tkt-bb00 should not appear (no match).
	for _, result := range results {
		if result.ID == "tkt-bb00" {
			t.Errorf("tkt-bb00 should not match 'authentication'")
		}
	}
}

func TestSearchExactIDMatch(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-abcd", ticket.TicketContent{
		Title:  "Minor cleanup task",
		Body:   "Just some cleanup.",
		Status: "open",
	})
	index.Put("tkt-ef00", ticket.TicketContent{
		Title:  "Authentication overhaul",
		Body:   "Major authentication rework needed.",
		Status: "open",
	})

	// Search for tkt-abcd with additional keyword "authentication".
	// tkt-abcd should be first (exact ID match boost) despite
	// tkt-ef00 being a better BM25 match for "authentication".
	results := index.Search("tkt-abcd authentication", Filter{}, 0)
	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}
	if results[0].ID != "tkt-abcd" {
		t.Errorf("expected tkt-abcd first (exact ID match), got %s", results[0].ID)
	}
	if results[0].Score < boostExactMatch {
		t.Errorf("exact match score should be >= %v, got %v", boostExactMatch, results[0].Score)
	}
}

func TestSearchGraphExpansionBlockedBy(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-b10c", ticket.TicketContent{
		Title:  "Infrastructure migration",
		Body:   "Migrate to new infrastructure.",
		Status: "open",
	})
	index.Put("tkt-de0e", ticket.TicketContent{
		Title:     "Deploy new service",
		Body:      "Deploy after infra is ready.",
		Status:    "open",
		BlockedBy: []string{"tkt-b10c"},
	})
	index.Put("tkt-0a0b", ticket.TicketContent{
		Title:  "Update documentation",
		Body:   "Documentation update.",
		Status: "open",
	})

	// Search for the blocker's ID. Should return:
	// 1. tkt-b10c (exact match)
	// 2. tkt-de0e (dependent, direct neighbor)
	results := index.Search("tkt-b10c", Filter{}, 0)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results (blocker + dependent), got %d", len(results))
	}
	if results[0].ID != "tkt-b10c" {
		t.Errorf("expected tkt-b10c first, got %s", results[0].ID)
	}
	if results[1].ID != "tkt-de0e" {
		t.Errorf("expected tkt-de0e second, got %s", results[1].ID)
	}

	// tkt-0a0b should not appear (no BM25 match, no graph edge).
	for _, result := range results {
		if result.ID == "tkt-0a0b" {
			t.Errorf("tkt-0a0b should not appear in graph expansion results")
		}
	}
}

func TestSearchGraphExpansionChildren(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-e01c", ticket.TicketContent{
		Title:  "Epic: authentication overhaul",
		Body:   "Parent epic for all auth work.",
		Status: "open",
		Type:   "epic",
	})
	index.Put("tkt-c001", ticket.TicketContent{
		Title:  "Implement OAuth provider",
		Body:   "Add OAuth support.",
		Status: "open",
		Parent: "tkt-e01c",
	})
	index.Put("tkt-c002", ticket.TicketContent{
		Title:  "Add token rotation",
		Body:   "Rotate tokens periodically.",
		Status: "open",
		Parent: "tkt-e01c",
	})

	results := index.Search("tkt-e01c", Filter{}, 0)
	if len(results) < 3 {
		t.Fatalf("expected at least 3 results (epic + 2 children), got %d", len(results))
	}
	if results[0].ID != "tkt-e01c" {
		t.Errorf("expected tkt-e01c first (exact match), got %s", results[0].ID)
	}

	// Both children should appear as direct neighbors.
	childIDs := make(map[string]bool)
	for _, result := range results[1:] {
		childIDs[result.ID] = true
	}
	if !childIDs["tkt-c001"] {
		t.Error("tkt-c001 should appear as graph neighbor")
	}
	if !childIDs["tkt-c002"] {
		t.Error("tkt-c002 should appear as graph neighbor")
	}
}

func TestSearchTextualReference(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-face", ticket.TicketContent{
		Title:  "Fix memory leak in proxy",
		Body:   "The proxy leaks memory under load.",
		Status: "open",
	})
	index.Put("tkt-bead", ticket.TicketContent{
		Title:  "Investigate proxy crashes",
		Body:   "Crashes may be related to the memory leak. See tkt-face for details.",
		Status: "open",
	})
	index.Put("tkt-dead", ticket.TicketContent{
		Title:  "Unrelated database task",
		Body:   "Optimize database queries.",
		Status: "open",
	})

	results := index.Search("tkt-face", Filter{}, 0)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	if results[0].ID != "tkt-face" {
		t.Errorf("expected tkt-face first (exact match), got %s", results[0].ID)
	}

	// tkt-bead should appear because its body mentions tkt-face.
	found := false
	for _, result := range results {
		if result.ID == "tkt-bead" {
			found = true
			if result.Score < boostTextualReference {
				t.Errorf("textual reference score should be >= %v, got %v", boostTextualReference, result.Score)
			}
			break
		}
	}
	if !found {
		t.Error("tkt-bead should appear (textual reference to tkt-face)")
	}
}

func TestSearchFilterApplied(t *testing.T) {
	index := NewIndex()
	for _, tc := range []struct {
		id     string
		title  string
		status string
	}{
		{"tkt-aa00", "Authentication bug in proxy", "open"},
		{"tkt-bb00", "Authentication token issue", "open"},
		{"tkt-cc00", "Authentication refactor done", "closed"},
		{"tkt-dd00", "Authentication improvement", "in_progress"},
		{"tkt-ee00", "Authentication cleanup", "closed"},
	} {
		index.Put(tc.id, ticket.TicketContent{
			Title:  tc.title,
			Body:   "Details about authentication.",
			Status: tc.status,
		})
	}

	results := index.Search("authentication", Filter{Status: "open"}, 0)
	for _, result := range results {
		if result.Content.Status != "open" {
			t.Errorf("expected only open tickets, got %s (status: %s)", result.ID, result.Content.Status)
		}
	}
	if len(results) != 2 {
		t.Errorf("expected 2 open matches, got %d", len(results))
	}
}

func TestSearchLimit(t *testing.T) {
	index := NewIndex()
	for i := 0; i < 10; i++ {
		// Generate hex IDs: tkt-a000, tkt-a001, ..., tkt-a009
		id := "tkt-a00" + string(rune('0'+i))
		index.Put(id, ticket.TicketContent{
			Title:  "Performance optimization task",
			Body:   "Optimize performance in various areas.",
			Status: "open",
		})
	}

	results := index.Search("performance optimization", Filter{}, 3)
	if len(results) != 3 {
		t.Errorf("expected 3 results with limit=3, got %d", len(results))
	}
}

func TestSearchDirtyRebuild(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Initial ticket about caching",
		Body:   "Implement caching layer.",
		Status: "open",
	})

	// First search triggers initial build.
	results := index.Search("caching", Filter{}, 0)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Add a new ticket.
	index.Put("tkt-bb00", ticket.TicketContent{
		Title:  "Cache invalidation strategy",
		Body:   "Design caching invalidation approach.",
		Status: "open",
	})

	// Second search should include the new ticket (dirty rebuild).
	results = index.Search("caching", Filter{}, 0)
	if len(results) != 2 {
		t.Errorf("expected 2 results after Put, got %d", len(results))
	}

	// Remove the first ticket.
	index.Remove("tkt-aa00")

	// Third search should reflect the removal.
	results = index.Search("caching", Filter{}, 0)
	if len(results) != 1 {
		t.Errorf("expected 1 result after Remove, got %d", len(results))
	}
	if results[0].ID != "tkt-bb00" {
		t.Errorf("expected tkt-bb00, got %s", results[0].ID)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Some ticket",
		Body:   "Content.",
		Status: "open",
	})

	results := index.Search("", Filter{}, 0)
	if results != nil {
		t.Errorf("expected nil for empty query, got %d results", len(results))
	}
}

func TestSearchNoMatch(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Fix login page",
		Body:   "The login page has a rendering bug.",
		Status: "open",
	})

	results := index.Search("cryptocurrency blockchain", Filter{}, 0)
	if len(results) != 0 {
		t.Errorf("expected 0 results for non-matching query, got %d", len(results))
	}
}

func TestSearchArtifactIDReference(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Publish build artifact",
		Body:   "Build and publish the release artifact.",
		Status: "open",
		Attachments: []ticket.TicketAttachment{
			{Ref: "art-cafe1234", Label: "release binary", ContentType: "application/octet-stream"},
		},
	})
	index.Put("tkt-bb00", ticket.TicketContent{
		Title:  "Unrelated ticket",
		Body:   "Nothing to do with artifacts.",
		Status: "open",
	})

	results := index.Search("art-cafe1234", Filter{}, 0)
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for artifact ID search")
	}

	// tkt-aa00 should be boosted because its attachment references
	// the artifact ID.
	found := false
	for _, result := range results {
		if result.ID == "tkt-aa00" {
			found = true
			if result.Score < boostTextualReference {
				t.Errorf("artifact reference score should be >= %v, got %v", boostTextualReference, result.Score)
			}
			break
		}
	}
	if !found {
		t.Error("tkt-aa00 should appear (attachment references art-cafe1234)")
	}
}

func TestSearchLabelWeight(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Review access controls",
		Body:   "Review the access control implementation.",
		Status: "open",
		Labels: []string{"security"},
	})
	index.Put("tkt-bb00", ticket.TicketContent{
		Title:  "Update deployment docs",
		Body:   "Document the security practices for deployment.",
		Status: "open",
	})

	results := index.Search("security", Filter{}, 0)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// tkt-aa00 has "security" as a label (weight 3), while tkt-bb00
	// has "security" only in body (weight 2). The label match should
	// rank higher.
	if results[0].ID != "tkt-aa00" {
		t.Errorf("expected tkt-aa00 first (label weight > body weight), got %s", results[0].ID)
	}
}

func TestSearchGateTicketIDReference(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-9a7e", ticket.TicketContent{
		Title:  "Deploy service A",
		Body:   "Deploy the service.",
		Status: "open",
	})
	index.Put("tkt-9a7d", ticket.TicketContent{
		Title:  "Deploy service B",
		Body:   "Deploy after service A is ready.",
		Status: "open",
		Gates: []ticket.TicketGate{
			{ID: "wait-for-a", Type: "ticket", TicketID: "tkt-9a7e", Status: "pending"},
		},
	})

	results := index.Search("tkt-9a7e", Filter{}, 0)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// tkt-9a7d should appear because its gate references tkt-9a7e.
	found := false
	for _, result := range results {
		if result.ID == "tkt-9a7d" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tkt-9a7d should appear (gate references tkt-9a7e)")
	}
}

// TestExtractIDs verifies the ID extraction regex.
func TestExtractIDs(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"tkt-abcd", []string{"tkt-abcd"}},
		{"art-cafe1234", []string{"art-cafe1234"}},
		{"find tkt-aaaa and tkt-bbbb", []string{"tkt-aaaa", "tkt-bbbb"}},
		{"no ids here", nil},
		{"tkt-ab", nil},                              // too short (< 4 hex chars)
		{"TKT-ABCD", []string{"tkt-abcd"}},           // case insensitive
		{"tkt-abcd tkt-abcd", []string{"tkt-abcd"}},  // deduplication
		{"proj-deadbeef", []string{"proj-deadbeef"}}, // custom prefix
		{"tkt-blocker", nil},                         // non-hex suffix
		{"tkt-epic", nil},                            // non-hex suffix
	}

	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			got := extractIDs(test.query)
			if len(got) != len(test.want) {
				t.Fatalf("extractIDs(%q) = %v, want %v", test.query, got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("extractIDs(%q)[%d] = %q, want %q", test.query, i, got[i], test.want[i])
				}
			}
		})
	}
}

func TestSearchActiveFilter(t *testing.T) {
	index := NewIndex()
	index.Put("tkt-aa00", ticket.TicketContent{
		Title:  "Open auth bug",
		Body:   "Authentication issue.",
		Status: "open",
	})
	index.Put("tkt-bb00", ticket.TicketContent{
		Title:  "In-progress auth fix",
		Body:   "Working on authentication fix.",
		Status: "in_progress",
	})
	index.Put("tkt-cc00", ticket.TicketContent{
		Title:  "Closed auth cleanup",
		Body:   "Authentication cleanup done.",
		Status: "closed",
	})

	results := index.Search("authentication", Filter{Status: "active"}, 0)
	for _, result := range results {
		if result.Content.Status != "open" && result.Content.Status != "in_progress" {
			t.Errorf("active filter should match open/in_progress, got %s (status: %s)",
				result.ID, result.Content.Status)
		}
	}
	if len(results) != 2 {
		t.Errorf("expected 2 active matches, got %d", len(results))
	}
}
