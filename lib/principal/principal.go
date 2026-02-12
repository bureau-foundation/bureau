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

	// MaxLocalpartLength is the maximum allowed length for a Bureau
	// principal localpart. Derived from the unix socket path limit with
	// the default run directory:
	//   108 (sun_path) - len("/run/bureau/principal/") - len(".sock") = 82
	// We use 80 for a clean limit with 2 bytes of margin.
	MaxLocalpartLength = 80

	// SocketBasePath is the base directory under which principal sockets
	// are created with the default run directory. The localpart maps
	// directly to the path under this.
	SocketBasePath = DefaultRunDir + "/principal/"

	// AdminSocketBasePath is the base directory for proxy admin sockets
	// with the default run directory. These are daemon-only — never
	// bind-mounted into sandboxes. Separate from SocketBasePath so the
	// security boundary is visible at the filesystem level: agents see
	// <run-dir>/principal/, the daemon also sees <run-dir>/admin/.
	AdminSocketBasePath = DefaultRunDir + "/admin/"

	// SocketSuffix is the file extension for principal sockets.
	SocketSuffix = ".sock"

	// principalSubdir is the subdirectory under the run directory for
	// agent-facing principal sockets.
	principalSubdir = "/principal/"

	// adminSubdir is the subdirectory under the run directory for
	// daemon-only admin sockets.
	adminSubdir = "/admin/"
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
//   - Maximum 80 characters (derived from unix socket path limit)
func ValidateLocalpart(localpart string) error {
	if localpart == "" {
		return fmt.Errorf("localpart is empty")
	}

	if len(localpart) > MaxLocalpartLength {
		return fmt.Errorf("localpart is %d characters, maximum is %d", len(localpart), MaxLocalpartLength)
	}

	// Check every character against the allowed set.
	for i := 0; i < len(localpart); i++ {
		if !allowedChars[localpart[i]] {
			return fmt.Errorf("invalid character %q at position %d (allowed: a-z, 0-9, ., _, =, -, /)", localpart[i], i)
		}
	}

	// Structural checks on the slash-separated segments.
	if localpart[0] == '/' {
		return fmt.Errorf("localpart must not start with /")
	}
	if localpart[len(localpart)-1] == '/' {
		return fmt.Errorf("localpart must not end with /")
	}

	segments := strings.Split(localpart, "/")
	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("localpart contains empty segment (double slash)")
		}
		if segment == ".." {
			return fmt.Errorf("localpart contains '..' segment (path traversal)")
		}
		if segment[0] == '.' {
			return fmt.Errorf("segment %q starts with '.' (hidden file/directory)", segment)
		}
	}

	return nil
}

// MatrixUserID constructs a full Matrix user ID from a localpart and
// server name. The localpart is NOT validated — call ValidateLocalpart
// first if the input is untrusted.
//
//	MatrixUserID("iree/amdgpu/pm", "bureau.local") → "@iree/amdgpu/pm:bureau.local"
func MatrixUserID(localpart, serverName string) string {
	return "@" + localpart + ":" + serverName
}

// RoomAlias constructs a Matrix room alias from a local alias name and
// server name.
//
//	RoomAlias("iree/amdgpu/general", "bureau.local") → "#iree/amdgpu/general:bureau.local"
func RoomAlias(localAlias, serverName string) string {
	return "#" + localAlias + ":" + serverName
}

// RoomAliasLocalpart extracts the local alias name from a full Matrix room alias.
// Returns the part between # and :server. Uses the first colon as the separator
// because colons cannot appear in room alias localparts (same invariant as
// Matrix user ID localparts), but server names may contain ports.
//
//	RoomAliasLocalpart("#bureau/machine:bureau.local") → "bureau/machine"
//	RoomAliasLocalpart("#bureau/config/machine/workstation:bureau.local") → "bureau/config/machine/workstation"
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

// SocketPath returns the unix socket path for a principal's localpart
// using the default run directory.
//
//	SocketPath("iree/amdgpu/pm") → "/run/bureau/principal/iree/amdgpu/pm.sock"
//
// The caller is responsible for creating intermediate directories.
// The localpart should be validated with ValidateLocalpart before calling this.
func SocketPath(localpart string) string {
	return SocketBasePath + localpart + SocketSuffix
}

// AdminSocketPath returns the admin socket path for a principal using the
// default run directory. The daemon connects here to configure service routing;
// agents never see these sockets.
//
//	AdminSocketPath("iree/amdgpu/pm") → "/run/bureau/admin/iree/amdgpu/pm.sock"
func AdminSocketPath(localpart string) string {
	return AdminSocketBasePath + localpart + SocketSuffix
}

// RunDirSocketPath returns the principal socket path for a custom run directory.
//
//	RunDirSocketPath("/tmp/test", "iree/amdgpu/pm") → "/tmp/test/principal/iree/amdgpu/pm.sock"
func RunDirSocketPath(runDir, localpart string) string {
	return runDir + principalSubdir + localpart + SocketSuffix
}

// RunDirAdminSocketPath returns the admin socket path for a custom run directory.
//
//	RunDirAdminSocketPath("/tmp/test", "iree/amdgpu/pm") → "/tmp/test/admin/iree/amdgpu/pm.sock"
func RunDirAdminSocketPath(runDir, localpart string) string {
	return runDir + adminSubdir + localpart + SocketSuffix
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
// socket path limits. The longest subpath under run-dir is
// /principal/<localpart>.sock, where the overhead is len("/principal/") +
// len(".sock") = 16 characters. The total path must fit in 108 bytes
// (sun_path limit).
//
// Returns an error only if the run directory is too long for any localpart
// at all (zero usable characters). Returns nil otherwise. The maximum
// localpart length for a given run-dir is 107 - len(runDir) - 16.
//
// MaxLocalpartAvailable returns the actual limit for callers that want to
// check or log the effective localpart budget.
func ValidateRunDir(runDir string) error {
	available := MaxLocalpartAvailable(runDir)
	if available < 1 {
		overhead := len(principalSubdir) + len(SocketSuffix)
		return fmt.Errorf("--run-dir %q is %d bytes, too long for any localpart "+
			"(unix socket path limit is 108 bytes, overhead is %d bytes)",
			runDir, len(runDir), overhead+1)
	}
	return nil
}

// MaxLocalpartAvailable returns the maximum localpart length supported by
// the given run directory, accounting for the unix socket 108-byte sun_path
// limit. The longest socket subpath is /principal/<localpart>.sock.
// Returns 0 if the run directory is too long for any localpart.
func MaxLocalpartAvailable(runDir string) int {
	overhead := len(principalSubdir) + len(SocketSuffix) // 11 + 5 = 16
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
