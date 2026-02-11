// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests in this file use t.Setenv (BUREAU_LAUNCHER_SESSION) which modifies
// process-global state, so none of them can be t.Parallel().

func TestResolveLocalMachine(t *testing.T) {
	directory := t.TempDir()
	sessionPath := filepath.Join(directory, "session.json")

	sessionJSON := `{
  "homeserver_url": "http://localhost:6167",
  "user_id": "@machine/workstation:bureau.local",
  "access_token": "syt_secret_token"
}`
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_LAUNCHER_SESSION", sessionPath)

	localpart, err := ResolveLocalMachine()
	if err != nil {
		t.Fatalf("ResolveLocalMachine: %v", err)
	}
	if localpart != "machine/workstation" {
		t.Errorf("localpart = %q, want %q", localpart, "machine/workstation")
	}
}

func TestResolveLocalMachineMultiSegment(t *testing.T) {
	directory := t.TempDir()
	sessionPath := filepath.Join(directory, "session.json")

	sessionJSON := `{"user_id": "@machine/ec2/us-east-1/gpu-01:bureau.local"}`
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_LAUNCHER_SESSION", sessionPath)

	localpart, err := ResolveLocalMachine()
	if err != nil {
		t.Fatalf("ResolveLocalMachine: %v", err)
	}
	if localpart != "machine/ec2/us-east-1/gpu-01" {
		t.Errorf("localpart = %q, want %q", localpart, "machine/ec2/us-east-1/gpu-01")
	}
}

func TestResolveLocalMachineMissingFile(t *testing.T) {
	t.Setenv("BUREAU_LAUNCHER_SESSION", filepath.Join(t.TempDir(), "nonexistent.json"))

	_, err := ResolveLocalMachine()
	if err == nil {
		t.Fatal("ResolveLocalMachine should return an error for a missing file")
	}

	errorMessage := err.Error()
	if !contains(errorMessage, "no launcher session found") {
		t.Errorf("error = %q, should mention 'no launcher session found'", errorMessage)
	}
	if !contains(errorMessage, "BUREAU_LAUNCHER_SESSION") {
		t.Errorf("error = %q, should mention BUREAU_LAUNCHER_SESSION env var", errorMessage)
	}
}

func TestResolveLocalMachineEmptyUserID(t *testing.T) {
	directory := t.TempDir()
	sessionPath := filepath.Join(directory, "session.json")

	if err := os.WriteFile(sessionPath, []byte(`{"user_id": ""}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_LAUNCHER_SESSION", sessionPath)

	_, err := ResolveLocalMachine()
	if err == nil {
		t.Fatal("ResolveLocalMachine should reject empty user_id")
	}
}

func TestResolveLocalMachineMalformedJSON(t *testing.T) {
	directory := t.TempDir()
	sessionPath := filepath.Join(directory, "session.json")

	if err := os.WriteFile(sessionPath, []byte("not json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_LAUNCHER_SESSION", sessionPath)

	_, err := ResolveLocalMachine()
	if err == nil {
		t.Fatal("ResolveLocalMachine should reject malformed JSON")
	}
}
