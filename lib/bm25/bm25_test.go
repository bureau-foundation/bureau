// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package bm25

import (
	"testing"
)

// toolDocument builds a BM25 Document mimicking the toolsearch field
// weights: name 3x, description 2x, argument names 2x, argument
// descriptions 1x. Used by tests ported from the toolsearch package.
func toolDocument(name, description string, argumentNames, argumentDescriptions []string) Document {
	fields := []Field{
		{Text: name, Weight: 3},
		{Text: description, Weight: 2},
	}
	for _, argumentName := range argumentNames {
		fields = append(fields, Field{Text: argumentName, Weight: 2})
	}
	for _, argumentDescription := range argumentDescriptions {
		fields = append(fields, Field{Text: argumentDescription, Weight: 1})
	}
	return Document{Name: name, Fields: fields}
}

func TestSearch(t *testing.T) {
	documents := []Document{
		toolDocument(
			"bureau_ticket_create",
			"Create a new ticket in a project room",
			[]string{"project", "title", "body"},
			[]string{"project room alias", "ticket title", "ticket body text"},
		),
		toolDocument(
			"bureau_ticket_list",
			"List tickets in a project room with optional filters",
			[]string{"project", "status", "assignee"},
			[]string{"project room alias", "filter by status", "filter by assignee"},
		),
		toolDocument(
			"bureau_workspace_create",
			"Create a new workspace with a git worktree",
			[]string{"template", "name"},
			[]string{"workspace template reference", "workspace name"},
		),
		toolDocument(
			"bureau_credential_push",
			"Provision encrypted credentials for a machine",
			[]string{"machine", "account"},
			[]string{"machine name", "account to provision"},
		),
		toolDocument(
			"bureau_observe",
			"Observe a running agent's terminal output in real time",
			[]string{"principal"},
			[]string{"principal localpart to observe"},
		),
		toolDocument(
			"bureau_machine_list",
			"List registered machines and their status",
			nil, nil,
		),
		toolDocument(
			"bureau_service_list",
			"List available services in the service directory",
			[]string{"filter"},
			[]string{"filter by service name prefix"},
		),
	}

	index := New(documents)

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

func TestSearch_EmptyQuery(t *testing.T) {
	index := New([]Document{
		{Name: "foo", Fields: []Field{{Text: "does things", Weight: 1}}},
	})

	results := index.Search("", 5)
	if len(results) != 0 {
		t.Errorf("empty query returned %d results, want 0", len(results))
	}
}

func TestSearch_NoDocuments(t *testing.T) {
	index := New(nil)
	results := index.Search("anything", 5)
	if len(results) != 0 {
		t.Errorf("empty index returned %d results, want 0", len(results))
	}
}

func TestSearch_NoMatch(t *testing.T) {
	index := New([]Document{
		{Name: "foo", Fields: []Field{{Text: "manages widgets", Weight: 1}}},
	})

	results := index.Search("zzzzzzz", 5)
	if len(results) != 0 {
		t.Errorf("non-matching query returned %d results, want 0", len(results))
	}
}

func TestSearch_Limit(t *testing.T) {
	documents := make([]Document, 20)
	for i := range documents {
		documents[i] = Document{
			Name:   "tool",
			Fields: []Field{{Text: "does shared thing", Weight: 1}},
		}
	}

	index := New(documents)
	results := index.Search("shared thing", 3)
	if len(results) != 3 {
		t.Errorf("limit 3 returned %d results", len(results))
	}
}

func TestSearch_ScoreOrdering(t *testing.T) {
	index := New([]Document{
		{Name: "alpha", Fields: []Field{{Text: "alpha does search once", Weight: 1}}},
		{Name: "beta", Fields: []Field{{Text: "beta does something else entirely", Weight: 1}}},
		{Name: "gamma_search", Fields: []Field{
			{Text: "gamma_search", Weight: 3},
			{Text: "gamma search finds items in a search index", Weight: 2},
		}},
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

func TestFieldWeights(t *testing.T) {
	// Two documents with the same text, but one has it in a high-weight
	// field and the other in a low-weight field. The high-weight field
	// should produce a higher score.
	highWeight := Document{
		Name: "high",
		Fields: []Field{
			{Text: "authentication token refresh", Weight: 5},
			{Text: "unrelated filler text", Weight: 1},
		},
	}
	lowWeight := Document{
		Name: "low",
		Fields: []Field{
			{Text: "unrelated filler text", Weight: 5},
			{Text: "authentication token refresh", Weight: 1},
		},
	}

	index := New([]Document{highWeight, lowWeight})
	results := index.Search("authentication token refresh", 10)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Name != "high" {
		t.Errorf("top result = %q, want %q (higher weight should win)", results[0].Name, "high")
	}
	if results[0].Score <= results[1].Score {
		t.Errorf("high-weight score (%.3f) should exceed low-weight score (%.3f)",
			results[0].Score, results[1].Score)
	}
}

func TestFieldWeightZeroSkipped(t *testing.T) {
	// A field with weight 0 should not contribute to scoring.
	document := Document{
		Name: "test",
		Fields: []Field{
			{Text: "visible content", Weight: 1},
			{Text: "invisible secret", Weight: 0},
			{Text: "also invisible", Weight: -1},
		},
	}

	index := New([]Document{document})

	// "visible" should match.
	results := index.Search("visible", 5)
	if len(results) != 1 {
		t.Errorf("expected 1 result for 'visible', got %d", len(results))
	}

	// "secret" should not match (weight 0).
	results = index.Search("secret", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'secret' (weight 0 field), got %d", len(results))
	}

	// "invisible" should not match (weight -1).
	results = index.Search("invisible", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'invisible' (weight -1 field), got %d", len(results))
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
			got := Tokenize(test.input)
			if len(got) != len(test.want) {
				t.Fatalf("Tokenize(%q) = %v (len %d), want %v (len %d)",
					test.input, got, len(got), test.want, len(test.want))
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("Tokenize(%q)[%d] = %q, want %q",
						test.input, i, got[i], test.want[i])
				}
			}
		})
	}
}
