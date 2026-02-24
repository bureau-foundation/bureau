// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package stewardship

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q is missing", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// intPointer returns a pointer to the given int.
func intPointer(value int) *int {
	return &value
}

// validStewardshipContent returns a StewardshipContent with all required
// fields set to valid values. Tests modify individual fields to test
// validation.
func validStewardshipContent() StewardshipContent {
	return StewardshipContent{
		Version:          StewardshipContentVersion,
		ResourcePatterns: []string{"fleet/gpu/**"},
		Tiers: []StewardshipTier{
			{
				Principals: []string{"iree/amdgpu/pm:bureau.local"},
				Escalation: "immediate",
			},
		},
	}
}

func TestStewardshipContentRoundTrip(t *testing.T) {
	original := StewardshipContent{
		Version:          StewardshipContentVersion,
		ResourcePatterns: []string{"fleet/gpu/**", "fleet/cpu/batch/**"},
		Description:      "GPU and CPU fleet capacity governance",
		GateTypes:        []ticket.TicketType{ticket.TypeResourceRequest, ticket.TypeDeployment},
		NotifyTypes:      []ticket.TicketType{ticket.TypeBug, ticket.TypeQuestion},
		Tiers: []StewardshipTier{
			{
				Principals: []string{"iree/amdgpu/pm:bureau.local", "other-project/pm:bureau.local"},
				Threshold:  intPointer(2),
				Escalation: "immediate",
			},
			{
				Principals: []string{"ben:bureau.local"},
				Threshold:  intPointer(1),
				Escalation: "last_pending",
			},
		},
		OverlapPolicy:  "independent",
		DigestInterval: "4h",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded StewardshipContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", decoded, original)
	}
}

func TestStewardshipContentOmitsEmptyOptionals(t *testing.T) {
	content := StewardshipContent{
		Version:          1,
		ResourcePatterns: []string{"fleet/**"},
		Tiers: []StewardshipTier{
			{
				Principals: []string{"ben:bureau.local"},
			},
		},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}

	for _, key := range []string{"description", "gate_types", "notify_types", "overlap_policy", "digest_interval"} {
		if _, exists := raw[key]; exists {
			t.Errorf("expected omitted field %q to be absent from JSON, but it is present", key)
		}
	}

	// Required fields must be present.
	for _, key := range []string{"version", "resource_patterns", "tiers"} {
		if _, exists := raw[key]; !exists {
			t.Errorf("expected required field %q to be present in JSON, but it is absent", key)
		}
	}
}

func TestStewardshipTierOmitsEmptyOptionals(t *testing.T) {
	tier := StewardshipTier{
		Principals: []string{"ben:bureau.local"},
	}

	data, err := json.Marshal(tier)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}

	for _, key := range []string{"threshold", "escalation"} {
		if _, exists := raw[key]; exists {
			t.Errorf("expected omitted field %q to be absent from JSON, but it is present", key)
		}
	}
}

func TestStewardshipContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*StewardshipContent)
		wantErr string
	}{
		{
			name:    "valid_minimal",
			modify:  func(_ *StewardshipContent) {},
			wantErr: "",
		},
		{
			name: "valid_full",
			modify: func(s *StewardshipContent) {
				s.Description = "GPU fleet governance"
				s.GateTypes = []ticket.TicketType{ticket.TypeResourceRequest, ticket.TypeDeployment}
				s.NotifyTypes = []ticket.TicketType{ticket.TypeBug}
				s.OverlapPolicy = "cooperative"
				s.DigestInterval = "4h"
				s.Tiers[0].Threshold = intPointer(1)
			},
			wantErr: "",
		},
		{
			name:    "version_zero",
			modify:  func(s *StewardshipContent) { s.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "empty_resource_patterns",
			modify:  func(s *StewardshipContent) { s.ResourcePatterns = nil },
			wantErr: "resource_patterns is required",
		},
		{
			name:    "empty_string_in_resource_patterns",
			modify:  func(s *StewardshipContent) { s.ResourcePatterns = []string{"fleet/**", ""} },
			wantErr: "resource_patterns[1]: pattern cannot be empty",
		},
		{
			name:    "empty_tiers",
			modify:  func(s *StewardshipContent) { s.Tiers = nil },
			wantErr: "tiers is required",
		},
		{
			name: "tier_empty_principals",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Principals = nil
			},
			wantErr: "principals is required",
		},
		{
			name: "tier_bare_localpart",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Principals = []string{"iree/amdgpu/pm"}
			},
			wantErr: "bare localpart",
		},
		{
			name: "tier_empty_principal_pattern",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Principals = []string{""}
			},
			wantErr: "pattern cannot be empty",
		},
		{
			name: "invalid_gate_type",
			modify: func(s *StewardshipContent) {
				s.GateTypes = []ticket.TicketType{ticket.TicketType("nonexistent_type")}
			},
			wantErr: "unknown ticket type",
		},
		{
			name: "invalid_notify_type",
			modify: func(s *StewardshipContent) {
				s.NotifyTypes = []ticket.TicketType{ticket.TicketType("nonexistent_type")}
			},
			wantErr: "unknown ticket type",
		},
		{
			name: "type_in_both_gate_and_notify",
			modify: func(s *StewardshipContent) {
				s.GateTypes = []ticket.TicketType{ticket.TypeReview}
				s.NotifyTypes = []ticket.TicketType{ticket.TypeReview}
			},
			wantErr: "appears in both gate_types and notify_types",
		},
		{
			name: "invalid_overlap_policy",
			modify: func(s *StewardshipContent) {
				s.OverlapPolicy = "merge"
			},
			wantErr: "unknown overlap_policy",
		},
		{
			name: "invalid_digest_interval",
			modify: func(s *StewardshipContent) {
				s.DigestInterval = "not-a-duration"
			},
			wantErr: "invalid digest_interval",
		},
		{
			name: "negative_digest_interval",
			modify: func(s *StewardshipContent) {
				s.DigestInterval = "-1h"
			},
			wantErr: "must be non-negative",
		},
		{
			name: "threshold_zero",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Threshold = intPointer(0)
			},
			wantErr: "threshold must be >= 1",
		},
		{
			name: "threshold_negative",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Threshold = intPointer(-1)
			},
			wantErr: "threshold must be >= 1",
		},
		{
			name: "invalid_escalation",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Escalation = "deferred"
			},
			wantErr: "unknown escalation",
		},
		{
			name: "valid_threshold_nil",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Threshold = nil
			},
			wantErr: "",
		},
		{
			name: "valid_threshold_positive",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Threshold = intPointer(2)
			},
			wantErr: "",
		},
		{
			name: "valid_escalation_empty",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Escalation = ""
			},
			wantErr: "",
		},
		{
			name: "valid_escalation_last_pending",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Escalation = "last_pending"
			},
			wantErr: "",
		},
		{
			name: "valid_principal_with_wildcard_server",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Principals = []string{"bureau/dev/*/tpm:bureau.local"}
			},
			wantErr: "",
		},
		{
			name: "valid_principal_with_double_star_server",
			modify: func(s *StewardshipContent) {
				s.Tiers[0].Principals = []string{"ben:**"}
			},
			wantErr: "",
		},
		{
			name: "valid_overlap_policy_cooperative",
			modify: func(s *StewardshipContent) {
				s.OverlapPolicy = "cooperative"
			},
			wantErr: "",
		},
		{
			name: "valid_overlap_policy_independent",
			modify: func(s *StewardshipContent) {
				s.OverlapPolicy = "independent"
			},
			wantErr: "",
		},
		{
			name: "valid_digest_interval_zero",
			modify: func(s *StewardshipContent) {
				s.DigestInterval = "0s"
			},
			wantErr: "",
		},
		{
			name: "valid_multiple_tiers",
			modify: func(s *StewardshipContent) {
				s.Tiers = []StewardshipTier{
					{
						Principals: []string{"iree/amdgpu/pm:bureau.local", "other-project/pm:bureau.local"},
						Threshold:  intPointer(2),
						Escalation: "immediate",
					},
					{
						Principals: []string{"ben:bureau.local"},
						Threshold:  intPointer(1),
						Escalation: "last_pending",
					},
				}
			},
			wantErr: "",
		},
		{
			name: "valid_gate_and_notify_disjoint",
			modify: func(s *StewardshipContent) {
				s.GateTypes = []ticket.TicketType{ticket.TypeReview, ticket.TypeDeployment}
				s.NotifyTypes = []ticket.TicketType{ticket.TypeBug, ticket.TypeQuestion}
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := validStewardshipContent()
			tt.modify(&content)
			err := content.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() = nil, want error containing %q", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, tt.wantErr)
				}
			}
		})
	}
}

func TestStewardshipContentCanModify(t *testing.T) {
	// Current version — safe to modify.
	current := StewardshipContent{Version: StewardshipContentVersion}
	if err := current.CanModify(); err != nil {
		t.Errorf("CanModify() for current version: %v", err)
	}

	// Older version — safe to modify (we can handle all its fields).
	// Version 1 is the only version, so this is the same as current.

	// Newer version — unsafe to modify.
	newer := StewardshipContent{Version: StewardshipContentVersion + 1}
	if err := newer.CanModify(); err == nil {
		t.Errorf("CanModify() for version %d: expected error, got nil", newer.Version)
	} else if !strings.Contains(err.Error(), "exceeds supported version") {
		t.Errorf("CanModify() error = %q, want error containing %q", err, "exceeds supported version")
	}
}

func TestStewardshipContentForwardCompatibility(t *testing.T) {
	// Simulate a newer version with an unknown field.
	newerJSON := `{
		"version": 2,
		"resource_patterns": ["fleet/**"],
		"tiers": [{"principals": ["ben:bureau.local"]}],
		"future_field": "some_value"
	}`

	var content StewardshipContent
	if err := json.Unmarshal([]byte(newerJSON), &content); err != nil {
		t.Fatalf("Unmarshal newer version: %v", err)
	}

	// Should unmarshal successfully — unknown fields are ignored.
	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if len(content.ResourcePatterns) != 1 || content.ResourcePatterns[0] != "fleet/**" {
		t.Errorf("ResourcePatterns = %v, want [fleet/**]", content.ResourcePatterns)
	}

	// CanModify should reject — we don't know about future_field.
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() for version 2: expected error, got nil")
	}

	// Validate still works for reads (version >= 1).
	if err := content.Validate(); err != nil {
		t.Errorf("Validate() for version 2: %v", err)
	}
}

func TestThresholdJSONRoundTrip(t *testing.T) {
	// Threshold nil — should be absent from JSON.
	tier := StewardshipTier{
		Principals: []string{"ben:bureau.local"},
	}
	data, err := json.Marshal(tier)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, exists := raw["threshold"]; exists {
		t.Error("nil Threshold should be absent from JSON")
	}

	// Threshold set — should round-trip.
	tier.Threshold = intPointer(2)
	data, err = json.Marshal(tier)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded StewardshipTier
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Threshold == nil || *decoded.Threshold != 2 {
		t.Errorf("Threshold round-trip: got %v, want 2", decoded.Threshold)
	}

	// Threshold absent in JSON — should decode as nil.
	noThresholdJSON := `{"principals": ["ben:bureau.local"]}`
	var noThreshold StewardshipTier
	if err := json.Unmarshal([]byte(noThresholdJSON), &noThreshold); err != nil {
		t.Fatalf("Unmarshal no threshold: %v", err)
	}
	if noThreshold.Threshold != nil {
		t.Errorf("absent threshold should decode as nil, got %v", *noThreshold.Threshold)
	}
}
