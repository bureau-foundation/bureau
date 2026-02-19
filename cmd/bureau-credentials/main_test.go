// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
)

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		expected  string
	}{
		{
			name:      "fleet-scoped alias",
			fullAlias: "#bureau/fleet/prod/machine/workstation:bureau.local",
			expected:  "bureau/fleet/prod/machine/workstation",
		},
		{
			name:      "simple alias",
			fullAlias: "#bureau/machine:bureau.local",
			expected:  "bureau/machine",
		},
		{
			name:      "no prefix or suffix",
			fullAlias: "bureau/fleet/staging/service/stt",
			expected:  "bureau/fleet/staging/service/stt",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := principal.RoomAliasLocalpart(test.fullAlias)
			if result != test.expected {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q",
					test.fullAlias, result, test.expected)
			}
		})
	}
}

func TestLoadMatrixConfig(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "config")
		content := `MATRIX_HOMESERVER_URL=http://localhost:6167
MATRIX_ADMIN_TOKEN=syt_test_token_123
MATRIX_ADMIN_USER=@bureau-admin:bureau.local
`
		if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
			t.Fatalf("writing config: %v", err)
		}

		config, err := loadMatrixConfig(configPath, "bureau.local")
		if err != nil {
			t.Fatalf("loadMatrixConfig() error: %v", err)
		}

		if config.HomeserverURL != "http://localhost:6167" {
			t.Errorf("HomeserverURL = %q, want %q", config.HomeserverURL, "http://localhost:6167")
		}
		if config.AdminToken != "syt_test_token_123" {
			t.Errorf("AdminToken = %q, want %q", config.AdminToken, "syt_test_token_123")
		}
		if config.AdminUserID != "@bureau-admin:bureau.local" {
			t.Errorf("AdminUserID = %q, want %q", config.AdminUserID, "@bureau-admin:bureau.local")
		}
		if config.ServerName != "bureau.local" {
			t.Errorf("ServerName = %q, want %q", config.ServerName, "bureau.local")
		}
	})

	t.Run("missing homeserver url", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "config")
		content := `MATRIX_ADMIN_TOKEN=syt_test
MATRIX_ADMIN_USER=@admin:local
`
		os.WriteFile(configPath, []byte(content), 0600)

		_, err := loadMatrixConfig(configPath, "bureau.local")
		if err == nil {
			t.Fatal("expected error for missing homeserver URL")
		}
	})

	t.Run("missing admin token", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "config")
		content := `MATRIX_HOMESERVER_URL=http://localhost:6167
MATRIX_ADMIN_USER=@admin:local
`
		os.WriteFile(configPath, []byte(content), 0600)

		_, err := loadMatrixConfig(configPath, "bureau.local")
		if err == nil {
			t.Fatal("expected error for missing admin token")
		}
	})

	t.Run("missing admin user", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "config")
		content := `MATRIX_HOMESERVER_URL=http://localhost:6167
MATRIX_ADMIN_TOKEN=syt_test
`
		os.WriteFile(configPath, []byte(content), 0600)

		_, err := loadMatrixConfig(configPath, "bureau.local")
		if err == nil {
			t.Fatal("expected error for missing admin user")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		// FileCredentialSource silently returns empty values for missing files.
		_, err := loadMatrixConfig("/nonexistent/config", "bureau.local")
		if err == nil {
			t.Fatal("expected error for nonexistent config file")
		}
	})
}

func TestKeygen(t *testing.T) {
	// Capture stderr and stdout. Since keygen writes to os.Stderr and os.Stdout
	// directly, we'd need to redirect those. Instead, just verify the function
	// returns no error.
	err := runKeygen()
	if err != nil {
		t.Fatalf("runKeygen() error: %v", err)
	}
}
