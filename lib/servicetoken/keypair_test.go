// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateKeypair(t *testing.T) {
	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if len(public) != ed25519.PublicKeySize {
		t.Errorf("public key size = %d, want %d", len(public), ed25519.PublicKeySize)
	}
	if len(private) != ed25519.PrivateKeySize {
		t.Errorf("private key size = %d, want %d", len(private), ed25519.PrivateKeySize)
	}

	// Verify the keypair is functional.
	message := []byte("test message")
	signature := ed25519.Sign(private, message)
	if !ed25519.Verify(public, message, signature) {
		t.Error("generated keypair failed sign/verify round-trip")
	}
}

func TestSaveAndLoadKeypair(t *testing.T) {
	stateDir := t.TempDir()

	public, private, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if err := SaveKeypair(stateDir, public, private); err != nil {
		t.Fatalf("SaveKeypair: %v", err)
	}

	// Verify file permissions.
	privatePath := filepath.Join(stateDir, privateKeyFile)
	info, err := os.Stat(privatePath)
	if err != nil {
		t.Fatalf("Stat private key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("private key permissions = %o, want 0600", mode)
	}

	// Load and verify.
	loadedPublic, loadedPrivate, err := LoadKeypair(stateDir)
	if err != nil {
		t.Fatalf("LoadKeypair: %v", err)
	}

	if !public.Equal(loadedPublic) {
		t.Error("loaded public key does not match saved")
	}
	if !private.Equal(loadedPrivate) {
		t.Error("loaded private key does not match saved")
	}
}

func TestLoadKeypair_MissingFiles(t *testing.T) {
	stateDir := t.TempDir()

	_, _, err := LoadKeypair(stateDir)
	if err == nil {
		t.Fatal("LoadKeypair should fail with missing files")
	}
}

func TestLoadKeypair_CorruptedKey(t *testing.T) {
	stateDir := t.TempDir()

	// Write a truncated private key.
	privatePath := filepath.Join(stateDir, privateKeyFile)
	if err := os.WriteFile(privatePath, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}

	publicPath := filepath.Join(stateDir, publicKeyFile)
	public := make([]byte, ed25519.PublicKeySize)
	if err := os.WriteFile(publicPath, public, 0644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadKeypair(stateDir)
	if err == nil {
		t.Fatal("LoadKeypair should fail with corrupted private key")
	}
}

func TestLoadOrGenerateKeypair_FirstBoot(t *testing.T) {
	stateDir := t.TempDir()

	public, private, generated, err := LoadOrGenerateKeypair(stateDir)
	if err != nil {
		t.Fatalf("LoadOrGenerateKeypair: %v", err)
	}
	if !generated {
		t.Error("expected generated=true on first boot")
	}
	if len(public) != ed25519.PublicKeySize {
		t.Errorf("public key size = %d", len(public))
	}
	if len(private) != ed25519.PrivateKeySize {
		t.Errorf("private key size = %d", len(private))
	}

	// Files should exist.
	if _, err := os.Stat(filepath.Join(stateDir, privateKeyFile)); err != nil {
		t.Errorf("private key file not created: %v", err)
	}
}

func TestLoadOrGenerateKeypair_SubsequentBoot(t *testing.T) {
	stateDir := t.TempDir()

	// First boot: generate.
	originalPublic, _, _, err := LoadOrGenerateKeypair(stateDir)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}

	// Second boot: load existing.
	loadedPublic, _, generated, err := LoadOrGenerateKeypair(stateDir)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if generated {
		t.Error("expected generated=false on subsequent boot")
	}
	if !originalPublic.Equal(loadedPublic) {
		t.Error("loaded key does not match original")
	}
}

func TestLoadOrGenerateKeypair_CorruptedReturnsError(t *testing.T) {
	stateDir := t.TempDir()

	// Write a corrupted private key file (exists but wrong size).
	privatePath := filepath.Join(stateDir, privateKeyFile)
	if err := os.WriteFile(privatePath, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := LoadOrGenerateKeypair(stateDir)
	if err == nil {
		t.Fatal("LoadOrGenerateKeypair should fail with corrupted key file")
	}
}
