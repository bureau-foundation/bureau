// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestCompareNil(t *testing.T) {
	diff, err := Compare(nil, "abc", "def", "/some/path")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if diff != nil {
		t.Error("expected nil diff for nil BureauVersion")
	}
}

func TestCompareAllIdentical(t *testing.T) {
	directory := t.TempDir()
	content := []byte("identical binary content")

	daemonPath := filepath.Join(directory, "bureau-daemon")
	if err := os.WriteFile(daemonPath, content, 0755); err != nil {
		t.Fatalf("WriteFile daemon: %v", err)
	}
	launcherPath := filepath.Join(directory, "bureau-launcher")
	if err := os.WriteFile(launcherPath, content, 0755); err != nil {
		t.Fatalf("WriteFile launcher: %v", err)
	}
	proxyDesiredPath := filepath.Join(directory, "bureau-proxy-desired")
	if err := os.WriteFile(proxyDesiredPath, content, 0755); err != nil {
		t.Fatalf("WriteFile proxy desired: %v", err)
	}
	proxyCurrentPath := filepath.Join(directory, "bureau-proxy-current")
	if err := os.WriteFile(proxyCurrentPath, content, 0755); err != nil {
		t.Fatalf("WriteFile proxy current: %v", err)
	}

	digest := sha256.Sum256(content)
	hexHash := binhash.FormatDigest(digest)

	desired := &schema.BureauVersion{
		DaemonStorePath:   daemonPath,
		LauncherStorePath: launcherPath,
		ProxyStorePath:    proxyDesiredPath,
	}

	diff, err := Compare(desired, hexHash, hexHash, proxyCurrentPath)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if diff.DaemonChanged {
		t.Error("daemon should not be changed (identical content)")
	}
	if diff.LauncherChanged {
		t.Error("launcher should not be changed (identical content)")
	}
	if diff.ProxyChanged {
		t.Error("proxy should not be changed (identical content)")
	}
	if diff.NeedsUpdate() {
		t.Error("NeedsUpdate should be false when all binaries are identical")
	}
}

func TestCompareDaemonChanged(t *testing.T) {
	directory := t.TempDir()

	desiredContent := []byte("new daemon binary v2")
	desiredPath := filepath.Join(directory, "bureau-daemon-new")
	if err := os.WriteFile(desiredPath, desiredContent, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Current daemon has different content.
	currentDigest := sha256.Sum256([]byte("old daemon binary v1"))
	currentHash := binhash.FormatDigest(currentDigest)

	desired := &schema.BureauVersion{
		DaemonStorePath: desiredPath,
	}

	diff, err := Compare(desired, currentHash, "", "")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !diff.DaemonChanged {
		t.Error("daemon should be changed (different content)")
	}
	if diff.LauncherChanged {
		t.Error("launcher should not be changed (empty store path)")
	}
	if diff.ProxyChanged {
		t.Error("proxy should not be changed (empty store path)")
	}
	if !diff.NeedsUpdate() {
		t.Error("NeedsUpdate should be true when daemon changed")
	}
}

func TestCompareLauncherChanged(t *testing.T) {
	directory := t.TempDir()

	desiredContent := []byte("new launcher binary")
	desiredPath := filepath.Join(directory, "bureau-launcher-new")
	if err := os.WriteFile(desiredPath, desiredContent, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	currentDigest := sha256.Sum256([]byte("old launcher binary"))
	currentHash := binhash.FormatDigest(currentDigest)

	desired := &schema.BureauVersion{
		LauncherStorePath: desiredPath,
	}

	diff, err := Compare(desired, "", currentHash, "")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if diff.DaemonChanged {
		t.Error("daemon should not be changed (empty store path)")
	}
	if !diff.LauncherChanged {
		t.Error("launcher should be changed (different content)")
	}
}

func TestCompareProxyChanged(t *testing.T) {
	directory := t.TempDir()

	desiredContent := []byte("new proxy binary")
	desiredPath := filepath.Join(directory, "bureau-proxy-new")
	if err := os.WriteFile(desiredPath, desiredContent, 0755); err != nil {
		t.Fatalf("WriteFile desired: %v", err)
	}

	currentContent := []byte("old proxy binary")
	currentPath := filepath.Join(directory, "bureau-proxy-current")
	if err := os.WriteFile(currentPath, currentContent, 0755); err != nil {
		t.Fatalf("WriteFile current: %v", err)
	}

	desired := &schema.BureauVersion{
		ProxyStorePath: desiredPath,
	}

	diff, err := Compare(desired, "", "", currentPath)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !diff.ProxyChanged {
		t.Error("proxy should be changed (different content)")
	}
}

func TestCompareProxyIdenticalDifferentPath(t *testing.T) {
	// The core value of content hashing: two different store paths
	// containing byte-identical binaries should NOT trigger an update.
	directory := t.TempDir()
	content := []byte("identical proxy binary")

	desiredPath := filepath.Join(directory, "nix-store-new", "bureau-proxy")
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(desiredPath, content, 0755); err != nil {
		t.Fatalf("WriteFile desired: %v", err)
	}

	currentPath := filepath.Join(directory, "nix-store-old", "bureau-proxy")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(currentPath, content, 0755); err != nil {
		t.Fatalf("WriteFile current: %v", err)
	}

	desired := &schema.BureauVersion{
		ProxyStorePath: desiredPath,
	}

	diff, err := Compare(desired, "", "", currentPath)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if diff.ProxyChanged {
		t.Error("proxy should NOT be changed (byte-identical binary at different paths)")
	}
}

func TestCompareProxyNoCurrentPath(t *testing.T) {
	directory := t.TempDir()

	desiredPath := filepath.Join(directory, "bureau-proxy")
	if err := os.WriteFile(desiredPath, []byte("proxy"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	desired := &schema.BureauVersion{
		ProxyStorePath: desiredPath,
	}

	diff, err := Compare(desired, "", "", "")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !diff.ProxyChanged {
		t.Error("proxy should be changed when no current path is known")
	}
}

func TestCompareEmptyCurrentHashes(t *testing.T) {
	directory := t.TempDir()

	daemonPath := filepath.Join(directory, "daemon")
	if err := os.WriteFile(daemonPath, []byte("daemon"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	launcherPath := filepath.Join(directory, "launcher")
	if err := os.WriteFile(launcherPath, []byte("launcher"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	desired := &schema.BureauVersion{
		DaemonStorePath:   daemonPath,
		LauncherStorePath: launcherPath,
	}

	// Empty current hashes indicate the hash was not computed (e.g.,
	// startup failure). Treat as changed to ensure an update is attempted.
	diff, err := Compare(desired, "", "", "")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !diff.DaemonChanged {
		t.Error("daemon should be changed when current hash is empty")
	}
	if !diff.LauncherChanged {
		t.Error("launcher should be changed when current hash is empty")
	}
}

func TestCompareDesiredPathMissing(t *testing.T) {
	desired := &schema.BureauVersion{
		DaemonStorePath: "/nonexistent/path/bureau-daemon",
	}

	_, err := Compare(desired, "abc123", "", "")
	if err == nil {
		t.Fatal("Compare should fail when desired path doesn't exist")
	}
}

func TestComparePartialConfig(t *testing.T) {
	// BureauVersion with only DaemonStorePath set — the others are empty
	// and should not trigger changes.
	directory := t.TempDir()
	content := []byte("daemon binary")

	daemonPath := filepath.Join(directory, "bureau-daemon")
	if err := os.WriteFile(daemonPath, content, 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	digest := sha256.Sum256(content)
	hexHash := binhash.FormatDigest(digest)

	desired := &schema.BureauVersion{
		DaemonStorePath: daemonPath,
	}

	diff, err := Compare(desired, hexHash, "", "")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if diff.DaemonChanged {
		t.Error("daemon should not be changed (identical content)")
	}
	if diff.LauncherChanged {
		t.Error("launcher should not be changed (empty store path)")
	}
	if diff.ProxyChanged {
		t.Error("proxy should not be changed (empty store path)")
	}
	if diff.NeedsUpdate() {
		t.Error("NeedsUpdate should be false")
	}
}

func TestComputeSelfHash(t *testing.T) {
	hash, binaryPath, err := ComputeSelfHash()
	if err != nil {
		t.Fatalf("ComputeSelfHash: %v", err)
	}
	if length := len(hash); length != 64 {
		t.Errorf("hash length = %d, want 64 (hex-encoded SHA256)", length)
	}
	if binaryPath == "" {
		t.Error("binaryPath should not be empty")
	}

	// Deterministic: hashing the same running binary twice.
	hash2, binaryPath2, err := ComputeSelfHash()
	if err != nil {
		t.Fatalf("ComputeSelfHash (second): %v", err)
	}
	if hash != hash2 {
		t.Error("ComputeSelfHash hash should be deterministic")
	}
	if binaryPath != binaryPath2 {
		t.Error("ComputeSelfHash path should be deterministic")
	}
}

func TestDiffNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		diff     Diff
		expected bool
	}{
		{"nothing changed", Diff{}, false},
		{"daemon changed", Diff{DaemonChanged: true}, true},
		{"launcher changed", Diff{LauncherChanged: true}, true},
		{"proxy changed", Diff{ProxyChanged: true}, true},
		{"all changed", Diff{DaemonChanged: true, LauncherChanged: true, ProxyChanged: true}, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.diff.NeedsUpdate(); got != test.expected {
				t.Errorf("NeedsUpdate() = %v, want %v", got, test.expected)
			}
		})
	}
}
