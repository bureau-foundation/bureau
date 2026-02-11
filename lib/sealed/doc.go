// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package sealed provides age encryption and decryption for Bureau
// credential bundles. It wraps filippo.io/age for the specific
// operations Bureau needs: generate x25519 keypairs, encrypt to
// multiple recipients, and decrypt with a private key.
//
// Ciphertext is base64-encoded for storage in Matrix state event JSON
// fields. Callers pass plaintext []byte to [Encrypt] and receive a
// base64 string; [Decrypt] accepts a base64 string and returns
// plaintext. Private keys and decrypted plaintext are returned as
// [secret.Buffer] values backed by mmap memory outside the Go heap
// (locked against swap, excluded from core dumps, zeroed on Close).
//
// Key exports:
//
//   - [GenerateKeypair] -- new age x25519 keypair in a secret.Buffer
//   - [Encrypt] / [EncryptJSON] -- encrypt to age public key recipients
//   - [Decrypt] / [DecryptJSON] -- decrypt with a secret.Buffer key
//   - [ParsePublicKey] / [ParsePrivateKey] -- key validation
//
// Used by the launcher (decrypt credential bundles) and
// bureau-credentials (encrypt bundles to machine public keys).
//
// Depends on lib/secret for secure memory allocation.
package sealed
