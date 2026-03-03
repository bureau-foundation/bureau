// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

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

// --- Sync request/response types ---

// SyncRequest describes a batch of alias operations to apply. Used
// by the model/sync action to create, update, or delete model aliases
// from catalog discovery rules.
type SyncRequest struct {
	// Operations is the list of alias operations to apply.
	Operations []AliasOperation `json:"operations" desc:"alias operations to apply"`
}

// AliasOperation is a single create, update, or delete operation on
// a model alias.
type AliasOperation struct {
	// Action is "create", "update", or "delete".
	Action string `json:"action" desc:"operation type: create, update, or delete"`

	// Alias is the alias name (state key in m.bureau.model_alias).
	Alias string `json:"alias" desc:"alias name"`

	// Content is the alias configuration. Required for create and
	// update operations. Ignored for delete.
	Content *ModelAliasContent `json:"content,omitempty" desc:"alias configuration"`
}

// SyncResponse summarizes the result of a sync operation.
type SyncResponse struct {
	// Created is the number of new aliases created.
	Created int `json:"created" desc:"aliases created"`

	// Updated is the number of existing aliases updated.
	Updated int `json:"updated" desc:"aliases updated"`

	// Deleted is the number of aliases deleted.
	Deleted int `json:"deleted" desc:"aliases deleted"`

	// Errors lists any operations that failed, with details.
	Errors []string `json:"errors,omitempty" desc:"failed operations"`
}
