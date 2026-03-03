// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// TestServiceCommandHasSubcommands verifies the service command group
// contains the expected set of subcommands.
func TestServiceCommandHasSubcommands(t *testing.T) {
	command := Command()

	if command.Name != "service" {
		t.Errorf("command name: got %q, want %q", command.Name, "service")
	}

	expectedSubcommands := map[string]bool{
		"create":  false,
		"list":    false,
		"show":    false,
		"destroy": false,
		"plan":    false,
		"place":   false,
		"unplace": false,
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

// TestCommandAnnotations verifies every MCP-visible service command has
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
			t.Errorf("service %s: MCP-visible command missing Annotations", sub.Name)
		}
	}
}

// TestPlacementCommandsRequireServiceLocalpart verifies that plan, place,
// and unplace commands validate that a service localpart argument is provided.
func TestPlacementCommandsRequireServiceLocalpart(t *testing.T) {
	tests := []struct {
		name        string
		commandFunc func() *cli.Command
		wantPrefix  string
	}{
		{
			name:        "plan requires service",
			commandFunc: planCommand,
			wantPrefix:  "service localpart required",
		},
		{
			name:        "place requires service",
			commandFunc: placeCommand,
			wantPrefix:  "service localpart required",
		},
		{
			name:        "unplace requires service",
			commandFunc: unplaceCommand,
			wantPrefix:  "service localpart required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := test.commandFunc()
			err := command.Execute(nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.HasPrefix(err.Error(), test.wantPrefix) {
				t.Errorf("error: got %q, want prefix %q", err.Error(), test.wantPrefix)
			}
		})
	}
}

// TestUnplaceRequiresMachineFlag verifies the unplace command fails with
// a clear error when --machine is omitted.
func TestUnplaceRequiresMachineFlag(t *testing.T) {
	command := unplaceCommand()
	// Provide a service localpart but no --machine.
	err := command.Execute([]string{"service/test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--machine is required") {
		t.Errorf("error: got %q, want to contain %q", err.Error(), "--machine is required")
	}
}
