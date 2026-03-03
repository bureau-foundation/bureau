// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelregistry

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func openrouterProvider() model.ModelProviderContent {
	return model.ModelProviderContent{
		Endpoint:     "https://openrouter.ai/api",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat", "embeddings", "streaming"},
	}
}

func llamaLocalProvider() model.ModelProviderContent {
	return model.ModelProviderContent{
		Endpoint:     "unix:///run/bureau/service/llama.sock",
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"chat", "embeddings", "batch"},
		BatchSupport: true,
		MaxBatchSize: 32,
	}
}

func codexAlias() model.ModelAliasContent {
	return model.ModelAliasContent{
		Provider:      "openrouter",
		ProviderModel: "openai/gpt-4.1",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  2000,
			OutputPerMtokMicrodollars: 8000,
		},
		Capabilities: []string{"code", "reasoning", "streaming"},
	}
}

func localEmbedAlias() model.ModelAliasContent {
	return model.ModelAliasContent{
		Provider:      "llama-local",
		ProviderModel: "nomic-embed-text-v1.5",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars: 0,
		},
		Capabilities: []string{"embeddings", "batch"},
	}
}

func reasoningAlias() model.ModelAliasContent {
	return model.ModelAliasContent{
		Provider:      "openrouter",
		ProviderModel: "anthropic/claude-opus-4-20250514",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  15000,
			OutputPerMtokMicrodollars: 75000,
		},
		Capabilities: []string{"code", "reasoning", "streaming", "vision"},
	}
}

func populatedRegistry() *Registry {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())
	registry.SetProvider("llama-local", llamaLocalProvider())
	registry.SetAlias("codex", codexAlias())
	registry.SetAlias("local-embed", localEmbedAlias())
	registry.SetAlias("reasoning", reasoningAlias())
	return registry
}

func TestResolve_KnownAlias(t *testing.T) {
	registry := populatedRegistry()

	resolution, err := registry.Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex): %v", err)
	}

	if resolution.Alias != "codex" {
		t.Errorf("Alias = %q, want %q", resolution.Alias, "codex")
	}
	if resolution.ProviderName != "openrouter" {
		t.Errorf("ProviderName = %q, want %q", resolution.ProviderName, "openrouter")
	}
	if resolution.ProviderModel != "openai/gpt-4.1" {
		t.Errorf("ProviderModel = %q, want %q", resolution.ProviderModel, "openai/gpt-4.1")
	}
	if resolution.Provider.Endpoint != "https://openrouter.ai/api" {
		t.Errorf("Provider.Endpoint = %q, want %q", resolution.Provider.Endpoint, "https://openrouter.ai/api")
	}
	if resolution.Pricing.InputPerMtokMicrodollars != 2000 {
		t.Errorf("Pricing.Input = %d, want 2000", resolution.Pricing.InputPerMtokMicrodollars)
	}
	if resolution.Pricing.OutputPerMtokMicrodollars != 8000 {
		t.Errorf("Pricing.Output = %d, want 8000", resolution.Pricing.OutputPerMtokMicrodollars)
	}
}

func TestResolve_LocalProvider(t *testing.T) {
	registry := populatedRegistry()

	resolution, err := registry.Resolve("local-embed")
	if err != nil {
		t.Fatalf("Resolve(local-embed): %v", err)
	}

	if resolution.ProviderName != "llama-local" {
		t.Errorf("ProviderName = %q, want %q", resolution.ProviderName, "llama-local")
	}
	if resolution.Provider.AuthMethod != model.AuthMethodNone {
		t.Errorf("AuthMethod = %q, want %q", resolution.Provider.AuthMethod, model.AuthMethodNone)
	}
	if resolution.Pricing.InputPerMtokMicrodollars != 0 {
		t.Errorf("Pricing.Input = %d, want 0 (free local inference)", resolution.Pricing.InputPerMtokMicrodollars)
	}
}

func TestResolve_UnknownAlias(t *testing.T) {
	registry := populatedRegistry()

	_, err := registry.Resolve("nonexistent")
	if !errors.Is(err, ErrUnknownAlias) {
		t.Errorf("Resolve(nonexistent): got %v, want ErrUnknownAlias", err)
	}
}

func TestResolve_UnknownProvider(t *testing.T) {
	registry := New(slog.Default())
	// Alias references a provider that doesn't exist.
	registry.SetAlias("broken", model.ModelAliasContent{
		Provider:      "deleted-provider",
		ProviderModel: "some-model",
	})

	_, err := registry.Resolve("broken")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("Resolve(broken): got %v, want ErrUnknownProvider", err)
	}
}

func TestResolveAuto_CheapestMatch(t *testing.T) {
	registry := populatedRegistry()

	// Request "code" capability — both codex (10000 total cost) and
	// reasoning (90000 total cost) match. Auto should pick codex.
	resolution, err := registry.ResolveAuto([]string{"code"})
	if err != nil {
		t.Fatalf("ResolveAuto(code): %v", err)
	}

	if resolution.Alias != "codex" {
		t.Errorf("Alias = %q, want %q (cheapest match)", resolution.Alias, "codex")
	}
}

func TestResolveAuto_MultipleCapabilities(t *testing.T) {
	registry := populatedRegistry()

	// Request "code" + "vision" — only reasoning has both.
	resolution, err := registry.ResolveAuto([]string{"code", "vision"})
	if err != nil {
		t.Fatalf("ResolveAuto(code, vision): %v", err)
	}

	if resolution.Alias != "reasoning" {
		t.Errorf("Alias = %q, want %q (only match with vision)", resolution.Alias, "reasoning")
	}
}

func TestResolveAuto_EmbeddingsSelectsLocal(t *testing.T) {
	registry := populatedRegistry()

	// Request "embeddings" — both openrouter and local-embed match.
	// local-embed is free (cost 0), openrouter providers have
	// non-zero cost. Auto should pick local-embed.
	resolution, err := registry.ResolveAuto([]string{"embeddings"})
	if err != nil {
		t.Fatalf("ResolveAuto(embeddings): %v", err)
	}

	if resolution.Alias != "local-embed" {
		t.Errorf("Alias = %q, want %q (free local inference)", resolution.Alias, "local-embed")
	}
}

func TestResolveAuto_NoMatch(t *testing.T) {
	registry := populatedRegistry()

	_, err := registry.ResolveAuto([]string{"audio-generation"})
	if !errors.Is(err, ErrNoMatchingModel) {
		t.Errorf("ResolveAuto(audio-generation): got %v, want ErrNoMatchingModel", err)
	}
}

func TestResolveAuto_EmptyCapabilities(t *testing.T) {
	registry := populatedRegistry()

	// Empty capabilities means "any model" — should pick cheapest overall.
	resolution, err := registry.ResolveAuto([]string{})
	if err != nil {
		t.Fatalf("ResolveAuto(empty): %v", err)
	}

	// local-embed is free (0 total cost), cheapest.
	if resolution.Alias != "local-embed" {
		t.Errorf("Alias = %q, want %q (cheapest overall)", resolution.Alias, "local-embed")
	}
}

func TestResolveAuto_SkipsMissingProvider(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())
	// Don't register "llama-local" but add an alias referencing it.
	registry.SetAlias("broken-embed", model.ModelAliasContent{
		Provider:      "llama-local",
		ProviderModel: "nomic-embed-text-v1.5",
		Capabilities:  []string{"embeddings"},
	})
	registry.SetAlias("codex", codexAlias())

	// Auto should skip broken-embed and still find codex.
	resolution, err := registry.ResolveAuto([]string{"code"})
	if err != nil {
		t.Fatalf("ResolveAuto(code): %v", err)
	}
	if resolution.Alias != "codex" {
		t.Errorf("Alias = %q, want %q", resolution.Alias, "codex")
	}
}

func TestResolveAuto_DeterministicTieBreaking(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("same-provider", model.ModelProviderContent{
		Endpoint:     "https://example.com",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})

	// Two aliases with identical cost.
	registry.SetAlias("beta-model", model.ModelAliasContent{
		Provider:      "same-provider",
		ProviderModel: "model-b",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 1000},
		Capabilities:  []string{"chat"},
	})
	registry.SetAlias("alpha-model", model.ModelAliasContent{
		Provider:      "same-provider",
		ProviderModel: "model-a",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 1000},
		Capabilities:  []string{"chat"},
	})

	resolution, err := registry.ResolveAuto([]string{"chat"})
	if err != nil {
		t.Fatalf("ResolveAuto(chat): %v", err)
	}

	// Alphabetically first: "alpha-model" < "beta-model".
	if resolution.Alias != "alpha-model" {
		t.Errorf("Alias = %q, want %q (alphabetical tie-break)", resolution.Alias, "alpha-model")
	}
}

func TestSetRemove_Provider(t *testing.T) {
	registry := New(slog.Default())

	registry.SetProvider("test", openrouterProvider())
	if registry.ProviderCount() != 1 {
		t.Fatalf("ProviderCount = %d, want 1", registry.ProviderCount())
	}

	registry.RemoveProvider("test")
	if registry.ProviderCount() != 0 {
		t.Fatalf("ProviderCount = %d, want 0", registry.ProviderCount())
	}
}

func TestSetRemove_Alias(t *testing.T) {
	registry := New(slog.Default())

	registry.SetAlias("test", codexAlias())
	if registry.AliasCount() != 1 {
		t.Fatalf("AliasCount = %d, want 1", registry.AliasCount())
	}

	registry.RemoveAlias("test")
	if registry.AliasCount() != 0 {
		t.Fatalf("AliasCount = %d, want 0", registry.AliasCount())
	}
}

func TestUpdate_ProviderOverwrite(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())

	// Update with different endpoint.
	updated := openrouterProvider()
	updated.Endpoint = "https://new-openrouter.ai/api"
	registry.SetProvider("openrouter", updated)

	providers := registry.Providers()
	if providers["openrouter"].Endpoint != "https://new-openrouter.ai/api" {
		t.Errorf("Endpoint = %q, want updated value", providers["openrouter"].Endpoint)
	}
	if registry.ProviderCount() != 1 {
		t.Errorf("ProviderCount = %d, want 1 (overwrite, not duplicate)", registry.ProviderCount())
	}
}

func TestUpdate_AliasOverwrite(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())
	registry.SetAlias("codex", codexAlias())

	// Alias now points to a different model.
	updated := codexAlias()
	updated.ProviderModel = "openai/gpt-4.5"
	registry.SetAlias("codex", updated)

	resolution, err := registry.Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolution.ProviderModel != "openai/gpt-4.5" {
		t.Errorf("ProviderModel = %q, want updated value", resolution.ProviderModel)
	}
}

func TestSnapshots_AreCopies(t *testing.T) {
	registry := populatedRegistry()

	aliases := registry.Aliases()
	providers := registry.Providers()

	// Mutating the returned maps should not affect the registry.
	delete(aliases, "codex")
	delete(providers, "openrouter")

	if registry.AliasCount() != 3 {
		t.Errorf("AliasCount = %d, want 3 (snapshot mutation leaked)", registry.AliasCount())
	}
	if registry.ProviderCount() != 2 {
		t.Errorf("ProviderCount = %d, want 2 (snapshot mutation leaked)", registry.ProviderCount())
	}
}

func TestEmptyRegistry(t *testing.T) {
	registry := New(slog.Default())

	if registry.AliasCount() != 0 {
		t.Errorf("AliasCount = %d, want 0", registry.AliasCount())
	}
	if registry.ProviderCount() != 0 {
		t.Errorf("ProviderCount = %d, want 0", registry.ProviderCount())
	}

	_, err := registry.Resolve("anything")
	if !errors.Is(err, ErrUnknownAlias) {
		t.Errorf("Resolve on empty: got %v, want ErrUnknownAlias", err)
	}

	_, err = registry.ResolveAuto([]string{"chat"})
	if !errors.Is(err, ErrNoMatchingModel) {
		t.Errorf("ResolveAuto on empty: got %v, want ErrNoMatchingModel", err)
	}
}

func TestRemove_NonexistentIsNoop(t *testing.T) {
	registry := New(slog.Default())

	// Should not panic or error.
	registry.RemoveProvider("nonexistent")
	registry.RemoveAlias("nonexistent")

	if registry.ProviderCount() != 0 {
		t.Errorf("ProviderCount = %d after removing nonexistent", registry.ProviderCount())
	}
}

// --- ResolveChain tests ---

func TestResolveChain_NoFallbacks(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())
	registry.SetAlias("codex", codexAlias())

	chain, err := registry.ResolveChain("codex")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 resolution, got %d", len(chain))
	}
	if chain[0].ProviderName != "openrouter" {
		t.Errorf("provider = %q, want openrouter", chain[0].ProviderName)
	}
	if chain[0].ProviderModel != "openai/gpt-4.1" {
		t.Errorf("model = %q, want openai/gpt-4.1", chain[0].ProviderModel)
	}
}

func TestResolveChain_WithFallbacks(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())
	registry.SetProvider("anthropic-direct", model.ModelProviderContent{
		Endpoint:     "https://api.anthropic.com",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat", "streaming"},
	})

	alias := codexAlias()
	alias.Fallbacks = []model.ModelAliasFallback{
		{Provider: "anthropic-direct", ProviderModel: "claude-sonnet-4-6"},
	}
	registry.SetAlias("codex", alias)

	chain, err := registry.ResolveChain("codex")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 resolutions, got %d", len(chain))
	}

	if chain[0].ProviderName != "openrouter" {
		t.Errorf("chain[0] provider = %q, want openrouter", chain[0].ProviderName)
	}
	if chain[1].ProviderName != "anthropic-direct" {
		t.Errorf("chain[1] provider = %q, want anthropic-direct", chain[1].ProviderName)
	}
	if chain[1].ProviderModel != "claude-sonnet-4-6" {
		t.Errorf("chain[1] model = %q, want claude-sonnet-4-6", chain[1].ProviderModel)
	}
	// Fallback inherits the alias name and pricing from the primary.
	if chain[1].Alias != "codex" {
		t.Errorf("chain[1] alias = %q, want codex", chain[1].Alias)
	}
}

func TestResolveChain_MissingFallbackProvider(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("openrouter", openrouterProvider())

	alias := codexAlias()
	alias.Fallbacks = []model.ModelAliasFallback{
		{Provider: "nonexistent", ProviderModel: "some-model"},
	}
	registry.SetAlias("codex", alias)

	chain, err := registry.ResolveChain("codex")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	// Missing fallback provider is skipped with a warning (not an error).
	// The chain contains only the primary.
	if len(chain) != 1 {
		t.Fatalf("expected 1 resolution (fallback skipped), got %d", len(chain))
	}
}

func TestResolveChain_UnknownAlias(t *testing.T) {
	registry := New(slog.Default())

	_, err := registry.ResolveChain("nonexistent")
	if !errors.Is(err, ErrUnknownAlias) {
		t.Fatalf("expected ErrUnknownAlias, got %v", err)
	}
}

func TestResolveChain_MissingPrimaryProvider(t *testing.T) {
	registry := New(slog.Default())
	// Alias references a provider that isn't registered.
	registry.SetAlias("broken", codexAlias())

	_, err := registry.ResolveChain("broken")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", err)
	}
}

func TestResolveChain_MultipleFallbacks(t *testing.T) {
	registry := New(slog.Default())
	registry.SetProvider("primary", model.ModelProviderContent{
		Endpoint:     "https://primary.ai",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})
	registry.SetProvider("secondary", model.ModelProviderContent{
		Endpoint:     "https://secondary.ai",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})
	registry.SetProvider("tertiary", model.ModelProviderContent{
		Endpoint:     "unix:///run/local-llm.sock",
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"chat"},
	})

	alias := model.ModelAliasContent{
		Provider:      "primary",
		ProviderModel: "gpt-5",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 5000},
		Fallbacks: []model.ModelAliasFallback{
			{Provider: "secondary", ProviderModel: "claude-opus-4.6"},
			{Provider: "tertiary", ProviderModel: "qwen3-32b"},
		},
	}
	registry.SetAlias("ha-reasoning", alias)

	chain, err := registry.ResolveChain("ha-reasoning")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 resolutions, got %d", len(chain))
	}
	if chain[0].ProviderName != "primary" {
		t.Errorf("chain[0] = %q, want primary", chain[0].ProviderName)
	}
	if chain[1].ProviderName != "secondary" {
		t.Errorf("chain[1] = %q, want secondary", chain[1].ProviderName)
	}
	if chain[2].ProviderName != "tertiary" {
		t.Errorf("chain[2] = %q, want tertiary", chain[2].ProviderName)
	}
}
