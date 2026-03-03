// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import "testing"

func TestRuleMatches(t *testing.T) {
	sonnet := CatalogEntry{
		ID:                            "anthropic/claude-sonnet-4.6",
		ContextLength:                 200000,
		InputPriceMicrodollarsPerMtok: 3_000_000,
		SupportedParameters:           []string{"tools", "temperature", "reasoning"},
	}
	qwen32b := CatalogEntry{
		ID:                            "qwenlm/qwen3-32b",
		ContextLength:                 131072,
		InputPriceMicrodollarsPerMtok: 200_000,
		SupportedParameters:           []string{"temperature", "top_p"},
	}
	qwenFree := CatalogEntry{
		ID:                            "qwenlm/qwen3-8b:free",
		ContextLength:                 32768,
		InputPriceMicrodollarsPerMtok: 0,
		SupportedParameters:           []string{"temperature"},
	}
	preview := CatalogEntry{
		ID:                            "openai/gpt-5-preview",
		ContextLength:                 128000,
		InputPriceMicrodollarsPerMtok: 10_000_000,
		SupportedParameters:           []string{"tools", "temperature"},
	}

	tests := []struct {
		name  string
		rule  Rule
		entry CatalogEntry
		want  bool
	}{
		{"exact match", Rule{Match: "anthropic/claude-sonnet-4.6"}, sonnet, true},
		{"glob match", Rule{Match: "anthropic/*"}, sonnet, true},
		{"prefix glob", Rule{Match: "qwenlm/qwen3*"}, qwen32b, true},
		{"prefix glob free", Rule{Match: "qwenlm/qwen3*"}, qwenFree, true},
		{"no match", Rule{Match: "openai/*"}, sonnet, false},
		{"empty match", Rule{Match: ""}, sonnet, false},
		{"wildcard all", Rule{Match: "**"}, sonnet, true},

		// Exclusion
		{"exclude free", Rule{Match: "qwenlm/*", Exclude: []string{"**/*:free"}}, qwen32b, true},
		{"excluded free", Rule{Match: "qwenlm/*", Exclude: []string{"**/*:free"}}, qwenFree, false},
		{"exclude preview", Rule{Match: "**", Exclude: []string{"**/*preview*"}}, preview, false},
		{"exclude preview spares others", Rule{Match: "**", Exclude: []string{"**/*preview*"}}, sonnet, true},

		// Context length
		{"min context pass", Rule{Match: "**", MinContextLength: 100000}, sonnet, true},
		{"min context fail", Rule{Match: "**", MinContextLength: 100000}, qwenFree, false},
		{"min context zero", Rule{Match: "**", MinContextLength: 0}, qwenFree, true},

		// Price
		{"max price pass", Rule{Match: "**", MaxInputPriceMicrodollarsPerMtok: 5_000_000}, sonnet, true},
		{"max price fail", Rule{Match: "**", MaxInputPriceMicrodollarsPerMtok: 1_000_000}, sonnet, false},
		{"max price zero means no limit", Rule{Match: "**", MaxInputPriceMicrodollarsPerMtok: 0}, sonnet, true},
		{"free model under max", Rule{Match: "**", MaxInputPriceMicrodollarsPerMtok: 1_000_000}, qwenFree, true},

		// Required parameters
		{"required param present", Rule{Match: "**", RequiredParameters: []string{"tools"}}, sonnet, true},
		{"required param missing", Rule{Match: "**", RequiredParameters: []string{"tools"}}, qwen32b, false},
		{"multiple required all present", Rule{Match: "**", RequiredParameters: []string{"tools", "reasoning"}}, sonnet, true},
		{"multiple required one missing", Rule{Match: "**", RequiredParameters: []string{"tools", "reasoning"}}, preview, false},

		// Combined filters
		{
			"combined: cheap qwen with context",
			Rule{
				Match:                            "qwenlm/qwen3*",
				Exclude:                          []string{"*:free"},
				MinContextLength:                 64000,
				MaxInputPriceMicrodollarsPerMtok: 1_000_000,
			},
			qwen32b,
			true,
		},
		{
			"combined: cheap qwen rejects free",
			Rule{
				Match:                            "qwenlm/qwen3*",
				Exclude:                          []string{"*:free"},
				MinContextLength:                 64000,
				MaxInputPriceMicrodollarsPerMtok: 1_000_000,
			},
			qwenFree,
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.rule.Matches(test.entry)
			if got != test.want {
				t.Errorf("rule.Matches(%s) = %v, want %v", test.entry.ID, got, test.want)
			}
		})
	}
}

func TestApplyRules(t *testing.T) {
	catalog := []CatalogEntry{
		{ID: "anthropic/claude-sonnet-4.6", ContextLength: 200000, InputPriceMicrodollarsPerMtok: 3_000_000},
		{ID: "anthropic/claude-haiku-4.5", ContextLength: 200000, InputPriceMicrodollarsPerMtok: 800_000},
		{ID: "qwenlm/qwen3-32b", ContextLength: 131072, InputPriceMicrodollarsPerMtok: 200_000},
		{ID: "qwenlm/qwen3-8b:free", ContextLength: 32768, InputPriceMicrodollarsPerMtok: 0},
		{ID: "openai/gpt-4.1", ContextLength: 1047576, InputPriceMicrodollarsPerMtok: 2_000_000},
	}

	t.Run("single rule", func(t *testing.T) {
		rules := []Rule{{Match: "anthropic/*"}}
		result := ApplyRules(catalog, rules)
		if len(result) != 2 {
			t.Fatalf("got %d entries, want 2", len(result))
		}
	})

	t.Run("multiple rules union", func(t *testing.T) {
		rules := []Rule{
			{Match: "anthropic/*"},
			{Match: "qwenlm/*", Exclude: []string{"**/*:free"}},
		}
		result := ApplyRules(catalog, rules)
		if len(result) != 3 {
			t.Fatalf("got %d entries, want 3 (2 anthropic + 1 qwen non-free)", len(result))
		}
	})

	t.Run("no rules", func(t *testing.T) {
		result := ApplyRules(catalog, nil)
		if result != nil {
			t.Fatalf("got %d entries, want nil", len(result))
		}
	})

	t.Run("no matches", func(t *testing.T) {
		rules := []Rule{{Match: "google/*"}}
		result := ApplyRules(catalog, rules)
		if len(result) != 0 {
			t.Fatalf("got %d entries, want 0", len(result))
		}
	})

	t.Run("deduplication", func(t *testing.T) {
		// Two rules both match the same model.
		rules := []Rule{
			{Match: "anthropic/*"},
			{Match: "**", MaxInputPriceMicrodollarsPerMtok: 5_000_000},
		}
		result := ApplyRules(catalog, rules)

		ids := make(map[string]int)
		for _, entry := range result {
			ids[entry.ID]++
		}
		for id, count := range ids {
			if count > 1 {
				t.Errorf("model %q appeared %d times, want 1", id, count)
			}
		}
	})
}
