// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvCredentialSource_Get(t *testing.T) {
	// Set up test env vars
	os.Setenv("TEST_GITHUB_PAT", "secret-token")
	os.Setenv("BUREAU_TEST_API_KEY", "api-key-value")
	defer func() {
		os.Unsetenv("TEST_GITHUB_PAT")
		os.Unsetenv("BUREAU_TEST_API_KEY")
	}()

	tests := []struct {
		name   string
		source EnvCredentialSource
		key    string
		want   string
	}{
		{
			name:   "no prefix",
			source: EnvCredentialSource{},
			key:    "test-github-pat",
			want:   "secret-token",
		},
		{
			name:   "with prefix",
			source: EnvCredentialSource{Prefix: "BUREAU_"},
			key:    "test-api-key",
			want:   "api-key-value",
		},
		{
			name:   "not found",
			source: EnvCredentialSource{},
			key:    "nonexistent",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.Get(tt.key); got != tt.want {
				t.Errorf("EnvCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestSystemdCredentialSource_Get(t *testing.T) {
	// Create temp directory with credential files
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "github-pat"), []byte("gh-token\n"), 0600); err != nil {
		t.Fatalf("failed to write test credential: %v", err)
	}

	tests := []struct {
		name   string
		source SystemdCredentialSource
		key    string
		want   string
	}{
		{
			name:   "found credential",
			source: SystemdCredentialSource{Directory: tempDir},
			key:    "github-pat",
			want:   "gh-token",
		},
		{
			name:   "not found",
			source: SystemdCredentialSource{Directory: tempDir},
			key:    "nonexistent",
			want:   "",
		},
		{
			name:   "no directory",
			source: SystemdCredentialSource{},
			key:    "anything",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.Get(tt.key); got != tt.want {
				t.Errorf("SystemdCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMapCredentialSource_Get(t *testing.T) {
	source := &MapCredentialSource{
		Credentials: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"key1", "value1"},
		{"key2", "value2"},
		{"key3", ""},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := source.Get(tt.key); got != tt.want {
				t.Errorf("MapCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMapCredentialSource_NilMap(t *testing.T) {
	source := &MapCredentialSource{}
	if got := source.Get("anything"); got != "" {
		t.Errorf("MapCredentialSource.Get() with nil map = %q, want empty", got)
	}
}

func TestChainCredentialSource_Get(t *testing.T) {
	source := &ChainCredentialSource{
		Sources: []CredentialSource{
			&MapCredentialSource{Credentials: map[string]string{"key1": "from-first"}},
			&MapCredentialSource{Credentials: map[string]string{"key1": "from-second", "key2": "from-second"}},
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"key1", "from-first"},  // First source wins
		{"key2", "from-second"}, // Falls through to second
		{"key3", ""},            // Not found in any
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := source.Get(tt.key); got != tt.want {
				t.Errorf("ChainCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}
