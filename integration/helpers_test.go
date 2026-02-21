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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

const (
	testServerName        = "test.bureau.local"
	testRegistrationToken = "test-registration-token"
)

// testServer is the parsed ref.ServerName form of testServerName for use
// with library functions that require typed server names.
var testServer = ref.MustParseServerName(testServerName)

// testNamespace is the Bureau namespace shared by all integration tests.
// Individual tests create per-test fleets under this namespace via
// createTestFleet.
var testNamespace = func() ref.Namespace {
	namespace, err := ref.NewNamespace(testServer, "bureau")
	if err != nil {
		panic(err)
	}
	return namespace
}()

var (
	// testHomeserverURL is set in TestMain after Docker Compose allocates
	// a random host port for the Continuwuity container. Each test
	// invocation gets its own port, allowing parallel runs.
	testHomeserverURL string

	// composeProjectName is unique per process so that concurrent test
	// runs (e.g., --runs_per_test=N) don't collide on Docker resources.
	composeProjectName = fmt.Sprintf("bureau-test-%d", os.Getpid())
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

	// Credential file goes in a temp directory so parallel runs don't
	// collide on the same file path.
	credentialDirectory, err := os.MkdirTemp("", "bureau-it-creds-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create credential temp dir: %v\n", err)
		os.Exit(1)
	}
	credentialFile = filepath.Join(credentialDirectory, "bureau-creds")

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

	// Clean up leaked Docker resources from previous interrupted runs.
	// When Bazel kills a test process (timeout, user cancel), the
	// cleanup code after m.Run() never executes, leaving orphaned
	// containers, networks, and volumes. Each leaked stack consumes a
	// Docker bridge network from the finite address pool. Without this
	// sweep, repeated interrupted runs exhaust all network addresses.
	cleanupLeakedDockerResources()

	// Install a signal handler so that SIGTERM (sent by Bazel on
	// timeout) triggers docker compose cleanup before the process
	// exits. Without this, the stack leaks when Bazel kills us.
	installComposeCleanupSignalHandler()

	if err := dockerCompose("up", "-d"); err != nil {
		fmt.Fprintf(os.Stderr, "compose up failed: %v\n", err)
		os.Exit(1)
	}

	// Discover the dynamically allocated host port. The compose file
	// binds 127.0.0.1:0:6167, so Docker picks a free port. We query
	// it via `docker compose port` which outputs "127.0.0.1:<port>".
	hostAddress, err := dockerComposeOutput("port", "matrix", "6167")
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover allocated port: %v\n", err)
		_ = dockerCompose("down", "-v")
		os.Exit(1)
	}
	testHomeserverURL = "http://" + strings.TrimSpace(hostAddress)

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
	_ = os.RemoveAll(credentialDirectory)
	os.Exit(code)
}

// --- Infrastructure Helpers ---

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

// dockerComposeOutput runs docker compose and returns its stdout as a string.
// Stderr goes to os.Stderr for diagnostic visibility.
func dockerComposeOutput(args ...string) (string, error) {
	composeFile := filepath.Join(workspaceRoot, "deploy", "test", "docker-compose.yaml")
	fullArgs := []string{
		"compose",
		"-f", composeFile,
		"-p", composeProjectName,
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.Command("docker", fullArgs...)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	return string(output), err
}

// cleanupLeakedDockerResources removes containers, networks, and volumes
// from previous integration test runs that were killed before their cleanup
// code could execute. Only targets resources from processes that are no
// longer running — resources from live sibling test processes are left
// alone.
//
// The project name format is "bureau-test-<pid>", so we extract the PID
// from each resource name and check if that process is still alive.
func cleanupLeakedDockerResources() {
	// Identify orphaned project names by listing containers with
	// bureau-test- in their name, extracting the PID from the project
	// name, and checking if the process is still alive.
	orphanedProjects := findOrphanedComposeProjects()
	if len(orphanedProjects) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "cleaning up %d leaked test stacks from dead processes\n", len(orphanedProjects))

	for _, project := range orphanedProjects {
		// Use docker compose down -v to clean up the full stack
		// for each orphaned project. This handles containers,
		// networks, and volumes in the correct order.
		composeFile := filepath.Join(workspaceRoot, "deploy", "test", "docker-compose.yaml")
		cmd := exec.Command("docker", "compose", "-f", composeFile, "-p", project, "down", "-v")
		_ = cmd.Run()
	}
}

// findOrphanedComposeProjects returns compose project names from dead
// test processes. Lists all bureau-test-* containers, extracts the PID
// from each project name, and returns projects whose PID is no longer
// alive.
func findOrphanedComposeProjects() []string {
	cmd := exec.Command("docker", "ps", "-a",
		"--filter", "name=bureau-test",
		"--format", "{{.Labels}}")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	// Extract unique project names from container labels.
	// Docker compose sets com.docker.compose.project=<name>.
	projectSet := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		for _, label := range strings.Split(line, ",") {
			if strings.HasPrefix(label, "com.docker.compose.project=") {
				project := strings.TrimPrefix(label, "com.docker.compose.project=")
				projectSet[project] = true
			}
		}
	}

	// Check each project's PID. The project name format is
	// "bureau-test-<pid>".
	var orphaned []string
	for project := range projectSet {
		pidString := strings.TrimPrefix(project, "bureau-test-")
		if pidString == project {
			continue // not our naming pattern
		}
		pid, err := strconv.Atoi(pidString)
		if err != nil {
			continue
		}

		// Signal 0 checks process existence without sending a
		// signal. If the process is gone, the stack is orphaned.
		process, err := os.FindProcess(pid)
		if err != nil {
			orphaned = append(orphaned, project)
			continue
		}
		if process.Signal(syscall.Signal(0)) != nil {
			orphaned = append(orphaned, project)
		}
	}
	return orphaned
}

// installComposeCleanupSignalHandler starts a goroutine that catches
// SIGTERM and runs `docker compose down -v` before exiting. Bazel
// sends SIGTERM when a test times out, then SIGKILL after a grace
// period. This handler ensures the compose stack is torn down during
// that grace window.
func installComposeCleanupSignalHandler() {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGTERM)
	go func() {
		<-signalChannel
		fmt.Fprintln(os.Stderr, "SIGTERM received, cleaning up docker compose stack...")
		_ = dockerCompose("down", "-v")
		os.Exit(1)
	}()
}

// waitForHealthy polls the Continuwuity versions endpoint until it responds.
func waitForHealthy(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("timeout after %s waiting for %s", timeout, testHomeserverURL)
		}
		response, err := http.Get(testHomeserverURL + "/_matrix/client/versions")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		runtime.Gosched()
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
func adminSession(t *testing.T) *messaging.DirectSession {
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
	userIDRaw := credentials["MATRIX_ADMIN_USER"]
	if userIDRaw == "" {
		t.Fatal("MATRIX_ADMIN_USER missing from credential file")
	}
	userID, err := ref.ParseUserID(userIDRaw)
	if err != nil {
		t.Fatalf("parse admin user ID %q: %v", userIDRaw, err)
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

// --- Fleet Test Helpers ---

// testFleet holds the fleet-scoped rooms for a single test. Every test
// that uses a daemon or fleet controller gets its own fleet to prevent
// cross-contamination through shared rooms.
type testFleet struct {
	Ref           ref.Fleet  // typed reference (carries namespace, fleet name, server)
	Prefix        string     // e.g., "bureau/fleet/TestSomeTest"
	FleetRoomID   ref.RoomID // fleet config room
	MachineRoomID ref.RoomID // fleet machine presence room
	ServiceRoomID ref.RoomID // fleet service directory room
}

// createTestFleet creates the three fleet-scoped rooms (config, machine,
// service) with proper aliases so daemons and fleet controllers can
// resolve them. The fleet prefix is derived from the test name.
func createTestFleet(t *testing.T, admin *messaging.DirectSession) *testFleet {
	t.Helper()
	ctx := t.Context()

	// Derive a fleet prefix from the test name. The homeserver is fresh
	// per test run, so t.Name() is sufficient for uniqueness. Lowercased
	// because Matrix localparts only allow a-z, and ref.ParseFleet
	// validates this constraint.
	fleetName := strings.ToLower(t.Name())
	namespace := "bureau"

	namespaceRef, err := ref.NewNamespace(testServer, namespace)
	if err != nil {
		t.Fatalf("create namespace ref: %v", err)
	}
	fleetRef, err := ref.NewFleet(namespaceRef, fleetName)
	if err != nil {
		t.Fatalf("create fleet ref: %v", err)
	}
	prefix := fleetRef.Localpart()

	// Create the three fleet-scoped rooms with aliases. The daemon and
	// fleet controller resolve these aliases at startup.
	fleetAlias := fleetRef.Localpart()
	machineAlias := fleetRef.MachineRoomAliasLocalpart()
	serviceAlias := fleetRef.ServiceRoomAliasLocalpart()

	fleetRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Preset:                    "private_chat",
		Alias:                     fleetAlias,
		PowerLevelContentOverride: schema.FleetRoomPowerLevels(admin.UserID()),
	})
	if err != nil {
		t.Fatalf("create fleet room: %v", err)
	}

	machineRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Preset:                    "private_chat",
		Alias:                     machineAlias,
		PowerLevelContentOverride: schema.MachineRoomPowerLevels(admin.UserID()),
	})
	if err != nil {
		t.Fatalf("create fleet machine room: %v", err)
	}

	serviceRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Preset:                    "private_chat",
		Alias:                     serviceAlias,
		PowerLevelContentOverride: schema.ServiceRoomPowerLevels(admin.UserID()),
	})
	if err != nil {
		t.Fatalf("create fleet service room: %v", err)
	}

	return &testFleet{
		Ref:           fleetRef,
		Prefix:        prefix,
		FleetRoomID:   fleetRoom.RoomID,
		MachineRoomID: machineRoom.RoomID,
		ServiceRoomID: serviceRoom.RoomID,
	}
}

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

	if binary == "" {
		t.Fatal("binary path is required")
	}

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
		testutil.RequireReceive(t, done, 5*time.Second,
			fmt.Sprintf("%s did not exit after SIGTERM", name))
		t.Logf("%s stopped", name)
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

// waitForFile blocks until a file exists on disk. Uses inotify for
// deterministic, event-driven detection — zero CPU cost while waiting.
// Bounded by t.Context() (the test's own deadline), not an arbitrary
// intermediate timeout.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("path is required")
	}
	if err := inotifyWaitCreate(t.Context(), path); err != nil {
		t.Fatalf("waiting for file %s: %v", path, err)
	}
}

// waitForFileGone blocks until a file no longer exists on disk. Uses
// inotify for deterministic, event-driven detection — zero CPU cost
// while waiting. Bounded by t.Context().
func waitForFileGone(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("path is required")
	}
	if err := inotifyWaitDelete(t.Context(), path); err != nil {
		t.Fatalf("waiting for file to disappear %s: %v", path, err)
	}
}

// --- Command Result Classification Helpers ---

// findAcceptedEvent searches command result events for the "accepted"
// acknowledgment posted by async daemon handlers (worktree.add, worktree.remove,
// pipeline.execute). The accepted event has content["result"]["status"] == "accepted".
// Returns the full event content map. Fatals if no accepted event is found.
func findAcceptedEvent(t *testing.T, events []messaging.Event) map[string]any {
	t.Helper()

	for _, event := range events {
		resultField, _ := event.Content["result"].(map[string]any)
		if resultField == nil {
			continue
		}
		status, _ := resultField["status"].(string)
		if status == "accepted" {
			return event.Content
		}
	}

	t.Fatalf("accepted event not found among %d command results", len(events))
	return nil
}

// findPipelineEvent searches command result events for the pipeline execution
// result. Pipeline results are distinguished from the "accepted" ack by the
// presence of an "exit_code" field in the content. Returns the full event
// content map. Fatals if no pipeline event is found.
func findPipelineEvent(t *testing.T, events []messaging.Event) map[string]any {
	t.Helper()

	for _, event := range events {
		if _, hasExitCode := event.Content["exit_code"]; hasExitCode {
			return event.Content
		}
	}

	t.Fatalf("pipeline event not found among %d command results", len(events))
	return nil
}

// --- Pipeline Test Helpers ---

// runnerEnvOnce caches the result of the single nix build invocation so
// parallel tests don't each spawn their own nix build process. Under
// --runs_per_test=10 with 4 pipeline-using tests per process, the
// uncached version created 40 concurrent nix build calls that contended
// on the Nix daemon's store lock.
var (
	runnerEnvOnce       sync.Once
	runnerEnvPath       string
	runnerEnvSkipReason string // non-empty means skip (Nix unavailable or build failed)
	runnerEnvFatalMsg   string // non-empty means fatal (broken Nix derivation)
)

// findRunnerEnv builds the Nix integration-test-env and returns the store
// path. The integration-test-env provides a shell and coreutils needed by
// sandbox commands (pipeline steps, workspace scripts) inside bwrap sandboxes.
// Production environments come from the environment repo; this derivation
// provides them for integration tests without pulling in the full repo.
// Skips the test if Nix is not installed or the build fails.
func findRunnerEnv(t *testing.T) string {
	t.Helper()

	runnerEnvOnce.Do(func() {
		// The nix binary is at a well-known path, not necessarily in PATH
		// when running under Bazel.
		nixBinary := "/nix/var/nix/profiles/default/bin/nix"
		if _, err := os.Stat(nixBinary); err != nil {
			runnerEnvSkipReason = "nix not available: integration tests requiring sandbox environments need Nix"
			return
		}

		cmd := exec.Command(nixBinary, "build", ".#integration-test-env",
			"--print-out-paths", "--no-link")
		cmd.Dir = workspaceRoot
		output, err := cmd.Output()
		if err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				runnerEnvSkipReason = fmt.Sprintf("nix build .#integration-test-env failed: %v\nstderr: %s", err, exitError.Stderr)
			} else {
				runnerEnvSkipReason = fmt.Sprintf("nix build .#integration-test-env failed: %v", err)
			}
			return
		}

		storePath := strings.TrimSpace(string(output))
		if storePath == "" {
			runnerEnvSkipReason = "nix build .#integration-test-env produced empty output"
			return
		}

		// Verify the store path has the expected bin directory with a shell.
		// These are fatal (not skip) because a successfully-built derivation
		// missing its binaries indicates a broken flake output.
		binDirectory := filepath.Join(storePath, "bin")
		if _, err := os.Stat(binDirectory); err != nil {
			runnerEnvFatalMsg = fmt.Sprintf("integration-test-env bin directory missing at %s: %v", binDirectory, err)
			return
		}
		if _, err := os.Stat(filepath.Join(binDirectory, "sh")); err != nil {
			runnerEnvFatalMsg = fmt.Sprintf("integration-test-env missing sh: %v", err)
			return
		}

		runnerEnvPath = storePath
	})

	if runnerEnvFatalMsg != "" {
		t.Fatal(runnerEnvFatalMsg)
	}
	if runnerEnvSkipReason != "" {
		t.Skip(runnerEnvSkipReason)
	}
	return runnerEnvPath
}

// initTestGitRepo creates a regular git repo with one commit on a "main"
// branch. Used as the seed for git clone --bare to create Bureau's .bare/
// directory layout. A regular repo is needed because git init --bare
// creates an empty repo with no HEAD, and most git operations (worktree
// add, fetch) require at least one commit.
func initTestGitRepo(t *testing.T, ctx context.Context, directory string) {
	t.Helper()

	for _, step := range []struct {
		args []string
		desc string
	}{
		{[]string{"init", "-b", "main", directory}, "git init"},
		{[]string{"-C", directory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", directory, "config", "user.email", "test@test"}, "git config user.email"},
	} {
		cmd := exec.CommandContext(ctx, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.desc, err)
		}
	}

	readmeFile := filepath.Join(directory, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test Project\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	for _, step := range []struct {
		args []string
		desc string
	}{
		{[]string{"-C", directory, "add", "README.md"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", "Initial commit"}, "git commit"},
	} {
		cmd := exec.CommandContext(ctx, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.desc, err)
		}
	}
}

// --- Room Watch (sync-based event waiting) ---

// roomWatch captures a position in the Matrix sync stream. Create one with
// watchRoom BEFORE triggering the action that generates the expected event,
// then call one of the Wait methods to receive events that arrive after the
// checkpoint.
//
// All Wait methods use Matrix /sync long-polling: the server holds the
// connection until new events arrive, then returns immediately. There is no
// client-side sleep or polling interval. Waits are bounded by t.Context()
// (test timeout).
type roomWatch struct {
	session   *messaging.DirectSession
	roomID    ref.RoomID
	nextBatch string            // sync token capturing the stream position at watch creation
	pending   []messaging.Event // events received from sync but not yet consumed by a Wait call
}

// watchRoom captures the current position in the Matrix sync stream. The
// returned roomWatch only sees events arriving after this call returns.
func watchRoom(t *testing.T, session *messaging.DirectSession, roomID ref.RoomID) roomWatch {
	t.Helper()
	if roomID.IsZero() {
		t.Fatal("roomID is required")
	}
	response, err := session.Sync(t.Context(), messaging.SyncOptions{
		SetTimeout: true,
		Timeout:    0, // immediate return with current state
	})
	if err != nil {
		t.Fatalf("watchRoom: initial sync: %v", err)
	}
	return roomWatch{
		session:   session,
		roomID:    roomID,
		nextBatch: response.NextBatch,
	}
}

// WaitForEvent blocks until an event matching the predicate arrives in the
// watched room. Events are buffered: when a /sync response delivers multiple
// events, all are stored in w.pending. The predicate scans pending events
// before issuing a new /sync, so events are never dropped when multiple
// matching events arrive in the same sync batch.
//
// The description appears in the fatal message if the test context expires.
func (w *roomWatch) WaitForEvent(t *testing.T, predicate func(messaging.Event) bool, description string) messaging.Event {
	t.Helper()
	ctx := t.Context()
	var syncRetries int
	for {
		// Scan pending events from previous sync responses before
		// issuing a new /sync. This is critical for correctness: when
		// a sync response contains multiple matching events (e.g., two
		// command_result messages in the same batch), the first
		// WaitForEvent call consumes one and the second call must find
		// the other here without needing another round-trip.
		for i, event := range w.pending {
			if predicate(event) {
				w.pending = append(w.pending[:i], w.pending[i+1:]...)
				return event
			}
		}

		// On retry after a sync error, use a short server-side
		// timeout (1s) so the HTTP round-trip itself provides
		// backoff. On first attempt or after success, use the
		// normal 30s long-poll hold.
		syncTimeout := 30000
		if syncRetries > 0 {
			syncTimeout = 1000
		}
		response, err := w.session.Sync(ctx, messaging.SyncOptions{
			Since:      w.nextBatch,
			SetTimeout: true,
			Timeout:    syncTimeout,
		})
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("test context cancelled waiting for event: %s (room %s)",
					description, w.roomID)
			}
			syncRetries++
			// TCP-level errors (connection reset, EOF) often indicate
			// a poisoned connection in Go's HTTP pool. Drop idle
			// connections so the next attempt opens a fresh socket.
			w.session.CloseIdleConnections()
			if syncRetries > 5 {
				t.Fatalf("sync failed %d consecutive times waiting for event: %s (room %s): %v",
					syncRetries, description, w.roomID, err)
			}
			t.Logf("roomWatch sync error (attempt %d/5, pool reset): %v", syncRetries, err)
			continue
		}
		syncRetries = 0
		w.nextBatch = response.NextBatch

		if joined, ok := response.Rooms.Join[w.roomID]; ok {
			w.pending = append(w.pending, joined.State.Events...)
			w.pending = append(w.pending, joined.Timeline.Events...)
		}
	}
}

// WaitForMessage blocks until a message from senderID containing bodyContains
// arrives in the watched room. Returns the full message body.
func (w *roomWatch) WaitForMessage(t *testing.T, bodyContains, senderID string) string {
	t.Helper()
	event := w.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.MatrixEventTypeMessage {
			return false
		}
		if event.Sender != senderID {
			return false
		}
		body, _ := event.Content["body"].(string)
		return strings.Contains(body, bodyContains)
	}, fmt.Sprintf("message containing %q from %s", bodyContains, senderID))
	body, _ := event.Content["body"].(string)
	return body
}

// WaitForStateEvent blocks until a state event with the given type and
// state_key arrives in the watched room. Returns the raw JSON content.
func (w *roomWatch) WaitForStateEvent(t *testing.T, eventType, stateKey string) json.RawMessage {
	t.Helper()
	event := w.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != eventType {
			return false
		}
		return event.StateKey != nil && *event.StateKey == stateKey
	}, fmt.Sprintf("state event %s/%s", eventType, stateKey))
	content, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal state event content: %v", err)
	}
	return content
}

// WaitForMachineStatus blocks until a MachineStatus state event for the given
// stateKey arrives and satisfies the predicate. Returns the decoded status.
func (w *roomWatch) WaitForMachineStatus(t *testing.T, stateKey string, predicate func(schema.MachineStatus) bool, description string) schema.MachineStatus {
	t.Helper()
	var status schema.MachineStatus
	w.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeMachineStatus {
			return false
		}
		if event.StateKey == nil || *event.StateKey != stateKey {
			return false
		}
		// Round-trip through JSON to decode Content (map[string]any) into
		// the typed struct.
		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			return false
		}
		var candidate schema.MachineStatus
		if json.Unmarshal(contentJSON, &candidate) != nil {
			return false
		}
		if predicate(candidate) {
			status = candidate
			return true
		}
		return false
	}, description)
	return status
}

// WaitForCommandResults blocks until at least count m.bureau.command_result
// messages with the given requestID arrive in the watched room. Returns all
// matching events collected across sync responses.
func (w *roomWatch) WaitForCommandResults(t *testing.T, requestID string, count int) []messaging.Event {
	t.Helper()
	var collected []messaging.Event
	for len(collected) < count {
		event := w.WaitForEvent(t, func(event messaging.Event) bool {
			if event.Type != schema.MatrixEventTypeMessage {
				return false
			}
			msgtype, _ := event.Content["msgtype"].(string)
			if msgtype != schema.MsgTypeCommandResult {
				return false
			}
			eventRequestID, _ := event.Content["request_id"].(string)
			if eventRequestID != requestID {
				return false
			}
			// Avoid re-matching events we already collected.
			for _, existing := range collected {
				if existing.EventID == event.EventID {
					return false
				}
			}
			return true
		}, fmt.Sprintf("command result %d/%d for request %s", len(collected)+1, count, requestID))
		collected = append(collected, event)
	}
	return collected
}

// waitForNotification blocks until a typed notification message with the
// given msgtype arrives from senderID in the watched room. Returns the
// decoded message struct. The predicate is optional — pass nil to accept
// the first matching message.
//
// This is the typed replacement for WaitForMessage. Tests match on struct
// fields instead of body substrings.
func waitForNotification[T any](t *testing.T, w *roomWatch, msgtype string, senderID string, predicate func(T) bool, description string) T {
	t.Helper()
	if msgtype == "" {
		t.Fatal("msgtype is required")
	}
	if senderID == "" {
		t.Fatal("senderID is required")
	}
	var result T
	w.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.MatrixEventTypeMessage {
			return false
		}
		if event.Sender != senderID {
			return false
		}
		eventMsgtype, _ := event.Content["msgtype"].(string)
		if eventMsgtype != msgtype {
			return false
		}
		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			return false
		}
		if err := json.Unmarshal(contentJSON, &result); err != nil {
			return false
		}
		return predicate == nil || predicate(result)
	}, description)
	return result
}

// --- Proxy Test Helpers ---

// proxyHTTPClient creates an HTTP client that connects through a proxy Unix
// socket. Requests to any hostname are routed to the proxy — the hostname
// in the URL is ignored (the proxy uses the path to route to services).
func proxyHTTPClient(socketPath string) *http.Client {
	if socketPath == "" {
		panic("proxyHTTPClient: socketPath is required")
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// proxyWhoami calls the Matrix whoami endpoint through a proxy and returns the
// authenticated user ID. This verifies that the proxy correctly injects the
// principal's access token.
func proxyWhoami(t *testing.T, client *http.Client) string {
	t.Helper()
	response, err := client.Get("http://proxy/v1/matrix/whoami")
	if err != nil {
		t.Fatalf("whoami request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("whoami status = %d: %s", response.StatusCode, body)
	}
	var result struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	return result.UserID
}

// proxyJoinRoom joins a room through a proxy by posting to the structured
// join endpoint. The proxy injects the principal's access token and checks
// authorization grants (a matrix/join grant must be present).
func proxyJoinRoom(t *testing.T, client *http.Client, roomID ref.RoomID) {
	t.Helper()
	if roomID.IsZero() {
		t.Fatal("roomID is required")
	}
	body, _ := json.Marshal(map[string]string{"room": roomID.String()})
	request, err := http.NewRequest("POST",
		"http://proxy/v1/matrix/join",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create join request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("join request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("join room %s: status %d: %s", roomID, response.StatusCode, responseBody)
	}
}

// proxyTryJoinRoom attempts to join a room through a proxy and returns the
// HTTP status code and response body. Unlike proxyJoinRoom, this does not
// fatal on non-200 responses — callers can assert on expected failures
// (e.g., grants blocking the join with 403 Forbidden).
func proxyTryJoinRoom(t *testing.T, client *http.Client, roomID ref.RoomID) (int, string) {
	t.Helper()
	if roomID.IsZero() {
		t.Fatal("roomID is required")
	}
	body, _ := json.Marshal(map[string]string{"room": roomID.String()})
	request, err := http.NewRequest("POST",
		"http://proxy/v1/matrix/join",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create join request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("join request connection error: %v", err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(responseBody)
}

// proxySendMessage sends a text message to a room through a proxy. Returns the
// event ID assigned by the homeserver.
func proxySendMessage(t *testing.T, client *http.Client, roomID ref.RoomID, body string) string {
	t.Helper()
	if roomID.IsZero() {
		t.Fatal("roomID is required")
	}
	requestBody, _ := json.Marshal(map[string]any{
		"room":       roomID.String(),
		"event_type": schema.MatrixEventTypeMessage,
		"content":    messaging.NewTextMessage(body),
	})
	request, err := http.NewRequest("POST", "http://proxy/v1/matrix/event", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create send request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send message request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("send message to %s: status %d: %s", roomID, response.StatusCode, responseBody)
	}
	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	return result.EventID
}

// proxySyncRoomTimeline performs an initial /sync through a proxy and returns
// the timeline events for the specified room. Uses timeout=0 for an immediate
// response with all current state.
func proxySyncRoomTimeline(t *testing.T, client *http.Client, roomID ref.RoomID) []messaging.Event {
	t.Helper()
	if roomID.IsZero() {
		t.Fatal("roomID is required")
	}
	response, err := client.Get("http://proxy/v1/matrix/sync?timeout=0")
	if err != nil {
		t.Fatalf("sync request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("sync status = %d: %s", response.StatusCode, body)
	}
	var syncResponse messaging.SyncResponse
	if err := json.NewDecoder(response.Body).Decode(&syncResponse); err != nil {
		t.Fatalf("decode sync response: %v", err)
	}
	joined, ok := syncResponse.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("room %s not in sync response (have %d joined rooms)", roomID, len(syncResponse.Rooms.Join))
	}
	return joined.Timeline.Events
}

// assertMessagePresent checks that an event list contains a message from the
// expected sender with the expected body text.
func assertMessagePresent(t *testing.T, events []messaging.Event, sender, expectedBody string) {
	t.Helper()
	for _, event := range events {
		if event.Type != schema.MatrixEventTypeMessage {
			continue
		}
		body, _ := event.Content["body"].(string)
		if event.Sender == sender && body == expectedBody {
			return
		}
	}
	t.Errorf("message from %s with body %q not found in %d events", sender, expectedBody, len(events))
	for _, event := range events {
		if event.Type == schema.MatrixEventTypeMessage {
			body, _ := event.Content["body"].(string)
			t.Logf("  event %s from %s: %q", event.EventID, event.Sender, body)
		}
	}
}

// --- Service Discovery Test Helpers ---

// serviceDirectoryEntry mirrors proxy.ServiceDirectoryEntry for test decoding.
// Defined locally to avoid importing the proxy package from integration tests.
type serviceDirectoryEntry struct {
	Localpart    string         `json:"localpart"`
	Principal    string         `json:"principal"`
	Machine      string         `json:"machine"`
	Protocol     string         `json:"protocol"`
	Description  string         `json:"description,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// proxyServiceDiscovery queries GET /v1/services on the proxy's agent socket
// with optional query parameters and returns the parsed service directory.
// The queryParams string is appended as-is after "?" (e.g., "protocol=http").
func proxyServiceDiscovery(t *testing.T, client *http.Client, queryParams string) []serviceDirectoryEntry {
	t.Helper()

	requestURL := "http://proxy/v1/services"
	if queryParams != "" {
		requestURL += "?" + queryParams
	}

	response, err := client.Get(requestURL)
	if err != nil {
		t.Fatalf("GET /v1/services: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET /v1/services status = %d: %s", response.StatusCode, body)
	}

	var entries []serviceDirectoryEntry
	if err := json.NewDecoder(response.Body).Decode(&entries); err != nil {
		t.Fatalf("decode service directory: %v", err)
	}
	return entries
}
