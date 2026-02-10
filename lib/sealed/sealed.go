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
// Private keys and decrypted plaintext are returned as *secret.Buffer values,
// which are backed by mmap memory outside the Go heap (locked against swap,
// excluded from core dumps, zeroed on close).
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

	"github.com/bureau-foundation/bureau/lib/secret"
)

// Keypair holds an age x25519 keypair. The private key is stored in a
// secret.Buffer (mmap-backed, locked against swap, excluded from core dumps).
// The public key is a plain string (safe to publish).
//
// The caller must call Close when the keypair is no longer needed.
type Keypair struct {
	// PrivateKey is the secret key in AGE-SECRET-KEY-1... format, stored
	// in mmap memory outside the Go heap. Must never be logged, stored in
	// plaintext on disk, or included in CLI arguments. See CREDENTIALS.md
	// for the sealed storage lifecycle.
	PrivateKey *secret.Buffer

	// PublicKey is the corresponding public key in age1... format.
	// Safe to publish (e.g., in m.bureau.machine_key state events).
	PublicKey string
}

// Close releases the private key memory (zeros, unlocks, unmaps).
// Idempotent — safe to call multiple times.
func (k *Keypair) Close() error {
	if k.PrivateKey != nil {
		return k.PrivateKey.Close()
	}
	return nil
}

// GenerateKeypair generates a new age x25519 keypair. The private key is
// returned in a secret.Buffer. The private key should be sealed to the TPM
// or kernel keyring immediately after generation — see CREDENTIALS.md for
// the machine key lifecycle.
//
// The caller must call Close on the returned Keypair when done.
func GenerateKeypair() (*Keypair, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generating age keypair: %w", err)
	}

	// Move the private key string into mmap-backed memory immediately.
	privateKeyString := identity.String()
	privateKeyBytes := []byte(privateKeyString)
	privateKey, err := secret.NewFromBytes(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("protecting private key: %w", err)
	}
	// privateKeyBytes is zeroed by NewFromBytes. privateKeyString is on the
	// heap and will be GC'd — unavoidable since age.GenerateX25519Identity
	// returns a struct with string methods. The mmap buffer is the durable
	// copy.

	return &Keypair{
		PrivateKey: privateKey,
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
// key. Returns the plaintext in a secret.Buffer (mmap-backed, zeroed on close).
//
// The private key is borrowed (read via .String() to parse the age identity)
// and is NOT closed by this function.
//
// The caller must call Close on the returned buffer when the plaintext is no
// longer needed.
func Decrypt(ciphertext string, privateKey *secret.Buffer) (*secret.Buffer, error) {
	// Convert the buffer to a string at the API boundary — age.ParseX25519Identity
	// requires a string. The heap copy is brief and request-scoped.
	identity, err := age.ParseX25519Identity(privateKey.String())
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

	// Move the decrypted plaintext into mmap-backed memory immediately.
	// NewFromBytes zeros the heap copy.
	if len(plaintext) == 0 {
		// age can produce empty plaintext (encrypted empty file).
		// Return a minimal buffer.
		buffer, err := secret.New(1)
		if err != nil {
			return nil, fmt.Errorf("protecting decrypted plaintext: %w", err)
		}
		return buffer, nil
	}

	buffer, err := secret.NewFromBytes(plaintext)
	if err != nil {
		// Zero the plaintext before returning the error.
		for index := range plaintext {
			plaintext[index] = 0
		}
		return nil, fmt.Errorf("protecting decrypted plaintext: %w", err)
	}
	return buffer, nil
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

// ParsePrivateKey validates and parses an age private key stored in a
// secret.Buffer. Returns an error if the key is not a valid age x25519
// private key.
func ParsePrivateKey(privateKey *secret.Buffer) error {
	_, err := age.ParseX25519Identity(privateKey.String())
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
// and returns the plaintext JSON in a secret.Buffer. The caller is responsible
// for using buffer.Bytes() with json.Unmarshal, then calling buffer.Close().
func DecryptJSON(ciphertext string, privateKey *secret.Buffer) (*secret.Buffer, error) {
	return Decrypt(ciphertext, privateKey)
}

// FormatRecipients formats a list of recipient public keys as a multi-line
// string suitable for display or logging (no private keys involved).
func FormatRecipients(recipientKeys []string) string {
	return strings.Join(recipientKeys, "\n")
}
