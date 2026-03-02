// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package modelregistry provides an in-memory index of model providers
// and aliases for the model service. The sync loop populates the
// registry from Matrix state events (m.bureau.model_provider and
// m.bureau.model_alias); request handlers query it to resolve model
// aliases to concrete provider routing information.
//
// The registry is thread-safe: the sync loop calls Set/Remove methods
// while request handlers concurrently call Resolve. An RWMutex
// protects the maps.
//
// model-service.md defines the design. lib/schema/model defines the
// state event content types.
package modelregistry

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// Errors returned by resolution methods.
var (
	// ErrUnknownAlias means the requested alias has no matching
	// m.bureau.model_alias state event in the registry.
	ErrUnknownAlias = errors.New("modelregistry: unknown model alias")

	// ErrUnknownProvider means an alias references a provider name
	// that has no matching m.bureau.model_provider state event.
	// This is a configuration error — the alias and provider events
	// are out of sync.
	ErrUnknownProvider = errors.New("modelregistry: alias references unknown provider")

	// ErrNoMatchingModel means auto-selection found no model whose
	// capabilities are a superset of the requested capabilities.
	ErrNoMatchingModel = errors.New("modelregistry: no model matches requested capabilities")

	// ErrNoMatchingAccount means no account matches the (project,
	// provider) pair. Either no account covers the provider, or no
	// account's project list includes the requesting project (or
	// the wildcard "*").
	ErrNoMatchingAccount = errors.New("modelregistry: no account matches (project, provider) pair")
)

// Resolution is the result of resolving a model alias to a concrete
// provider and model. Contains everything the model service needs to
// route a request: where to send it, what model name to use, and how
// much it costs.
type Resolution struct {
	// Alias is the alias name that was resolved (e.g., "codex").
	Alias string

	// ProviderName is the provider's state key (e.g., "openrouter").
	ProviderName string

	// Provider is the full provider configuration: endpoint, auth
	// method, capabilities, batch support.
	Provider model.ModelProviderContent

	// ProviderModel is the model identifier as the provider knows it
	// (e.g., "openai/gpt-4.1"). This is the value sent in the
	// provider API's "model" field.
	ProviderModel string

	// Pricing is the cost structure for this model, used for cost
	// calculation after the request completes.
	Pricing model.Pricing

	// Capabilities describes what this model supports, carried from
	// the alias configuration.
	Capabilities []string
}

// Registry is a thread-safe in-memory index of model providers,
// aliases, and accounts. The sync loop calls Set/Remove methods to
// populate it from Matrix state events (m.bureau.model_provider,
// m.bureau.model_alias, m.bureau.model_account); request handlers
// call Resolve and SelectAccount to route model requests.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]model.ModelProviderContent // state key (provider name) → content
	aliases   map[string]model.ModelAliasContent    // state key (alias name) → content
	accounts  map[string]model.ModelAccountContent  // state key (account name) → content
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		providers: make(map[string]model.ModelProviderContent),
		aliases:   make(map[string]model.ModelAliasContent),
		accounts:  make(map[string]model.ModelAccountContent),
	}
}

// SetProvider adds or updates a provider in the registry. Called by
// the sync loop when an m.bureau.model_provider state event arrives.
// The name is the state key (provider name).
func (r *Registry) SetProvider(name string, content model.ModelProviderContent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = content
}

// RemoveProvider removes a provider from the registry. Called when
// an m.bureau.model_provider state event has empty content (deletion).
func (r *Registry) RemoveProvider(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, name)
}

// SetAlias adds or updates a model alias in the registry. Called by
// the sync loop when an m.bureau.model_alias state event arrives.
// The name is the state key (alias name).
func (r *Registry) SetAlias(name string, content model.ModelAliasContent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[name] = content
}

// RemoveAlias removes a model alias from the registry.
func (r *Registry) RemoveAlias(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.aliases, name)
}

// Resolve looks up a model alias and returns the full routing
// information. Returns ErrUnknownAlias if the alias doesn't exist,
// or ErrUnknownProvider if the alias references a provider that isn't
// registered.
//
// The special alias "auto" is handled by ResolveAuto — callers should
// check for "auto" and call ResolveAuto with the desired capabilities
// instead of calling Resolve("auto").
func (r *Registry) Resolve(alias string) (Resolution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	aliasContent, ok := r.aliases[alias]
	if !ok {
		return Resolution{}, fmt.Errorf("%w: %q", ErrUnknownAlias, alias)
	}

	provider, ok := r.providers[aliasContent.Provider]
	if !ok {
		return Resolution{}, fmt.Errorf("%w: alias %q references provider %q",
			ErrUnknownProvider, alias, aliasContent.Provider)
	}

	return Resolution{
		Alias:         alias,
		ProviderName:  aliasContent.Provider,
		Provider:      provider,
		ProviderModel: aliasContent.ProviderModel,
		Pricing:       aliasContent.Pricing,
		Capabilities:  aliasContent.Capabilities,
	}, nil
}

// ResolveAuto selects the cheapest model whose capabilities are a
// superset of the requested capabilities. Returns ErrNoMatchingModel
// if no alias satisfies all requested capabilities. If multiple models
// tie on total cost (input + output pricing), the one with the
// alphabetically-first alias name wins (deterministic tie-breaking).
//
// "Cheapest" is defined as the sum of input and output per-million-token
// microdollar pricing. This is a rough heuristic — it doesn't account
// for actual token volumes. For cost-sensitive workloads, agents should
// specify an explicit alias rather than relying on auto-selection.
func (r *Registry) ResolveAuto(capabilities []string) (Resolution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var bestAlias string
	var bestCost int64 = math.MaxInt64
	var bestResolution Resolution

	for alias, aliasContent := range r.aliases {
		if !hasAllCapabilities(aliasContent.Capabilities, capabilities) {
			continue
		}

		provider, ok := r.providers[aliasContent.Provider]
		if !ok {
			// Skip aliases with missing providers rather than
			// failing auto-selection entirely. The specific alias
			// will fail with ErrUnknownProvider if addressed directly.
			continue
		}

		cost := aliasContent.Pricing.InputPerMtokMicrodollars +
			aliasContent.Pricing.OutputPerMtokMicrodollars

		if cost < bestCost || (cost == bestCost && alias < bestAlias) {
			bestAlias = alias
			bestCost = cost
			bestResolution = Resolution{
				Alias:         alias,
				ProviderName:  aliasContent.Provider,
				Provider:      provider,
				ProviderModel: aliasContent.ProviderModel,
				Pricing:       aliasContent.Pricing,
				Capabilities:  aliasContent.Capabilities,
			}
		}
	}

	if bestAlias == "" {
		return Resolution{}, fmt.Errorf("%w: capabilities %v", ErrNoMatchingModel, capabilities)
	}

	return bestResolution, nil
}

// GetProvider returns the configuration for a single provider by name.
// Returns false if the provider is not registered.
func (r *Registry) GetProvider(name string) (model.ModelProviderContent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.providers[name]
	return provider, ok
}

// Aliases returns a snapshot of all registered aliases. The returned
// map is a copy — callers can read it without holding the lock.
func (r *Registry) Aliases() map[string]model.ModelAliasContent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]model.ModelAliasContent, len(r.aliases))
	for name, content := range r.aliases {
		result[name] = content
	}
	return result
}

// Providers returns a snapshot of all registered providers.
func (r *Registry) Providers() map[string]model.ModelProviderContent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]model.ModelProviderContent, len(r.providers))
	for name, content := range r.providers {
		result[name] = content
	}
	return result
}

// AliasCount returns the number of registered aliases.
func (r *Registry) AliasCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.aliases)
}

// ProviderCount returns the number of registered providers.
func (r *Registry) ProviderCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// hasAllCapabilities reports whether available is a superset of required.
func hasAllCapabilities(available, required []string) bool {
	for _, req := range required {
		if !slices.Contains(available, req) {
			return false
		}
	}
	return true
}
