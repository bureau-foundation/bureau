// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"strings"
)

// GlobFilter validates commands against glob patterns.
// This is useful for CLI services where you want to allow/block specific commands.
type GlobFilter struct {
	// Allowed is a list of glob patterns for allowed commands.
	// Empty means all commands are allowed (subject to Blocked).
	Allowed []string

	// Blocked is a list of glob patterns for blocked commands.
	// Blocked takes precedence over Allowed.
	Blocked []string
}

// Check validates that a command is allowed.
func (f *GlobFilter) Check(args []string) error {
	command := strings.Join(args, " ")

	// Check blocked patterns first (they take precedence)
	for _, pattern := range f.Blocked {
		if matchGlob(pattern, command) {
			return fmt.Errorf("matches blocked pattern: %s", pattern)
		}
	}

	// If no allowed patterns, everything (not blocked) is allowed
	if len(f.Allowed) == 0 {
		return nil
	}

	// Check if command matches any allowed pattern
	for _, pattern := range f.Allowed {
		if matchGlob(pattern, command) {
			return nil
		}
	}

	return fmt.Errorf("does not match any allowed pattern")
}

// matchGlob performs simple glob matching.
// Supports * as wildcard matching any characters.
func matchGlob(pattern, str string) bool {
	parts := strings.Split(pattern, "*")

	if len(parts) == 1 {
		// No wildcards, exact match
		return pattern == str
	}

	// Check prefix
	if !strings.HasPrefix(str, parts[0]) {
		return false
	}
	str = str[len(parts[0]):]

	// Check middle parts and suffix
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(str, parts[i])
		if idx == -1 {
			return false
		}
		str = str[idx+len(parts[i]):]
	}

	// Check suffix
	return strings.HasSuffix(str, parts[len(parts)-1])
}

// AllowAllFilter allows all requests. Useful for testing or trusted services.
type AllowAllFilter struct{}

// Check always returns nil (allows all requests).
func (f *AllowAllFilter) Check(args []string) error {
	return nil
}

// DenyAllFilter denies all requests. Useful for disabled services.
type DenyAllFilter struct {
	Reason string
}

// Check always returns an error.
func (f *DenyAllFilter) Check(args []string) error {
	if f.Reason != "" {
		return fmt.Errorf("service disabled: %s", f.Reason)
	}
	return fmt.Errorf("service disabled")
}

// Verify filters implement Filter interface.
var (
	_ Filter = (*GlobFilter)(nil)
	_ Filter = (*AllowAllFilter)(nil)
	_ Filter = (*DenyAllFilter)(nil)
)
