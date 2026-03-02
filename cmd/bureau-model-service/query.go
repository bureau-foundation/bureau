// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// handleList returns the list of registered model aliases with their
// resolved provider information. This is an AuthActionFunc: the
// socket server handles the response envelope.
func (ms *ModelService) handleList(_ context.Context, token *servicetoken.Token, _ []byte) (any, error) {
	if err := requireActionGrant(token, model.ActionList); err != nil {
		return nil, err
	}

	aliases := ms.registry.Aliases()
	providers := ms.registry.Providers()

	entries := make([]model.ModelListEntry, 0, len(aliases))
	for name, alias := range aliases {
		entry := model.ModelListEntry{
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

	return &model.ModelListResponse{
		Aliases:   entries,
		Providers: ms.registry.ProviderCount(),
		Accounts:  ms.registry.AccountCount(),
	}, nil
}

// handleStatus returns quota usage for all accounts. This is an
// AuthActionFunc: the socket server handles the response envelope.
func (ms *ModelService) handleStatus(_ context.Context, token *servicetoken.Token, _ []byte) (any, error) {
	if err := requireActionGrant(token, model.ActionStatus); err != nil {
		return nil, err
	}

	accounts := ms.registry.Accounts()

	entries := make([]model.AccountStatusEntry, 0, len(accounts))
	for name, account := range accounts {
		entries = append(entries, model.AccountStatusEntry{
			Name:                     name,
			Provider:                 account.Provider,
			Projects:                 account.Projects,
			Priority:                 account.Priority,
			Quota:                    account.Quota,
			DailySpendMicrodollars:   ms.quotaTracker.DailySpend(name),
			MonthlySpendMicrodollars: ms.quotaTracker.MonthlySpend(name),
		})
	}

	return &model.AccountStatusResponse{
		Accounts: entries,
	}, nil
}
