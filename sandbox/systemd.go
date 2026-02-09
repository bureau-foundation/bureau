// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
)

// SystemdScope wraps command execution in a systemd scope for resource limits.
type SystemdScope struct {
	// Name is the scope name (e.g., "bureau-agent-alice").
	Name string

	// Resources defines the resource limits.
	Resources ResourceConfig

	// User runs the scope as the current user (--user flag).
	User bool
}

// NewSystemdScope creates a new systemd scope wrapper.
func NewSystemdScope(name string, resources ResourceConfig) *SystemdScope {
	return &SystemdScope{
		Name:      name,
		Resources: resources,
		User:      true, // Default to user scope.
	}
}

// Available checks if systemd-run is available.
func (s *SystemdScope) Available() bool {
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// WrapCommand wraps a command with systemd-run for resource limits.
// Returns the original command unchanged if systemd is not available
// or no limits are configured.
func (s *SystemdScope) WrapCommand(cmd []string) []string {
	if !s.Available() {
		return cmd
	}

	if !s.Resources.HasLimits() {
		return cmd
	}

	args := []string{"systemd-run"}

	if s.User {
		args = append(args, "--user")
	}

	args = append(args, "--scope")

	if s.Name != "" {
		args = append(args, "--unit="+s.Name)
	}

	// Add resource limits as properties.
	if s.Resources.TasksMax > 0 {
		args = append(args, fmt.Sprintf("--property=TasksMax=%d", s.Resources.TasksMax))
	}

	if s.Resources.MemoryMax != "" {
		args = append(args, fmt.Sprintf("--property=MemoryMax=%s", s.Resources.MemoryMax))
	}

	if s.Resources.CPUQuota != "" {
		args = append(args, fmt.Sprintf("--property=CPUQuota=%s", s.Resources.CPUQuota))
	}

	// Separator and original command.
	args = append(args, "--")
	args = append(args, cmd...)

	return args
}

// ParseMemoryLimit parses a memory limit string (e.g., "2G", "512M").
// Returns the value in bytes, or 0 if unlimited/empty.
func ParseMemoryLimit(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(s)
	if s == "infinity" {
		return 0, nil
	}

	var multiplier uint64 = 1
	var numStr string

	switch {
	case strings.HasSuffix(s, "K"):
		multiplier = 1024
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "T"):
		multiplier = 1024 * 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	default:
		numStr = s
	}

	var value uint64
	if _, err := fmt.Sscanf(numStr, "%d", &value); err != nil {
		return 0, fmt.Errorf("invalid memory limit %q: %w", s, err)
	}

	return value * multiplier, nil
}

// ParseCPUQuota parses a CPU quota string (e.g., "200%", "100%").
// Returns the percentage as an integer, or 0 if unlimited/empty.
func ParseCPUQuota(s string) (int, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(s)
	if s == "infinity" {
		return 0, nil
	}

	s = strings.TrimSuffix(s, "%")

	var value int
	if _, err := fmt.Sscanf(s, "%d", &value); err != nil {
		return 0, fmt.Errorf("invalid CPU quota %q: %w", s, err)
	}

	return value, nil
}
