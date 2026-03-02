// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCredentialsEmptyDir(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", directory)

	store, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials failed: %v", err)
	}
	defer store.Close()

	if got := store.Get("nonexistent"); got != "" {
		t.Errorf("expected empty string for missing credential, got %q", got)
	}
}

func TestLoadCredentialsReadsFiles(t *testing.T) {
	directory := t.TempDir()

	// Write two credential files.
	if err := os.WriteFile(filepath.Join(directory, "openrouter-alice"), []byte("sk-alice-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "openrouter-shared"), []byte("sk-shared-key"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", directory)
	store, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials failed: %v", err)
	}
	defer store.Close()

	// Trailing newline should be stripped.
	if got := store.Get("openrouter-alice"); got != "sk-alice-key" {
		t.Errorf("expected 'sk-alice-key', got %q", got)
	}
	if got := store.Get("openrouter-shared"); got != "sk-shared-key" {
		t.Errorf("expected 'sk-shared-key', got %q", got)
	}
	if got := store.Get("nonexistent"); got != "" {
		t.Errorf("expected empty string for missing credential, got %q", got)
	}
}

func TestLoadCredentialsSkipsDirectories(t *testing.T) {
	directory := t.TempDir()

	// Create a subdirectory — should be skipped.
	if err := os.Mkdir(filepath.Join(directory, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "valid-key"), []byte("sk-test"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", directory)
	store, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials failed: %v", err)
	}
	defer store.Close()

	if got := store.Get("valid-key"); got != "sk-test" {
		t.Errorf("expected 'sk-test', got %q", got)
	}
}

func TestLoadCredentialsSkipsEmptyFiles(t *testing.T) {
	directory := t.TempDir()

	// Empty file (or whitespace-only) should be skipped.
	if err := os.WriteFile(filepath.Join(directory, "empty-key"), []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", directory)
	store, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials failed: %v", err)
	}
	defer store.Close()

	if got := store.Get("empty-key"); got != "" {
		t.Errorf("expected empty string for whitespace-only credential, got %q", got)
	}
}

func TestLoadCredentialsNoEnvVar(t *testing.T) {
	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", "")

	store, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials failed: %v", err)
	}
	defer store.Close()

	// Empty store, no error.
	if got := store.Get("anything"); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}
