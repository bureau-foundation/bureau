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
		"create":    false,
		"define":    false,
		"list":      false,
		"show":      false,
		"instances": false,
		"destroy":   false,
		"delete":    false,
		"scale":     false,
		"plan":      false,
		"place":     false,
		"unplace":   false,
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

func TestDefineRequiresLocalpart(t *testing.T) {
	command := defineCommand()
	err := command.Execute(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "service localpart is required") {
		t.Errorf("error: got %q, want prefix %q", err.Error(), "service localpart is required")
	}
}

func TestDefineRequiresTemplate(t *testing.T) {
	command := defineCommand()
	err := command.Execute([]string{"service/test"})
	if err == nil {
		t.Fatal("expected error for missing --template, got nil")
	}
	if !strings.Contains(err.Error(), "--template is required") {
		t.Errorf("error: got %q, want to contain %q", err.Error(), "--template is required")
	}
}

func TestDefineRequiresFailover(t *testing.T) {
	command := defineCommand()
	err := command.Execute([]string{"service/test", "--template", "bureau/template:test"})
	if err == nil {
		t.Fatal("expected error for missing --failover, got nil")
	}
	if !strings.Contains(err.Error(), "--failover is required") {
		t.Errorf("error: got %q, want to contain %q", err.Error(), "--failover is required")
	}
}

func TestDefineRejectsInvalidFailover(t *testing.T) {
	command := defineCommand()
	err := command.Execute([]string{"service/test", "--template", "bureau/template:test", "--failover", "invalid"})
	if err == nil {
		t.Fatal("expected error for invalid --failover, got nil")
	}
	if !strings.Contains(err.Error(), "--failover must be one of") {
		t.Errorf("error: got %q, want to contain %q", err.Error(), "--failover must be one of")
	}
}

func TestScaleRequiresReplicaFlag(t *testing.T) {
	command := scaleCommand()
	err := command.Execute([]string{"service/test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "at least one of --replicas or --max-replicas") {
		t.Errorf("error: got %q, want to contain replica requirement", err.Error())
	}
}

func TestDeleteRequiresLocalpart(t *testing.T) {
	command := deleteCommand()
	err := command.Execute(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "service localpart is required") {
		t.Errorf("error: got %q, want prefix %q", err.Error(), "service localpart is required")
	}
}

func TestInstancesRequiresLocalpart(t *testing.T) {
	command := instancesCommand()
	err := command.Execute(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "service localpart is required") {
		t.Errorf("error: got %q, want prefix %q", err.Error(), "service localpart is required")
	}
}
