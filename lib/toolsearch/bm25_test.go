// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import (
	"testing"
)

func TestBM25Index_Search(t *testing.T) {
	documents := []Document{
		{
			Name:                 "bureau_ticket_create",
			Description:          "Create a new ticket in a project room",
			ArgumentNames:        []string{"project", "title", "body"},
			ArgumentDescriptions: []string{"project room alias", "ticket title", "ticket body text"},
		},
		{
			Name:                 "bureau_ticket_list",
			Description:          "List tickets in a project room with optional filters",
			ArgumentNames:        []string{"project", "status", "assignee"},
			ArgumentDescriptions: []string{"project room alias", "filter by status", "filter by assignee"},
		},
		{
			Name:                 "bureau_workspace_create",
			Description:          "Create a new workspace with a git worktree",
			ArgumentNames:        []string{"template", "name"},
			ArgumentDescriptions: []string{"workspace template reference", "workspace name"},
		},
		{
			Name:                 "bureau_credential_push",
			Description:          "Provision encrypted credentials for a machine",
			ArgumentNames:        []string{"machine", "account"},
			ArgumentDescriptions: []string{"machine name", "account to provision"},
		},
		{
			Name:                 "bureau_observe",
			Description:          "Observe a running agent's terminal output in real time",
			ArgumentNames:        []string{"principal"},
			ArgumentDescriptions: []string{"principal localpart to observe"},
		},
		{
			Name:                 "bureau_machine_list",
			Description:          "List registered machines and their status",
			ArgumentNames:        []string{},
			ArgumentDescriptions: []string{},
		},
		{
			Name:                 "bureau_service_list",
			Description:          "List available services in the service directory",
			ArgumentNames:        []string{"filter"},
			ArgumentDescriptions: []string{"filter by service name prefix"},
		},
	}

	index := NewBM25Index(documents)

	tests := []struct {
		query     string
		wantFirst string
		wantAny   []string // at least one of these should appear in results
	}{
		{
			query:     "create ticket",
			wantFirst: "bureau_ticket_create",
		},
		{
			query:     "list tickets",
			wantFirst: "bureau_ticket_list",
		},
		{
			query:     "provision credentials",
			wantFirst: "bureau_credential_push",
		},
		{
			query:     "workspace",
			wantFirst: "bureau_workspace_create",
		},
		{
			query:     "observe agent terminal",
			wantFirst: "bureau_observe",
		},
		{
			query:     "machines",
			wantFirst: "bureau_machine_list",
		},
		{
			query:   "project",
			wantAny: []string{"bureau_ticket_create", "bureau_ticket_list"},
		},
		{
			query:     "service directory",
			wantFirst: "bureau_service_list",
		},
	}

	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			results := index.Search(test.query, 5)
			if len(results) == 0 {
				t.Fatal("expected results, got none")
			}

			if test.wantFirst != "" && results[0].Name != test.wantFirst {
				t.Errorf("top result = %q (score %.3f), want %q", results[0].Name, results[0].Score, test.wantFirst)
				for i, result := range results {
					t.Logf("  [%d] %s (%.3f)", i, result.Name, result.Score)
				}
			}

			if len(test.wantAny) > 0 {
				found := false
				for _, result := range results {
					for _, wanted := range test.wantAny {
						if result.Name == wanted {
							found = true
							break
						}
					}
				}
				if !found {
					t.Errorf("expected any of %v in results, got:", test.wantAny)
					for i, result := range results {
						t.Logf("  [%d] %s (%.3f)", i, result.Name, result.Score)
					}
				}
			}
		})
	}
}

func TestBM25Index_EmptyQuery(t *testing.T) {
	index := NewBM25Index([]Document{
		{Name: "foo", Description: "does things"},
	})

	results := index.Search("", 5)
	if len(results) != 0 {
		t.Errorf("empty query returned %d results, want 0", len(results))
	}
}

func TestBM25Index_NoDocuments(t *testing.T) {
	index := NewBM25Index(nil)
	results := index.Search("anything", 5)
	if len(results) != 0 {
		t.Errorf("empty index returned %d results, want 0", len(results))
	}
}

func TestBM25Index_NoMatch(t *testing.T) {
	index := NewBM25Index([]Document{
		{Name: "foo", Description: "manages widgets"},
	})

	results := index.Search("zzzzzzz", 5)
	if len(results) != 0 {
		t.Errorf("non-matching query returned %d results, want 0", len(results))
	}
}

func TestBM25Index_Limit(t *testing.T) {
	documents := make([]Document, 20)
	for i := range documents {
		documents[i] = Document{
			Name:        "tool",
			Description: "does shared thing",
		}
	}

	index := NewBM25Index(documents)
	results := index.Search("shared thing", 3)
	if len(results) != 3 {
		t.Errorf("limit 3 returned %d results", len(results))
	}
}

func TestBM25Index_ScoreOrdering(t *testing.T) {
	index := NewBM25Index([]Document{
		{Name: "alpha", Description: "alpha does search once"},
		{Name: "beta", Description: "beta does something else entirely"},
		{Name: "gamma_search", Description: "gamma search finds items in a search index"},
	})

	results := index.Search("search", 10)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted by descending score: [%d] %.3f > [%d] %.3f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}

	// gamma_search should rank highest: "search" appears in name (3x weight)
	// and twice in description.
	if results[0].Name != "gamma_search" {
		t.Errorf("top result = %q, want gamma_search (name match should win)", results[0].Name)
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"Hello-World_Foo", []string{"hello", "world", "foo"}},
		{"a I", nil},               // all tokens < 2 chars
		{"a I an", []string{"an"}}, // "an" is 2 chars, passes filter
		{"bureau_ticket_create", []string{"bureau", "ticket", "create"}},
		{"CamelCase123", []string{"camelcase123"}},
		{"", nil},
		{"x", nil}, // single char discarded
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got := tokenize(test.input)
			if len(got) != len(test.want) {
				t.Fatalf("tokenize(%q) = %v (len %d), want %v (len %d)",
					test.input, got, len(got), test.want, len(test.want))
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("tokenize(%q)[%d] = %q, want %q",
						test.input, i, got[i], test.want[i])
				}
			}
		})
	}
}
