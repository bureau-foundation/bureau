// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestFleetCommandHasSubcommands verifies the fleet command group
// contains the expected set of subcommands.
func TestFleetCommandHasSubcommands(t *testing.T) {
	command := Command()

	if command.Name != "fleet" {
		t.Errorf("command name: got %q, want %q", command.Name, "fleet")
	}

	expectedSubcommands := map[string]bool{
		"create":        false,
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

// TestFleetConnectionConfig verifies that the fleet service connection
// config has the expected sandbox paths and environment variable names.
func TestFleetConnectionConfig(t *testing.T) {
	if fleetConnectionConfig.Role != "fleet" {
		t.Errorf("role: got %q, want %q", fleetConnectionConfig.Role, "fleet")
	}
	if fleetConnectionConfig.SandboxSocket != "/run/bureau/service/fleet.sock" {
		t.Errorf("sandbox socket: got %q, want %q", fleetConnectionConfig.SandboxSocket, "/run/bureau/service/fleet.sock")
	}
	if fleetConnectionConfig.SandboxToken != "/run/bureau/service/token/fleet.token" {
		t.Errorf("sandbox token: got %q, want %q", fleetConnectionConfig.SandboxToken, "/run/bureau/service/token/fleet.token")
	}
	if fleetConnectionConfig.SocketEnvVar != "BUREAU_FLEET_SOCKET" {
		t.Errorf("socket env var: got %q, want %q", fleetConnectionConfig.SocketEnvVar, "BUREAU_FLEET_SOCKET")
	}
	if fleetConnectionConfig.TokenEnvVar != "BUREAU_FLEET_TOKEN" {
		t.Errorf("token env var: got %q, want %q", fleetConnectionConfig.TokenEnvVar, "BUREAU_FLEET_TOKEN")
	}
}

// TestConnectionAddFlags verifies that FleetConnection registers the
// expected flags and applies defaults.
func TestConnectionAddFlags(t *testing.T) {
	var connection FleetConnection

	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	connection.AddFlags(flagSet)

	// Verify flags exist. Outside a sandbox (the test environment), the
	// defaults come from the sandbox paths in fleetConnectionConfig.
	expectedSocket := fleetConnectionConfig.SandboxSocket
	expectedToken := fleetConnectionConfig.SandboxToken

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

	// Verify service mode flags exist.
	serviceFlag := flagSet.Lookup("service")
	if serviceFlag == nil {
		t.Fatal("--service flag not registered")
	}
	if serviceFlag.DefValue != "false" {
		t.Errorf("--service default: got %q, want %q", serviceFlag.DefValue, "false")
	}

	daemonFlag := flagSet.Lookup("daemon-socket")
	if daemonFlag == nil {
		t.Fatal("--daemon-socket flag not registered")
	}

	// Parse with direct mode custom values.
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
	if connection.ServiceMode {
		t.Error("service mode should be false by default")
	}
}

// TestConnectionAddFlagsServiceMode verifies that --service flag sets
// ServiceMode and --daemon-socket overrides the daemon path.
func TestConnectionAddFlagsServiceMode(t *testing.T) {
	var connection FleetConnection

	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	connection.AddFlags(flagSet)

	err := flagSet.Parse([]string{"--service", "--daemon-socket", "/tmp/daemon.sock"})
	if err != nil {
		t.Fatalf("parsing flags: %v", err)
	}
	if !connection.ServiceMode {
		t.Error("service mode should be true after --service")
	}
	if connection.DaemonSocket != "/tmp/daemon.sock" {
		t.Errorf("daemon socket: got %q, want %q", connection.DaemonSocket, "/tmp/daemon.sock")
	}
}

// TestConnectDirectModeRequiresTokenFile verifies that connect() in
// direct mode fails when the token file does not exist, producing a
// clear error rather than silently proceeding.
func TestConnectDirectModeRequiresTokenFile(t *testing.T) {
	var connection FleetConnection
	connection.SocketPath = "/tmp/nonexistent.sock"
	connection.TokenPath = "/tmp/nonexistent-token"

	_, err := connection.connect()
	if err == nil {
		t.Fatal("expected error when token file does not exist, got nil")
	}
}

// TestConnectServiceModeRequiresSession verifies that connect() in
// service mode fails with a clear error when no operator session exists.
// This exercises the MintServiceToken → LoadSession path without
// requiring a running daemon.
func TestConnectServiceModeRequiresSession(t *testing.T) {
	// Point the session to a nonexistent path so LoadSession fails.
	t.Setenv("BUREAU_SESSION_FILE", "/tmp/nonexistent-session.json")

	var connection FleetConnection
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	connection.AddFlags(flagSet)

	err := flagSet.Parse([]string{"--service", "--daemon-socket", "/tmp/nonexistent-daemon.sock"})
	if err != nil {
		t.Fatalf("parsing flags: %v", err)
	}

	_, err = connection.connect()
	if err == nil {
		t.Fatal("expected error when no operator session exists, got nil")
	}
}

// TestEnableRequiresArgs verifies the enable command validates required
// arguments: fleet localpart (positional) and --host flag.
func TestEnableRequiresArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: "fleet localpart is required (e.g., bureau/fleet/prod)",
		},
		{
			name:    "too many arguments",
			args:    []string{"bureau/fleet/prod", "bureau/fleet/staging"},
			wantErr: "expected exactly one argument (fleet localpart), got 2",
		},
		{
			name:    "missing host",
			args:    []string{"bureau/fleet/prod"},
			wantErr: "--host is required (machine name within the fleet, e.g., workstation)",
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

// TestExtractMachineName verifies the machine name extraction from both
// fleet-scoped and legacy localpart formats.
func TestExtractMachineName(t *testing.T) {
	tests := []struct {
		name      string
		localpart string
		wantName  string
		wantErr   bool
	}{
		{
			name:      "legacy simple",
			localpart: "machine/workstation",
			wantName:  "workstation",
		},
		{
			name:      "legacy multi-segment",
			localpart: "machine/ec2/us-east-1/gpu-01",
			wantName:  "ec2/us-east-1/gpu-01",
		},
		{
			name:      "fleet-scoped simple",
			localpart: "bureau/fleet/prod/machine/workstation",
			wantName:  "workstation",
		},
		{
			name:      "fleet-scoped multi-segment",
			localpart: "bureau/fleet/prod/machine/ec2/us-east-1/gpu-01",
			wantName:  "ec2/us-east-1/gpu-01",
		},
		{
			name:      "not a machine",
			localpart: "service/fleet/prod",
			wantErr:   true,
		},
		{
			name:      "empty",
			localpart: "",
			wantErr:   true,
		},
		{
			name:      "bare machine word",
			localpart: "machine",
			wantErr:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, err := extractMachineName(test.localpart)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got name %q", name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != test.wantName {
				t.Errorf("name = %q, want %q", name, test.wantName)
			}
		})
	}
}

// TestConfigRequiresFleetLocalpart verifies the config command validates
// that a fleet localpart argument is provided.
func TestConfigRequiresFleetLocalpart(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: "fleet localpart is required (e.g., bureau/fleet/prod)",
		},
		{
			name:    "too many arguments",
			args:    []string{"bureau/fleet/prod", "bureau/fleet/staging"},
			wantErr: "expected exactly one argument (fleet localpart), got 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := configCommand()
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

// TestCreateRequiresFleetLocalpart verifies the create command validates
// that a fleet localpart argument is provided.
func TestCreateRequiresFleetLocalpart(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: "fleet localpart is required (e.g., bureau/fleet/prod)",
		},
		{
			name:    "too many arguments",
			args:    []string{"bureau/fleet/prod", "bureau/fleet/staging"},
			wantErr: "expected exactly one argument (fleet localpart), got 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := createCommand()
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
