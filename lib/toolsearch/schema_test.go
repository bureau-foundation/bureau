// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestExtractArguments(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"project": {"type": "string", "description": "project room alias"},
			"title": {"type": "string", "description": "ticket title"},
			"body": {"type": "string", "description": "ticket body text"}
		},
		"required": ["project", "title"]
	}`)

	names, descriptions := ExtractArguments(schema)
	if len(names) != 3 {
		t.Fatalf("expected 3 arguments, got %d", len(names))
	}

	// Map iteration order is non-deterministic; sort for comparison.
	type pair struct{ name, description string }
	pairs := make([]pair, len(names))
	for i := range names {
		pairs[i] = pair{names[i], descriptions[i]}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].name < pairs[b].name })

	want := []pair{
		{"body", "ticket body text"},
		{"project", "project room alias"},
		{"title", "ticket title"},
	}

	for i, got := range pairs {
		if got != want[i] {
			t.Errorf("argument[%d] = %v, want %v", i, got, want[i])
		}
	}
}

func TestExtractArguments_Empty(t *testing.T) {
	names, descriptions := ExtractArguments(nil)
	if names != nil || descriptions != nil {
		t.Errorf("nil schema: names=%v descriptions=%v, want nil/nil", names, descriptions)
	}

	names, descriptions = ExtractArguments(json.RawMessage(`{}`))
	if len(names) != 0 || len(descriptions) != 0 {
		t.Errorf("empty schema: names=%v descriptions=%v, want empty", names, descriptions)
	}

	names, descriptions = ExtractArguments(json.RawMessage(`not valid json`))
	if names != nil || descriptions != nil {
		t.Errorf("invalid json: names=%v descriptions=%v, want nil/nil", names, descriptions)
	}
}

func TestExtractArguments_NoDescription(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	names, descriptions := ExtractArguments(schema)
	if len(names) != 1 {
		t.Fatalf("expected 1 argument, got %d", len(names))
	}
	if names[0] != "name" {
		t.Errorf("name = %q, want %q", names[0], "name")
	}
	if descriptions[0] != "" {
		t.Errorf("description = %q, want empty", descriptions[0])
	}
}
