// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// VersionDiff describes which Bureau core binaries have changed between
// the currently running versions and the desired versions from a
// BureauVersion config. The daemon uses this to decide which components
// to restart: daemon via exec(), launcher via exec-update IPC, proxy by
// updating the launcher's binary path for future sandbox creation.
type VersionDiff struct {
	// DaemonChanged is true when the binary at the desired daemon
	// store path has different content (SHA256) than the running daemon.
	DaemonChanged bool

	// LauncherChanged is true when the binary at the desired launcher
	// store path has different content than the running launcher.
	LauncherChanged bool

	// ProxyChanged is true when the binary at the desired proxy store
	// path has different content than the proxy binary the launcher is
	// currently using for new sandbox creation. Existing proxy processes
	// are not affected — they continue running their current binary
	// until their sandbox is recycled.
	ProxyChanged bool
}

// NeedsUpdate returns true if any component needs updating.
func (d *VersionDiff) NeedsUpdate() bool {
	return d.DaemonChanged || d.LauncherChanged || d.ProxyChanged
}

// computeSelfHash returns the SHA256 hex digest and absolute filesystem
// path of the currently running binary (the daemon). Uses os.Executable()
// to resolve the binary path, which on Linux reads /proc/self/exe —
// always pointing to the original binary even if it has been replaced on
// disk since the process started.
func computeSelfHash() (hash string, binaryPath string, err error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("resolving own executable path: %w", err)
	}
	digest, err := binhash.HashFile(executable)
	if err != nil {
		return "", "", fmt.Errorf("hashing own binary at %s: %w", executable, err)
	}
	return binhash.FormatDigest(digest), executable, nil
}

// CompareBureauVersion compares desired BureauVersion store paths against
// currently running binary hashes. For each component (daemon, launcher,
// proxy), it hashes the binary at the desired store path and compares
// against the corresponding current hash. The desired store paths must
// already exist in the local Nix store (the caller must prefetch first).
//
// currentDaemonHash and currentLauncherHash are hex-encoded SHA256
// digests of the running binaries. currentProxyBinaryPath is the
// filesystem path of the proxy binary the launcher is currently using;
// it is hashed on the fly for comparison against the desired proxy.
//
// Returns nil when desired is nil (no version management configured).
func CompareBureauVersion(
	desired *schema.BureauVersion,
	currentDaemonHash string,
	currentLauncherHash string,
	currentProxyBinaryPath string,
) (*VersionDiff, error) {
	if desired == nil {
		return nil, nil
	}

	diff := &VersionDiff{}

	if desired.DaemonStorePath != "" {
		unchanged, err := binaryUnchanged(desired.DaemonStorePath, currentDaemonHash)
		if err != nil {
			return nil, fmt.Errorf("comparing daemon binary: %w", err)
		}
		diff.DaemonChanged = !unchanged
	}

	if desired.LauncherStorePath != "" {
		unchanged, err := binaryUnchanged(desired.LauncherStorePath, currentLauncherHash)
		if err != nil {
			return nil, fmt.Errorf("comparing launcher binary: %w", err)
		}
		diff.LauncherChanged = !unchanged
	}

	if desired.ProxyStorePath != "" && currentProxyBinaryPath != "" {
		desiredHash, err := binhash.HashFile(desired.ProxyStorePath)
		if err != nil {
			return nil, fmt.Errorf("hashing desired proxy at %s: %w", desired.ProxyStorePath, err)
		}
		currentHash, err := binhash.HashFile(currentProxyBinaryPath)
		if err != nil {
			return nil, fmt.Errorf("hashing current proxy at %s: %w", currentProxyBinaryPath, err)
		}
		diff.ProxyChanged = desiredHash != currentHash
	} else if desired.ProxyStorePath != "" && currentProxyBinaryPath == "" {
		// No current proxy binary known (launcher status not yet queried,
		// or proxy binary not configured). Treat as changed so the daemon
		// informs the launcher of the desired path.
		diff.ProxyChanged = true
	}

	return diff, nil
}

// binaryUnchanged hashes the file at desiredPath and compares it against
// the hex-encoded currentHash. Returns true when they match (the binary
// is unchanged and no restart is needed). Returns false when currentHash
// is empty (component hash not yet computed — treat as changed to trigger
// an update on first config delivery).
func binaryUnchanged(desiredPath string, currentHash string) (bool, error) {
	if currentHash == "" {
		return false, nil
	}

	desiredDigest, err := binhash.HashFile(desiredPath)
	if err != nil {
		return false, fmt.Errorf("hashing %s: %w", desiredPath, err)
	}

	return binhash.FormatDigest(desiredDigest) == currentHash, nil
}
