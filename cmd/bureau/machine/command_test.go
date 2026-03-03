// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// TestMachineCommandHasSubcommands verifies the machine command group
// contains the expected set of subcommands.
func TestMachineCommandHasSubcommands(t *testing.T) {
	command := Command()

	if command.Name != "machine" {
		t.Errorf("command name: got %q, want %q", command.Name, "machine")
	}

	expectedSubcommands := map[string]bool{
		"doctor":       false,
		"deploy":       false,
		"provision":    false,
		"list":         false,
		"show":         false,
		"upgrade":      false,
		"cordon":       false,
		"uncordon":     false,
		"label":        false,
		"decommission": false,
		"revoke":       false,
		"uninstall":    false,
	}

	for _, sub := range command.Subcommands {
		if _, expected := expectedSubcommands[sub.Name]; !expected {
			t.Errorf("unexpected subcommand: %q", sub.Name)
			continue
		}
		expectedSubcommands[sub.Name] = true
	}

	for name, found := range expectedSubcommands {
		if !found {
			t.Errorf("missing expected subcommand: %q", name)
		}
	}
}

// TestCommandAnnotations verifies every MCP-visible machine command has
// Annotations set.
func TestCommandAnnotations(t *testing.T) {
	command := Command()

	for _, sub := range command.Subcommands {
		if sub.Params == nil || sub.Run == nil {
			continue
		}
		if len(sub.RequiredGrants) == 0 {
			continue
		}
		if sub.Annotations == nil {
			t.Errorf("machine %s: MCP-visible command missing Annotations", sub.Name)
		}
	}
}

// TestShowRequiresMachineLocalpart verifies that the show command
// validates that a machine localpart argument is provided.
func TestShowRequiresMachineLocalpart(t *testing.T) {
	command := showCommand()
	err := command.Execute(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "machine localpart required") {
		t.Errorf("error: got %q, want prefix %q", err.Error(), "machine localpart required")
	}
}

// TestListRejectsExtraArgs verifies that the list command rejects
// more than one positional argument.
func TestListRejectsExtraArgs(t *testing.T) {
	command := listCommand()
	err := command.Execute([]string{"bureau/fleet/prod", "extra"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expected at most one argument") {
		t.Errorf("error: got %q, want to contain %q", err.Error(), "expected at most one argument")
	}
}

// requiresGrant checks that the named subcommand declares the expected grant.
func findSubcommand(parent *cli.Command, name string) *cli.Command {
	for _, sub := range parent.Subcommands {
		if sub.Name == name {
			return sub
		}
	}
	return nil
}

func TestShowGrant(t *testing.T) {
	command := Command()
	show := findSubcommand(command, "show")
	if show == nil {
		t.Fatal("show subcommand not found")
	}
	if len(show.RequiredGrants) == 0 {
		t.Fatal("show command has no required grants")
	}
	if show.RequiredGrants[0] != "command/machine/show" {
		t.Errorf("show grant: got %q, want %q", show.RequiredGrants[0], "command/machine/show")
	}
}
