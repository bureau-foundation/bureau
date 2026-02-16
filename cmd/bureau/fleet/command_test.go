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

// TestDefaultFleetSocketPath verifies the default socket path outside a
// sandbox falls back to the host-side principal socket path.
func TestDefaultFleetSocketPath(t *testing.T) {
	// Tests run outside a sandbox, so the sandbox mount point does not
	// exist and the function falls back to the host-side path.
	got := defaultFleetSocketPath()
	want := principal.SocketPath("service/fleet/main")
	if got != want {
		t.Errorf("defaultFleetSocketPath(): got %q, want %q", got, want)
	}
}

// TestDefaultFleetTokenPath verifies the default token path outside a
// sandbox returns the sandbox token path (which won't exist on disk,
// but is the canonical location).
func TestDefaultFleetTokenPath(t *testing.T) {
	got := defaultFleetTokenPath()
	want := sandboxTokenPath
	if got != want {
		t.Errorf("defaultFleetTokenPath(): got %q, want %q", got, want)
	}
}

// TestConnectionAddFlags verifies that FleetConnection registers the
// expected flags and applies defaults.
func TestConnectionAddFlags(t *testing.T) {
	var connection FleetConnection

	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	connection.AddFlags(flagSet)

	// Verify flags exist. Outside a sandbox (the test environment), the
	// defaults come from the host-side fallback paths.
	expectedSocket := defaultFleetSocketPath()
	expectedToken := defaultFleetTokenPath()

	socketFlag := flagSet.Lookup("socket")
	if socketFlag == nil {
		t.Fatal("--socket flag not registered")
	}
	if socketFlag.DefValue != expectedSocket {
		t.Errorf("--socket default: got %q, want %q", socketFlag.DefValue, expectedSocket)
	}

	tokenFlag := flagSet.Lookup("token-file")
	if tokenFlag == nil {
		t.Fatal("--token-file flag not registered")
	}
	if tokenFlag.DefValue != expectedToken {
		t.Errorf("--token-file default: got %q, want %q", tokenFlag.DefValue, expectedToken)
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
