// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// TestCommandTreeAnnotations walks the full production command tree
// and validates that every MCP-visible command (one with Params, Run,
// and RequiredGrants) declares Annotations. Without annotations,
// agents can't determine whether a tool is read-only, destructive,
// idempotent, or interacts with external systems — they must assume
// the worst (MCP defaults: destructive, non-idempotent, open-world).
//
// Use cli.ReadOnly(), cli.Idempotent(), cli.Create(), or
// cli.Destructive() to set appropriate annotations on each command.
func TestCommandTreeAnnotations(t *testing.T) {
	root := rootCommand()
	walkCommands(root, nil, func(command *cli.Command, path []string) {
		if command.Params == nil || command.Run == nil {
			return
		}
		if len(command.RequiredGrants) == 0 {
			return
		}
		if command.Annotations == nil {
			t.Errorf("%s: MCP-visible command missing Annotations", strings.Join(path, " "))
		}
	})
}

// walkCommands recursively visits every command in the tree,
// calling visit for each node with the accumulated command path.
func walkCommands(command *cli.Command, path []string, visit func(*cli.Command, []string)) {
	current := make([]string, len(path)+1)
	copy(current, path)
	current[len(path)] = command.Name
	visit(command, current)
	for _, sub := range command.Subcommands {
		walkCommands(sub, current, visit)
	}
}
