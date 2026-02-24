// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestResolveMachineArg(t *testing.T) {
	defaultServer := ref.MustParseServerName("bureau.local")

	tests := []struct {
		name          string
		arg           string
		wantLocalpart string
		wantServer    string
		wantErr       bool
	}{
		{
			name:          "bare localpart uses default server",
			arg:           "bureau/fleet/prod/machine/workstation",
			wantLocalpart: "bureau/fleet/prod/machine/workstation",
			wantServer:    "bureau.local",
		},
		{
			name:          "full Matrix user ID uses embedded server",
			arg:           "@bureau/fleet/prod/machine/workstation:remote.server",
			wantLocalpart: "bureau/fleet/prod/machine/workstation",
			wantServer:    "remote.server",
		},
		{
			name:    "invalid localpart",
			arg:     "not-a-machine",
			wantErr: true,
		},
		{
			name:    "empty arg",
			arg:     "",
			wantErr: true,
		},
		{
			name:    "@ sigil but invalid user ID",
			arg:     "@invalid",
			wantErr: true,
		},
		{
			name:    "wrong entity type (service instead of machine)",
			arg:     "bureau/fleet/prod/service/ticket:bureau.local",
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			machine, err := resolveMachineArg(testCase.arg, defaultServer)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected error for arg %q, got machine %s", testCase.arg, machine.Localpart())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for arg %q: %v", testCase.arg, err)
			}
			if machine.Localpart() != testCase.wantLocalpart {
				t.Errorf("localpart: got %q, want %q", machine.Localpart(), testCase.wantLocalpart)
			}
			if machine.Server().String() != testCase.wantServer {
				t.Errorf("server: got %q, want %q", machine.Server().String(), testCase.wantServer)
			}
		})
	}
}

func TestResolveHostEnvBinaries(t *testing.T) {
	// Create a temp directory structure mimicking a bureau-host-env Nix
	// derivation: host-env/bin/{bureau-daemon,bureau-launcher,bureau-proxy}
	// as symlinks pointing into /nix/store/ paths.
	hostEnv := t.TempDir()
	binDir := filepath.Join(hostEnv, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create fake Nix store paths with actual files (EvalSymlinks needs
	// the target to exist).
	storeBase := t.TempDir()
	daemonPath := filepath.Join(storeBase, "abc-bureau-daemon", "bin", "bureau-daemon")
	launcherPath := filepath.Join(storeBase, "def-bureau-launcher", "bin", "bureau-launcher")
	proxyPath := filepath.Join(storeBase, "ghi-bureau-proxy", "bin", "bureau-proxy")

	for _, path := range []string{daemonPath, launcherPath, proxyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Symlink from host-env/bin/ to the store paths.
	if err := os.Symlink(daemonPath, filepath.Join(binDir, "bureau-daemon")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(launcherPath, filepath.Join(binDir, "bureau-launcher")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(proxyPath, filepath.Join(binDir, "bureau-proxy")); err != nil {
		t.Fatal(err)
	}

	// resolveHostEnvBinaries validates that resolved paths are under
	// /nix/store/, but our test paths are in a temp dir. To test the
	// symlink resolution logic without requiring /nix/store/, we test
	// the error case separately and test the happy path by verifying
	// the structure resolves (and then check the validation separately).
	//
	// For the happy path, we need to test with actual /nix/store/ paths
	// or skip the prefix validation. Since we can't create files in
	// /nix/store/ in tests, we test the validation separately.
	_, err := resolveHostEnvBinaries(hostEnv)
	if err == nil {
		t.Fatal("expected error for paths not under /nix/store/, got nil")
	}
	if expected := "not under /nix/store/"; !strings.Contains(err.Error(), expected) {
		t.Errorf("error %q should mention %q", err.Error(), expected)
	}
}

func TestResolveHostEnvBinaries_MissingBinary(t *testing.T) {
	hostEnv := t.TempDir()
	binDir := filepath.Join(hostEnv, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an empty bin/ directory with no symlinks at all. The first
	// binary (bureau-daemon) should produce a "not found" error.
	_, err := resolveHostEnvBinaries(hostEnv)
	if err == nil {
		t.Fatal("expected error for missing binaries")
	}
	if expected := "not found"; !strings.Contains(err.Error(), expected) {
		t.Errorf("error %q should mention %q", err.Error(), expected)
	}
}

func TestResolveHostEnvBinaries_NixStorePaths(t *testing.T) {
	// Test the happy path with real /nix/store/ paths. Skip if
	// /nix/store/ doesn't exist (non-Nix machines).
	if _, err := os.Stat("/nix/store"); os.IsNotExist(err) {
		t.Skip("/nix/store does not exist, skipping Nix store path test")
	}

	// Find a real Nix store path to use as a symlink target. We need
	// any file under /nix/store/ — use the first entry we find.
	entries, err := os.ReadDir("/nix/store")
	if err != nil {
		t.Skipf("cannot read /nix/store: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("/nix/store is empty")
	}

	// Find a real file to symlink to.
	var realStorePath string
	for _, entry := range entries {
		candidate := filepath.Join("/nix/store", entry.Name())
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		if info.IsDir() {
			// Look for any file inside this directory.
			subEntries, readErr := os.ReadDir(candidate)
			if readErr != nil {
				continue
			}
			for _, subEntry := range subEntries {
				subPath := filepath.Join(candidate, subEntry.Name())
				subInfo, subStatErr := os.Stat(subPath)
				if subStatErr != nil || subInfo.IsDir() {
					continue
				}
				realStorePath = subPath
				break
			}
		}
		if realStorePath != "" {
			break
		}
	}

	if realStorePath == "" {
		t.Skip("could not find a usable file under /nix/store/")
	}

	// Create host-env with symlinks pointing to the real Nix store path.
	hostEnv := t.TempDir()
	binDir := filepath.Join(hostEnv, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bureau-daemon", "bureau-launcher", "bureau-proxy"} {
		if err := os.Symlink(realStorePath, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	version, err := resolveHostEnvBinaries(hostEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All three should resolve to the same real store path.
	if version.DaemonStorePath != realStorePath {
		t.Errorf("daemon: got %q, want %q", version.DaemonStorePath, realStorePath)
	}
	if version.LauncherStorePath != realStorePath {
		t.Errorf("launcher: got %q, want %q", version.LauncherStorePath, realStorePath)
	}
	if version.ProxyStorePath != realStorePath {
		t.Errorf("proxy: got %q, want %q", version.ProxyStorePath, realStorePath)
	}
}
