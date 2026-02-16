// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

// ToolAnnotations describes behavioral properties of a CLI command
// when exposed as a tool by a tool server (e.g., MCP). Tool servers
// translate these properties into protocol-specific hints that help
// agents decide which tools are safe to call, which can be retried,
// and which require confirmation.
//
// All fields are pointers. A nil field means "unspecified" — the tool
// server applies its own defaults (which in MCP are: not read-only,
// destructive, not idempotent, open-world).
//
// Command authors should set Annotations on every MCP-visible command
// using one of the preset constructors: [ReadOnly], [Idempotent],
// [Create], or [Destructive]. The build-time lint test enforces this.
type ToolAnnotations struct {
	// ReadOnly is true when the command only reads state and never
	// modifies it. Agents may call read-only tools freely without
	// confirmation prompts.
	ReadOnly *bool

	// Destructive is true when the command may irreversibly remove
	// or damage data. Agents should require explicit confirmation
	// before calling destructive tools.
	Destructive *bool

	// Idempotent is true when repeated calls with identical arguments
	// produce the same result. Agents may safely retry idempotent
	// tools on transient failures.
	Idempotent *bool

	// OpenWorld is true when the command interacts with entities
	// beyond the Bureau system boundary (external services, remote
	// machines, etc.). Most Bureau commands operate within the
	// system's own Matrix infrastructure and are closed-world.
	OpenWorld *bool
}

// ReadOnly returns annotations for commands that query state without
// modifying it: list, show, get, status, describe, search, grep, etc.
func ReadOnly() *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnly:    boolPtr(true),
		Destructive: boolPtr(false),
		Idempotent:  boolPtr(true),
		OpenWorld:   boolPtr(false),
	}
}

// Idempotent returns annotations for commands that modify state but
// converge to the same result when called repeatedly with identical
// arguments: update, set, close, reopen, pin, tag, enable, etc.
func Idempotent() *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnly:    boolPtr(false),
		Destructive: boolPtr(false),
		Idempotent:  boolPtr(true),
		OpenWorld:   boolPtr(false),
	}
}

// Create returns annotations for commands that create new resources
// or produce side effects that accumulate on repeated calls: create,
// send, import, note, store, push, execute, etc.
func Create() *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnly:    boolPtr(false),
		Destructive: boolPtr(false),
		Idempotent:  boolPtr(false),
		OpenWorld:   boolPtr(false),
	}
}

// Destructive returns annotations for commands that irreversibly
// remove or destroy resources: delete, gc, kick, etc.
func Destructive() *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnly:    boolPtr(false),
		Destructive: boolPtr(true),
		Idempotent:  boolPtr(false),
		OpenWorld:   boolPtr(false),
	}
}

func boolPtr(value bool) *bool {
	return &value
}
