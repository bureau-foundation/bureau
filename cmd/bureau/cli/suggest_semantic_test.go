// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

func TestSuggestSemantic_MatchesByDescription(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("echo message", root, 5)
	if len(results) == 0 {
		t.Fatal("expected results for 'echo message'")
	}
	if results[0].Path != "test echo" {
		t.Errorf("top result = %q, want %q", results[0].Path, "test echo")
	}
	if results[0].Summary == "" {
		t.Error("top result summary is empty")
	}
	if results[0].Score <= 0 {
		t.Errorf("top result score = %f, want > 0", results[0].Score)
	}
}

func TestSuggestSemantic_NoMatch(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("zzzzxyzzy", root, 5)
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
}

func TestSuggestSemantic_SearchesNestedCommands(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("list items collection", root, 5)
	if len(results) == 0 {
		t.Fatal("expected results for 'list items collection'")
	}
	found := false
	for _, result := range results {
		if result.Path == "test nested list" {
			found = true
			break
		}
	}
	if !found {
		paths := make([]string, len(results))
		for i, result := range results {
			paths[i] = result.Path
		}
		t.Errorf("expected 'test nested list' in results, got %v", paths)
	}
}

func TestSuggestSemantic_RespectsLimit(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("test", root, 1)
	if len(results) > 1 {
		t.Errorf("expected at most 1 result, got %d", len(results))
	}
}

func TestSuggestSemantic_RankedByRelevance(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("echo", root, 10)
	if len(results) < 2 {
		t.Skipf("need at least 2 results, got %d", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted by descending score: [%d]=%f > [%d]=%f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

func TestSuggestSemantic_EmptyQuery(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	results := SuggestSemantic("", root, 5)
	if len(results) != 0 {
		t.Errorf("expected no results for empty query, got %d", len(results))
	}
}

func TestSuggestSemantic_MatchesByFlagName(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	// The "create" command has a --title flag.
	results := SuggestSemantic("title", root, 5)
	if len(results) == 0 {
		t.Fatal("expected results for 'title' (flag name)")
	}
	found := false
	for _, result := range results {
		if result.Path == "test nested create" {
			found = true
			break
		}
	}
	if !found {
		paths := make([]string, len(results))
		for i, result := range results {
			paths[i] = result.Path
		}
		t.Errorf("expected 'test nested create' in results, got %v", paths)
	}
}

func TestWalkLeafCommands_SkipsNonRunnable(t *testing.T) {
	t.Parallel()

	root := semanticTestTree()
	var paths []string
	walkLeafCommands(root, "", func(path string, _ *Command) {
		paths = append(paths, path)
	})

	// Root and "nested" have no Run, should not appear.
	for _, path := range paths {
		if path == "test" || path == "test nested" {
			t.Errorf("non-runnable command %q should not be visited", path)
		}
	}

	// Leaf commands should appear.
	expected := map[string]bool{
		"test echo":          false,
		"test nested list":   false,
		"test nested create": false,
	}
	for _, path := range paths {
		if _, ok := expected[path]; ok {
			expected[path] = true
		}
	}
	for path, found := range expected {
		if !found {
			t.Errorf("expected %q in paths, got %v", path, paths)
		}
	}
}

func TestWalkLeafCommands_IncludesFallThroughCommands(t *testing.T) {
	t.Parallel()

	// A command with both Run and Subcommands should be visited.
	root := &Command{
		Name: "root",
		Run:  func(args []string) error { return nil },
		Subcommands: []*Command{
			{
				Name: "child",
				Run:  func(args []string) error { return nil },
			},
		},
	}

	var paths []string
	walkLeafCommands(root, "", func(path string, _ *Command) {
		paths = append(paths, path)
	})

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %v", paths)
	}
	if paths[0] != "root" {
		t.Errorf("paths[0] = %q, want %q", paths[0], "root")
	}
	if paths[1] != "root child" {
		t.Errorf("paths[1] = %q, want %q", paths[1], "root child")
	}
}

func TestFormatSemanticSuggestions(t *testing.T) {
	t.Parallel()

	suggestions := []SemanticSuggestion{
		{Path: "bureau matrix user create", Summary: "Create a Matrix user", Score: 5.0},
		{Path: "bureau auth grant", Summary: "Grant access", Score: 3.0},
	}
	result := formatSemanticSuggestions("usre", suggestions, "bureau")

	if !strings.Contains(result, `unknown command "usre"`) {
		t.Errorf("missing unknown command header in:\n%s", result)
	}
	if !strings.Contains(result, "Did you mean:") {
		t.Errorf("missing 'Did you mean:' in:\n%s", result)
	}
	if !strings.Contains(result, "bureau matrix user create") {
		t.Errorf("missing first suggestion in:\n%s", result)
	}
	if !strings.Contains(result, "bureau auth grant") {
		t.Errorf("missing second suggestion in:\n%s", result)
	}
	if !strings.Contains(result, "Run 'bureau --help' for usage.") {
		t.Errorf("missing help footer in:\n%s", result)
	}
}

func TestFormatSemanticSuggestions_NoSummary(t *testing.T) {
	t.Parallel()

	suggestions := []SemanticSuggestion{
		{Path: "bureau something", Summary: "", Score: 1.0},
	}
	result := formatSemanticSuggestions("smthng", suggestions, "bureau")

	if !strings.Contains(result, "bureau something") {
		t.Errorf("missing suggestion path in:\n%s", result)
	}
}

func TestRoot_ReturnsRootCommand(t *testing.T) {
	t.Parallel()

	root := &Command{Name: "root"}
	child := &Command{Name: "child", parent: root}
	grandchild := &Command{Name: "grandchild", parent: child}

	if got := root.root(); got != root {
		t.Errorf("root.root() = %q, want %q", got.Name, root.Name)
	}
	if got := child.root(); got != root {
		t.Errorf("child.root() = %q, want %q", got.Name, root.Name)
	}
	if got := grandchild.root(); got != root {
		t.Errorf("grandchild.root() = %q, want %q", got.Name, root.Name)
	}
}

// semanticTestTree builds a small command tree for testing semantic
// search. It has enough structure to verify nested traversal, flag
// extraction, and BM25 ranking.
func semanticTestTree() *Command {
	type createParams struct {
		Title string `flag:"title" desc:"item title"`
		Body  string `flag:"body" desc:"item body text"`
	}
	var params createParams

	return &Command{
		Name: "test",
		Subcommands: []*Command{
			{
				Name:        "echo",
				Summary:     "Echo a message",
				Description: "Returns the input message unchanged. Useful for testing.",
				Run:         func(args []string) error { return nil },
			},
			{
				Name:    "nested",
				Summary: "Nested commands",
				Subcommands: []*Command{
					{
						Name:        "list",
						Summary:     "List items in a collection",
						Description: "Enumerates all items with optional filtering.",
						Run:         func(args []string) error { return nil },
					},
					{
						Name:        "create",
						Summary:     "Create a new item",
						Description: "Creates a new item in the collection.",
						Params:      func() any { return &params },
						Run:         func(args []string) error { return nil },
					},
				},
			},
		},
	}
}
