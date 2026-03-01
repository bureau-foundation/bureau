// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"
)

// --- ForgeIdentity ---

func validForgeIdentity() ForgeIdentity {
	return ForgeIdentity{
		Version:    ForgeIdentityVersion,
		Provider:   "forgejo",
		MatrixUser: "@iree/amdgpu/pm:bureau.local",
		ForgeUser:  "iree-amdgpu-pm",
	}
}

func TestForgeIdentityValidate(t *testing.T) {
	t.Run("valid_per_principal", func(t *testing.T) {
		identity := validForgeIdentity()
		identity.ForgeUserID = 42
		identity.TokenProvisioned = true
		identity.SigningKeyRegistered = true
		identity.LastSynced = "2026-02-27T10:00:00Z"
		if err := identity.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("valid_shared_account", func(t *testing.T) {
		// GitHub: no forge user, no credentials — just the mapping.
		identity := ForgeIdentity{
			Version:    ForgeIdentityVersion,
			Provider:   "github",
			MatrixUser: "@iree/amdgpu/pm:bureau.local",
		}
		if err := identity.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*ForgeIdentity)
		wantErr string
	}{
		{
			name:    "zero_version",
			mutate:  func(fi *ForgeIdentity) { fi.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "empty_provider",
			mutate:  func(fi *ForgeIdentity) { fi.Provider = "" },
			wantErr: "provider is required",
		},
		{
			name:    "unknown_provider",
			mutate:  func(fi *ForgeIdentity) { fi.Provider = "bitbucket" },
			wantErr: "unknown provider",
		},
		{
			name:    "empty_matrix_user",
			mutate:  func(fi *ForgeIdentity) { fi.MatrixUser = "" },
			wantErr: "matrix_user is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := validForgeIdentity()
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

// --- ForgeAutoSubscribeRules ---

func TestForgeAutoSubscribeRulesValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		rules := DefaultAutoSubscribeRules()
		if err := rules.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("valid_all_disabled", func(t *testing.T) {
		rules := ForgeAutoSubscribeRules{
			Version: ForgeAutoSubscribeRulesVersion,
		}
		if err := rules.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})

	t.Run("zero_version", func(t *testing.T) {
		rules := ForgeAutoSubscribeRules{}
		err := rules.Validate()
		if err == nil {
			t.Fatal("Validate() = nil, want error")
		}
		if !strings.Contains(err.Error(), "version must be >= 1") {
			t.Errorf("Validate() = %q, want error containing %q", err, "version must be >= 1")
		}
	})
}

func TestDefaultAutoSubscribeRules(t *testing.T) {
	rules := DefaultAutoSubscribeRules()
	if rules.Version != ForgeAutoSubscribeRulesVersion {
		t.Errorf("Version = %d, want %d", rules.Version, ForgeAutoSubscribeRulesVersion)
	}
	if !rules.OnAuthor {
		t.Error("OnAuthor = false, want true")
	}
	if !rules.OnAssign {
		t.Error("OnAssign = false, want true")
	}
	if !rules.OnMention {
		t.Error("OnMention = false, want true")
	}
	if !rules.OnReviewRequest {
		t.Error("OnReviewRequest = false, want true")
	}
}
