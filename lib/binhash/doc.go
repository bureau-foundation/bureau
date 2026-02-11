// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package binhash provides SHA256 content hashing for binary files.
//
// Bureau uses binary content hashes to determine whether process
// binaries actually changed when Nix store paths change. An
// input-addressed Nix derivation produces a new store path whenever any
// input changes (source code, dependencies, build flags), but the
// output binary is often byte-identical across those rebuilds.
// Comparing SHA256 digests of the actual binary files avoids
// unnecessary process restarts during daemon and launcher self-updates.
//
// The API surface is three functions:
//
//   - [HashFile] -- streams a file through SHA256, returning a [32]byte
//     digest with constant memory usage regardless of file size
//   - [FormatDigest] -- converts a [32]byte digest to its canonical
//     hex-encoded string representation, used in IPC messages, watchdog
//     state files, and log output
//   - [ParseDigest] -- parses a hex-encoded digest string back to a
//     [32]byte array, validating length and encoding
//
// This package has no dependencies on other Bureau packages.
package binhash
