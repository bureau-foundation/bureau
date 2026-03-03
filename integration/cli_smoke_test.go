// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestCLILoginAndWhoAmI exercises the full operator authentication lifecycle:
// bureau login writes a session file using the homeserver auto-detected from
// machine.conf, then bureau whoami --verify reads and validates that session
// against the homeserver.
//
// This test builds its own environment rather than using setupOperatorEnv
// because it tests the login flow itself — it controls session file creation.
func TestCLILoginAndWhoAmI(t *testing.T) {
	t.Parallel()

	localpart := uniqueAdminLocalpart(t)
	password := "test-cli-login-password"
	registerPrincipal(t, localpart, password)

	passwordFile := writePasswordFile(t, password)

	// Machine.conf with only the homeserver URL — tests auto-detection
	// for bureau login (which falls back to localhost:6167 without this).
	machineConf := writeMachineConf(t, testHomeserverURL, "", "", "")

	// Session file path in a temp directory — does not exist yet.
	sessionFile := filepath.Join(t.TempDir(), "session.json")

	env := []string{
		"BUREAU_MACHINE_CONF=" + machineConf,
		"BUREAU_SESSION_FILE=" + sessionFile,
	}

	// Login — this should auto-detect homeserver from machine.conf and
	// write the session file.
	runBureauWithEnvOrFail(t, env, "login", localpart, "--password-file", passwordFile)

	// Verify the session file was created.
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file not created after login: %v", err)
	}

	// WhoAmI with --verify contacts the homeserver to confirm the token.
	output := runBureauWithEnvOrFail(t, env, "whoami", "--verify", "--json")

	var result struct {
		UserID      string `json:"user_id"`
		Homeserver  string `json:"homeserver"`
		SessionFile string `json:"session_file"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse whoami JSON: %v\noutput:\n%s", err, output)
	}

	if !strings.Contains(result.UserID, localpart) {
		t.Errorf("user_id %q does not contain localpart %q", result.UserID, localpart)
	}
	if result.Homeserver != testHomeserverURL {
		t.Errorf("homeserver = %q, want %q", result.Homeserver, testHomeserverURL)
	}
	if result.SessionFile != sessionFile {
		t.Errorf("session_file = %q, want %q", result.SessionFile, sessionFile)
	}
	if !strings.HasPrefix(result.Status, "valid") {
		t.Errorf("status = %q, expected prefix \"valid\"", result.Status)
	}
}

// TestCLIMachineList exercises bureau machine list --json with a real machine
// stack and fleet controller. The fleet controller is required — machine list
// queries both Matrix state events (machine keys, hardware info) and the fleet
// controller (CPU, memory, assignments, labels).
func TestCLIMachineList(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)
	machine := newTestMachine(t, fleet, "cli-machine-list")

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	fleetController := startFleetController(t, admin, machine, "service/fleet/cli-machine-list", fleet)

	// Mint a fleet token for CLI direct-mode access.
	tokenBytes := mintFleetToken(t, fleet, machine, []string{"fleet/*"})
	tokenFile := writeTokenFile(t, tokenBytes)

	credentialFile := writeTestCredentialFile(t,
		testHomeserverURL, admin.UserID().String(), admin.AccessToken())
	machineConf := writeMachineConf(t,
		testHomeserverURL, testServerName, fleet.Prefix, machine.Name)

	env := []string{
		"BUREAU_MACHINE_CONF=" + machineConf,
		"BUREAU_FLEET_SOCKET=" + fleetController.SocketPath,
		"BUREAU_FLEET_TOKEN=" + tokenFile,
	}

	output := runBureauWithEnvOrFail(t, env,
		"machine", "list", "--credential-file", credentialFile, "--json")

	var result struct {
		Machines []struct {
			Name      string `json:"name"`
			PublicKey string `json:"public_key"`
			Algorithm string `json:"algorithm"`
		} `json:"machines"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse machine list JSON: %v\noutput:\n%s", err, output)
	}

	if len(result.Machines) == 0 {
		t.Fatal("machine list returned no entries")
	}

	var found bool
	for _, entry := range result.Machines {
		if entry.Name == machine.Name {
			found = true
			if entry.PublicKey == "" {
				t.Error("machine has empty public key")
			}
			if entry.Algorithm == "" {
				t.Error("machine has empty algorithm")
			}
			break
		}
	}
	if !found {
		t.Errorf("machine %q not found in %d entries", machine.Name, len(result.Machines))
	}
}

// TestCLITemplateList exercises bureau template list with operator session
// auth (ConnectOperator reads BUREAU_SESSION_FILE) and server-name
// auto-detected from machine.conf. The global template room is populated
// during TestMain setup with "base" and "base-networked" templates.
func TestCLITemplateList(t *testing.T) {
	t.Parallel()

	op := setupOperatorEnv(t)

	output := op.run(t, "template", "list", "bureau/template", "--json")

	var templates []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Inherits    []string `json:"inherits,omitempty"`
	}
	if err := json.Unmarshal([]byte(output), &templates); err != nil {
		t.Fatalf("parse template list JSON: %v\noutput:\n%s", err, output)
	}

	if len(templates) < 2 {
		t.Fatalf("expected at least 2 templates (base, base-networked), got %d", len(templates))
	}

	// Verify "base" template exists with a description.
	var baseFound bool
	for _, template := range templates {
		if template.Name == "base" {
			baseFound = true
			if template.Description == "" {
				t.Error("base template has empty description")
			}
			break
		}
	}
	if !baseFound {
		t.Error("base template not found in template list output")
	}

	// Verify "base-networked" inherits from "bureau/template:base".
	for _, template := range templates {
		if template.Name == "base-networked" {
			if !slices.Contains(template.Inherits, "bureau/template:base") {
				t.Errorf("base-networked inherits = %v, want to contain \"bureau/template:base\"", template.Inherits)
			}
			return
		}
	}
	t.Error("base-networked template not found in template list output")
}

// TestCLITemplateShow exercises bureau template show --json with inheritance
// resolution. "base-networked" inherits from "base" and overrides namespace
// settings. The resolved output merges fields from both templates.
func TestCLITemplateShow(t *testing.T) {
	t.Parallel()

	op := setupOperatorEnv(t)

	output := op.run(t, "template", "show", "bureau/template:base-networked", "--json")

	var template struct {
		Description string                     `json:"description"`
		Filesystem  []schema.TemplateMount     `json:"filesystem,omitempty"`
		Namespaces  *schema.TemplateNamespaces `json:"namespaces,omitempty"`
	}
	if err := json.Unmarshal([]byte(output), &template); err != nil {
		t.Fatalf("parse template show JSON: %v\noutput:\n%s", err, output)
	}

	if template.Description == "" {
		t.Error("template show: description is empty")
	}

	// Resolved template should have filesystem mounts inherited from "base".
	if len(template.Filesystem) == 0 {
		t.Error("template show: expected filesystem mounts from base template")
	}

	// base-networked overrides network namespace to false (host network).
	if template.Namespaces == nil {
		t.Fatal("template show: namespaces is nil")
	}
	if template.Namespaces.Net {
		t.Error("template show: expected namespaces.net=false for base-networked (host network)")
	}
}

// TestCLIDoctorJSON exercises bureau doctor --json with a fully provisioned
// operator environment. Verifies that operator session, machine configuration,
// homeserver reachability, and bureau space checks all pass. Systemd service
// and socket checks are expected to fail in the test environment.
func TestCLIDoctorJSON(t *testing.T) {
	t.Parallel()

	op := setupOperatorEnv(t)

	// Doctor exits non-zero when any check fails (systemd checks will fail
	// in the test environment), so use the non-fatal variant.
	output, _ := runBureauWithEnv(op.Env, "doctor", "--json")

	var result doctor.JSONOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse doctor JSON: %v\noutput:\n%s", err, output)
	}

	// Build a map for easy lookup by check name.
	checks := make(map[string]doctor.Result)
	for _, check := range result.Checks {
		checks[check.Name] = check
	}

	// These checks should pass with a fully provisioned operatorEnv.
	expectPass := []string{
		"operator session",
		"machine configuration",
		"homeserver reachable",
		"bureau space",
	}
	for _, name := range expectPass {
		check, ok := checks[name]
		if !ok {
			t.Errorf("doctor: %q check not found in output", name)
			continue
		}
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: %q status = %q, want %q (message: %s)",
				name, check.Status, doctor.StatusPass, check.Message)
		}
	}

	// Standard rooms should be accessible.
	standardRooms := []string{"bureau/system", "bureau/template", "bureau/pipeline", "bureau/artifact"}
	for _, room := range standardRooms {
		check, ok := checks[room]
		if !ok {
			t.Errorf("doctor: %q check not found", room)
			continue
		}
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: %q status = %q, want %q (message: %s)",
				room, check.Status, doctor.StatusPass, check.Message)
		}
	}
}
