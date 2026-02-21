// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"path"
	"strings"
)

// MatchPattern checks whether a localpart matches a glob pattern using
// Bureau's hierarchical namespace conventions:
//
//   - Exact match: "bureau-admin" matches only "bureau-admin"
//   - Single-segment wildcard: "iree/*" matches "iree/pm" but not "iree/amdgpu/pm"
//   - Recursive wildcard: "iree/**" matches "iree/pm", "iree/amdgpu/pm", etc.
//   - Universal: "**" matches any localpart
//   - Interior recursive: "iree/**/pm" matches "iree/pm", "iree/amdgpu/pm", etc.
//   - Character wildcards: "?" matches a single non-slash character
//
// Wildcards in * and ? work in all positions, including around **. For
// example, "team-*/**/build-?" matches "team-a/sub/build-x". The
// single-segment wildcard "*" does not match "/" — this is the standard
// path.Match behavior and matches the gitignore convention. Use "**" to
// match across hierarchy boundaries.
//
// Returns false for malformed patterns (unmatched brackets, etc.) rather
// than propagating errors — a malformed pattern should never grant access.
func MatchPattern(pattern, localpart string) bool {
	// Universal match.
	if pattern == "**" {
		return true
	}

	// No ** in the pattern — delegate to path.Match which handles
	// single-segment * and ? correctly (not matching /).
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, localpart)
		if err != nil {
			// Malformed pattern — deny.
			return false
		}
		return matched
	}

	// Pattern contains **. Handle the three cases: suffix, prefix, interior.

	// Suffix: "iree/**" or "team-*/**" — match the prefix (with glob
	// wildcards), then anything after.
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3]
		// ** matches zero additional segments: entire localpart is the prefix.
		if matchGlob(prefix, localpart) {
			return true
		}
		// ** matches one or more additional segments.
		return hasMatchingPrefix(prefix, localpart)
	}

	// Prefix: "**/pm" or "**/build-*" — match anything before, then the
	// suffix (with glob wildcards).
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		// ** matches zero additional segments: entire localpart is the suffix.
		if matchGlob(suffix, localpart) {
			return true
		}
		// ** matches one or more additional segments.
		return hasMatchingSuffix(suffix, localpart)
	}

	// Interior: "iree/**/pm" or "team-*/**/build-?" — split on the first
	// /**, match prefix and suffix independently with glob wildcards.
	separatorIndex := strings.Index(pattern, "/**/")
	if separatorIndex >= 0 {
		prefix := pattern[:separatorIndex]
		suffix := pattern[separatorIndex+4:]

		// Zero-segment case: ** matches nothing, prefix and suffix
		// are adjacent. "iree/**/pm" matches "iree/pm".
		if matchGlob(prefix+"/"+suffix, localpart) {
			return true
		}

		// Multi-segment case: prefix matches the start, suffix matches
		// the end, with at least one segment between for ** to consume.
		prefixDepth := strings.Count(prefix, "/") + 1
		suffixDepth := strings.Count(suffix, "/") + 1
		segments := strings.Split(localpart, "/")

		if len(segments) < prefixDepth+1+suffixDepth {
			return false
		}

		prefixCandidate := strings.Join(segments[:prefixDepth], "/")
		if !matchGlob(prefix, prefixCandidate) {
			return false
		}

		suffixCandidate := strings.Join(segments[len(segments)-suffixDepth:], "/")
		if !matchGlob(suffix, suffixCandidate) {
			return false
		}

		// Verify segments consumed by ** are all non-empty (reject
		// localparts with consecutive slashes between prefix and suffix).
		for _, segment := range segments[prefixDepth : len(segments)-suffixDepth] {
			if segment == "" {
				return false
			}
		}
		return true
	}

	// Multiple ** segments or other complex patterns — not supported.
	// Deny by default.
	return false
}

// matchGlob matches a pattern against a string using path.Match semantics
// (wildcards * and ? do not cross / boundaries). Returns false for
// malformed patterns.
func matchGlob(pattern, s string) bool {
	matched, err := path.Match(pattern, s)
	return err == nil && matched
}

// hasMatchingPrefix reports whether the localpart starts with segments
// that match the given glob pattern, with at least one additional segment
// after the matched portion. The pattern's depth (number of /-separated
// segments) determines how many leading segments of localpart are tested.
func hasMatchingPrefix(pattern, localpart string) bool {
	depth := strings.Count(pattern, "/") + 1
	segments := strings.SplitN(localpart, "/", depth+1)
	if len(segments) <= depth {
		return false
	}
	candidate := strings.Join(segments[:depth], "/")
	return matchGlob(pattern, candidate)
}

// hasMatchingSuffix reports whether the localpart ends with segments
// that match the given glob pattern, with at least one additional segment
// before the matched portion.
func hasMatchingSuffix(pattern, localpart string) bool {
	depth := strings.Count(pattern, "/") + 1
	segments := strings.Split(localpart, "/")
	if len(segments) <= depth {
		return false
	}
	candidate := strings.Join(segments[len(segments)-depth:], "/")
	return matchGlob(pattern, candidate)
}

// MatchAnyPattern checks whether a localpart matches any of the given
// glob patterns. Returns true on the first match. Returns false if the
// patterns slice is empty (default-deny).
func MatchAnyPattern(patterns []string, localpart string) bool {
	for _, pattern := range patterns {
		if MatchPattern(pattern, localpart) {
			return true
		}
	}
	return false
}

// MatchUserID checks whether a Matrix user ID matches a user ID pattern.
// This is the server-aware counterpart to MatchPattern: patterns match
// both the localpart hierarchy and the homeserver name, preventing
// cross-server identity confusion.
//
// Pattern format: "localpart_pattern:server_pattern"
//
//   - The localpart component uses the same glob syntax as MatchPattern
//     (*, **, ?, and exact segments separated by /)
//   - The server component uses path.Match glob syntax (* and ? only;
//     server names are flat, not hierarchical)
//   - Both components must match for the overall pattern to match
//
// The user ID argument should be a full Matrix user ID. The leading "@"
// sigil is stripped before matching.
//
// Patterns without a ":" separator are rejected (return false). This is
// a deliberate safety measure: bare localpart patterns could silently
// match principals across servers, which is a federation security
// violation. Every identity pattern must be explicit about which
// servers it applies to.
//
// Examples:
//
//	"bureau/fleet/prod/agent/**:bureau.local"  — any agent in this fleet
//	"**:bureau.local"                          — any entity on bureau.local
//	"**:**"                                    — any entity on any server
//	"bureau/fleet/*/agent/**:*"                — agents in any fleet on any server
//	"bureau/fleet/prod/agent/pm:bureau.local"  — exact match
func MatchUserID(pattern, userID string) bool {
	// Split pattern into localpart pattern and server pattern.
	colonIndex := strings.LastIndex(pattern, ":")
	if colonIndex < 0 {
		// No ":" in pattern — reject. Bare localpart patterns are a
		// security hazard in a federated system.
		return false
	}
	localpartPattern := pattern[:colonIndex]
	serverPattern := pattern[colonIndex+1:]

	// Strip leading "@" from the user ID if present.
	if len(userID) > 0 && userID[0] == '@' {
		userID = userID[1:]
	}

	// Split user ID into localpart and server.
	userColonIndex := strings.LastIndex(userID, ":")
	if userColonIndex < 0 {
		// Not a valid Matrix user ID — no server component.
		return false
	}
	localpart := userID[:userColonIndex]
	server := userID[userColonIndex+1:]

	// Both components must match.
	if !MatchPattern(localpartPattern, localpart) {
		return false
	}
	return matchGlob(serverPattern, server)
}

// MatchAnyUserID checks whether a Matrix user ID matches any of the
// given user ID patterns. Returns true on the first match. Returns
// false if the patterns slice is empty (default-deny).
func MatchAnyUserID(patterns []string, userID string) bool {
	for _, pattern := range patterns {
		if MatchUserID(pattern, userID) {
			return true
		}
	}
	return false
}
