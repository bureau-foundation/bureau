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
