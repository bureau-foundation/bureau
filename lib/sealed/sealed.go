// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package sealed provides age encryption and decryption for Bureau credential
// bundles. It wraps filippo.io/age to provide a simple interface for the
// specific operations Bureau needs: generate keypairs, encrypt plaintext to
// multiple recipients, decrypt ciphertext with a private key.
//
// Ciphertext is base64-encoded for storage in Matrix state event JSON fields.
// The base64 encoding is handled internally — callers pass plaintext []byte in
// and get base64 strings out (and vice versa for decryption).
//
// This package is used by:
//   - The launcher (decrypt credential bundles with the machine's private key)
//   - bureau-credentials (encrypt credential bundles to machine public keys + operator escrow)
package sealed

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// Keypair holds an age x25519 keypair. The private key is an AGE-SECRET-KEY-1
// Bech32 string; the public key is an age1 Bech32 string.
type Keypair struct {
	// PrivateKey is the secret key in AGE-SECRET-KEY-1... format.
	// Must never be logged, stored in plaintext on disk, or included in
	// CLI arguments. See CREDENTIALS.md for the sealed storage lifecycle.
	PrivateKey string

	// PublicKey is the corresponding public key in age1... format.
	// Safe to publish (e.g., in m.bureau.machine_key state events).
	PublicKey string
}

// GenerateKeypair generates a new age x25519 keypair. The private key should
// be sealed to the TPM or kernel keyring immediately after generation —
// see CREDENTIALS.md for the machine key lifecycle.
func GenerateKeypair() (*Keypair, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generating age keypair: %w", err)
	}
	return &Keypair{
		PrivateKey: identity.String(),
		PublicKey:  identity.Recipient().String(),
	}, nil
}

// Encrypt encrypts plaintext to one or more recipients specified by their age
// public key strings (age1... format). Returns the ciphertext as a standard
// base64-encoded string suitable for storage in Matrix state event JSON fields.
//
// At least one recipient is required. For Bureau credential bundles, recipients
// are typically the target machine's public key plus the operator's escrow key.
func Encrypt(plaintext []byte, recipientKeys []string) (string, error) {
	if len(recipientKeys) == 0 {
		return "", fmt.Errorf("at least one recipient is required")
	}

	recipients := make([]age.Recipient, 0, len(recipientKeys))
	for _, key := range recipientKeys {
		recipient, err := age.ParseX25519Recipient(key)
		if err != nil {
			return "", fmt.Errorf("parsing recipient key %q: %w", key, err)
		}
		recipients = append(recipients, recipient)
	}

	var ciphertextBuffer bytes.Buffer
	writer, err := age.Encrypt(&ciphertextBuffer, recipients...)
	if err != nil {
		return "", fmt.Errorf("creating age encryptor: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return "", fmt.Errorf("writing plaintext to age encryptor: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finalizing age encryption: %w", err)
	}

	return base64.StdEncoding.EncodeToString(ciphertextBuffer.Bytes()), nil
}

// Decrypt decrypts a base64-encoded ciphertext string using the given private
// key (AGE-SECRET-KEY-1... format). Returns the plaintext bytes.
//
// The private key is typically loaded from the kernel keyring by the launcher.
func Decrypt(ciphertext string, privateKey string) ([]byte, error) {
	identity, err := age.ParseX25519Identity(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	rawCiphertext, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decoding base64 ciphertext: %w", err)
	}

	reader, err := age.Decrypt(bytes.NewReader(rawCiphertext), identity)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}

	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading decrypted plaintext: %w", err)
	}

	return plaintext, nil
}

// ParsePublicKey validates and parses an age public key string. Returns an
// error if the key is not a valid age x25519 public key. This is useful for
// validating keys received from Matrix state events before using them.
func ParsePublicKey(publicKey string) error {
	_, err := age.ParseX25519Recipient(publicKey)
	if err != nil {
		return fmt.Errorf("invalid age public key: %w", err)
	}
	return nil
}

// ParsePrivateKey validates and parses an age private key string. Returns an
// error if the key is not a valid age x25519 private key.
func ParsePrivateKey(privateKey string) error {
	_, err := age.ParseX25519Identity(privateKey)
	if err != nil {
		return fmt.Errorf("invalid age private key: %w", err)
	}
	return nil
}

// EncryptJSON is a convenience wrapper that encrypts a JSON credential bundle
// (as produced by json.Marshal) to multiple recipients. It is identical to
// Encrypt but makes the intended use case explicit in the call site.
func EncryptJSON(jsonPayload []byte, recipientKeys []string) (string, error) {
	return Encrypt(jsonPayload, recipientKeys)
}

// DecryptJSON is a convenience wrapper that decrypts a base64-encoded ciphertext
// and returns the plaintext JSON bytes. The caller is responsible for
// json.Unmarshaling the result. It is identical to Decrypt but makes the
// intended use case explicit in the call site.
func DecryptJSON(ciphertext string, privateKey string) ([]byte, error) {
	return Decrypt(ciphertext, privateKey)
}

// FormatRecipients formats a list of recipient public keys as a multi-line
// string suitable for display or logging (no private keys involved).
func FormatRecipients(recipientKeys []string) string {
	return strings.Join(recipientKeys, "\n")
}
