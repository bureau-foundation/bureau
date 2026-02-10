// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package integration_test provides end-to-end integration tests that exercise
// the full Bureau stack against real services. These tests require Docker and
// are tagged "manual" in Bazel so they don't run with //... .
//
// The test lifecycle:
//   - TestMain starts a Docker Compose stack (Continuwuity homeserver)
//   - TestMain runs "bureau matrix setup" to bootstrap the server
//   - Individual tests verify the resulting state via CLI and API
//   - TestMain tears down the stack and removes volumes
package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

const (
	testHomeserverURL     = "http://localhost:6168"
	testServerName        = "test.bureau.local"
	testRegistrationToken = "test-registration-token"
	composeProjectName    = "bureau-test"
)

var (
	// workspaceRoot is the real filesystem path to the Bureau source tree.
	// Resolved in TestMain from Bazel runfiles or by walking up from CWD.
	workspaceRoot string

	// bureauBinary is the path to the compiled bureau CLI binary.
	// Resolved from BUREAU_BINARY env var (Bazel data dep) or bazel-bin.
	bureauBinary string

	// credentialFile is the path where setup writes Bureau credentials.
	credentialFile string
)

func TestMain(m *testing.M) {
	var err error

	workspaceRoot, err = findWorkspaceRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find workspace root: %v\n", err)
		os.Exit(1)
	}

	bureauBinary, err = findBureauBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau binary not found: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Bazel: bazel test //integration:integration_test")
		fmt.Fprintln(os.Stderr, "  Go:    BUREAU_BINARY=$(bazel info bazel-bin)/cmd/bureau/bureau_/bureau go test -v ./integration/")
		os.Exit(1)
	}

	credentialFile = filepath.Join(workspaceRoot, "deploy", "test", "bureau-creds")

	if err := checkDockerAccess(); err != nil {
		fmt.Fprintf(os.Stderr, "Docker not available: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  The Bazel server inherits groups from its first invocation.")
		fmt.Fprintln(os.Stderr, "  Restart the server under the docker group:")
		fmt.Fprintln(os.Stderr, "    sg docker -c 'bazel shutdown; bazel test //integration:integration_test'")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  Or add your user to the docker group permanently:")
		fmt.Fprintln(os.Stderr, "    sudo usermod -aG docker $USER && newgrp docker")
		os.Exit(1)
	}

	// Clean up any leftover state from a previous interrupted run.
	_ = dockerCompose("down", "-v")

	if err := dockerCompose("up", "-d"); err != nil {
		fmt.Fprintf(os.Stderr, "compose up failed: %v\n", err)
		os.Exit(1)
	}

	if err := waitForHealthy(30 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Continuwuity did not become healthy: %v\n", err)
		_ = dockerCompose("logs")
		_ = dockerCompose("down", "-v")
		os.Exit(1)
	}

	if err := runBureauSetup(); err != nil {
		fmt.Fprintf(os.Stderr, "bureau matrix setup failed: %v\n", err)
		_ = dockerCompose("down", "-v")
		os.Exit(1)
	}

	code := m.Run()

	_ = dockerCompose("down", "-v")
	os.Exit(code)
}

// --- Tests ---

func TestDoctorPassesAfterSetup(t *testing.T) {
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
	)
	if !strings.Contains(output, "All checks passed") {
		t.Errorf("expected 'All checks passed' in doctor output:\n%s", output)
	}
}

func TestSetupIsIdempotent(t *testing.T) {
	// Run setup a second time — should succeed without errors.
	if err := runBureauSetup(); err != nil {
		t.Fatalf("setup re-run failed: %v", err)
	}

	// Doctor should still pass.
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
	)
	if !strings.Contains(output, "All checks passed") {
		t.Errorf("expected 'All checks passed' after idempotent setup:\n%s", output)
	}
}

func TestRoomsExistViaAPI(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	expectedAliases := []string{
		"#bureau:" + testServerName,
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range expectedAliases {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("alias %s: %v", alias, err)
			continue
		}
		if roomID == "" {
			t.Errorf("alias %s resolved to empty room ID", alias)
		}
	}
}

func TestSpaceHierarchy(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	spaceRoomID, err := session.ResolveAlias(t.Context(), "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve space alias: %v", err)
	}

	// Read space state to find m.space.child events.
	events, err := session.GetRoomState(t.Context(), spaceRoomID)
	if err != nil {
		t.Fatalf("get space state: %v", err)
	}

	children := make(map[string]bool)
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			children[*event.StateKey] = true
		}
	}

	// Verify each standard room is a child of the space.
	childRooms := []string{
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range childRooms {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("resolve %s: %v", alias, err)
			continue
		}
		if !children[roomID] {
			t.Errorf("room %s (%s) is not a child of the Bureau space", alias, roomID)
		}
	}
}

func TestTemplatesPublished(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	templatesRoomID, err := session.ResolveAlias(t.Context(), "#bureau/templates:"+testServerName)
	if err != nil {
		t.Fatalf("resolve templates alias: %v", err)
	}

	// Read base template state event.
	content, err := session.GetStateEvent(t.Context(), templatesRoomID, "m.bureau.template", "base")
	if err != nil {
		t.Fatalf("get base template: %v", err)
	}

	var template map[string]any
	if err := json.Unmarshal(content, &template); err != nil {
		t.Fatalf("unmarshal base template: %v", err)
	}

	description, ok := template["description"].(string)
	if !ok || description == "" {
		t.Errorf("base template missing description, got: %v", template)
	}

	// Verify base-networked template exists and inherits from base.
	content, err = session.GetStateEvent(t.Context(), templatesRoomID, "m.bureau.template", "base-networked")
	if err != nil {
		t.Fatalf("get base-networked template: %v", err)
	}

	if err := json.Unmarshal(content, &template); err != nil {
		t.Fatalf("unmarshal base-networked template: %v", err)
	}

	inherits, ok := template["inherits"].(string)
	if !ok || inherits != "bureau/templates:base" {
		t.Errorf("expected base-networked to inherit from bureau/templates:base, got %q", inherits)
	}
}

func TestJoinRulesAreInviteOnly(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	rooms := []string{
		"#bureau:" + testServerName,
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range rooms {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("resolve %s: %v", alias, err)
			continue
		}

		content, err := session.GetStateEvent(t.Context(), roomID, "m.room.join_rules", "")
		if err != nil {
			t.Errorf("get join rules for %s: %v", alias, err)
			continue
		}

		var joinRules map[string]any
		if err := json.Unmarshal(content, &joinRules); err != nil {
			t.Errorf("unmarshal join rules for %s: %v", alias, err)
			continue
		}

		if rule, _ := joinRules["join_rule"].(string); rule != "invite" {
			t.Errorf("room %s: join_rule is %q, expected invite", alias, rule)
		}
	}
}

func TestDoctorJSONOutput(t *testing.T) {
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
		"--json",
	)

	var result struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("cannot parse doctor JSON output: %v\noutput:\n%s", err, output)
	}

	if !result.OK {
		t.Error("doctor JSON reports ok=false")
		for _, check := range result.Checks {
			if check.Status == "fail" {
				t.Errorf("  [FAIL] %s", check.Name)
			}
		}
	}

	if len(result.Checks) == 0 {
		t.Error("doctor JSON returned no checks")
	}
}

// --- Helpers ---

// findWorkspaceRoot returns the real filesystem path to the Bureau source tree.
// In Bazel, this resolves through the runfiles symlink tree. Outside Bazel,
// it walks up from the current directory looking for MODULE.bazel.
func findWorkspaceRoot() (string, error) {
	// Bazel: resolve through the MODULE.bazel file in runfiles.
	if runfilesDirectory := os.Getenv("RUNFILES_DIR"); runfilesDirectory != "" {
		moduleFile := filepath.Join(runfilesDirectory, "_main", "MODULE.bazel")
		realPath, err := filepath.EvalSymlinks(moduleFile)
		if err == nil {
			return filepath.Dir(realPath), nil
		}
	}

	// Outside Bazel: walk up from CWD.
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "MODULE.bazel")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("MODULE.bazel not found in any parent directory")
		}
		directory = parent
	}
}

// findBureauBinary locates the compiled bureau CLI binary.
// Checks BUREAU_BINARY env (Bazel data dep) first, then falls back to
// a well-known bazel-bin path relative to the workspace root.
func findBureauBinary() (string, error) {
	// Bazel: resolve via data dependency environment variable.
	if rlocationPath := os.Getenv("BUREAU_BINARY"); rlocationPath != "" {
		runfilesDirectory := os.Getenv("RUNFILES_DIR")
		if runfilesDirectory == "" {
			return "", fmt.Errorf("BUREAU_BINARY set but RUNFILES_DIR missing")
		}
		absolutePath := filepath.Join(runfilesDirectory, rlocationPath)
		if _, err := os.Stat(absolutePath); err != nil {
			return "", fmt.Errorf("bureau binary not found at %s: %w", absolutePath, err)
		}
		return absolutePath, nil
	}

	// Outside Bazel: check well-known bazel-bin location.
	candidate := filepath.Join(workspaceRoot, "bazel-bin", "cmd", "bureau", "bureau_", "bureau")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}

	return "", fmt.Errorf("not found (set BUREAU_BINARY or build with bazel build //cmd/bureau)")
}

// checkDockerAccess verifies that the Docker daemon is reachable.
func checkDockerAccess() error {
	cmd := exec.Command("docker", "compose", "version")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// dockerCompose runs docker compose with the test project configuration.
func dockerCompose(args ...string) error {
	composeFile := filepath.Join(workspaceRoot, "deploy", "test", "docker-compose.yaml")
	fullArgs := []string{
		"compose",
		"-f", composeFile,
		"-p", composeProjectName,
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.Command("docker", fullArgs...)
	cmd.Stdout = os.Stderr // test infrastructure output goes to stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitForHealthy polls the Continuwuity versions endpoint until it responds.
func waitForHealthy(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout after %s waiting for %s", timeout, testHomeserverURL)
		default:
			response, err := http.Get(testHomeserverURL + "/_matrix/client/versions")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// runBureauSetup runs "bureau matrix setup" against the test homeserver.
// The registration token is piped via stdin.
func runBureauSetup() error {
	cmd := exec.Command(bureauBinary, "matrix", "setup",
		"--homeserver", testHomeserverURL,
		"--server-name", testServerName,
		"--registration-token-file", "-",
		"--credential-file", credentialFile,
	)
	cmd.Stdin = strings.NewReader(testRegistrationToken)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runBureau executes the bureau CLI with the given arguments and returns
// its combined stdout output as a string.
func runBureau(args ...string) (string, error) {
	cmd := exec.Command(bureauBinary, args...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.String(), err
}

// runBureauOrFail runs the bureau CLI and fails the test on error.
func runBureauOrFail(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runBureau(args...)
	if err != nil {
		t.Fatalf("bureau %s failed: %v\noutput:\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

// adminSession creates an authenticated Matrix session using the credentials
// written by setup. The caller must close the returned session.
func adminSession(t *testing.T) *messaging.Session {
	t.Helper()

	credentials := loadCredentials(t)
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		t.Fatal("MATRIX_HOMESERVER_URL missing from credential file")
	}
	token := credentials["MATRIX_ADMIN_TOKEN"]
	if token == "" {
		t.Fatal("MATRIX_ADMIN_TOKEN missing from credential file")
	}
	userID := credentials["MATRIX_ADMIN_USER"]
	if userID == "" {
		t.Fatal("MATRIX_ADMIN_USER missing from credential file")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	session, err := client.SessionFromToken(userID, token)
	if err != nil {
		t.Fatalf("session from token: %v", err)
	}
	return session
}

// loadCredentials reads the key=value credential file written by setup.
func loadCredentials(t *testing.T) map[string]string {
	t.Helper()

	file, err := os.Open(credentialFile)
	if err != nil {
		t.Fatalf("open credential file: %v", err)
	}
	defer file.Close()

	credentials := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found {
			credentials[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	return credentials
}

// --- Machine Fleet Tests ---

// TestMachineJoinsFleet verifies the complete machine bootstrap path:
// launcher registers with the homeserver and publishes its key, daemon
// starts, performs initial sync, creates the config room, and publishes
// a MachineStatus heartbeat. This proves the full launcher→daemon→Matrix
// lifecycle works end-to-end.
func TestMachineJoinsFleet(t *testing.T) {
	const machineName = "machine/test"
	machineUserID := "@machine/test:" + testServerName

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	// Resolve global rooms so we can invite the machine.
	machinesRoomID, err := admin.ResolveAlias(ctx, "#bureau/machines:"+testServerName)
	if err != nil {
		t.Fatalf("resolve machines room: %v", err)
	}
	servicesRoomID, err := admin.ResolveAlias(ctx, "#bureau/services:"+testServerName)
	if err != nil {
		t.Fatalf("resolve services room: %v", err)
	}

	// Pre-invite the machine user to global rooms. These rooms use the
	// private_chat preset (invite-only), so the machine needs an
	// invitation before it can join. In production, the admin does this
	// as part of fleet provisioning.
	if err := admin.InviteUser(ctx, machinesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to machines room: %v", err)
	}
	if err := admin.InviteUser(ctx, servicesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to services room: %v", err)
	}

	// Create isolated directories. socketDir uses /tmp for short paths
	// (Unix socket limit is 108 bytes; Bazel's TEST_TMPDIR is too deep).
	stateDir := t.TempDir()
	socketDir := tempSocketDir(t)

	launcherSocket := filepath.Join(socketDir, "launcher.sock")
	tmuxSocket := filepath.Join(socketDir, "tmux.sock")
	observeSocket := filepath.Join(socketDir, "observe.sock")
	relaySocket := filepath.Join(socketDir, "relay.sock")
	principalSocketBase := filepath.Join(socketDir, "p") + "/"
	adminSocketBase := filepath.Join(socketDir, "a") + "/"
	os.MkdirAll(principalSocketBase, 0755)
	os.MkdirAll(adminSocketBase, 0755)

	// Write the registration token to a file for the launcher.
	tokenFile := filepath.Join(stateDir, "reg-token")
	if err := os.WriteFile(tokenFile, []byte(testRegistrationToken), 0600); err != nil {
		t.Fatalf("write registration token: %v", err)
	}

	launcherWorkspaceRoot := filepath.Join(stateDir, "workspace")

	// Start the launcher (first boot). It generates a keypair, registers
	// a Matrix account, publishes the machine key, saves the session, and
	// listens on the IPC socket.
	startProcess(t, "launcher", launcherBinary,
		"--homeserver", testHomeserverURL,
		"--registration-token-file", tokenFile,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--socket", launcherSocket,
		"--tmux-socket", tmuxSocket,
		"--socket-base-path", principalSocketBase,
		"--admin-base-path", adminSocketBase,
		"--workspace-root", launcherWorkspaceRoot,
	)

	// Wait for the launcher's IPC socket — its appearance signals that
	// first-boot setup (registration, key publication) is complete and
	// the launcher is listening for daemon requests.
	waitForFile(t, launcherSocket, 15*time.Second)

	// Verify the machine key was published to #bureau/machines.
	machineKeyJSON := waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_key", machineName, 10*time.Second)

	var machineKey struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}
	if machineKey.Algorithm != "age-x25519" {
		t.Errorf("machine key algorithm = %q, want age-x25519", machineKey.Algorithm)
	}
	if machineKey.PublicKey == "" {
		t.Error("machine key has empty public key")
	}

	// Start the daemon. It loads the session saved by the launcher,
	// ensures the per-machine config room, joins global rooms, does
	// an initial Matrix sync, and publishes a MachineStatus heartbeat.
	startProcess(t, "daemon", daemonBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--launcher-socket", launcherSocket,
		"--observe-socket", observeSocket,
		"--relay-socket", relaySocket,
		"--tmux-socket", tmuxSocket,
		"--admin-user", "bureau-admin",
		"--admin-base-path", adminSocketBase,
	)

	// Wait for the MachineStatus heartbeat in #bureau/machines. The
	// daemon publishes immediately on startup, so this also serves as
	// a readiness signal.
	statusJSON := waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_status", machineName, 15*time.Second)

	var status struct {
		Principal string `json:"principal"`
		Sandboxes struct {
			Running int `json:"running"`
		} `json:"sandboxes"`
		UptimeSeconds int64 `json:"uptime_seconds"`
	}
	if err := json.Unmarshal(statusJSON, &status); err != nil {
		t.Fatalf("unmarshal machine status: %v", err)
	}
	if status.Principal != machineUserID {
		t.Errorf("machine status principal = %q, want %q", status.Principal, machineUserID)
	}
	if status.Sandboxes.Running != 0 {
		t.Errorf("expected 0 running sandboxes, got %d", status.Sandboxes.Running)
	}
	if status.UptimeSeconds == 0 {
		t.Error("machine status has zero uptime")
	}

	// Verify the per-machine config room was created by the daemon.
	configAlias := "#bureau/config/" + machineName + ":" + testServerName
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room %s not created: %v", configAlias, err)
	}
	if configRoomID == "" {
		t.Fatal("config room resolved to empty room ID")
	}

	// Verify the admin was invited to the config room (the daemon's
	// ensureConfigRoom invites the admin user so the admin can write
	// MachineConfig and Credentials state events).
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin could not join config room: %v", err)
	}

	// Verify config room power levels: admin should have level 100.
	powerLevelJSON, err := admin.GetStateEvent(ctx, configRoomID, "m.room.power_levels", "")
	if err != nil {
		t.Fatalf("get config room power levels: %v", err)
	}
	var powerLevels struct {
		Users map[string]int `json:"users"`
	}
	if err := json.Unmarshal(powerLevelJSON, &powerLevels); err != nil {
		t.Fatalf("unmarshal power levels: %v", err)
	}
	adminUserID := "@bureau-admin:" + testServerName
	if level, ok := powerLevels.Users[adminUserID]; !ok || level != 100 {
		t.Errorf("admin power level = %d (present=%v), want 100", level, ok)
	}
	if level, ok := powerLevels.Users[machineUserID]; !ok || level != 50 {
		t.Errorf("machine power level = %d (present=%v), want 50", level, ok)
	}
}

// TestPrincipalAssignment verifies the full sandbox lifecycle: admin writes
// a MachineConfig assigning a principal, the daemon reconciles, the launcher
// creates a proxy, and the proxy correctly serves the principal's Matrix
// identity. This proves credential encryption, IPC, proxy spawning, and
// reconciliation work end-to-end.
func TestPrincipalAssignment(t *testing.T) {
	const machineName = "machine/sandbox"
	const principalLocalpart = "test/agent"
	machineUserID := "@machine/sandbox:" + testServerName
	principalUserID := "@test/agent:" + testServerName

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	// Resolve global rooms and invite the machine.
	machinesRoomID, err := admin.ResolveAlias(ctx, "#bureau/machines:"+testServerName)
	if err != nil {
		t.Fatalf("resolve machines room: %v", err)
	}
	servicesRoomID, err := admin.ResolveAlias(ctx, "#bureau/services:"+testServerName)
	if err != nil {
		t.Fatalf("resolve services room: %v", err)
	}

	if err := admin.InviteUser(ctx, machinesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to machines room: %v", err)
	}
	if err := admin.InviteUser(ctx, servicesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to services room: %v", err)
	}

	// Create isolated directories.
	stateDir := t.TempDir()
	socketDir := tempSocketDir(t)

	launcherSocket := filepath.Join(socketDir, "launcher.sock")
	tmuxSocket := filepath.Join(socketDir, "tmux.sock")
	observeSocket := filepath.Join(socketDir, "observe.sock")
	relaySocket := filepath.Join(socketDir, "relay.sock")
	principalSocketBase := filepath.Join(socketDir, "p") + "/"
	adminSocketBase := filepath.Join(socketDir, "a") + "/"
	os.MkdirAll(principalSocketBase, 0755)
	os.MkdirAll(adminSocketBase, 0755)

	tokenFile := filepath.Join(stateDir, "reg-token")
	if err := os.WriteFile(tokenFile, []byte(testRegistrationToken), 0600); err != nil {
		t.Fatalf("write registration token: %v", err)
	}

	launcherWorkspaceRoot := filepath.Join(stateDir, "workspace")

	// Start the launcher with --proxy-binary so it can spawn proxies.
	startProcess(t, "launcher", launcherBinary,
		"--homeserver", testHomeserverURL,
		"--registration-token-file", tokenFile,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--socket", launcherSocket,
		"--tmux-socket", tmuxSocket,
		"--socket-base-path", principalSocketBase,
		"--admin-base-path", adminSocketBase,
		"--workspace-root", launcherWorkspaceRoot,
		"--proxy-binary", proxyBinary,
	)

	waitForFile(t, launcherSocket, 15*time.Second)

	// Get the machine's public key (needed to encrypt credentials).
	machineKeyJSON := waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_key", machineName, 10*time.Second)
	var machineKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}
	if machineKey.PublicKey == "" {
		t.Fatal("machine key has empty public key")
	}

	// Start the daemon.
	startProcess(t, "daemon", daemonBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--launcher-socket", launcherSocket,
		"--observe-socket", observeSocket,
		"--relay-socket", relaySocket,
		"--tmux-socket", tmuxSocket,
		"--admin-user", "bureau-admin",
		"--admin-base-path", adminSocketBase,
		"--status-interval", "2s",
	)

	// Wait for daemon readiness (MachineStatus heartbeat).
	waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_status", machineName, 15*time.Second)

	// Resolve the config room created by the daemon.
	configAlias := "#bureau/config/" + machineName + ":" + testServerName
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room %s not created: %v", configAlias, err)
	}

	// Admin joins the config room so it can write state events.
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}

	// --- Register the principal's Matrix account ---
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal registration: %v", err)
	}

	principalSession, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          principalLocalpart,
		Password:          "test-principal-password",
		RegistrationToken: testRegistrationToken,
	})
	if err != nil {
		t.Fatalf("register principal %q: %v", principalLocalpart, err)
	}
	principalToken := principalSession.AccessToken()
	principalSession.Close()

	// --- Encrypt credentials for the principal ---
	credentialBundle := map[string]string{
		"MATRIX_TOKEN":          principalToken,
		"MATRIX_USER_ID":        principalUserID,
		"MATRIX_HOMESERVER_URL": testHomeserverURL,
	}
	credentialJSON, err := json.Marshal(credentialBundle)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}

	ciphertext, err := sealed.Encrypt(credentialJSON, []string{machineKey.PublicKey})
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}

	// --- Push Credentials state event ---
	_, err = admin.SendStateEvent(ctx, configRoomID, "m.bureau.credentials", principalLocalpart, map[string]any{
		"version":        1,
		"principal":      principalUserID,
		"encrypted_for":  []string{machineUserID},
		"keys":           []string{"MATRIX_TOKEN", "MATRIX_USER_ID", "MATRIX_HOMESERVER_URL"},
		"ciphertext":     ciphertext,
		"provisioned_by": "@bureau-admin:" + testServerName,
		"provisioned_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("push credentials: %v", err)
	}

	// --- Push MachineConfig state event ---
	_, err = admin.SendStateEvent(ctx, configRoomID, "m.bureau.machine_config", machineName, map[string]any{
		"principals": []map[string]any{
			{
				"localpart":  principalLocalpart,
				"template":   "",
				"auto_start": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// --- Wait for the proxy socket to appear ---
	// The daemon's sync loop picks up the config change, reconciles, and
	// sends create-sandbox IPC to the launcher. The launcher decrypts the
	// credentials, spawns the proxy, and creates a tmux session. The proxy
	// socket appearance signals that the full lifecycle completed.
	proxySocket := principalSocketBase + principalLocalpart + ".sock"
	waitForFile(t, proxySocket, 15*time.Second)

	// --- Verify the proxy serves the principal's identity ---
	// Send an HTTP request through the proxy's Unix socket to the Matrix
	// whoami endpoint. The proxy injects the principal's access token.
	proxyHTTPClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("unix", proxySocket)
			},
		},
		Timeout: 5 * time.Second,
	}

	whoamiResponse, err := proxyHTTPClient.Get("http://proxy/http/matrix/_matrix/client/v3/account/whoami")
	if err != nil {
		t.Fatalf("whoami through proxy: %v", err)
	}
	defer whoamiResponse.Body.Close()

	if whoamiResponse.StatusCode != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200", whoamiResponse.StatusCode)
	}

	var whoami struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(whoamiResponse.Body).Decode(&whoami); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if whoami.UserID != principalUserID {
		t.Errorf("whoami user_id = %q, want %q", whoami.UserID, principalUserID)
	}

	// --- Verify MachineStatus reflects the running sandbox ---
	// Poll for updated MachineStatus showing 1 running sandbox. The daemon
	// publishes a new heartbeat on each status tick after reconciliation.
	deadline := time.Now().Add(15 * time.Second)
	for {
		statusJSON, err := admin.GetStateEvent(ctx, machinesRoomID,
			"m.bureau.machine_status", machineName)
		if err == nil {
			var status struct {
				Sandboxes struct {
					Running int `json:"running"`
				} `json:"sandboxes"`
			}
			if err := json.Unmarshal(statusJSON, &status); err == nil && status.Sandboxes.Running > 0 {
				t.Logf("machine status shows %d running sandbox(es)", status.Sandboxes.Running)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for MachineStatus to show running sandboxes")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// --- Fleet Test Helpers ---

// resolvedBinary resolves a binary path from a Bazel environment variable.
// Skips the test if the binary is not available (allows running the Matrix
// setup tests without the launcher/daemon binaries).
func resolvedBinary(t *testing.T, envVar string) string {
	t.Helper()

	rlocationPath := os.Getenv(envVar)
	if rlocationPath == "" {
		t.Skipf("%s not set (run via Bazel to test machine lifecycle)", envVar)
	}

	runfilesDirectory := os.Getenv("RUNFILES_DIR")
	if runfilesDirectory == "" {
		t.Skipf("%s set but RUNFILES_DIR missing", envVar)
	}

	absolutePath := filepath.Join(runfilesDirectory, rlocationPath)
	if _, err := os.Stat(absolutePath); err != nil {
		t.Skipf("binary not found at %s: %v", absolutePath, err)
	}
	return absolutePath
}

// startProcess starts a binary as a subprocess, wiring its output to the
// test log. Registers a cleanup function that sends SIGTERM and waits for
// the process to exit (with a 5-second SIGKILL fallback). Cleanup runs in
// LIFO order, so starting the daemon after the launcher ensures the daemon
// is stopped first.
func startProcess(t *testing.T, name, binary string, args ...string) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}

	t.Logf("%s started (pid %d)", name, cmd.Process.Pid)

	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			t.Logf("%s stopped", name)
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Logf("%s killed after timeout", name)
		}
	})
}

// tempSocketDir creates a short-named temporary directory under /tmp for
// Unix sockets. Bazel's TEST_TMPDIR paths are too deep for the 108-byte
// Unix socket name limit.
func tempSocketDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "bureau-it-")
	if err != nil {
		t.Fatalf("create socket temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return directory
}

// waitForFile polls until a file exists on disk.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for file: %s", timeout, path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForStateEvent polls a Matrix room for a state event until it appears
// or the timeout expires. Returns the raw JSON content of the event.
func waitForStateEvent(t *testing.T, session *messaging.Session, roomID, eventType, stateKey string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		content, err := session.GetStateEvent(t.Context(), roomID, eventType, stateKey)
		if err == nil {
			return content
		}
		lastError = err
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for state event %s/%s in room %s: %v",
				timeout, eventType, stateKey, roomID, lastError)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
