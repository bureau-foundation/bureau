// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package binhash provides SHA256 content hashing for binary files. Bureau
// uses binary content hashes to determine whether process binaries actually
// changed when Nix store paths change. An input-addressed Nix derivation
// produces a new store path whenever any input changes (source, dependencies,
// build flags), but the output binary is often byte-identical. Comparing
// SHA256 digests avoids unnecessary process restarts.
package binhash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HashFile computes the SHA256 digest of the file at path. The file is
// streamed through the hash function in chunks (via io.Copy) to keep
// memory usage constant regardless of file size.
func HashFile(path string) ([32]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, fmt.Errorf("opening %s for hashing: %w", path, err)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return [32]byte{}, fmt.Errorf("hashing %s: %w", path, err)
	}

	var digest [32]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

// FormatDigest returns the hex-encoded string representation of a
// SHA256 digest. This is the canonical format used in IPC messages,
// watchdog files, and log output.
func FormatDigest(digest [32]byte) string {
	return hex.EncodeToString(digest[:])
}

// ParseDigest parses a hex-encoded SHA256 digest string into a
// 32-byte array. Returns an error if the string is not a valid
// 64-character hex encoding of 32 bytes.
func ParseDigest(hexString string) ([32]byte, error) {
	var digest [32]byte
	decoded, err := hex.DecodeString(hexString)
	if err != nil {
		return digest, fmt.Errorf("parsing hash digest: %w", err)
	}
	if len(decoded) != 32 {
		return digest, fmt.Errorf("hash digest is %d bytes, want 32", len(decoded))
	}
	copy(digest[:], decoded)
	return digest, nil
}
