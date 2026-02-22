// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestArtifactScopeRoundTrip(t *testing.T) {
	original := ArtifactScope{
		Version:          1,
		ServicePrincipal: "@service/artifact/main:bureau.local",
		TagGlobs:         []string{"iree/resnet50/**", "shared/datasets/*"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "service_principal", "@service/artifact/main:bureau.local")

	var decoded ArtifactScope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestArtifactScopeOmitsEmptyOptionals(t *testing.T) {
	scope := ArtifactScope{
		Version:          1,
		ServicePrincipal: "@service/artifact/main:bureau.local",
	}

	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["tag_globs"]; exists {
		t.Error("tag_globs should be omitted when empty, but is present")
	}
}

func TestArtifactScopeValidate(t *testing.T) {
	tests := []struct {
		name    string
		scope   ArtifactScope
		wantErr string
	}{
		{
			name: "valid",
			scope: ArtifactScope{
				Version:          1,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			},
			wantErr: "",
		},
		{
			name: "valid_with_globs",
			scope: ArtifactScope{
				Version:          1,
				ServicePrincipal: "@service/artifact/main:bureau.local",
				TagGlobs:         []string{"iree/**"},
			},
			wantErr: "",
		},
		{
			name: "version_zero",
			scope: ArtifactScope{
				Version:          0,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			},
			wantErr: "version must be >= 1",
		},
		{
			name: "service_principal_empty",
			scope: ArtifactScope{
				Version: 1,
			},
			wantErr: "service_principal is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.scope.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestArtifactScopeCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", ArtifactScopeVersion, false},
		{"older_version", ArtifactScopeVersion - 1, false},
		{"newer_version", ArtifactScopeVersion + 1, true},
		{"far_future_version", ArtifactScopeVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := ArtifactScope{
				Version:          test.version,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			}
			err := scope.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestArtifactScopeForwardCompatibility(t *testing.T) {
	// Simulate a v2 event with an unknown top-level field. This
	// documents the behavior that CanModify guards against: unknown
	// fields are silently dropped on unmarshal/re-marshal.
	v2JSON := `{
		"version": 2,
		"service_principal": "@service/artifact/main:bureau.local",
		"tag_globs": ["iree/**"],
		"new_v2_field": "this field does not exist in v1 ArtifactScope"
	}`

	// v1 code can unmarshal v2 events without error.
	var scope ArtifactScope
	if err := json.Unmarshal([]byte(v2JSON), &scope); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if scope.Version != 2 {
		t.Errorf("Version = %d, want 2", scope.Version)
	}
	if scope.ServicePrincipal != "@service/artifact/main:bureau.local" {
		t.Errorf("ServicePrincipal mismatch")
	}

	// CanModify rejects modification of v2 events.
	if err := scope.CanModify(); err == nil {
		t.Fatal("CanModify() = nil for v2 event, want error")
	}

	// Re-marshal drops the unknown field (this is what CanModify prevents).
	remarshaled, _ := json.Marshal(scope)
	var raw map[string]any
	json.Unmarshal(remarshaled, &raw)
	if _, exists := raw["new_v2_field"]; exists {
		t.Error("new_v2_field survived round-trip through v1 struct (unexpected)")
	}
}

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
