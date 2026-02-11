// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package nix

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFindBinary_NixOnPath(t *testing.T) {
	t.Parallel()

	// This test verifies that FindBinary resolves nix on this machine.
	// Skipped on machines without Nix installed.
	path, err := FindBinary("nix")
	if err != nil {
		t.Skipf("nix not available: %v", err)
	}
	if path == "" {
		t.Fatal("FindBinary(\"nix\") returned empty string with no error")
	}
	if !strings.Contains(path, "nix") {
		t.Errorf("FindBinary(\"nix\") = %q, expected path containing 'nix'", path)
	}
}

func TestFindBinary_NixStoreOnPath(t *testing.T) {
	t.Parallel()

	path, err := FindBinary("nix-store")
	if err != nil {
		t.Skipf("nix-store not available: %v", err)
	}
	if !strings.Contains(path, "nix-store") {
		t.Errorf("FindBinary(\"nix-store\") = %q, expected path containing 'nix-store'", path)
	}
}

func TestFindBinary_NonexistentBinary(t *testing.T) {
	t.Parallel()

	_, err := FindBinary("nix-definitely-does-not-exist-abcxyz")
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("error = %v, want error containing 'not found on PATH'", err)
	}
	if !strings.Contains(err.Error(), "script/setup-nix") {
		t.Errorf("error = %v, want error containing installation hint", err)
	}
}

func TestFormatError_PrefersStderr(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	stderr.WriteString("error: flake 'github:foo/bar' does not provide attribute\n")

	err := formatError("nix", []string{"build", "github:foo/bar#pkg"}, &stderr, nil)
	if err == nil {
		t.Fatal("expected non-nil error")
	}

	errorString := err.Error()
	if !strings.HasPrefix(errorString, "nix build github:foo/bar#pkg: ") {
		t.Errorf("error prefix = %q, want 'nix build github:foo/bar#pkg: '", errorString)
	}
	if !strings.Contains(errorString, "does not provide attribute") {
		t.Errorf("error = %q, want stderr content included", errorString)
	}
}

func TestFormatError_FallsBackToExecError(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	execError := context.DeadlineExceeded

	err := formatError("nix-store", []string{"--realise", "/nix/store/abc"}, &stderr, execError)
	if err == nil {
		t.Fatal("expected non-nil error")
	}

	errorString := err.Error()
	if !strings.Contains(errorString, "nix-store --realise /nix/store/abc") {
		t.Errorf("error = %q, want command in error", errorString)
	}
	if !strings.Contains(errorString, "deadline exceeded") {
		t.Errorf("error = %q, want exec error included", errorString)
	}
}
