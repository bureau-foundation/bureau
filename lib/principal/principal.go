// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"fmt"
	"strings"
)

const (
	// DefaultRunDir is the default runtime directory for Bureau sockets
	// and ephemeral state. All runtime socket paths derive from this root.
	// Typically a tmpfs mount (/run), so everything is discarded on reboot.
	DefaultRunDir = "/run/bureau"

	// DefaultStateDir is the default directory for persistent state
	// (keypair, Matrix session). Survives reboots.
	DefaultStateDir = "/var/lib/bureau"

	// DefaultWorkspaceRoot is the default root for project workspaces.
	DefaultWorkspaceRoot = "/var/bureau/workspace"

	// DefaultCacheRoot is the default root for the machine-level tool
	// and model cache. Unlike workspace/.cache/ (which is workspace-
	// adjacent and backed up with workspace data), this is machine
	// infrastructure managed by the sysadmin principal. Agents get
	// read-only access to specific subdirectories via template mounts.
	//
	// Subdirectory conventions:
	//   npm/   — npm global cache
	//   pip/   — pip package cache
	//   bin/   — installed tool binaries (claude, codex, etc.)
	//   hf/    — HuggingFace model cache
	//   go/    — Go module cache
	//   nix/   — Nix store subset for agents needing nix-built tools
	DefaultCacheRoot = "/var/bureau/cache"

	// MaxLocalpartLength is the maximum allowed length for a Bureau
	// principal localpart. Derived from the unix socket path limit with
	// the default run directory. The longest socket suffixes are
	// ".proxy.sock" and ".admin.sock" (both 11 chars). The overhead is
	// len("/") + len(".proxy.sock") = 12 characters:
	//   107 (usable sun_path bytes) - 11 (DefaultRunDir) - 12 (overhead) = 84
	MaxLocalpartLength = 84

	// ServiceSocketSuffix is the file extension for service CBOR
	// endpoint sockets where services listen for incoming requests.
	ServiceSocketSuffix = ".sock"

	// ProxySocketSuffix is the file extension for per-principal proxy
	// sockets where the proxy listens for HTTP requests from the
	// sandboxed process.
	ProxySocketSuffix = ".proxy.sock"

	// ProxyAdminSocketSuffix is the file extension for daemon-only
	// proxy admin sockets used to push configuration to proxies.
	ProxyAdminSocketSuffix = ".admin.sock"
)

// allowedChars is the set of characters permitted in Matrix localparts
// (per the Matrix spec: a-z, 0-9, and the symbols . _ = - /).
// We check this via a lookup table for O(1) per-character validation.
var allowedChars [256]bool

func init() {
	for c := 'a'; c <= 'z'; c++ {
		allowedChars[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		allowedChars[c] = true
	}
	// Matrix localpart spec allows: a-z, 0-9, ., _, =, -, /
	allowedChars['.'] = true
	allowedChars['_'] = true
	allowedChars['='] = true
	allowedChars['-'] = true
	allowedChars['/'] = true
}

// ValidateLocalpart checks that a Bureau principal localpart is safe to
// use as both a Matrix user ID component and a filesystem path.
//
// Rules enforced:
//   - Non-empty
//   - Only lowercase a-z, 0-9, ., _, =, -, / (Matrix localpart charset)
//   - No ".." segments (path traversal)
//   - No segments starting with "." (hidden files/directories)
//   - No empty segments (double slashes "//" or leading/trailing "/")
//   - Maximum 84 characters (derived from unix socket path limit)
func ValidateLocalpart(localpart string) error {
	if localpart == "" {
		return fmt.Errorf("localpart is empty")
	}

	if len(localpart) > MaxLocalpartLength {
		return fmt.Errorf("localpart is %d characters, maximum is %d", len(localpart), MaxLocalpartLength)
	}

	return validatePath(localpart, "localpart")
}

// ValidateRelativePath checks that a path is safe for use in filesystem
// operations and shell interpolation. Enforces the same character whitelist
// and segment rules as ValidateLocalpart but without the length restriction.
//
// Used by daemon-side validation of workspace names and worktree paths.
// The character whitelist prevents shell metacharacters from reaching
// pipeline shell commands via variable substitution.
//
// The label parameter names the path in error messages (e.g., "worktree path",
// "workspace name").
func ValidateRelativePath(path, label string) error {
	if path == "" {
		return fmt.Errorf("%s is empty", label)
	}

	return validatePath(path, label)
}

// validatePath enforces the Bureau path safety rules shared by localparts,
// workspace names, and worktree paths:
//
//   - Characters restricted to a-z, 0-9, ., _, =, -, / (the Matrix
//     localpart charset — no shell metacharacters, no uppercase)
//   - No leading or trailing /
//   - No empty segments (double slashes)
//   - No ".." segments (path traversal)
//   - No segments starting with "." (hidden files/directories)
func validatePath(path, label string) error {
	// Check every character against the allowed set.
	for i := 0; i < len(path); i++ {
		if !allowedChars[path[i]] {
			return fmt.Errorf("%s: invalid character %q at position %d (allowed: a-z, 0-9, ., _, =, -, /)", label, path[i], i)
		}
	}

	// Structural checks on the slash-separated segments.
	if path[0] == '/' {
		return fmt.Errorf("%s must not start with /", label)
	}
	if path[len(path)-1] == '/' {
		return fmt.Errorf("%s must not end with /", label)
	}

	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("%s contains empty segment (double slash)", label)
		}
		if segment == ".." {
			return fmt.Errorf("%s contains '..' segment (path traversal)", label)
		}
		if segment[0] == '.' {
			return fmt.Errorf("%s segment %q starts with '.' (hidden file/directory)", label, segment)
		}
	}

	return nil
}

// RoomAliasLocalpart extracts the local alias name from a full Matrix room alias.
// Returns the part between # and :server. Uses the first colon as the separator
// because colons cannot appear in room alias localparts (same invariant as
// Matrix user ID localparts), but server names may contain ports.
//
//	RoomAliasLocalpart("#bureau/machine:bureau.local") → "bureau/machine"
//	RoomAliasLocalpart("#bureau/fleet/prod/machine/workstation:bureau.local") → "bureau/fleet/prod/machine/workstation"
//	RoomAliasLocalpart("#agents:example.org:8448") → "agents"
func RoomAliasLocalpart(fullAlias string) string {
	localpart := fullAlias
	if strings.HasPrefix(localpart, "#") {
		localpart = localpart[1:]
	}
	if colonIndex := strings.Index(localpart, ":"); colonIndex >= 0 {
		localpart = localpart[:colonIndex]
	}
	return localpart
}

// LauncherSocketPath returns the launcher IPC socket path for a run directory.
//
//	LauncherSocketPath("/run/bureau") → "/run/bureau/launcher.sock"
func LauncherSocketPath(runDir string) string {
	return runDir + "/launcher.sock"
}

// TmuxSocketPath returns the Bureau tmux server socket path for a run directory.
//
//	TmuxSocketPath("/run/bureau") → "/run/bureau/tmux.sock"
func TmuxSocketPath(runDir string) string {
	return runDir + "/tmux.sock"
}

// RelaySocketPath returns the transport relay socket path for a run directory.
//
//	RelaySocketPath("/run/bureau") → "/run/bureau/relay.sock"
func RelaySocketPath(runDir string) string {
	return runDir + "/relay.sock"
}

// ObserveSocketPath returns the observation request socket path for a run directory.
//
//	ObserveSocketPath("/run/bureau") → "/run/bureau/observe.sock"
func ObserveSocketPath(runDir string) string {
	return runDir + "/observe.sock"
}

// ValidateRunDir checks that a run directory path is short enough for unix
// socket path limits. The longest socket subpath is /<localpart>.admin.sock,
// where the overhead is len("/") + len(".admin.sock") = 12 characters.
// The total path must fit in 108 bytes (sun_path limit).
//
// Returns an error only if the run directory is too long for any localpart
// at all (zero usable characters). Returns nil otherwise. The maximum
// localpart length for a given run-dir is 107 - len(runDir) - 12.
//
// MaxLocalpartAvailable returns the actual limit for callers that want to
// check or log the effective localpart budget.
func ValidateRunDir(runDir string) error {
	available := MaxLocalpartAvailable(runDir)
	if available < 1 {
		overhead := len("/") + len(ProxyAdminSocketSuffix)
		return fmt.Errorf("--run-dir %q is %d bytes, too long for any localpart "+
			"(unix socket path limit is 108 bytes, overhead is %d bytes)",
			runDir, len(runDir), overhead+1)
	}
	return nil
}

// MaxLocalpartAvailable returns the maximum localpart length supported by
// the given run directory, accounting for the unix socket 108-byte sun_path
// limit. The longest socket subpath is /<localpart>.admin.sock, where the
// overhead is len("/") + len(".admin.sock") = 12 characters.
// Returns 0 if the run directory is too long for any localpart.
func MaxLocalpartAvailable(runDir string) int {
	overhead := len("/") + len(ProxyAdminSocketSuffix) // 1 + 11 = 12
	available := 107 - len(runDir) - overhead
	if available < 0 {
		return 0
	}
	return available
}

// ProxyServiceName converts a hierarchical service localpart to a flat name
// suitable for HTTP proxy routing paths. The proxy routes requests as
// /http/<name>/..., where <name> is a single path segment — no slashes.
//
//	ProxyServiceName("service/stt/whisper") → "service-stt-whisper"
//	ProxyServiceName("stt") → "stt"
func ProxyServiceName(localpart string) string {
	return strings.ReplaceAll(localpart, "/", "-")
}

// LocalpartFromMatrixID extracts the localpart from a full Matrix user ID.
// Returns an error if the ID doesn't have the expected @localpart:server format.
//
//	LocalpartFromMatrixID("@iree/amdgpu/pm:bureau.local") → "iree/amdgpu/pm"
//
// Uses the first colon after @ as the separator, since colons are not valid
// in Matrix localparts but server names can contain ports (e.g. example.org:8448).
func LocalpartFromMatrixID(matrixID string) (string, error) {
	if len(matrixID) < 2 || matrixID[0] != '@' {
		return "", fmt.Errorf("invalid Matrix user ID %q: must start with @", matrixID)
	}

	// Find the first colon after the @ sign. Localparts cannot contain colons,
	// so this is always the localpart/server boundary, even when the server
	// name includes a port (e.g. @alice:example.org:8448).
	colonIndex := strings.Index(matrixID[1:], ":")
	if colonIndex < 0 {
		return "", fmt.Errorf("invalid Matrix user ID %q: missing :server component", matrixID)
	}
	colonIndex++ // adjust for the [1:] offset

	if colonIndex < 2 {
		return "", fmt.Errorf("invalid Matrix user ID %q: empty localpart", matrixID)
	}

	return matrixID[1:colonIndex], nil
}
