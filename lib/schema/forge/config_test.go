// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"
)

// --- Provider ---

func TestProviderIsKnown(t *testing.T) {
	known := []Provider{ProviderGitHub, ProviderForgejo, ProviderGitLab}
	for _, provider := range known {
		if !provider.IsKnown() {
			t.Errorf("Provider(%q).IsKnown() = false, want true", provider)
		}
	}

	unknown := []Provider{"bitbucket", "", "GITHUB"}
	for _, provider := range unknown {
		if provider.IsKnown() {
			t.Errorf("Provider(%q).IsKnown() = true, want false", provider)
		}
	}
}

// --- IssueSyncMode ---

func TestIssueSyncModeIsKnown(t *testing.T) {
	known := []IssueSyncMode{IssueSyncNone, IssueSyncImport, IssueSyncBidirectional}
	for _, mode := range known {
		if !mode.IsKnown() {
			t.Errorf("IssueSyncMode(%q).IsKnown() = false, want true", mode)
		}
	}

	unknown := []IssueSyncMode{"mirror", "", "IMPORT"}
	for _, mode := range unknown {
		if mode.IsKnown() {
			t.Errorf("IssueSyncMode(%q).IsKnown() = true, want false", mode)
		}
	}
}

// --- RepositoryBinding ---

func validRepositoryBinding() RepositoryBinding {
	return RepositoryBinding{
		Version:    RepositoryBindingVersion,
		Provider:   "github",
		Owner:      "bureau-foundation",
		Repo:       "bureau",
		URL:        "https://github.com/bureau-foundation/bureau",
		CloneHTTPS: "https://github.com/bureau-foundation/bureau.git",
		CloneSSH:   "git@github.com:bureau-foundation/bureau.git",
	}
}

func TestRepositoryBindingValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		binding := validRepositoryBinding()
		if err := binding.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("valid_without_clone_ssh", func(t *testing.T) {
		binding := validRepositoryBinding()
		binding.CloneSSH = ""
		if err := binding.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*RepositoryBinding)
		wantErr string
	}{
		{
			name:    "zero_version",
			mutate:  func(b *RepositoryBinding) { b.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "empty_provider",
			mutate:  func(b *RepositoryBinding) { b.Provider = "" },
			wantErr: "provider is required",
		},
		{
			name:    "unknown_provider",
			mutate:  func(b *RepositoryBinding) { b.Provider = "bitbucket" },
			wantErr: "unknown provider",
		},
		{
			name:    "empty_owner",
			mutate:  func(b *RepositoryBinding) { b.Owner = "" },
			wantErr: "owner is required",
		},
		{
			name:    "empty_repo",
			mutate:  func(b *RepositoryBinding) { b.Repo = "" },
			wantErr: "repo is required",
		},
		{
			name:    "empty_url",
			mutate:  func(b *RepositoryBinding) { b.URL = "" },
			wantErr: "url is required",
		},
		{
			name:    "empty_clone_https",
			mutate:  func(b *RepositoryBinding) { b.CloneHTTPS = "" },
			wantErr: "clone_https is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := validRepositoryBinding()
			tt.mutate(&binding)
			err := binding.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRepositoryBindingStateKey(t *testing.T) {
	binding := validRepositoryBinding()
	got := binding.StateKey()
	want := "github/bureau-foundation/bureau"
	if got != want {
		t.Errorf("StateKey() = %q, want %q", got, want)
	}
}

// --- ForgeConfig ---

func validForgeConfig() ForgeConfig {
	return ForgeConfig{
		Version:  ForgeConfigVersion,
		Provider: "github",
		Repo:     "bureau-foundation/bureau",
		Events:   []string{EventCategoryPush, EventCategoryPullRequest},
	}
}

func TestForgeConfigValidate(t *testing.T) {
	t.Run("valid_minimal", func(t *testing.T) {
		config := validForgeConfig()
		if err := config.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("valid_full", func(t *testing.T) {
		config := validForgeConfig()
		config.IssueSync = IssueSyncImport
		config.PRReviewTickets = true
		config.CIMonitor = true
		config.AutoSubscribe = true
		config.TriageFilter = &TriageFilter{
			Labels:     []string{"bug", "feature-request"},
			EventTypes: []string{"issue_opened", "pr_opened"},
		}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("empty_issue_sync_is_valid", func(t *testing.T) {
		config := validForgeConfig()
		config.IssueSync = ""
		if err := config.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil (empty IssueSync is allowed)", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*ForgeConfig)
		wantErr string
	}{
		{
			name:    "zero_version",
			mutate:  func(c *ForgeConfig) { c.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "empty_provider",
			mutate:  func(c *ForgeConfig) { c.Provider = "" },
			wantErr: "provider is required",
		},
		{
			name:    "unknown_provider",
			mutate:  func(c *ForgeConfig) { c.Provider = "bitbucket" },
			wantErr: "unknown provider",
		},
		{
			name:    "empty_repo",
			mutate:  func(c *ForgeConfig) { c.Repo = "" },
			wantErr: "repo is required",
		},
		{
			name:    "unknown_issue_sync",
			mutate:  func(c *ForgeConfig) { c.IssueSync = "mirror" },
			wantErr: "unknown issue_sync mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validForgeConfig()
			tt.mutate(&config)
			err := config.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// --- ForgeWorkIdentity ---

func validForgeWorkIdentity() ForgeWorkIdentity {
	return ForgeWorkIdentity{
		Version:     ForgeWorkIdentityVersion,
		DisplayName: "IREE Team",
		Email:       "iree-team@agents.bureau.foundation",
	}
}

func TestForgeWorkIdentityValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		identity := validForgeWorkIdentity()
		if err := identity.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*ForgeWorkIdentity)
		wantErr string
	}{
		{
			name:    "zero_version",
			mutate:  func(w *ForgeWorkIdentity) { w.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "empty_display_name",
			mutate:  func(w *ForgeWorkIdentity) { w.DisplayName = "" },
			wantErr: "display_name is required",
		},
		{
			name:    "empty_email",
			mutate:  func(w *ForgeWorkIdentity) { w.Email = "" },
			wantErr: "email is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := validForgeWorkIdentity()
			tt.mutate(&identity)
			err := identity.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
