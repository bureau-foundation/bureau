// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests in this file use t.Setenv (BUREAU_LAUNCHER_SESSION, BUREAU_MACHINE_CONF)
// which modifies process-global state, so none of them can be t.Parallel().

// isolateFromHostConfig points BUREAU_MACHINE_CONF at a nonexistent file so
// tests don't pick up the real /etc/bureau/machine.conf on machines where
// Bureau is deployed.
func isolateFromHostConfig(t *testing.T) {
	t.Helper()
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent.conf"))
}

func TestResolveLocalMachine_MachineConf(t *testing.T) {
	directory := t.TempDir()
	confPath := filepath.Join(directory, "machine.conf")

	confContent := "BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/workstation\n"
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_MACHINE_CONF", confPath)
	// Session file does not exist — machine.conf should be sufficient.
	t.Setenv("BUREAU_LAUNCHER_SESSION", filepath.Join(directory, "nonexistent.json"))

	localpart, err := ResolveLocalMachine()
	if err != nil {
		t.Fatalf("ResolveLocalMachine: %v", err)
	}
	if localpart != "bureau/fleet/prod/machine/workstation" {
		t.Errorf("localpart = %q, want %q", localpart, "bureau/fleet/prod/machine/workstation")
	}
}

func TestResolveLocalMachine_SessionFallback(t *testing.T) {
	isolateFromHostConfig(t)

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

func TestResolveLocalMachine_SessionMultiSegment(t *testing.T) {
	isolateFromHostConfig(t)

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

func TestResolveLocalMachine_MissingBothSources(t *testing.T) {
	isolateFromHostConfig(t)
	t.Setenv("BUREAU_LAUNCHER_SESSION", filepath.Join(t.TempDir(), "nonexistent.json"))

	_, err := ResolveLocalMachine()
	if err == nil {
		t.Fatal("ResolveLocalMachine should return an error when both sources are missing")
	}

	errorMessage := err.Error()
	if !contains(errorMessage, "no machine identity found") {
		t.Errorf("error = %q, should mention 'no machine identity found'", errorMessage)
	}
}

func TestResolveLocalMachine_EmptyUserID(t *testing.T) {
	isolateFromHostConfig(t)

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

func TestResolveLocalMachine_MalformedJSON(t *testing.T) {
	isolateFromHostConfig(t)

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

func TestResolveLocalMachine_MachineConfTakesPrecedence(t *testing.T) {
	// When both machine.conf and session file exist, machine.conf wins.
	directory := t.TempDir()

	confPath := filepath.Join(directory, "machine.conf")
	confContent := "BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/from-conf\n"
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sessionPath := filepath.Join(directory, "session.json")
	sessionJSON := `{"user_id": "@machine/from-session:bureau.local"}`
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BUREAU_MACHINE_CONF", confPath)
	t.Setenv("BUREAU_LAUNCHER_SESSION", sessionPath)

	localpart, err := ResolveLocalMachine()
	if err != nil {
		t.Fatalf("ResolveLocalMachine: %v", err)
	}
	if localpart != "bureau/fleet/prod/machine/from-conf" {
		t.Errorf("localpart = %q, want %q (machine.conf should take precedence)", localpart, "bureau/fleet/prod/machine/from-conf")
	}
}
