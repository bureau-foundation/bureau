// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")

	original := &OperatorSession{
		UserID:      "@ben:bureau.local",
		AccessToken: "syt_test_token_12345",
		Homeserver:  "http://localhost:6167",
	}

	if err := SaveSessionTo(original, path); err != nil {
		t.Fatalf("SaveSessionTo: %v", err)
	}

	loaded, err := LoadSessionFrom(path)
	if err != nil {
		t.Fatalf("LoadSessionFrom: %v", err)
	}

	if loaded.UserID != original.UserID {
		t.Errorf("UserID = %q, want %q", loaded.UserID, original.UserID)
	}
	if loaded.AccessToken != original.AccessToken {
		t.Errorf("AccessToken = %q, want %q", loaded.AccessToken, original.AccessToken)
	}
	if loaded.Homeserver != original.Homeserver {
		t.Errorf("Homeserver = %q, want %q", loaded.Homeserver, original.Homeserver)
	}
}

func TestSessionFilePermissions(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "subdir", "session.json")

	session := &OperatorSession{
		UserID:      "@admin:bureau.local",
		AccessToken: "secret-token",
		Homeserver:  "http://localhost:6167",
	}

	if err := SaveSessionTo(session, path); err != nil {
		t.Fatalf("SaveSessionTo: %v", err)
	}

	// Verify the session file has mode 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("session file mode = %o, want 0600", mode)
	}

	// Verify the parent directory has mode 0700.
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat directory: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0700 {
		t.Errorf("directory mode = %o, want 0700", mode)
	}
}

func TestSessionFileFormat(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")

	session := &OperatorSession{
		UserID:      "@ben:bureau.local",
		AccessToken: "test-token",
		Homeserver:  "http://localhost:6167",
	}

	if err := SaveSessionTo(session, path); err != nil {
		t.Fatalf("SaveSessionTo: %v", err)
	}

	// Read raw bytes and verify it's valid indented JSON with a trailing newline.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("session file is empty")
	}
	if data[len(data)-1] != '\n' {
		t.Error("session file does not end with a newline")
	}

	// Verify it's valid JSON.
	var parsed map[string]string
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}
	if parsed["user_id"] != "@ben:bureau.local" {
		t.Errorf("user_id = %q, want @ben:bureau.local", parsed["user_id"])
	}
	if parsed["access_token"] != "test-token" {
		t.Errorf("access_token = %q, want test-token", parsed["access_token"])
	}
	if parsed["homeserver"] != "http://localhost:6167" {
		t.Errorf("homeserver = %q, want http://localhost:6167", parsed["homeserver"])
	}
}

func TestLoadSessionMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := LoadSessionFrom(path)
	if err == nil {
		t.Fatal("LoadSessionFrom should return an error for a missing file")
	}

	// Error should mention "bureau login".
	if got := err.Error(); !contains(got, "bureau login") {
		t.Errorf("error = %q, should mention 'bureau login'", got)
	}
}

func TestLoadSessionEmptyUserID(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")
	data := []byte(`{"user_id":"","access_token":"tok","homeserver":"http://localhost:6167"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSessionFrom(path)
	if err == nil {
		t.Fatal("LoadSessionFrom should reject empty user_id")
	}
}

func TestLoadSessionEmptyAccessToken(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")
	data := []byte(`{"user_id":"@a:b","access_token":"","homeserver":"http://localhost:6167"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSessionFrom(path)
	if err == nil {
		t.Fatal("LoadSessionFrom should reject empty access_token")
	}
}

func TestLoadSessionEmptyHomeserver(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")
	data := []byte(`{"user_id":"@a:b","access_token":"tok","homeserver":""}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSessionFrom(path)
	if err == nil {
		t.Fatal("LoadSessionFrom should reject empty homeserver")
	}
}

func TestLoadSessionMalformedJSON(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadSessionFrom(path)
	if err == nil {
		t.Fatal("LoadSessionFrom should reject malformed JSON")
	}
}

func TestSessionFilePathEnvOverride(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.
	customPath := filepath.Join(t.TempDir(), "custom-session.json")
	t.Setenv("BUREAU_SESSION_FILE", customPath)

	result := SessionFilePath()
	if result != customPath {
		t.Errorf("SessionFilePath() = %q, want %q", result, customPath)
	}
}

func TestSessionFilePathXDGConfigHome(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv modifies process environment.

	// Unset BUREAU_SESSION_FILE so it falls through to XDG.
	t.Setenv("BUREAU_SESSION_FILE", "")

	configDirectory := filepath.Join(t.TempDir(), "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", configDirectory)

	result := SessionFilePath()
	expected := filepath.Join(configDirectory, "bureau", "session.json")
	if result != expected {
		t.Errorf("SessionFilePath() = %q, want %q", result, expected)
	}
}

// contains checks if s contains substr. Avoids importing strings for a test
// helper that's used exactly once.
func contains(s, substr string) bool {
	for index := 0; index <= len(s)-len(substr); index++ {
		if s[index:index+len(substr)] == substr {
			return true
		}
	}
	return false
}
