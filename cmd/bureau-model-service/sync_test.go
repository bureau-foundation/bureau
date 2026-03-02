// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func TestSyncFilterIncludesModelEventTypes(t *testing.T) {
	filter := syncFilter

	for _, eventType := range []string{
		model.EventTypeModelProvider.String(),
		model.EventTypeModelAlias.String(),
		model.EventTypeModelAccount.String(),
	} {
		if !strings.Contains(filter, eventType) {
			t.Errorf("sync filter should include %s", eventType)
		}
	}
}

func TestSyncFilterIsValidJSON(t *testing.T) {
	// The filter is constructed at init time; if it were invalid JSON,
	// buildSyncFilter would panic. This test documents that expectation.
	if syncFilter == "" {
		t.Fatal("sync filter should not be empty")
	}
}

func TestRemarshalProvider(t *testing.T) {
	content := map[string]any{
		"endpoint":     "https://openrouter.ai/api/v1",
		"auth_method":  "bearer",
		"capabilities": []any{"chat", "streaming"},
	}

	var typed model.ModelProviderContent
	if err := remarshal(content, &typed); err != nil {
		t.Fatalf("remarshal failed: %v", err)
	}

	if typed.Endpoint != "https://openrouter.ai/api/v1" {
		t.Errorf("expected endpoint 'https://openrouter.ai/api/v1', got %q", typed.Endpoint)
	}
	if typed.AuthMethod != model.AuthMethodBearer {
		t.Errorf("expected auth_method 'bearer', got %q", typed.AuthMethod)
	}
	if len(typed.Capabilities) != 2 {
		t.Errorf("expected 2 capabilities, got %d", len(typed.Capabilities))
	}
}

func TestRemarshalAlias(t *testing.T) {
	content := map[string]any{
		"provider":       "openrouter",
		"provider_model": "openai/gpt-4.1",
		"pricing": map[string]any{
			"input_per_mtok_microdollars":  float64(2_500_000),
			"output_per_mtok_microdollars": float64(10_000_000),
		},
	}

	var typed model.ModelAliasContent
	if err := remarshal(content, &typed); err != nil {
		t.Fatalf("remarshal failed: %v", err)
	}

	if typed.Provider != "openrouter" {
		t.Errorf("expected provider 'openrouter', got %q", typed.Provider)
	}
	if typed.ProviderModel != "openai/gpt-4.1" {
		t.Errorf("expected provider_model 'openai/gpt-4.1', got %q", typed.ProviderModel)
	}
	if typed.Pricing.InputPerMtokMicrodollars != 2_500_000 {
		t.Errorf("expected input pricing 2500000, got %d", typed.Pricing.InputPerMtokMicrodollars)
	}
}

func TestRemarshalAccount(t *testing.T) {
	content := map[string]any{
		"provider":       "openrouter",
		"credential_ref": "openrouter-alice",
		"projects":       []any{"iree-amdgpu", "iree-compiler"},
		"priority":       float64(0),
		"quota": map[string]any{
			"daily_microdollars": float64(50_000_000),
		},
	}

	var typed model.ModelAccountContent
	if err := remarshal(content, &typed); err != nil {
		t.Fatalf("remarshal failed: %v", err)
	}

	if typed.Provider != "openrouter" {
		t.Errorf("expected provider 'openrouter', got %q", typed.Provider)
	}
	if typed.CredentialRef != "openrouter-alice" {
		t.Errorf("expected credential_ref 'openrouter-alice', got %q", typed.CredentialRef)
	}
	if len(typed.Projects) != 2 {
		t.Errorf("expected 2 projects, got %d", len(typed.Projects))
	}
	if typed.Quota == nil {
		t.Fatal("expected quota to be non-nil")
	}
	if typed.Quota.DailyMicrodollars != 50_000_000 {
		t.Errorf("expected daily quota 50000000, got %d", typed.Quota.DailyMicrodollars)
	}
}
