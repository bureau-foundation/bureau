// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package nix provides typed access to Nix CLI binaries (nix, nix-store).
// It centralizes binary resolution for the Determinate Nix installation
// pattern (PATH first, then /nix/var/nix/profiles/default/bin/) and
// provides uniform error formatting across all nix invocations.
//
// Bureau uses two Nix binaries:
//   - nix: for flake operations (show, build) in the environment CLI
//   - nix-store: for store path realization (prefetching) in the daemon
//
// Both are resolved identically: check PATH (works inside nix develop
// and on NixOS), then fall back to the Determinate Nix profile directory.
package nix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// determinateProfileBin is where Determinate Nix installs its binaries.
// This location is outside PATH by default, so we check it explicitly
// after the PATH lookup fails.
const determinateProfileBin = "/nix/var/nix/profiles/default/bin"

// FindBinary resolves a Nix binary by name (e.g., "nix", "nix-store"),
// checking PATH first and then the standard Determinate Nix installation
// directory. Returns the absolute path to the binary.
func FindBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}

	determinatePath := filepath.Join(determinateProfileBin, name)
	if _, err := os.Stat(determinatePath); err == nil {
		return determinatePath, nil
	}

	return "", fmt.Errorf("%s not found on PATH or at %s — install Nix first (see script/setup-nix)",
		name, determinatePath)
}

// Run executes "nix <args>" and returns the stdout output. The nix
// binary is resolved via FindBinary on each call. Stderr is captured
// and included in error messages (nix writes diagnostic output to
// stderr).
func Run(args ...string) (string, error) {
	return run(context.Background(), "nix", args)
}

// RunContext is like Run but accepts a context for cancellation.
func RunContext(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "nix", args)
}

// RunStore executes "nix-store <args>" with a context and returns the
// stdout output. Used by the daemon for store path realization
// (prefetching environments from binary caches).
func RunStore(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "nix-store", args)
}

// run resolves the named binary, executes it with the given arguments,
// and returns stdout. Stderr is captured separately and included in
// error messages.
func run(ctx context.Context, binaryName string, args []string) (string, error) {
	binaryPath, err := FindBinary(binaryName)
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, binaryPath, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return "", formatError(binaryName, args, &stderr, err)
	}
	return stdout.String(), nil
}

// nixStorePrefix is the standard Nix store root directory.
const nixStorePrefix = "/nix/store/"

// StoreDirectory extracts the Nix store directory from a path within it.
// A Nix store directory is the first path component after /nix/store/:
//
//	"/nix/store/abc-bureau-daemon/bin/bureau-daemon" → "/nix/store/abc-bureau-daemon"
//	"/nix/store/abc-bureau-daemon"                   → "/nix/store/abc-bureau-daemon"
//
// Returns an error for paths not under /nix/store/ or paths that are
// exactly /nix/store/ with no entry name.
func StoreDirectory(path string) (string, error) {
	if !strings.HasPrefix(path, nixStorePrefix) {
		return "", fmt.Errorf("path %q is not under /nix/store/", path)
	}

	// Everything after "/nix/store/" is the store entry name, potentially
	// followed by subdirectory components. The store directory is just the
	// first component.
	remainder := path[len(nixStorePrefix):]
	if remainder == "" {
		return "", fmt.Errorf("path %q has no store entry name", path)
	}

	// Find the first slash after the store entry name. If there is no
	// slash, the path IS the store directory.
	slashIndex := strings.IndexByte(remainder, '/')
	if slashIndex == -1 {
		return path, nil
	}

	return path[:len(nixStorePrefix)+slashIndex], nil
}

// formatError produces an error message for a failed nix command,
// preferring stderr output (which contains the actual nix error) over
// the generic exec error.
func formatError(binaryName string, args []string, stderr *bytes.Buffer, err error) error {
	commandString := binaryName + " " + strings.Join(args, " ")
	stderrText := strings.TrimSpace(stderr.String())
	if stderrText != "" {
		return fmt.Errorf("%s: %s", commandString, stderrText)
	}
	return fmt.Errorf("%s: %w", commandString, err)
}
