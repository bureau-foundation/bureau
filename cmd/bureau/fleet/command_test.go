// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"testing"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// TestFleetCommandHasSubcommands verifies the fleet command group
// contains the expected set of subcommands.
func TestFleetCommandHasSubcommands(t *testing.T) {
	command := Command()

	if command.Name != "fleet" {
		t.Errorf("command name: got %q, want %q", command.Name, "fleet")
	}

	expectedSubcommands := map[string]bool{
		"enable":        false,
		"config":        false,
		"status":        false,
		"list-machines": false,
		"list-services": false,
		"show-machine":  false,
		"show-service":  false,
		"plan":          false,
		"place":         false,
		"unplace":       false,
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

// TestStatusParamsDefaults verifies the default socket and token paths.
func TestStatusParamsDefaults(t *testing.T) {
	expectedSocket := principal.SocketPath("service/fleet/main")
	if defaultSocketPath != expectedSocket {
		t.Errorf("default socket path: got %q, want %q", defaultSocketPath, expectedSocket)
	}

	expectedToken := "/run/bureau/tokens/fleet"
	if defaultTokenPath != expectedToken {
		t.Errorf("default token path: got %q, want %q", defaultTokenPath, expectedToken)
	}
}

// TestConnectionAddFlags verifies that FleetConnection registers the
// expected flags and applies defaults.
func TestConnectionAddFlags(t *testing.T) {
	var connection FleetConnection

	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	connection.AddFlags(flagSet)

	// Verify flags exist.
	socketFlag := flagSet.Lookup("socket")
	if socketFlag == nil {
		t.Fatal("--socket flag not registered")
	}
	if socketFlag.DefValue != defaultSocketPath {
		t.Errorf("--socket default: got %q, want %q", socketFlag.DefValue, defaultSocketPath)
	}

	tokenFlag := flagSet.Lookup("token-file")
	if tokenFlag == nil {
		t.Fatal("--token-file flag not registered")
	}
	if tokenFlag.DefValue != defaultTokenPath {
		t.Errorf("--token-file default: got %q, want %q", tokenFlag.DefValue, defaultTokenPath)
	}

	// Parse with custom values.
	err := flagSet.Parse([]string{"--socket", "/tmp/test.sock", "--token-file", "/tmp/token"})
	if err != nil {
		t.Fatalf("parsing flags: %v", err)
	}
	if connection.SocketPath != "/tmp/test.sock" {
		t.Errorf("socket path after parse: got %q, want %q", connection.SocketPath, "/tmp/test.sock")
	}
	if connection.TokenPath != "/tmp/token" {
		t.Errorf("token path after parse: got %q, want %q", connection.TokenPath, "/tmp/token")
	}
}

// TestEnableRequiresFlags verifies the enable command validates required flags.
func TestEnableRequiresFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing name",
			args:    []string{"--host", "machine/test"},
			wantErr: "--name is required",
		},
		{
			name:    "missing host",
			args:    []string{"--name", "prod"},
			wantErr: "--host is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a fresh command for each test case so flag state
			// from a previous parse does not carry over.
			command := enableCommand()
			err := command.Execute(test.args)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != test.wantErr {
				t.Errorf("error: got %q, want %q", err.Error(), test.wantErr)
			}
		})
	}
}

// TestCommandAnnotations verifies every MCP-visible fleet command has
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
			t.Errorf("fleet %s: MCP-visible command missing Annotations", sub.Name)
		}
	}
}
