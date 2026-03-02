// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

// Query response types for the model service's introspection actions
// (model/list and model/status). These flow over the service socket
// encoded as CBOR through the standard SocketServer response envelope.
// The CBOR codec uses json tags for field names.
//
// Both the model service binary (server side) and the bureau CLI
// (client side) import these types. A single source of truth prevents
// silent deserialization mismatches from duplicated struct definitions.

// ModelListEntry is a single alias in the model/list response,
// enriched with resolved provider details.
type ModelListEntry struct {
	// Alias is the model alias name (state key in EventTypeModelAlias).
	Alias string `json:"alias" desc:"model alias name"`

	// Provider is the provider name this alias routes to.
	Provider string `json:"provider" desc:"provider name"`

	// ProviderModel is the provider-specific model identifier.
	ProviderModel string `json:"provider_model" desc:"provider-specific model ID"`

	// Capabilities lists what this model supports (e.g., "code",
	// "reasoning", "embeddings", "streaming").
	Capabilities []string `json:"capabilities,omitempty" desc:"model capabilities"`

	// Pricing is the cost per million tokens in microdollars.
	Pricing Pricing `json:"pricing" desc:"cost per million tokens in microdollars"`

	// Endpoint is the provider's API address. Included so operators
	// can verify routing without checking Matrix state events.
	Endpoint string `json:"endpoint,omitempty" desc:"provider endpoint URL"`

	// AuthMethod is how the model service authenticates to this
	// provider (bearer token or none for local).
	AuthMethod AuthMethod `json:"auth_method,omitempty" desc:"authentication method"`
}

// ModelListResponse is the response for the model/list action.
type ModelListResponse struct {
	// Aliases is the list of registered model aliases.
	Aliases []ModelListEntry `json:"aliases" desc:"registered model aliases"`

	// Providers is the number of registered providers.
	Providers int `json:"providers" desc:"number of registered providers"`

	// Accounts is the number of registered accounts.
	Accounts int `json:"accounts" desc:"number of registered accounts"`
}

// AccountStatusEntry describes the quota state of a single account
// in the model/status response. Combines the static account
// configuration (provider, projects, quota limits) with live spend
// data from the quota tracker.
type AccountStatusEntry struct {
	// Name is the account name (state key in EventTypeModelAccount).
	Name string `json:"name" desc:"account name"`

	// Provider is the provider this account authenticates to.
	Provider string `json:"provider" desc:"provider name"`

	// Projects lists the project names this account covers.
	Projects []string `json:"projects" desc:"covered project names"`

	// Priority determines account selection when multiple accounts
	// match the same (project, provider) pair.
	Priority int `json:"priority" desc:"account selection priority"`

	// Quota defines spending limits. Nil means unlimited.
	Quota *Quota `json:"quota,omitempty" desc:"spending limits"`

	// DailySpendMicrodollars is the cumulative spend in the current
	// UTC day.
	DailySpendMicrodollars int64 `json:"daily_spend_microdollars" desc:"current daily spend in microdollars"`

	// MonthlySpendMicrodollars is the cumulative spend in the
	// current UTC month.
	MonthlySpendMicrodollars int64 `json:"monthly_spend_microdollars" desc:"current monthly spend in microdollars"`
}

// AccountStatusResponse is the response for the model/status action.
type AccountStatusResponse struct {
	// Accounts lists quota status for all registered accounts.
	Accounts []AccountStatusEntry `json:"accounts" desc:"account quota status entries"`
}
