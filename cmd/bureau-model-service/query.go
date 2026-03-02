// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// modelEntry is a single alias entry in the list response.
type modelEntry struct {
	Alias         string           `json:"alias"`
	Provider      string           `json:"provider"`
	ProviderModel string           `json:"provider_model"`
	Capabilities  []string         `json:"capabilities,omitempty"`
	Pricing       model.Pricing    `json:"pricing"`
	Endpoint      string           `json:"endpoint,omitempty"`
	AuthMethod    model.AuthMethod `json:"auth_method,omitempty"`
}

// listResponse is returned by the model/list action.
type listResponse struct {
	Aliases   []modelEntry `json:"aliases"`
	Providers int          `json:"providers"`
	Accounts  int          `json:"accounts"`
}

// handleList returns the list of registered model aliases with their
// resolved provider information. This is an AuthActionFunc: the
// socket server handles the response envelope.
func (ms *ModelService) handleList(_ context.Context, _ *servicetoken.Token, _ []byte) (any, error) {
	aliases := ms.registry.Aliases()
	providers := ms.registry.Providers()

	entries := make([]modelEntry, 0, len(aliases))
	for name, alias := range aliases {
		entry := modelEntry{
			Alias:         name,
			Provider:      alias.Provider,
			ProviderModel: alias.ProviderModel,
			Capabilities:  alias.Capabilities,
			Pricing:       alias.Pricing,
		}

		// Enrich with provider details if available.
		if provider, ok := providers[alias.Provider]; ok {
			entry.Endpoint = provider.Endpoint
			entry.AuthMethod = provider.AuthMethod
		}

		entries = append(entries, entry)
	}

	return &listResponse{
		Aliases:   entries,
		Providers: ms.registry.ProviderCount(),
		Accounts:  ms.registry.AccountCount(),
	}, nil
}

// accountStatus describes the quota state of a single account.
type accountStatus struct {
	Name                     string       `json:"name"`
	Provider                 string       `json:"provider"`
	Projects                 []string     `json:"projects"`
	Priority                 int          `json:"priority"`
	Quota                    *model.Quota `json:"quota,omitempty"`
	DailySpendMicrodollars   int64        `json:"daily_spend_microdollars"`
	MonthlySpendMicrodollars int64        `json:"monthly_spend_microdollars"`
}

// statusResponse is returned by the model/status action.
type statusResponse struct {
	Accounts []accountStatus `json:"accounts"`
}

// handleStatus returns quota usage for all accounts. This is an
// AuthActionFunc: the socket server handles the response envelope.
func (ms *ModelService) handleStatus(_ context.Context, _ *servicetoken.Token, _ []byte) (any, error) {
	accounts := ms.registry.Accounts()

	entries := make([]accountStatus, 0, len(accounts))
	for name, account := range accounts {
		entries = append(entries, accountStatus{
			Name:                     name,
			Provider:                 account.Provider,
			Projects:                 account.Projects,
			Priority:                 account.Priority,
			Quota:                    account.Quota,
			DailySpendMicrodollars:   ms.quotaTracker.DailySpend(name),
			MonthlySpendMicrodollars: ms.quotaTracker.MonthlySpend(name),
		})
	}

	return &statusResponse{
		Accounts: entries,
	}, nil
}
