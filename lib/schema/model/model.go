// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package model defines the schema types for Bureau's model service —
// a unified inference gateway that routes completion, embedding, and
// streaming requests to external providers (OpenRouter, Anthropic) and
// local inference engines (llama.cpp, vLLM, IREE).
//
// Three categories of types live here:
//
//   - Matrix state event content structs (JSON): ModelProviderContent,
//     ModelAliasContent, ModelAccountContent. These configure the model
//     service via Matrix rooms and /sync.
//
//   - CBOR wire protocol types (wire.go): Request, Response, Message,
//     and related types for the native service socket API. These use
//     CBOR keyasint encoding for compact binary transmission.
//
//   - Authorization action constants (action.go): grant patterns for
//     the service token authorization system.
//
// model-service.md defines the full design. fundamentals.md defines the
// service socket mechanism. authorization.md defines the grant system.
package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// Event type constants for model service configuration. These are
// Matrix state events published to the model service's configuration
// room. The model service reads them at startup and via incremental
// /sync during operation.
const (
	// EventTypeModelProvider registers a backend that the model service
	// can route to. Providers are either HTTP upstreams (OpenRouter,
	// OpenAI) with bearer token auth, local inference engines (llama.cpp,
	// vLLM) on Unix sockets with no auth, or future CBOR-native engines
	// (IREE) with shared memory.
	//
	// State key: provider name (e.g., "openrouter", "llama-local")
	// Room: model service config room
	EventTypeModelProvider ref.EventType = "m.bureau.model_provider"

	// EventTypeModelAlias maps a stable alias to a (provider, model)
	// pair. When a provider's model names change (version rotations,
	// deprecations), a single alias update propagates to all agents.
	// Agents request models by alias, never by provider-specific name.
	//
	// State key: alias name (e.g., "codex", "local-embed", "reasoning")
	// Room: model service config room
	EventTypeModelAlias ref.EventType = "m.bureau.model_alias"

	// EventTypeModelAccount binds a provider API key to a set of
	// projects. The model service selects the most specific matching
	// account for each (project, provider) pair: explicit project match
	// wins over wildcard, higher priority wins ties.
	//
	// The actual API key is in the model service's sealed credential
	// bundle, delivered by the launcher. Only the credential name
	// appears in this event — keys never appear in Matrix state.
	//
	// State key: account name (e.g., "openrouter-alice", "openrouter-shared")
	// Room: model service config room
	EventTypeModelAccount ref.EventType = "m.bureau.model_account"
)

// AuthMethod identifies how the model service authenticates to a
// provider backend. HTTP upstreams use bearer tokens; local service
// principals use Bureau-internal service token auth (no external
// credential needed).
type AuthMethod string

const (
	// AuthMethodBearer injects a bearer token from the project's
	// account credential into the Authorization header. Used for
	// external HTTP providers (OpenRouter, OpenAI, Anthropic).
	AuthMethodBearer AuthMethod = "bearer"

	// AuthMethodNone means no external credential is needed. Used
	// for local inference principals (llama.cpp, vLLM) that
	// authenticate via Bureau service tokens on Unix sockets.
	AuthMethodNone AuthMethod = "none"
)

// IsKnown reports whether m is one of the defined AuthMethod values.
func (m AuthMethod) IsKnown() bool {
	switch m {
	case AuthMethodBearer, AuthMethodNone:
		return true
	}
	return false
}

// ModelProviderContent is the content of an EventTypeModelProvider
// state event. Registers a backend that the model service routes
// requests to. Provider types:
//
//   - HTTP upstream (OpenRouter, OpenAI): REST API at an HTTPS endpoint,
//     bearer token auth from project accounts.
//   - Local service (llama.cpp, vLLM): Bureau service principal in a
//     sandbox with GPU device access, OpenAI-compatible HTTP on a Unix
//     socket, no external credential.
//   - CBOR service (future IREE): native CBOR protocol on a Unix socket,
//     binary data without serialization overhead.
type ModelProviderContent struct {
	// Endpoint is the provider's API address. For HTTP upstreams, a
	// full URL (e.g., "https://openrouter.ai/api"). For local services,
	// a Unix socket URI (e.g., "unix:///run/bureau/service/llama.sock").
	Endpoint string `json:"endpoint"`

	// AuthMethod determines how the model service authenticates to
	// this provider. See AuthMethod constants.
	AuthMethod AuthMethod `json:"auth_method"`

	// Capabilities lists what this provider supports. The model service
	// uses these to validate that a requested operation (chat,
	// embeddings, streaming, batch) is available on the target provider.
	// Empty capabilities means the provider supports nothing — this is
	// an error, not a wildcard.
	Capabilities []string `json:"capabilities"`

	// BatchSupport indicates whether the provider accepts batched
	// requests (multiple inputs in a single API call). When true,
	// the model service's batch accumulator may send grouped requests
	// to this provider.
	BatchSupport bool `json:"batch_support,omitempty"`

	// MaxBatchSize is the maximum number of requests the provider
	// accepts in a single batch. Zero means no limit (provider default).
	// Only meaningful when BatchSupport is true.
	MaxBatchSize int `json:"max_batch_size,omitempty"`

	// HTTPAuthHeader is the HTTP header name for credential injection
	// in the HTTP compatibility proxy. When empty, defaults to
	// "Authorization" with "Bearer " scheme prefix. Set to a custom
	// header name (e.g., "x-api-key") for providers that use non-
	// standard auth headers — custom headers receive the raw credential
	// value without a scheme prefix.
	//
	// This field only affects the HTTP compatibility layer. The CBOR
	// service socket uses Bureau service tokens directly and does not
	// use HTTP headers.
	HTTPAuthHeader string `json:"http_auth_header,omitempty"`
}

// Validate checks that the provider content is well-formed. Returns
// an error describing the first invalid field.
func (content *ModelProviderContent) Validate() error {
	if content.Endpoint == "" {
		return errors.New("model provider: endpoint must not be empty")
	}
	if !content.AuthMethod.IsKnown() {
		return fmt.Errorf("model provider: unknown auth_method %q", content.AuthMethod)
	}
	if len(content.Capabilities) == 0 {
		return errors.New("model provider: capabilities must not be empty")
	}
	if content.MaxBatchSize < 0 {
		return fmt.Errorf("model provider: max_batch_size must be non-negative, got %d", content.MaxBatchSize)
	}
	if content.MaxBatchSize > 0 && !content.BatchSupport {
		return errors.New("model provider: max_batch_size set without batch_support")
	}
	return nil
}

// Pricing describes the cost of using a model through a provider, in
// microdollars per million tokens. Using microdollars (1 USD = 1,000,000
// microdollars) avoids floating-point and complies with Matrix canonical
// JSON (no fractional numbers). Per-million-token granularity matches
// how providers publish their pricing.
type Pricing struct {
	// InputPerMtokMicrodollars is the cost in microdollars per million
	// input tokens. Zero for free models (local inference).
	InputPerMtokMicrodollars int64 `json:"input_per_mtok_microdollars"`

	// OutputPerMtokMicrodollars is the cost in microdollars per million
	// output tokens. Zero for embedding models (no output tokens) and
	// free models.
	OutputPerMtokMicrodollars int64 `json:"output_per_mtok_microdollars,omitempty"`
}

// ModelAliasContent is the content of an EventTypeModelAlias state
// event. Maps a stable alias name to a specific provider and model,
// with pricing and capability metadata. Agents request models by alias;
// the model service resolves to (provider, provider_model) at request
// time.
//
// When a provider's model is updated (e.g., "gpt-4.1" to "gpt-4.1-2026-05"),
// changing one alias event updates all agents. No template changes, no
// credential bundle updates.
type ModelAliasContent struct {
	// Provider is the name of the provider (state key in
	// EventTypeModelProvider) that serves this model.
	Provider string `json:"provider"`

	// ProviderModel is the model identifier as the provider knows it
	// (e.g., "openai/gpt-4.1", "nomic-embed-text-v1.5"). This is the
	// value sent in the provider API's "model" field.
	ProviderModel string `json:"provider_model"`

	// Pricing is the cost structure for this model. Used for cost
	// estimation and accounting. Zero pricing (local models) is valid.
	Pricing Pricing `json:"pricing"`

	// Capabilities describes what this specific model supports. Used
	// for auto-selection when agents request model="auto". Examples:
	// "code", "reasoning", "embeddings", "streaming", "vision", "batch".
	Capabilities []string `json:"capabilities,omitempty"`

	// Fallbacks is an ordered list of alternative (provider, model)
	// pairs to try when the primary is unavailable. Used by HA agents
	// (sysadmins, fleet controllers) that must not die when their
	// primary model provider goes down or rotates models.
	//
	// Non-HA agents omit this field — fail-loud is correct when model
	// identity matters (code review agents, specialized analysis).
	Fallbacks []ModelAliasFallback `json:"fallbacks,omitempty"`
}

// ModelAliasFallback is an alternative (provider, model) pair in a
// fallback chain. Resolution tries the primary first, then fallbacks
// in order.
type ModelAliasFallback struct {
	// Provider is the name of the fallback provider (state key in
	// EventTypeModelProvider).
	Provider string `json:"provider"`

	// ProviderModel is the model identifier at the fallback provider.
	ProviderModel string `json:"provider_model"`
}

// Validate checks that the fallback entry is well-formed.
func (fallback *ModelAliasFallback) Validate() error {
	if fallback.Provider == "" {
		return errors.New("model alias fallback: provider must not be empty")
	}
	if fallback.ProviderModel == "" {
		return errors.New("model alias fallback: provider_model must not be empty")
	}
	return nil
}

// Validate checks that the alias content is well-formed.
func (content *ModelAliasContent) Validate() error {
	if content.Provider == "" {
		return errors.New("model alias: provider must not be empty")
	}
	if content.ProviderModel == "" {
		return errors.New("model alias: provider_model must not be empty")
	}
	if content.Pricing.InputPerMtokMicrodollars < 0 {
		return fmt.Errorf("model alias: input pricing must be non-negative, got %d", content.Pricing.InputPerMtokMicrodollars)
	}
	if content.Pricing.OutputPerMtokMicrodollars < 0 {
		return fmt.Errorf("model alias: output pricing must be non-negative, got %d", content.Pricing.OutputPerMtokMicrodollars)
	}
	for i := range content.Fallbacks {
		if err := content.Fallbacks[i].Validate(); err != nil {
			return fmt.Errorf("model alias: fallback[%d]: %w", i, err)
		}
	}
	return nil
}

// Quota defines spending limits for a model account. Both fields are
// optional — an account may have daily limits, monthly limits, both,
// or neither. The model service checks cumulative spend against these
// limits before forwarding requests. See model-service.md for the
// quota enforcement semantics.
type Quota struct {
	// DailyMicrodollars is the maximum spend per UTC day in
	// microdollars. Zero means no daily limit.
	DailyMicrodollars int64 `json:"daily_microdollars,omitempty"`

	// MonthlyMicrodollars is the maximum spend per calendar month
	// in microdollars. Zero means no monthly limit.
	MonthlyMicrodollars int64 `json:"monthly_microdollars,omitempty"`
}

// ModelAccountContent is the content of an EventTypeModelAccount state
// event. Binds a provider API key (via credential_ref) to a set of
// projects for cost attribution and quota enforcement.
//
// Account selection for a (project, provider) pair:
//   - Explicit project match wins over wildcard ("*").
//   - Higher priority (numerically larger) wins ties.
//   - This lets operators bring their own API keys for their projects
//     while having a shared fallback for everything else.
type ModelAccountContent struct {
	// Provider is the name of the provider (state key in
	// EventTypeModelProvider) this account authenticates to.
	Provider string `json:"provider"`

	// CredentialRef names an entry in the model service's sealed
	// credential bundle. The actual API key is delivered by the
	// launcher via the standard credential pipeline — it never
	// appears in Matrix state events.
	//
	// Empty for accounts that route to providers with
	// AuthMethodNone (local inference). The model service validates
	// at runtime that the credential exists when the provider
	// requires auth.
	CredentialRef string `json:"credential_ref,omitempty"`

	// Projects lists the project names this account covers. Each
	// entry is either an exact project name (e.g., "iree-amdgpu")
	// or the wildcard "*" for a fallback account.
	Projects []string `json:"projects"`

	// Priority determines account selection when multiple accounts
	// match the same (project, provider) pair. Higher values win.
	// Use 0 for normal accounts and negative values for fallback
	// accounts.
	Priority int `json:"priority"`

	// Quota defines spending limits for this account. Nil means
	// unlimited spending (no quota enforcement).
	Quota *Quota `json:"quota,omitempty"`
}

// Validate checks that the account content is well-formed.
func (content *ModelAccountContent) Validate() error {
	if content.Provider == "" {
		return errors.New("model account: provider must not be empty")
	}
	if len(content.Projects) == 0 {
		return errors.New("model account: projects must not be empty")
	}
	for i, project := range content.Projects {
		if project == "" {
			return fmt.Errorf("model account: projects[%d] must not be empty", i)
		}
		if project != "*" && strings.ContainsAny(project, " \t\n") {
			return fmt.Errorf("model account: projects[%d] %q contains whitespace", i, project)
		}
	}
	if content.Quota != nil {
		if content.Quota.DailyMicrodollars < 0 {
			return fmt.Errorf("model account: daily quota must be non-negative, got %d", content.Quota.DailyMicrodollars)
		}
		if content.Quota.MonthlyMicrodollars < 0 {
			return fmt.Errorf("model account: monthly quota must be non-negative, got %d", content.Quota.MonthlyMicrodollars)
		}
	}
	return nil
}
