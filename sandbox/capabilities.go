// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"os/exec"
	"strings"
)

// Capabilities describes what sandbox features are available on this system.
type Capabilities struct {
	// BwrapAvailable is true if bubblewrap is installed.
	BwrapAvailable bool

	// BwrapPath is the path to bwrap if available.
	BwrapPath string

	// BwrapVersion is the bwrap version string.
	BwrapVersion string

	// UserNamespacesEnabled is true if unprivileged user namespaces work.
	UserNamespacesEnabled bool

	// SystemdRunAvailable is true if systemd-run is available.
	SystemdRunAvailable bool

	// SystemdUserScopesWork is true if user scopes can be created.
	SystemdUserScopesWork bool

	// FuseOverlayfsAvailable is true if fuse-overlayfs is installed.
	// Required for overlay mounts (copy-on-write host cache sharing).
	FuseOverlayfsAvailable bool

	// FuseOverlayfsPath is the path to fuse-overlayfs if available.
	FuseOverlayfsPath string
}

// DetectCapabilities checks what sandbox features are available.
func DetectCapabilities() *Capabilities {
	caps := &Capabilities{}

	// Check bwrap.
	if path, err := BwrapPath(); err == nil {
		caps.BwrapAvailable = true
		caps.BwrapPath = path

		// Get version.
		if out, err := exec.Command(path, "--version").Output(); err == nil {
			caps.BwrapVersion = strings.TrimSpace(string(out))
		}
	}

	// Check user namespaces.
	caps.UserNamespacesEnabled = checkUserNamespaces()

	// Check systemd.
	if _, err := exec.LookPath("systemd-run"); err == nil {
		caps.SystemdRunAvailable = true

		// Try to create a user scope.
		cmd := exec.Command("systemd-run", "--user", "--scope", "--", "true")
		if err := cmd.Run(); err == nil {
			caps.SystemdUserScopesWork = true
		}
	}

	// Check fuse-overlayfs.
	if path, err := exec.LookPath("fuse-overlayfs"); err == nil {
		caps.FuseOverlayfsAvailable = true
		caps.FuseOverlayfsPath = path
	}

	return caps
}

// CanRunSandbox returns true if basic sandbox execution is possible.
func (c *Capabilities) CanRunSandbox() bool {
	return c.BwrapAvailable && c.UserNamespacesEnabled
}

// checkUserNamespaces tests if unprivileged user namespaces work.
func checkUserNamespaces() bool {
	// First check the sysctl.
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err == nil {
		if strings.TrimSpace(string(data)) == "0" {
			return false
		}
	}
	// File not existing usually means userns is allowed.

	// Try to actually create a user namespace with bwrap.
	bwrapPath, err := BwrapPath()
	if err != nil {
		return false
	}

	// Simple test: run true in a new user namespace.
	cmd := exec.Command(bwrapPath,
		"--unshare-user",
		"--ro-bind", "/", "/",
		"--",
		"true",
	)
	return cmd.Run() == nil
}

// SkipReason returns a human-readable reason why sandboxing isn't available,
// or empty string if it is available.
func (c *Capabilities) SkipReason() string {
	if !c.BwrapAvailable {
		return "bubblewrap not installed"
	}
	if !c.UserNamespacesEnabled {
		return "unprivileged user namespaces not enabled (set kernel.unprivileged_userns_clone=1)"
	}
	return ""
}
