// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelregistry

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func aliceAccount() model.ModelAccountContent {
	return model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "openrouter-alice",
		Projects:      []string{"iree-amdgpu", "iree-compiler"},
		Priority:      0,
		Quota: &model.Quota{
			DailyMicrodollars:   50000000,
			MonthlyMicrodollars: 500000000,
		},
	}
}

func sharedAccount() model.ModelAccountContent {
	return model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "openrouter-shared",
		Projects:      []string{"*"},
		Priority:      -1,
		Quota: &model.Quota{
			DailyMicrodollars: 10000000,
		},
	}
}

func localAccount() model.ModelAccountContent {
	return model.ModelAccountContent{
		Provider: "llama-local",
		Projects: []string{"*"},
		Priority: 0,
		// No CredentialRef — local providers need no external credential.
		// No Quota — local inference is free.
	}
}

func registryWithAccounts() *Registry {
	registry := populatedRegistry()
	registry.SetAccount("alice", aliceAccount())
	registry.SetAccount("shared", sharedAccount())
	registry.SetAccount("local", localAccount())
	return registry
}

func TestSelectAccount_ExplicitProjectMatch(t *testing.T) {
	registry := registryWithAccounts()

	selection, err := registry.SelectAccount("iree-amdgpu", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "alice" {
		t.Errorf("AccountName = %q, want %q (explicit project match)", selection.AccountName, "alice")
	}
	if selection.CredentialRef != "openrouter-alice" {
		t.Errorf("CredentialRef = %q, want %q", selection.CredentialRef, "openrouter-alice")
	}
	if selection.Provider != "openrouter" {
		t.Errorf("Provider = %q, want %q", selection.Provider, "openrouter")
	}
	if selection.Quota == nil {
		t.Fatal("Quota is nil, want non-nil")
	}
	if selection.Quota.DailyMicrodollars != 50000000 {
		t.Errorf("Quota.Daily = %d, want 50000000", selection.Quota.DailyMicrodollars)
	}
}

func TestSelectAccount_ExplicitBeatsWildcard(t *testing.T) {
	registry := registryWithAccounts()

	// iree-amdgpu matches both "alice" (explicit) and "shared" (wildcard).
	// Alice should win regardless of priority ordering.
	selection, err := registry.SelectAccount("iree-amdgpu", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "alice" {
		t.Errorf("AccountName = %q, want %q (explicit beats wildcard)", selection.AccountName, "alice")
	}
}

func TestSelectAccount_WildcardFallback(t *testing.T) {
	registry := registryWithAccounts()

	// "some-other-project" has no explicit match, falls back to wildcard.
	selection, err := registry.SelectAccount("some-other-project", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "shared" {
		t.Errorf("AccountName = %q, want %q (wildcard fallback)", selection.AccountName, "shared")
	}
	if selection.CredentialRef != "openrouter-shared" {
		t.Errorf("CredentialRef = %q, want %q", selection.CredentialRef, "openrouter-shared")
	}
}

func TestSelectAccount_LocalProviderNoCredential(t *testing.T) {
	registry := registryWithAccounts()

	selection, err := registry.SelectAccount("any-project", "llama-local")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "local" {
		t.Errorf("AccountName = %q, want %q", selection.AccountName, "local")
	}
	if selection.CredentialRef != "" {
		t.Errorf("CredentialRef = %q, want empty (local provider)", selection.CredentialRef)
	}
	if selection.Quota != nil {
		t.Errorf("Quota should be nil for local account")
	}
}

func TestSelectAccount_NoMatchingProvider(t *testing.T) {
	registry := registryWithAccounts()

	_, err := registry.SelectAccount("iree-amdgpu", "nonexistent-provider")
	if !errors.Is(err, ErrNoMatchingAccount) {
		t.Errorf("got %v, want ErrNoMatchingAccount", err)
	}
}

func TestSelectAccount_NoMatchingProject(t *testing.T) {
	registry := New(slog.Default())
	// Account only covers specific projects, no wildcard.
	registry.SetAccount("narrow", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred",
		Projects:      []string{"project-a"},
		Priority:      0,
	})

	_, err := registry.SelectAccount("project-b", "openrouter")
	if !errors.Is(err, ErrNoMatchingAccount) {
		t.Errorf("got %v, want ErrNoMatchingAccount", err)
	}
}

func TestSelectAccount_HigherPriorityWins(t *testing.T) {
	registry := New(slog.Default())
	registry.SetAccount("low-priority", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-low",
		Projects:      []string{"*"},
		Priority:      0,
	})
	registry.SetAccount("high-priority", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-high",
		Projects:      []string{"*"},
		Priority:      10,
	})

	selection, err := registry.SelectAccount("any-project", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "high-priority" {
		t.Errorf("AccountName = %q, want %q (higher priority wins)", selection.AccountName, "high-priority")
	}
	if selection.CredentialRef != "cred-high" {
		t.Errorf("CredentialRef = %q, want %q", selection.CredentialRef, "cred-high")
	}
}

func TestSelectAccount_ExplicitBeatsHigherPriorityWildcard(t *testing.T) {
	registry := New(slog.Default())
	// High-priority wildcard account.
	registry.SetAccount("wildcard-high", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-wildcard",
		Projects:      []string{"*"},
		Priority:      100,
	})
	// Low-priority but explicit match.
	registry.SetAccount("explicit-low", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-explicit",
		Projects:      []string{"my-project"},
		Priority:      -50,
	})

	selection, err := registry.SelectAccount("my-project", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "explicit-low" {
		t.Errorf("AccountName = %q, want %q (explicit beats wildcard regardless of priority)",
			selection.AccountName, "explicit-low")
	}
}

func TestSelectAccount_DeterministicTieBreaking(t *testing.T) {
	registry := New(slog.Default())
	// Two accounts with identical specificity and priority.
	registry.SetAccount("beta-account", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-beta",
		Projects:      []string{"*"},
		Priority:      0,
	})
	registry.SetAccount("alpha-account", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-alpha",
		Projects:      []string{"*"},
		Priority:      0,
	})

	selection, err := registry.SelectAccount("any-project", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	// Alphabetically first: "alpha-account" < "beta-account".
	if selection.AccountName != "alpha-account" {
		t.Errorf("AccountName = %q, want %q (alphabetical tie-break)",
			selection.AccountName, "alpha-account")
	}
}

func TestSelectAccount_EmptyRegistry(t *testing.T) {
	registry := New(slog.Default())

	_, err := registry.SelectAccount("project", "provider")
	if !errors.Is(err, ErrNoMatchingAccount) {
		t.Errorf("got %v, want ErrNoMatchingAccount", err)
	}
}

func TestSetRemove_Account(t *testing.T) {
	registry := New(slog.Default())

	registry.SetAccount("test", aliceAccount())
	if registry.AccountCount() != 1 {
		t.Fatalf("AccountCount = %d, want 1", registry.AccountCount())
	}

	registry.RemoveAccount("test")
	if registry.AccountCount() != 0 {
		t.Fatalf("AccountCount = %d, want 0", registry.AccountCount())
	}
}

func TestUpdate_AccountOverwrite(t *testing.T) {
	registry := New(slog.Default())
	registry.SetAccount("alice", aliceAccount())

	// Update with different credential.
	updated := aliceAccount()
	updated.CredentialRef = "openrouter-alice-v2"
	registry.SetAccount("alice", updated)

	accounts := registry.Accounts()
	if accounts["alice"].CredentialRef != "openrouter-alice-v2" {
		t.Errorf("CredentialRef = %q, want updated value", accounts["alice"].CredentialRef)
	}
	if registry.AccountCount() != 1 {
		t.Errorf("AccountCount = %d, want 1 (overwrite, not duplicate)", registry.AccountCount())
	}
}

func TestAccountSnapshot_IsCopy(t *testing.T) {
	registry := registryWithAccounts()

	accounts := registry.Accounts()
	delete(accounts, "alice")

	if registry.AccountCount() != 3 {
		t.Errorf("AccountCount = %d, want 3 (snapshot mutation leaked)", registry.AccountCount())
	}
}

func TestSelectAccount_MultipleExplicitSameProject(t *testing.T) {
	registry := New(slog.Default())
	// Two explicit accounts for the same project and provider,
	// different priorities.
	registry.SetAccount("personal", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-personal",
		Projects:      []string{"my-project"},
		Priority:      10,
	})
	registry.SetAccount("team", model.ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "cred-team",
		Projects:      []string{"my-project", "other-project"},
		Priority:      0,
	})

	selection, err := registry.SelectAccount("my-project", "openrouter")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}

	if selection.AccountName != "personal" {
		t.Errorf("AccountName = %q, want %q (higher priority explicit match)",
			selection.AccountName, "personal")
	}
}

func TestRemoveAccount_NonexistentIsNoop(t *testing.T) {
	registry := New(slog.Default())

	// Should not panic.
	registry.RemoveAccount("nonexistent")

	if registry.AccountCount() != 0 {
		t.Errorf("AccountCount = %d after removing nonexistent", registry.AccountCount())
	}
}
