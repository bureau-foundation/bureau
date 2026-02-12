// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCredentialFile(t *testing.T) {
	content := "# Comment\n" +
		"KEY1=value1\n" +
		"KEY2=value2\n" +
		"\n" +
		"KEY3=value3\n"

	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	credentials, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}

	if credentials["KEY1"] != "value1" {
		t.Errorf("KEY1: expected value1, got %q", credentials["KEY1"])
	}
	if credentials["KEY2"] != "value2" {
		t.Errorf("KEY2: expected value2, got %q", credentials["KEY2"])
	}
	if credentials["KEY3"] != "value3" {
		t.Errorf("KEY3: expected value3, got %q", credentials["KEY3"])
	}
}

func TestUpdateCredentialFile_UpdateExistingKeys(t *testing.T) {
	content := "# Bureau Matrix credentials\n" +
		"MATRIX_HOMESERVER_URL=http://localhost:6167\n" +
		"MATRIX_ADMIN_TOKEN=old-token\n" +
		"MATRIX_SPACE_ROOM=!old-space:local\n" +
		"MATRIX_SYSTEM_ROOM=!old-system:local\n"

	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	updates := map[string]string{
		"MATRIX_SPACE_ROOM":  "!new-space:local",
		"MATRIX_SYSTEM_ROOM": "!new-system:local",
	}
	if err := UpdateCredentialFile(path, updates); err != nil {
		t.Fatalf("UpdateCredentialFile: %v", err)
	}

	credentials, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}

	// Updated keys should have new values.
	if credentials["MATRIX_SPACE_ROOM"] != "!new-space:local" {
		t.Errorf("MATRIX_SPACE_ROOM: expected !new-space:local, got %q", credentials["MATRIX_SPACE_ROOM"])
	}
	if credentials["MATRIX_SYSTEM_ROOM"] != "!new-system:local" {
		t.Errorf("MATRIX_SYSTEM_ROOM: expected !new-system:local, got %q", credentials["MATRIX_SYSTEM_ROOM"])
	}

	// Unchanged keys should be preserved.
	if credentials["MATRIX_HOMESERVER_URL"] != "http://localhost:6167" {
		t.Errorf("MATRIX_HOMESERVER_URL should be preserved, got %q", credentials["MATRIX_HOMESERVER_URL"])
	}
	if credentials["MATRIX_ADMIN_TOKEN"] != "old-token" {
		t.Errorf("MATRIX_ADMIN_TOKEN should be preserved, got %q", credentials["MATRIX_ADMIN_TOKEN"])
	}
}

func TestUpdateCredentialFile_AddNewKeys(t *testing.T) {
	content := "# Bureau Matrix credentials\n" +
		"MATRIX_SPACE_ROOM=!space:local\n" +
		"MATRIX_SYSTEM_ROOM=!system:local\n"

	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	updates := map[string]string{
		"MATRIX_MACHINE_ROOM":  "!machine:local",
		"MATRIX_SERVICE_ROOM":  "!service:local",
		"MATRIX_TEMPLATE_ROOM": "!template:local",
		"MATRIX_PIPELINE_ROOM": "!pipeline:local",
	}
	if err := UpdateCredentialFile(path, updates); err != nil {
		t.Fatalf("UpdateCredentialFile: %v", err)
	}

	credentials, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}

	// Existing keys preserved.
	if credentials["MATRIX_SPACE_ROOM"] != "!space:local" {
		t.Errorf("MATRIX_SPACE_ROOM should be preserved, got %q", credentials["MATRIX_SPACE_ROOM"])
	}

	// New keys added.
	for key, expectedValue := range updates {
		if credentials[key] != expectedValue {
			t.Errorf("%s: expected %q, got %q", key, expectedValue, credentials[key])
		}
	}
}

func TestUpdateCredentialFile_PreservesComments(t *testing.T) {
	content := "# Bureau Matrix credentials\n" +
		"# Written by bureau matrix setup.\n" +
		"#\n" +
		"KEY=old-value\n"

	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := UpdateCredentialFile(path, map[string]string{"KEY": "new-value"}); err != nil {
		t.Fatalf("UpdateCredentialFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "# Bureau Matrix credentials") {
		t.Error("first comment line lost")
	}
	if !strings.Contains(result, "# Written by bureau matrix setup.") {
		t.Error("second comment line lost")
	}
	if !strings.Contains(result, "KEY=new-value") {
		t.Errorf("expected KEY=new-value, got:\n%s", result)
	}
}
