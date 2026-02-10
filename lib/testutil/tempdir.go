// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for Bureau packages.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// SocketDir creates a temporary directory suitable for Unix domain sockets.
//
// Unix domain sockets have a 108-byte path limit (sun_path in sockaddr_un).
// Build systems like Bazel set TEST_TMPDIR to deeply nested paths that
// exceed this limit, making t.TempDir() unsuitable for socket files.
// This function creates a short-named directory directly in /tmp.
//
// The directory is automatically removed when the test completes.
// DataBinary resolves a pre-built binary from Bazel's data dependencies.
// The environment variable must contain an rlocationpath (set via
// $(rlocationpath ...) in the BUILD file), which is resolved against
// RUNFILES_DIR to produce an absolute path.
//
// Fails the test if the environment variable is empty or the binary
// does not exist at the resolved path.
func DataBinary(t *testing.T, envVariable string) string {
	t.Helper()

	rlocationPath := os.Getenv(envVariable)
	if rlocationPath == "" {
		t.Fatalf("%s not set (run tests via bazel test)", envVariable)
	}

	runfilesDirectory := os.Getenv("RUNFILES_DIR")
	if runfilesDirectory == "" {
		t.Fatalf("RUNFILES_DIR not set (run tests via bazel test)")
	}

	absolutePath := filepath.Join(runfilesDirectory, rlocationPath)
	if _, err := os.Stat(absolutePath); err != nil {
		t.Fatalf("binary from %s not found at %s: %v", envVariable, absolutePath, err)
	}

	return absolutePath
}

// SocketDir creates a temporary directory suitable for Unix domain sockets.
func SocketDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "bureau-test-*")
	if err != nil {
		t.Fatalf("creating socket directory: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(directory)
	})
	return directory
}
