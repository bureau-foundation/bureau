// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelregistry

import (
	"fmt"
	"math"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// AccountSelection is the result of selecting an account for a
// (project, provider) pair. Contains the account name, credential
// reference, and quota configuration needed to authenticate and
// enforce limits for a model request.
type AccountSelection struct {
	// AccountName is the state key of the selected account (e.g.,
	// "openrouter-alice"). Used for cost attribution and quota
	// tracking.
	AccountName string

	// Provider is the provider name this account authenticates to.
	Provider string

	// CredentialRef names the entry in the model service's sealed
	// credential bundle that holds the API key. Empty for providers
	// with AuthMethodNone (local inference).
	CredentialRef string

	// Quota defines spending limits for this account. Nil means
	// unlimited spending (no quota enforcement).
	Quota *model.Quota
}

// SetAccount adds or updates an account in the registry. Called by
// the sync loop when an m.bureau.model_account state event arrives.
// The name is the state key (account name).
func (r *Registry) SetAccount(name string, content model.ModelAccountContent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[name] = content
}

// RemoveAccount removes an account from the registry. Called when
// an m.bureau.model_account state event has empty content (deletion).
func (r *Registry) RemoveAccount(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accounts, name)
}

// SelectAccount finds the best matching account for a (project,
// provider) pair. The selection algorithm follows this precedence:
//
//  1. Explicit project match beats wildcard ("*").
//  2. Among matches of the same specificity, higher priority
//     (numerically larger) wins.
//  3. Alphabetically first account name breaks ties.
//
// Returns ErrNoMatchingAccount if no account covers the requested
// (project, provider) pair.
func (r *Registry) SelectAccount(project, provider string) (AccountSelection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var bestName string
	var bestAccount model.ModelAccountContent
	var bestIsExplicit bool
	bestPriority := math.MinInt

	for name, account := range r.accounts {
		if account.Provider != provider {
			continue
		}

		isExplicit := false
		matches := false
		for _, projectPattern := range account.Projects {
			if projectPattern == project {
				isExplicit = true
				matches = true
				break
			}
			if projectPattern == "*" {
				matches = true
			}
		}
		if !matches {
			continue
		}

		// Apply selection precedence: explicit > wildcard, then
		// higher priority, then alphabetical.
		if bestName == "" ||
			(isExplicit && !bestIsExplicit) ||
			(isExplicit == bestIsExplicit && account.Priority > bestPriority) ||
			(isExplicit == bestIsExplicit && account.Priority == bestPriority && name < bestName) {
			bestName = name
			bestAccount = account
			bestIsExplicit = isExplicit
			bestPriority = account.Priority
		}
	}

	if bestName == "" {
		return AccountSelection{}, fmt.Errorf(
			"%w: project %q, provider %q", ErrNoMatchingAccount, project, provider)
	}

	return AccountSelection{
		AccountName:   bestName,
		Provider:      bestAccount.Provider,
		CredentialRef: bestAccount.CredentialRef,
		Quota:         bestAccount.Quota,
	}, nil
}

// Accounts returns a snapshot of all registered accounts. The
// returned map is a copy — callers can read it without holding
// the lock.
func (r *Registry) Accounts() map[string]model.ModelAccountContent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]model.ModelAccountContent, len(r.accounts))
	for name, content := range r.accounts {
		result[name] = content
	}
	return result
}

// AccountCount returns the number of registered accounts.
func (r *Registry) AccountCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.accounts)
}
