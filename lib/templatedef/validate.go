// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package templatedef

import (
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Validate checks a TemplateContent for common issues. Returns a list of
// human-readable descriptions. An empty list means the template is valid.
//
// This validates the template's own consistency — field types, value
// ranges, cross-field constraints. It does not verify that parent
// templates exist in Matrix (that requires network access and happens
// during Push or Resolve).
func Validate(content *schema.TemplateContent) []string {
	var issues []string

	if content.Description == "" {
		issues = append(issues, "description is empty (every template should have a human-readable description)")
	}

	// Validate isolation mode.
	if content.Isolation != "" && !content.Isolation.IsKnown() {
		issues = append(issues, fmt.Sprintf("unknown isolation mode %q (expected %q or %q)",
			content.Isolation, schema.IsolationModeStandard, schema.IsolationModeNone))
	}

	// Cross-validate isolation with namespaces: passthrough mode
	// contradicts namespace unsharing.
	if content.Isolation == schema.IsolationModeNone && content.Namespaces != nil {
		ns := content.Namespaces
		if ns.PID || ns.Net || ns.IPC || ns.UTS {
			issues = append(issues, "isolation \"none\" contradicts namespace configuration: "+
				"passthrough mode does not unshare any namespaces, but namespaces are configured")
		}
	}

	// If this template doesn't inherit, it should define enough to be usable.
	if len(content.Inherits) == 0 {
		if len(content.Command) == 0 {
			issues = append(issues, "no command defined and no parent template to inherit from")
		}
	} else {
		// Validate each parent reference is parseable.
		for index, parentRef := range content.Inherits {
			if _, err := schema.ParseTemplateRef(parentRef); err != nil {
				issues = append(issues, fmt.Sprintf("inherits[%d] reference %q is invalid: %v", index, parentRef, err))
			}
		}
	}

	// Validate filesystem mounts.
	for index, mount := range content.Filesystem {
		if mount.Dest == "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: dest is required", index))
		}
		if mount.Type != "" && mount.Type != "tmpfs" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: unknown type %q (expected \"\" for bind or \"tmpfs\")", index, mount.Type))
		}
		if mount.Mode != "" && !mount.Mode.IsKnown() {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: unknown mode %q (expected %q or %q)", index, mount.Mode, schema.MountModeRO, schema.MountModeRW))
		}
		if mount.Type == "tmpfs" && mount.Source != "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: tmpfs mounts should not have a source path", index))
		}
		if mount.Type == "" && mount.Source == "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: bind mounts require a source path", index))
		}
	}

	// Validate resource limits.
	if content.Resources != nil {
		if content.Resources.CPUShares < 0 {
			issues = append(issues, "resources.cpu_shares must be non-negative")
		}
		if content.Resources.MemoryLimitMB < 0 {
			issues = append(issues, "resources.memory_limit_mb must be non-negative")
		}
		if content.Resources.PidsLimit < 0 {
			issues = append(issues, "resources.pids_limit must be non-negative")
		}
	}

	// Validate prepend variables.
	for key, values := range content.PrependVariables {
		if key == "" {
			issues = append(issues, "prepend_variables: empty key")
		}
		if len(values) == 0 {
			issues = append(issues, fmt.Sprintf("prepend_variables[%q]: value list is empty", key))
		}
		for index, value := range values {
			if value == "" {
				issues = append(issues, fmt.Sprintf("prepend_variables[%q][%d]: empty string (likely a mistake — empty segments in colon-delimited variables have surprising behavior)", key, index))
			}
			if strings.Contains(value, ":") {
				issues = append(issues, fmt.Sprintf("prepend_variables[%q][%d]: contains colon — each entry should be a single path segment, not a colon-delimited list (use separate entries instead)", key, index))
			}
		}
	}

	// Validate roles have non-empty commands.
	for roleName, command := range content.Roles {
		if len(command) == 0 {
			issues = append(issues, fmt.Sprintf("roles[%q]: command is empty", roleName))
		}
	}

	return issues
}
