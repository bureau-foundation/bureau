// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Environment != Development {
		t.Errorf("expected environment=development, got %s", cfg.Environment)
	}

	if cfg.Sandbox.DefaultProfile != "developer" {
		t.Errorf("expected default_profile=developer, got %s", cfg.Sandbox.DefaultProfile)
	}

	if cfg.Proxy.SocketPath != "/run/bureau/proxy.sock" {
		t.Errorf("expected socket_path=/run/bureau/proxy.sock, got %s", cfg.Proxy.SocketPath)
	}

	if !cfg.Proxy.AutoStart {
		t.Error("expected auto_start=true for development")
	}
}

func TestLoad_RequiresBureauConfig(t *testing.T) {
	// Save and restore BUREAU_CONFIG.
	origConfig := os.Getenv("BUREAU_CONFIG")
	defer os.Setenv("BUREAU_CONFIG", origConfig)

	// Unset BUREAU_CONFIG - Load() should fail.
	os.Unsetenv("BUREAU_CONFIG")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when BUREAU_CONFIG not set, got nil")
	}

	expectedMsg := "BUREAU_CONFIG environment variable not set"
	if err.Error()[:len(expectedMsg)] != expectedMsg {
		t.Errorf("expected error message to start with %q, got %q", expectedMsg, err.Error())
	}
}

func TestLoad_WithBureauConfig(t *testing.T) {
	// Save and restore BUREAU_CONFIG.
	origConfig := os.Getenv("BUREAU_CONFIG")
	defer os.Setenv("BUREAU_CONFIG", origConfig)

	// Create temp config file.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bureau.yaml")

	configContent := `
environment: staging
paths:
  root: /test/root
proxy:
  socket_path: /test/proxy.sock
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Set BUREAU_CONFIG and load.
	os.Setenv("BUREAU_CONFIG", configPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Environment != Staging {
		t.Errorf("expected environment=staging, got %s", cfg.Environment)
	}

	if cfg.Paths.Root != "/test/root" {
		t.Errorf("expected root=/test/root, got %s", cfg.Paths.Root)
	}
}

func TestLoadFile(t *testing.T) {
	// Create temp config file.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bureau.yaml")

	configContent := `
environment: staging

paths:
  root: /custom/root

proxy:
  socket_path: /custom/socket.sock
  auto_start: false

sandbox:
  default_profile: readonly
  fallback:
    no_userns: warn

launcher:
  default_shell: /bin/zsh
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFile(configPath)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}

	if cfg.Environment != Staging {
		t.Errorf("expected environment=staging, got %s", cfg.Environment)
	}

	if cfg.Paths.Root != "/custom/root" {
		t.Errorf("expected root=/custom/root, got %s", cfg.Paths.Root)
	}

	if cfg.Proxy.SocketPath != "/custom/socket.sock" {
		t.Errorf("expected socket_path=/custom/socket.sock, got %s", cfg.Proxy.SocketPath)
	}

	if cfg.Proxy.AutoStart {
		t.Error("expected auto_start=false")
	}

	if cfg.Sandbox.DefaultProfile != "readonly" {
		t.Errorf("expected default_profile=readonly, got %s", cfg.Sandbox.DefaultProfile)
	}

	if cfg.Sandbox.Fallback.NoUserns != "warn" {
		t.Errorf("expected no_userns=warn, got %s", cfg.Sandbox.Fallback.NoUserns)
	}

	if cfg.Launcher.DefaultShell != "/bin/zsh" {
		t.Errorf("expected default_shell=/bin/zsh, got %s", cfg.Launcher.DefaultShell)
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bureau.yaml")

	configContent := `
environment: production

paths:
  root: /default/root

proxy:
  auto_start: true

sandbox:
  default_profile: developer
  fallback:
    no_userns: skip

production:
  paths:
    root: /prod/root
  proxy:
    auto_start: false
  sandbox:
    default_profile: assistant
    fallback:
      no_userns: error
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFile(configPath)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}

	// Production overrides should be applied.
	if cfg.Paths.Root != "/prod/root" {
		t.Errorf("expected root=/prod/root, got %s", cfg.Paths.Root)
	}

	if cfg.Proxy.AutoStart {
		t.Error("expected auto_start=false from production override")
	}

	if cfg.Sandbox.DefaultProfile != "assistant" {
		t.Errorf("expected default_profile=assistant, got %s", cfg.Sandbox.DefaultProfile)
	}

	if cfg.Sandbox.Fallback.NoUserns != "error" {
		t.Errorf("expected no_userns=error, got %s", cfg.Sandbox.Fallback.NoUserns)
	}
}

func TestEnvVarsDoNotOverride(t *testing.T) {
	// Verify that environment variables do NOT override config file values.
	// The config file is the single source of truth for deterministic configuration.

	// Save and restore env vars.
	origRoot := os.Getenv("BUREAU_ROOT")
	origSocket := os.Getenv("BUREAU_PROXY_SOCKET")
	origEnv := os.Getenv("BUREAU_ENVIRONMENT")
	defer func() {
		os.Setenv("BUREAU_ROOT", origRoot)
		os.Setenv("BUREAU_PROXY_SOCKET", origSocket)
		os.Setenv("BUREAU_ENVIRONMENT", origEnv)
	}()

	// Set env vars that should be ignored.
	os.Setenv("BUREAU_ROOT", "/env/root")
	os.Setenv("BUREAU_PROXY_SOCKET", "/env/proxy.sock")
	os.Setenv("BUREAU_ENVIRONMENT", "staging")

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bureau.yaml")

	configContent := `
environment: development
paths:
  root: /file/root
proxy:
  socket_path: /file/proxy.sock
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFile(configPath)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}

	// File values should be used, NOT env vars.
	if cfg.Environment != Development {
		t.Errorf("expected environment=development from file, got %s (env vars should not override)", cfg.Environment)
	}

	if cfg.Paths.Root != "/file/root" {
		t.Errorf("expected root=/file/root from file, got %s (env vars should not override)", cfg.Paths.Root)
	}

	if cfg.Proxy.SocketPath != "/file/proxy.sock" {
		t.Errorf("expected socket_path=/file/proxy.sock from file, got %s (env vars should not override)", cfg.Proxy.SocketPath)
	}
}

func TestExpandVars(t *testing.T) {
	tests := []struct {
		input    string
		vars     map[string]string
		expected string
	}{
		{
			input:    "${HOME}/bureau",
			vars:     map[string]string{"HOME": "/home/user"},
			expected: "/home/user/bureau",
		},
		{
			input:    "${MISSING:-default}",
			vars:     map[string]string{},
			expected: "default",
		},
		{
			input:    "${PRESENT:-default}",
			vars:     map[string]string{"PRESENT": "value"},
			expected: "value",
		},
		{
			input:    "${A}/${B}",
			vars:     map[string]string{"A": "first", "B": "second"},
			expected: "first/second",
		},
		{
			input:    "no variables here",
			vars:     map[string]string{},
			expected: "no variables here",
		},
	}

	for _, tt := range tests {
		result := expandVars(tt.input, tt.vars)
		if result != tt.expected {
			t.Errorf("expandVars(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default config",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name: "invalid environment",
			modify: func(c *Config) {
				c.Environment = "invalid"
			},
			wantErr: true,
		},
		{
			name: "empty root path",
			modify: func(c *Config) {
				c.Paths.Root = ""
			},
			wantErr: true,
		},
		{
			name: "empty socket path",
			modify: func(c *Config) {
				c.Proxy.SocketPath = ""
			},
			wantErr: true,
		},
		{
			name: "invalid fallback value",
			modify: func(c *Config) {
				c.Sandbox.Fallback.NoUserns = "invalid"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.modify(cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnsurePaths(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.Paths.Root = filepath.Join(tmpDir, "bureau")
	cfg.Paths.Worktrees = filepath.Join(cfg.Paths.Root, "worktrees")
	cfg.Paths.Archives = filepath.Join(cfg.Paths.Root, "archives")
	cfg.Paths.State = filepath.Join(cfg.Paths.Root, "state")

	if err := cfg.EnsurePaths(); err != nil {
		t.Fatalf("EnsurePaths failed: %v", err)
	}

	// Verify directories were created.
	for _, path := range []string{cfg.Paths.Root, cfg.Paths.Worktrees, cfg.Paths.Archives, cfg.Paths.State} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("path %s not created: %v", path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("path %s is not a directory", path)
		}
	}
}
