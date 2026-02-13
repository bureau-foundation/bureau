// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

const (
	privateKeyFile = "token-signing-key"
	publicKeyFile  = "token-signing-key.pub"
)

// GenerateKeypair creates a new Ed25519 keypair for token signing.
func GenerateKeypair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating Ed25519 keypair: %w", err)
	}
	return public, private, nil
}

// SaveKeypair writes an Ed25519 keypair to the state directory. The
// private key file has 0600 permissions; the public key file has 0644.
func SaveKeypair(stateDir string, public ed25519.PublicKey, private ed25519.PrivateKey) error {
	privatePath := filepath.Join(stateDir, privateKeyFile)
	if err := os.WriteFile(privatePath, private, 0600); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}

	publicPath := filepath.Join(stateDir, publicKeyFile)
	if err := os.WriteFile(publicPath, public, 0644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}

	return nil
}

// LoadKeypair loads an Ed25519 keypair from the state directory. Returns
// an error if either file is missing or has an unexpected size.
func LoadKeypair(stateDir string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	privatePath := filepath.Join(stateDir, privateKeyFile)
	privateBytes, err := os.ReadFile(privatePath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading private key: %w", err)
	}
	if len(privateBytes) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("private key has %d bytes, want %d", len(privateBytes), ed25519.PrivateKeySize)
	}

	publicPath := filepath.Join(stateDir, publicKeyFile)
	publicBytes, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading public key: %w", err)
	}
	if len(publicBytes) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("public key has %d bytes, want %d", len(publicBytes), ed25519.PublicKeySize)
	}

	return ed25519.PublicKey(publicBytes), ed25519.PrivateKey(privateBytes), nil
}

// LoadOrGenerateKeypair loads an existing keypair from stateDir, or
// generates and saves a new one if the files don't exist. Returns the
// keypair and whether it was newly generated.
func LoadOrGenerateKeypair(stateDir string) (ed25519.PublicKey, ed25519.PrivateKey, bool, error) {
	public, private, err := LoadKeypair(stateDir)
	if err == nil {
		return public, private, false, nil
	}

	// Check if the error is due to missing files (expected on first boot)
	// vs corruption or permissions (unexpected).
	privatePath := filepath.Join(stateDir, privateKeyFile)
	if _, statErr := os.Stat(privatePath); statErr == nil {
		// File exists but couldn't be loaded — corruption or bad size.
		return nil, nil, false, err
	}

	public, private, err = GenerateKeypair()
	if err != nil {
		return nil, nil, false, err
	}

	if err := SaveKeypair(stateDir, public, private); err != nil {
		return nil, nil, false, err
	}

	return public, private, true, nil
}
