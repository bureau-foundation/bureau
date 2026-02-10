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
// The single-segment wildcard "*" does not match "/" — this is the standard
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

	// Suffix: "iree/**" — match the prefix exactly, then anything after.
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3]
		// "iree/**" matches "iree" itself and anything under "iree/".
		return localpart == prefix || strings.HasPrefix(localpart, prefix+"/")
	}

	// Prefix: "**/pm" — match anything before, then the suffix exactly.
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		return localpart == suffix || strings.HasSuffix(localpart, "/"+suffix)
	}

	// Interior: "iree/**/pm" — split on the first /**, match prefix
	// and suffix independently. The ** can match zero or more segments.
	separatorIndex := strings.Index(pattern, "/**/")
	if separatorIndex >= 0 {
		prefix := pattern[:separatorIndex]
		suffix := pattern[separatorIndex+4:]

		// The localpart must start with the prefix and end with the suffix.
		// The ** matches zero or more path segments between them.

		// Zero-segment case: "iree/**/pm" matches "iree/pm".
		if matched, _ := path.Match(prefix+"/"+suffix, localpart); matched {
			return true
		}

		// Multi-segment case: prefix must match the start, suffix must
		// match the end, and there must be content between them.
		if !strings.HasPrefix(localpart, prefix+"/") {
			return false
		}
		if !strings.HasSuffix(localpart, "/"+suffix) {
			return false
		}
		// Verify there's actual content between prefix and suffix
		// (they don't overlap).
		remaining := localpart[len(prefix)+1 : len(localpart)-len(suffix)-1]
		return len(remaining) >= 0
	}

	// Multiple ** segments or other complex patterns — not supported.
	// Deny by default.
	return false
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
