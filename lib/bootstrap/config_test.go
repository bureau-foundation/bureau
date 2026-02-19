// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func validConfig() *Config {
	return &Config{
		HomeserverURL: "http://matrix.internal:6167",
		ServerName:    "bureau.local",
		MachineName:   "bureau/fleet/prod/machine/worker-01",
		Password:      "random-one-time-password",
		FleetPrefix:   "bureau/fleet/prod",
	}
}

func TestValidate(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		config := validConfig()
		if err := config.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing homeserver URL", func(t *testing.T) {
		config := validConfig()
		config.HomeserverURL = ""
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for missing homeserver_url")
		}
	})

	t.Run("missing server name", func(t *testing.T) {
		config := validConfig()
		config.ServerName = ""
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for missing server_name")
		}
	})

	t.Run("missing machine name", func(t *testing.T) {
		config := validConfig()
		config.MachineName = ""
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for missing machine_name")
		}
	})

	t.Run("invalid machine name", func(t *testing.T) {
		config := validConfig()
		config.MachineName = "../evil"
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for invalid machine_name")
		}
	})

	t.Run("missing password", func(t *testing.T) {
		config := validConfig()
		config.Password = ""
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for missing password")
		}
	})

	t.Run("missing fleet prefix", func(t *testing.T) {
		config := validConfig()
		config.FleetPrefix = ""
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for missing fleet_prefix")
		}
	})

	t.Run("invalid fleet prefix", func(t *testing.T) {
		config := validConfig()
		config.FleetPrefix = "no-fleet-segment"
		if err := config.Validate(); err == nil {
			t.Fatal("expected error for invalid fleet_prefix")
		}
	})
}

func TestWriteAndReadConfig(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "bootstrap.json")

	original := validConfig()
	if err := WriteConfig(configPath, original); err != nil {
		t.Fatalf("WriteConfig failed: %v", err)
	}

	// Verify file permissions are 0600.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %o, want 0600", info.Mode().Perm())
	}

	// Verify the file contains valid JSON.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	var rawCheck map[string]any
	if err := json.Unmarshal(data, &rawCheck); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}

	// Read it back and compare.
	loaded, err := ReadConfig(configPath)
	if err != nil {
		t.Fatalf("ReadConfig failed: %v", err)
	}
	if loaded.HomeserverURL != original.HomeserverURL {
		t.Errorf("homeserver_url = %q, want %q", loaded.HomeserverURL, original.HomeserverURL)
	}
	if loaded.ServerName != original.ServerName {
		t.Errorf("server_name = %q, want %q", loaded.ServerName, original.ServerName)
	}
	if loaded.MachineName != original.MachineName {
		t.Errorf("machine_name = %q, want %q", loaded.MachineName, original.MachineName)
	}
	if loaded.Password != original.Password {
		t.Errorf("password = %q, want %q", loaded.Password, original.Password)
	}
	if loaded.FleetPrefix != original.FleetPrefix {
		t.Errorf("fleet_prefix = %q, want %q", loaded.FleetPrefix, original.FleetPrefix)
	}
}

func TestWriteConfig_InvalidConfig(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "bootstrap.json")

	config := &Config{} // all fields empty
	if err := WriteConfig(configPath, config); err == nil {
		t.Fatal("expected error for invalid config")
	}

	// File should not have been created.
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("file should not exist after failed write")
	}
}

func TestReadConfig_FileNotFound(t *testing.T) {
	_, err := ReadConfig("/nonexistent/path/bootstrap.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadConfig_InvalidJSON(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "bootstrap.json")
	if err := os.WriteFile(configPath, []byte("not json"), 0600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	_, err := ReadConfig(configPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadConfig_MissingRequiredField(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "bootstrap.json")

	// Valid JSON but missing the password field.
	data := `{"homeserver_url": "http://localhost:6167", "server_name": "bureau.local", "machine_name": "bureau/fleet/prod/machine/test", "fleet_prefix": "bureau/fleet/prod"}`
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	_, err := ReadConfig(configPath)
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
}
