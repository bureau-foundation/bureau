// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package nix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"path/filepath"
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

func TestStoreDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "file path within store entry",
			path: "/nix/store/abc-bureau-daemon/bin/bureau-daemon",
			want: "/nix/store/abc-bureau-daemon",
		},
		{
			name: "bare store directory",
			path: "/nix/store/abc-bureau-daemon",
			want: "/nix/store/abc-bureau-daemon",
		},
		{
			name: "deeply nested file",
			path: "/nix/store/xyz-env/share/doc/readme.txt",
			want: "/nix/store/xyz-env",
		},
		{
			name:    "not under nix store",
			path:    "/usr/local/bin/bureau-daemon",
			wantErr: true,
		},
		{
			name:    "bare nix store root",
			path:    "/nix/store/",
			wantErr: true,
		},
		{
			name:    "nix store without trailing slash",
			path:    "/nix/store",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := StoreDirectory(testCase.path)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected error for path %q, got %q", testCase.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for path %q: %v", testCase.path, err)
			}
			if got != testCase.want {
				t.Errorf("StoreDirectory(%q) = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

func TestNARDigest(t *testing.T) {
	t.Parallel()

	// Find an arbitrary store path to test with. Skip if nix-store
	// is not available or the store is empty.
	_, err := FindBinary("nix-store")
	if err != nil {
		t.Skipf("nix-store not available: %v", err)
	}

	// Use nix-store --dump on the nix binary's own store directory to
	// compute a reference digest via shell pipeline, then compare with
	// our streaming implementation. Resolve symlinks first — the nix
	// binary path may be a profile symlink (e.g., /nix/var/nix/profiles/
	// default/bin/nix) that chains into /nix/store/.
	nixBinary, err := FindBinary("nix")
	if err != nil {
		t.Skipf("nix binary not available: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(nixBinary)
	if err != nil {
		t.Skipf("cannot resolve symlinks for %q: %v", nixBinary, err)
	}
	storeDirectory, err := StoreDirectory(resolvedPath)
	if err != nil {
		t.Skipf("cannot extract store directory from %q: %v", resolvedPath, err)
	}

	ctx := context.Background()

	// Compute digest with our streaming function.
	digest, err := NARDigest(ctx, storeDirectory)
	if err != nil {
		t.Fatalf("NARDigest(%q): %v", storeDirectory, err)
	}
	if len(digest) != 32 {
		t.Fatalf("digest length = %d, want 32 (SHA-256)", len(digest))
	}

	// Cross-check: compute the same digest via shell pipeline.
	// The reference implementation is: nix-store --dump <path> | sha256sum
	referenceOutput, err := RunStore(ctx, "--dump", storeDirectory)
	if err != nil {
		t.Fatalf("nix-store --dump (reference): %v", err)
	}

	// Hash the buffered reference output.
	referenceHash := sha256.Sum256([]byte(referenceOutput))
	referenceDigest := referenceHash[:]

	if !bytes.Equal(digest, referenceDigest) {
		t.Errorf("streaming digest = %x, reference digest = %x", digest, referenceDigest)
	}
}

func TestNARDigest_NonexistentPath(t *testing.T) {
	t.Parallel()

	_, err := FindBinary("nix-store")
	if err != nil {
		t.Skipf("nix-store not available: %v", err)
	}

	_, err = NARDigest(context.Background(), "/nix/store/definitely-does-not-exist-zzz")
	if err == nil {
		t.Fatal("expected error for nonexistent store path")
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
