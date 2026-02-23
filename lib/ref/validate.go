// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import (
	"fmt"
	"strings"
)

const (
	// maxLocalpartLength is the maximum allowed length for a Bureau
	// localpart. Derived from the unix socket path limit with the
	// default run directory. See lib/principal for the derivation.
	maxLocalpartLength = 84

	// fleetLiteral is the literal path segment that marks the fleet
	// boundary in a fleet-scoped localpart.
	fleetLiteral = "fleet"

	// serviceSocketSuffix is the file extension for service CBOR
	// endpoint sockets — where a service listens for incoming
	// requests from other principals.
	serviceSocketSuffix = ".sock"

	// proxySocketSuffix is the file extension for per-principal proxy
	// sockets — where the proxy listens for HTTP requests from the
	// sandboxed process. Bind-mounted into the sandbox at
	// /run/bureau/proxy.sock. Also reachable from the host for
	// integration tests and diagnostics.
	proxySocketSuffix = ".proxy.sock"

	// proxyAdminSocketSuffix is the file extension for daemon-only
	// proxy admin sockets — used by the daemon to push configuration
	// updates (authorization grants, service routes) to a proxy.
	// Not bind-mounted into sandboxes — only reachable from the host.
	proxyAdminSocketSuffix = ".admin.sock"
)

// allowedChars is the set of characters permitted in Matrix localparts
// (per the Matrix spec: a-z, 0-9, and the symbols . _ = - /).
var allowedChars [256]bool

func init() {
	for c := byte('a'); c <= 'z'; c++ {
		allowedChars[c] = true
	}
	for c := byte('0'); c <= '9'; c++ {
		allowedChars[c] = true
	}
	allowedChars['.'] = true
	allowedChars['_'] = true
	allowedChars['='] = true
	allowedChars['-'] = true
	allowedChars['/'] = true
}

// validatePath enforces Bureau path safety rules: characters restricted
// to a-z, 0-9, ., _, =, -, /; no leading or trailing /; no empty
// segments; no ".." segments; no segments starting with ".".
func validatePath(path, label string) error {
	for i := 0; i < len(path); i++ {
		if !allowedChars[path[i]] {
			return fmt.Errorf("%s: invalid character %q at position %d (allowed: a-z, 0-9, ., _, =, -, /)", label, path[i], i)
		}
	}

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

// validateLocalpart validates a complete Bureau localpart.
func validateLocalpart(localpart string) error {
	if localpart == "" {
		return fmt.Errorf("localpart is empty")
	}
	if len(localpart) > maxLocalpartLength {
		return fmt.Errorf("localpart %q is %d characters, maximum is %d", localpart, len(localpart), maxLocalpartLength)
	}
	return validatePath(localpart, "localpart")
}

// validateSegment checks that a value is a valid single path segment:
// non-empty and contains no slashes.
func validateSegment(value, label string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if strings.Contains(value, "/") {
		return fmt.Errorf("%s %q must not contain '/'", label, value)
	}
	return nil
}

// validateServer checks that a Matrix server name is minimally valid:
// non-empty, no control characters, no Matrix sigils.
func validateServer(server string) error {
	if server == "" {
		return fmt.Errorf("server name is empty")
	}
	for i := 0; i < len(server); i++ {
		c := server[i]
		if c <= ' ' || c == '@' || c == '#' {
			return fmt.Errorf("server name %q: invalid character at position %d", server, i)
		}
	}
	return nil
}

// ExtractEntityName extracts the entity type and bare name from a
// localpart in either fleet-scoped format
// (namespace/fleet/name/entityType/entityName) or legacy format
// (entityType/entityName). Returns the entity type ("machine",
// "service", "agent") and the bare entity name.
//
// This handles both pre-migration and post-migration localpart formats,
// allowing callers to work with existing deployments during the
// transition to fleet-scoped naming.
//
//	ExtractEntityName("machine/workstation") → ("machine", "workstation", nil)
//	ExtractEntityName("bureau/fleet/prod/machine/workstation") → ("machine", "workstation", nil)
//	ExtractEntityName("bureau/fleet/prod/service/stt/whisper") → ("service", "stt/whisper", nil)
func ExtractEntityName(localpart string) (entityType, entityName string, err error) {
	segments := strings.Split(localpart, "/")
	// Fleet-scoped: namespace/fleet/name/entityType/entityName...
	if len(segments) >= 5 && segments[1] == fleetLiteral {
		return segments[3], strings.Join(segments[4:], "/"), nil
	}
	// Legacy: entityType/entityName...
	if len(segments) >= 2 {
		return segments[0], strings.Join(segments[1:], "/"), nil
	}
	return "", "", fmt.Errorf("cannot extract entity from localpart %q: expected entityType/entityName or namespace/fleet/name/entityType/entityName", localpart)
}

// MatrixUserID constructs a Matrix user ID (@localpart:server) from
// its parts. Use this for non-Bureau Matrix accounts (admin users,
// raw usernames) that don't have fleet-scoped structure. For Bureau
// entities, use Entity.UserID() instead.
func MatrixUserID(localpart string, server ServerName) UserID {
	return UserID{id: "@" + localpart + ":" + server.name}
}

// ServerFromUserID extracts the Matrix server name from a user ID
// (@localpart:server). This is the standard way for CLI commands to
// determine the server name from a connected session.
func ServerFromUserID(userID string) (ServerName, error) {
	_, server, err := parseMatrixID(userID)
	if err != nil {
		return ServerName{}, err
	}
	return newServerName(server), nil
}

// parseMatrixID extracts localpart and server from @localpart:server.
func parseMatrixID(matrixID string) (localpart, server string, err error) {
	return parsePrefixedID(matrixID, '@', "Matrix user ID")
}

// parseRoomAlias extracts localpart and server from #localpart:server.
func parseRoomAlias(alias string) (localpart, server string, err error) {
	return parsePrefixedID(alias, '#', "room alias")
}

// parsePrefixedID extracts localpart and server from a Matrix identifier
// with the given sigil prefix (@ for user IDs, # for room aliases).
func parsePrefixedID(identifier string, sigil byte, kind string) (localpart, server string, err error) {
	if len(identifier) < 2 || identifier[0] != sigil {
		return "", "", fmt.Errorf("invalid %s %q: must start with %c", kind, identifier, sigil)
	}
	colonIndex := strings.Index(identifier[1:], ":")
	if colonIndex < 0 {
		return "", "", fmt.Errorf("invalid %s %q: missing :server", kind, identifier)
	}
	colonIndex++ // adjust for [1:] offset
	if colonIndex < 2 {
		return "", "", fmt.Errorf("invalid %s %q: empty localpart", kind, identifier)
	}
	localpart = identifier[1:colonIndex]
	server = identifier[colonIndex+1:]
	if server == "" {
		return "", "", fmt.Errorf("invalid %s %q: empty server", kind, identifier)
	}
	return localpart, server, nil
}

// parseFleetPrefix splits "namespace/fleet/fleetName" into components.
func parseFleetPrefix(prefix string) (namespace, fleetName string, err error) {
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

// parseFleetLocalpart splits a fleet-scoped localpart into components.
// Structure: namespace/fleet/fleetName/entityType/entityName
// The entityName may contain "/" (all segments from index 4 onward).
func parseFleetLocalpart(localpart string) (namespace, fleetName, entityType, entityName string, err error) {
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
