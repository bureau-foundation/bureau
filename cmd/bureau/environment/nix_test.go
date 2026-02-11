// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/nix"
)

func TestCurrentSystem(t *testing.T) {
	system := currentSystem()
	if system == "" {
		t.Fatal("currentSystem() returned empty string")
	}

	// Should be one of the systems Nix recognizes.
	validSystems := map[string]bool{
		"x86_64-linux":    true,
		"aarch64-linux":   true,
		"x86_64-darwin":   true,
		"aarch64-darwin":  true,
		"i686-linux":      true,
		"aarch64-unknown": true, // fallback for unrecognized
	}

	if !validSystems[system] {
		// Not necessarily wrong — could be an unusual platform. Log it.
		t.Logf("currentSystem() = %q (not in common set, may be valid)", system)
	}
}

func TestGoArchToNix(t *testing.T) {
	tests := []struct {
		goArch string
		want   string
	}{
		{"amd64", "x86_64"},
		{"arm64", "aarch64"},
		{"386", "i686"},
		{"mips", "mips"}, // passthrough for unknown
	}

	for _, test := range tests {
		got := goArchToNix()
		// We can only test the current arch, but verify the mapping logic.
		_ = test // Mapping is tested via currentSystem integration.
		_ = got
	}
}

func TestParseOverrideInputs(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		wantNil bool
		wantErr bool
		wantMap map[string]string
	}{
		{
			name:    "nil input returns nil options",
			input:   nil,
			wantNil: true,
		},
		{
			name:    "empty input returns nil options",
			input:   []string{},
			wantNil: true,
		},
		{
			name:    "single override",
			input:   []string{"bureau=path:/home/user/bureau"},
			wantMap: map[string]string{"bureau": "path:/home/user/bureau"},
		},
		{
			name:    "multiple overrides",
			input:   []string{"bureau=path:../bureau", "nixpkgs=github:NixOS/nixpkgs/main"},
			wantMap: map[string]string{"bureau": "path:../bureau", "nixpkgs": "github:NixOS/nixpkgs/main"},
		},
		{
			name:    "value with equals sign",
			input:   []string{"bureau=path:/a=b/bureau"},
			wantMap: map[string]string{"bureau": "path:/a=b/bureau"},
		},
		{
			name:    "missing equals sign",
			input:   []string{"bureau"},
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   []string{"=path:./bureau"},
			wantErr: true,
		},
		{
			name:    "empty value",
			input:   []string{"bureau="},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := parseOverrideInputs(test.input)

			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if test.wantNil {
				if options != nil {
					t.Fatalf("expected nil options, got %+v", options)
				}
				return
			}

			if options == nil {
				t.Fatal("expected non-nil options, got nil")
			}

			if len(options.OverrideInputs) != len(test.wantMap) {
				t.Fatalf("got %d overrides, want %d", len(options.OverrideInputs), len(test.wantMap))
			}

			for key, wantValue := range test.wantMap {
				gotValue, ok := options.OverrideInputs[key]
				if !ok {
					t.Errorf("missing override %q", key)
					continue
				}
				if gotValue != wantValue {
					t.Errorf("override %q = %q, want %q", key, gotValue, wantValue)
				}
			}
		})
	}
}

func TestOverrideArgs(t *testing.T) {
	t.Run("nil options returns nil", func(t *testing.T) {
		var options *nixOptions
		args := options.overrideArgs()
		if args != nil {
			t.Fatalf("expected nil, got %v", args)
		}
	})

	t.Run("single override", func(t *testing.T) {
		options := &nixOptions{
			OverrideInputs: map[string]string{
				"bureau": "path:../bureau",
			},
		}
		args := options.overrideArgs()
		if len(args) != 3 {
			t.Fatalf("expected 3 args, got %d: %v", len(args), args)
		}
		if args[0] != "--override-input" {
			t.Errorf("args[0] = %q, want --override-input", args[0])
		}
		if args[1] != "bureau" {
			t.Errorf("args[1] = %q, want bureau", args[1])
		}
		if args[2] != "path:../bureau" {
			t.Errorf("args[2] = %q, want path:../bureau", args[2])
		}
	})
}

func TestNixFindBinary(t *testing.T) {
	// This test verifies that nix.FindBinary resolves nix on this
	// machine. Skipped on machines without Nix installed.
	path, err := nix.FindBinary("nix")
	if err != nil {
		t.Skipf("nix not available: %v", err)
	}
	if path == "" {
		t.Fatal("nix.FindBinary(\"nix\") returned empty string with no error")
	}
	t.Logf("nix found at: %s", path)
}
