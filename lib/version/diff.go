// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Diff describes which Bureau core binaries have changed between
// the currently running versions and the desired versions from a
// BureauVersion config. The daemon uses this to decide which components
// to restart: daemon via exec(), launcher via exec-update IPC, proxy
// and log-relay by updating the launcher's binary paths for future
// sandbox creation.
type Diff struct {
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

	// LogRelayChanged is true when the binary at the desired log-relay
	// store path has different content than the log-relay binary the
	// launcher is currently using for new sandbox creation. Existing
	// sandboxes are not affected — they continue running their current
	// log-relay binary until recycled.
	LogRelayChanged bool
}

// NeedsUpdate returns true if any component needs updating.
func (d *Diff) NeedsUpdate() bool {
	return d.DaemonChanged || d.LauncherChanged || d.ProxyChanged || d.LogRelayChanged
}

// NeedsNonDaemonNotification returns true if any non-daemon component
// changed. Daemon changes are reported separately (pre-exec message and
// post-exec watchdog), so the reconcile notification covers everything
// else. Having this as a method on Diff means adding a new component
// only requires updating this method — callers don't need to maintain
// their own OR chains.
func (d *Diff) NeedsNonDaemonNotification() bool {
	return d.LauncherChanged || d.ProxyChanged || d.LogRelayChanged
}

// CurrentState groups the running binary hashes and paths needed by
// Compare. Using a struct instead of positional parameters prevents
// mix-ups when multiple components share the same type (string) and
// makes adding future binaries trivial.
type CurrentState struct {
	// DaemonHash is the hex-encoded SHA256 of the running daemon binary.
	DaemonHash string

	// LauncherHash is the hex-encoded SHA256 of the running launcher binary.
	LauncherHash string

	// ProxyBinaryPath is the filesystem path of the proxy binary the
	// launcher is currently using for new sandbox creation.
	ProxyBinaryPath string

	// LogRelayBinaryPath is the filesystem path of the log-relay binary
	// the launcher is currently using for new sandbox creation.
	LogRelayBinaryPath string
}

// ComputeSelfHash returns the SHA256 hex digest and absolute filesystem
// path of the currently running binary. Uses os.Executable() to resolve
// the binary path, which on Linux reads /proc/self/exe — always pointing
// to the original binary even if it has been replaced on disk since the
// process started.
func ComputeSelfHash() (hash string, binaryPath string, err error) {
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

// Compare compares desired BureauVersion store paths against currently
// running binary hashes and paths. For hash-compared components (daemon,
// launcher), it hashes the desired store path binary and compares against
// the current hash. For path-compared components (proxy, log-relay), it
// hashes both the desired and current binary paths and compares digests.
// The desired store paths must already exist in the local Nix store (the
// caller must prefetch first).
//
// Returns nil when desired is nil (no version management configured).
func Compare(desired *schema.BureauVersion, current CurrentState) (*Diff, error) {
	if desired == nil {
		return nil, nil
	}

	diff := &Diff{}

	if desired.DaemonStorePath != "" {
		unchanged, err := binaryUnchanged(desired.DaemonStorePath, current.DaemonHash)
		if err != nil {
			return nil, fmt.Errorf("comparing daemon binary: %w", err)
		}
		diff.DaemonChanged = !unchanged
	}

	if desired.LauncherStorePath != "" {
		unchanged, err := binaryUnchanged(desired.LauncherStorePath, current.LauncherHash)
		if err != nil {
			return nil, fmt.Errorf("comparing launcher binary: %w", err)
		}
		diff.LauncherChanged = !unchanged
	}

	changed, err := pathBinaryChanged(desired.ProxyStorePath, current.ProxyBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("comparing proxy binary: %w", err)
	}
	diff.ProxyChanged = changed

	changed, err = pathBinaryChanged(desired.LogRelayStorePath, current.LogRelayBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("comparing log-relay binary: %w", err)
	}
	diff.LogRelayChanged = changed

	return diff, nil
}

// pathBinaryChanged compares a desired store path against a current
// binary path. Returns true (changed) when:
//   - desired is set and current is empty (new binary to push)
//   - desired and current both exist but have different SHA256 content
//
// Returns false (unchanged) when:
//   - desired is empty (not configured)
//   - desired and current have identical content
func pathBinaryChanged(desiredStorePath, currentBinaryPath string) (bool, error) {
	if desiredStorePath == "" {
		return false, nil
	}
	if currentBinaryPath == "" {
		// No current binary known (launcher status not yet queried,
		// or binary not configured). Treat as changed so the daemon
		// informs the launcher of the desired path.
		return true, nil
	}

	desiredHash, err := binhash.HashFile(desiredStorePath)
	if err != nil {
		return false, fmt.Errorf("hashing desired at %s: %w", desiredStorePath, err)
	}
	currentHash, err := binhash.HashFile(currentBinaryPath)
	if err != nil {
		return false, fmt.Errorf("hashing current at %s: %w", currentBinaryPath, err)
	}
	return desiredHash != currentHash, nil
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
