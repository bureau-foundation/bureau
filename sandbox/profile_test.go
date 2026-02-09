// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
)

func TestProfileLoaderDefaults(t *testing.T) {
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	// Check that default profiles are loaded.
	profiles := loader.List()
	if len(profiles) == 0 {
		t.Fatal("no profiles loaded")
	}

	// Check for expected profiles.
	expectedProfiles := []string{"developer", "developer-gpu", "assistant", "readonly", "unrestricted"}
	for _, name := range expectedProfiles {
		found := false
		for _, p := range profiles {
			if p == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected profile %q not found", name)
		}
	}
}

func TestProfileLoaderResolve(t *testing.T) {
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	// Resolve developer profile.
	dev, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve(developer) failed: %v", err)
	}

	if dev.Name != "developer" {
		t.Errorf("expected name 'developer', got %q", dev.Name)
	}

	if !dev.Namespaces.PID {
		t.Error("expected PID namespace")
	}

	if !dev.Security.NewSession {
		t.Error("expected new_session")
	}

	// Resolve assistant profile (inherits from developer).
	assistant, err := loader.Resolve("assistant")
	if err != nil {
		t.Fatalf("Resolve(assistant) failed: %v", err)
	}

	if assistant.Name != "assistant" {
		t.Errorf("expected name 'assistant', got %q", assistant.Name)
	}

	// Should have inherited namespaces.
	if !assistant.Namespaces.PID {
		t.Error("assistant should inherit PID namespace")
	}

	// Should have its own resource limits.
	if assistant.Resources.MemoryMax != "2G" {
		t.Errorf("expected assistant memory_max=2G, got %q", assistant.Resources.MemoryMax)
	}
}

func TestProfileLoaderMultipleConfigs(t *testing.T) {
	loader := NewProfileLoader()

	// Load base config.
	baseYAML := `
profiles:
  base:
    description: "Base profile"
    namespaces:
      pid: true
`
	baseConfig, err := ParseProfilesConfig([]byte(baseYAML))
	if err != nil {
		t.Fatalf("ParseProfilesConfig failed: %v", err)
	}
	loader.configs = append(loader.configs, baseConfig)

	// Load override config (later configs win).
	overrideYAML := `
profiles:
  base:
    description: "Overridden base profile"
    namespaces:
      pid: false
      net: true
`
	overrideConfig, err := ParseProfilesConfig([]byte(overrideYAML))
	if err != nil {
		t.Fatalf("ParseProfilesConfig failed: %v", err)
	}
	loader.configs = append(loader.configs, overrideConfig)

	// Resolve should use the override.
	profile, err := loader.Resolve("base")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if profile.Description != "Overridden base profile" {
		t.Errorf("expected overridden description, got %q", profile.Description)
	}

	if profile.Namespaces.PID {
		t.Error("expected PID=false from override")
	}

	if !profile.Namespaces.Net {
		t.Error("expected Net=true from override")
	}
}

func TestProfileLoaderCache(t *testing.T) {
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	// Resolve twice should return same instance (cached).
	p1, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	p2, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if p1 != p2 {
		t.Error("expected cached profile to be same instance")
	}
}

func TestProfileLoaderNotFound(t *testing.T) {
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	_, err := loader.Resolve("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent profile")
	}
}

func TestDefaultVariables(t *testing.T) {
	vars := DefaultVariables()

	// BUREAU_ROOT should be set.
	if vars["BUREAU_ROOT"] == "" {
		t.Error("BUREAU_ROOT should be set")
	}

	// PROXY_SOCKET should default to /run/bureau/proxy.sock.
	if vars["PROXY_SOCKET"] != "/run/bureau/proxy.sock" {
		t.Errorf("expected PROXY_SOCKET=/run/bureau/proxy.sock, got %q", vars["PROXY_SOCKET"])
	}
}
