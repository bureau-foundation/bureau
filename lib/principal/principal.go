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
	// the default run directory. The longest socket subpath is
	// /<localpart>.admin.sock, where the overhead is len("/") +
	// len(".admin.sock") = 12 characters:
	//   107 (usable sun_path bytes) - 11 (DefaultRunDir) - 12 (overhead) = 84
	MaxLocalpartLength = 84

	// SocketSuffix is the file extension for agent-facing principal sockets.
	SocketSuffix = ".sock"

	// AdminSocketSuffix is the file extension for daemon-only admin sockets.
	// The ".admin.sock" suffix distinguishes admin sockets from agent-facing
	// sockets in the same directory — no parallel directory hierarchy needed.
	AdminSocketSuffix = ".admin.sock"
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

// SocketPath returns the agent-facing unix socket path for a principal's
// localpart using the default run directory.
//
//	SocketPath("iree/amdgpu/pm") → "/run/bureau/iree/amdgpu/pm.sock"
//
// The caller is responsible for creating intermediate directories.
// The localpart should be validated with ValidateLocalpart before calling this.
func SocketPath(localpart string) string {
	return DefaultRunDir + "/" + localpart + SocketSuffix
}

// AdminSocketPath returns the daemon-only admin socket path for a principal
// using the default run directory. The daemon connects here to configure
// service routing; agents never see these sockets.
//
//	AdminSocketPath("iree/amdgpu/pm") → "/run/bureau/iree/amdgpu/pm.admin.sock"
func AdminSocketPath(localpart string) string {
	return DefaultRunDir + "/" + localpart + AdminSocketSuffix
}

// RunDirSocketPath returns the agent-facing socket path for a custom run directory.
//
//	RunDirSocketPath("/tmp/test", "iree/amdgpu/pm") → "/tmp/test/iree/amdgpu/pm.sock"
func RunDirSocketPath(runDir, localpart string) string {
	return runDir + "/" + localpart + SocketSuffix
}

// RunDirAdminSocketPath returns the daemon-only admin socket path for a custom
// run directory.
//
//	RunDirAdminSocketPath("/tmp/test", "iree/amdgpu/pm") → "/tmp/test/iree/amdgpu/pm.admin.sock"
func RunDirAdminSocketPath(runDir, localpart string) string {
	return runDir + "/" + localpart + AdminSocketSuffix
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
		overhead := len("/") + len(AdminSocketSuffix)
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
	overhead := len("/") + len(AdminSocketSuffix) // 1 + 11 = 12
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

// fleetLiteral is the literal path segment that marks the fleet boundary
// in a fleet-scoped localpart: <namespace>/fleet/<fleetName>/<entityType>/<entityName>
const fleetLiteral = "fleet"

// FleetLocalpart constructs a fleet-scoped localpart from its components.
// The result follows the structure: namespace/fleet/fleetName/entityType/entityName.
//
//	FleetLocalpart("bureau", "prod", "machine", "gpu-box") → "bureau/fleet/prod/machine/gpu-box"
//	FleetLocalpart("acme", "staging", "service", "stt/whisper") → "acme/fleet/staging/service/stt/whisper"
//
// All components must be non-empty and must not contain "/". The entityName
// is the only component that may contain "/" when passed through
// ParseFleetLocalpart (it captures everything after entityType), but when
// constructing via FleetLocalpart, the entityName is used verbatim.
func FleetLocalpart(namespace, fleetName, entityType, entityName string) (string, error) {
	for _, component := range []struct {
		label string
		value string
	}{
		{"namespace", namespace},
		{"fleet name", fleetName},
		{"entity type", entityType},
		{"entity name", entityName},
	} {
		if component.value == "" {
			return "", fmt.Errorf("fleet localpart: %s is empty", component.label)
		}
		if strings.Contains(component.value, "/") {
			return "", fmt.Errorf("fleet localpart: %s %q contains '/'", component.label, component.value)
		}
	}
	return namespace + "/" + fleetLiteral + "/" + fleetName + "/" + entityType + "/" + entityName, nil
}

// ParseFleetLocalpart splits a fleet-scoped localpart into its components.
// The localpart must have the structure:
//
//	namespace/fleet/fleetName/entityType/entityName
//
// where "fleet" is a literal segment, and entityName may contain "/" (all
// segments from index 4 onward are joined). Minimum 5 segments required.
//
//	ParseFleetLocalpart("bureau/fleet/prod/machine/gpu-box")
//	  → ("bureau", "prod", "machine", "gpu-box", nil)
//
//	ParseFleetLocalpart("acme/fleet/staging/service/stt/whisper")
//	  → ("acme", "staging", "service", "stt/whisper", nil)
func ParseFleetLocalpart(localpart string) (namespace, fleetName, entityType, entityName string, err error) {
	segments := strings.Split(localpart, "/")
	if len(segments) < 5 {
		return "", "", "", "", fmt.Errorf("fleet localpart %q has %d segments, minimum is 5", localpart, len(segments))
	}
	if segments[1] != fleetLiteral {
		return "", "", "", "", fmt.Errorf("fleet localpart %q: segment 1 is %q, expected %q", localpart, segments[1], fleetLiteral)
	}
	namespace = segments[0]
	fleetName = segments[2]
	entityType = segments[3]
	entityName = strings.Join(segments[4:], "/")
	return namespace, fleetName, entityType, entityName, nil
}

// FleetRelativeName strips the namespace/fleet/fleetName/ prefix from a
// fleet-scoped localpart, returning entityType/entityName.
//
//	FleetRelativeName("bureau/fleet/prod/machine/gpu-box") → "machine/gpu-box"
//	FleetRelativeName("acme/fleet/staging/service/stt/whisper") → "service/stt/whisper"
func FleetRelativeName(localpart string) (string, error) {
	_, _, entityType, entityName, err := ParseFleetLocalpart(localpart)
	if err != nil {
		return "", err
	}
	return entityType + "/" + entityName, nil
}

// IsFleetScoped checks whether a localpart has fleet-scoped structure:
// at least 5 slash-separated segments with "fleet" as the second segment.
// This is a quick structural check, not a full validation.
func IsFleetScoped(localpart string) bool {
	segments := strings.Split(localpart, "/")
	return len(segments) >= 5 && segments[1] == fleetLiteral
}

// FleetPrefix returns the common prefix for all entities in a fleet:
// namespace/fleet/fleetName. Useful for constructing fleet-level room aliases
// or glob patterns.
//
//	FleetPrefix("bureau", "prod") → "bureau/fleet/prod"
func FleetPrefix(namespace, fleetName string) string {
	return namespace + "/" + fleetLiteral + "/" + fleetName
}

// ParseFleetPrefix is the inverse of FleetPrefix: it splits a fleet prefix
// string into its namespace and fleet name components. The prefix must
// contain exactly one "/fleet/" separator with non-empty parts on both sides.
//
//	ParseFleetPrefix("bureau/fleet/prod") → ("bureau", "prod", nil)
//	ParseFleetPrefix("acme/fleet/staging") → ("acme", "staging", nil)
//	ParseFleetPrefix("bureau/fleet/prod/machine") → error (trailing segments)
func ParseFleetPrefix(prefix string) (namespace, fleetName string, err error) {
	separator := "/" + fleetLiteral + "/"
	index := strings.Index(prefix, separator)
	if index < 0 {
		return "", "", fmt.Errorf("fleet prefix %q does not contain %q", prefix, separator)
	}
	namespace = prefix[:index]
	fleetName = prefix[index+len(separator):]
	if namespace == "" {
		return "", "", fmt.Errorf("fleet prefix %q has empty namespace", prefix)
	}
	if fleetName == "" {
		return "", "", fmt.Errorf("fleet prefix %q has empty fleet name", prefix)
	}
	if strings.Contains(fleetName, "/") {
		return "", "", fmt.Errorf("fleet prefix %q has trailing segments after fleet name", prefix)
	}
	return namespace, fleetName, nil
}
