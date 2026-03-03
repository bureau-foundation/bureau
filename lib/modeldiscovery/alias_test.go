// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"testing"
)

func TestGenerateAlias(t *testing.T) {
	entry := CatalogEntry{
		ID:                             "anthropic/claude-sonnet-4.6",
		Name:                           "Anthropic: Claude Sonnet 4.6",
		InputPriceMicrodollarsPerMtok:  3_000_000,
		OutputPriceMicrodollarsPerMtok: 15_000_000,
	}

	tests := []struct {
		name           string
		template       AliasTemplate
		wantAlias      string
		wantProvider   string
		wantModel      string
		wantInputPrice int64
	}{
		{
			"no prefix",
			AliasTemplate{Provider: "openrouter"},
			"claude-sonnet-4.6",
			"openrouter",
			"anthropic/claude-sonnet-4.6",
			3_000_000,
		},
		{
			"with prefix",
			AliasTemplate{Provider: "openrouter", Prefix: "anthropic/"},
			"anthropic/claude-sonnet-4.6",
			"openrouter",
			"anthropic/claude-sonnet-4.6",
			3_000_000,
		},
		{
			"different bureau provider",
			AliasTemplate{Provider: "my-provider"},
			"claude-sonnet-4.6",
			"my-provider",
			"anthropic/claude-sonnet-4.6",
			3_000_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alias, content := GenerateAlias(entry, test.template)
			if alias != test.wantAlias {
				t.Errorf("alias = %q, want %q", alias, test.wantAlias)
			}
			if content.Provider != test.wantProvider {
				t.Errorf("provider = %q, want %q", content.Provider, test.wantProvider)
			}
			if content.ProviderModel != test.wantModel {
				t.Errorf("provider_model = %q, want %q", content.ProviderModel, test.wantModel)
			}
			if content.Pricing.InputPerMtokMicrodollars != test.wantInputPrice {
				t.Errorf("input_price = %d, want %d", content.Pricing.InputPerMtokMicrodollars, test.wantInputPrice)
			}
		})
	}
}

func TestGenerateAlias_NoSlashInID(t *testing.T) {
	// Local model IDs might not have a provider prefix.
	entry := CatalogEntry{
		ID:   "qwen3-32b",
		Name: "Qwen3 32B",
	}

	alias, content := GenerateAlias(entry, AliasTemplate{Provider: "local"})
	if alias != "qwen3-32b" {
		t.Errorf("alias = %q, want %q", alias, "qwen3-32b")
	}
	if content.ProviderModel != "qwen3-32b" {
		t.Errorf("provider_model = %q, want %q", content.ProviderModel, "qwen3-32b")
	}
}

func TestGenerateAlias_Capabilities(t *testing.T) {
	entry := CatalogEntry{ID: "anthropic/claude-sonnet-4.6"}
	template := AliasTemplate{
		Provider:     "openrouter",
		Capabilities: []string{"code", "streaming", "reasoning"},
	}

	_, content := GenerateAlias(entry, template)
	if len(content.Capabilities) != 3 {
		t.Fatalf("capabilities length = %d, want 3", len(content.Capabilities))
	}
	if content.Capabilities[0] != "code" {
		t.Errorf("capabilities[0] = %q, want %q", content.Capabilities[0], "code")
	}
}

func TestGenerateAliases(t *testing.T) {
	entries := []CatalogEntry{
		{
			ID:                             "anthropic/claude-sonnet-4.6",
			InputPriceMicrodollarsPerMtok:  3_000_000,
			OutputPriceMicrodollarsPerMtok: 15_000_000,
		},
		{
			ID:                             "anthropic/claude-haiku-4.5",
			InputPriceMicrodollarsPerMtok:  800_000,
			OutputPriceMicrodollarsPerMtok: 4_000_000,
		},
	}

	template := AliasTemplate{Provider: "openrouter", Prefix: "claude/"}
	operations := GenerateAliases(entries, template)

	if len(operations) != 2 {
		t.Fatalf("got %d operations, want 2", len(operations))
	}

	// All initial operations should be "create".
	for i, operation := range operations {
		if operation.Action != "create" {
			t.Errorf("operations[%d].Action = %q, want %q", i, operation.Action, "create")
		}
		if operation.Content == nil {
			t.Errorf("operations[%d].Content is nil", i)
		}
	}

	if operations[0].Alias != "claude/claude-sonnet-4.6" {
		t.Errorf("operations[0].Alias = %q", operations[0].Alias)
	}
	if operations[1].Alias != "claude/claude-haiku-4.5" {
		t.Errorf("operations[1].Alias = %q", operations[1].Alias)
	}
}
