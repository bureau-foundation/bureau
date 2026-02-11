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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// waitForFileGone polls until a file no longer exists on disk. Used to
// verify that a proxy socket has been cleaned up after sandbox destruction.
func waitForFileGone(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for file to disappear: %s", timeout, path)
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

// --- Pipeline Test Helpers ---

// findRunnerEnv builds the Nix integration-test-env and returns the store
// path. The integration-test-env provides a shell and coreutils needed by
// sandbox commands (pipeline steps, workspace scripts) inside bwrap sandboxes.
// Production environments come from the environment repo; this derivation
// provides them for integration tests without pulling in the full repo.
// Skips the test if Nix is not installed or the build fails.
func findRunnerEnv(t *testing.T) string {
	t.Helper()

	// The nix binary is at a well-known path, not necessarily in PATH
	// when running under Bazel.
	nixBinary := "/nix/var/nix/profiles/default/bin/nix"
	if _, err := os.Stat(nixBinary); err != nil {
		t.Skip("nix not available: integration tests requiring sandbox environments need Nix")
	}

	cmd := exec.Command(nixBinary, "build", ".#integration-test-env",
		"--print-out-paths", "--no-link")
	cmd.Dir = workspaceRoot
	output, err := cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			t.Skipf("nix build .#integration-test-env failed: %v\nstderr: %s", err, exitError.Stderr)
		}
		t.Skipf("nix build .#integration-test-env failed: %v", err)
	}

	storePath := strings.TrimSpace(string(output))
	if storePath == "" {
		t.Skip("nix build .#integration-test-env produced empty output")
	}

	// Verify the store path has the expected bin directory with a shell.
	binDirectory := filepath.Join(storePath, "bin")
	if _, err := os.Stat(binDirectory); err != nil {
		t.Fatalf("integration-test-env bin directory missing at %s: %v", binDirectory, err)
	}
	if _, err := os.Stat(filepath.Join(binDirectory, "sh")); err != nil {
		t.Fatalf("integration-test-env missing sh: %v", err)
	}

	return storePath
}

// waitForCommandResults polls a room's timeline for m.bureau.command_result
// events with a matching request_id. Returns when at least count matching
// events are found or the timeout expires.
func waitForCommandResults(t *testing.T, session *messaging.Session, roomID, requestID string, count int, timeout time.Duration) []messaging.Event {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		response, err := session.RoomMessages(t.Context(), roomID, messaging.RoomMessagesOptions{
			Direction: "b",
			Limit:     50,
		})
		if err != nil {
			t.Fatalf("RoomMessages for room %s: %v", roomID, err)
		}

		var matching []messaging.Event
		for _, event := range response.Chunk {
			if event.Type != "m.room.message" {
				continue
			}
			msgtype, _ := event.Content["msgtype"].(string)
			if msgtype != "m.bureau.command_result" {
				continue
			}
			eventRequestID, _ := event.Content["request_id"].(string)
			if eventRequestID != requestID {
				continue
			}
			matching = append(matching, event)
		}

		if len(matching) >= count {
			return matching
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %d command results with request_id %q in room %s (found %d)",
				timeout, count, requestID, roomID, len(matching))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// --- Proxy Test Helpers ---

// proxyHTTPClient creates an HTTP client that connects through a proxy Unix
// socket. Requests to any hostname are routed to the proxy — the hostname
// in the URL is ignored (the proxy uses the path to route to services).
func proxyHTTPClient(socketPath string) *http.Client {
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
	response, err := client.Get("http://proxy/http/matrix/_matrix/client/v3/account/whoami")
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

// proxyJoinRoom joins a room through a proxy by posting to the Matrix join
// endpoint. The proxy injects the principal's access token and checks
// MatrixPolicy (AllowJoin must be true).
func proxyJoinRoom(t *testing.T, client *http.Client, roomID string) {
	t.Helper()
	request, err := http.NewRequest("POST",
		"http://proxy/http/matrix/_matrix/client/v3/join/"+url.PathEscape(roomID),
		strings.NewReader("{}"))
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
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("join room %s: status %d: %s", roomID, response.StatusCode, body)
	}
}

// proxySendMessage sends a text message to a room through a proxy. Returns the
// event ID assigned by the homeserver.
func proxySendMessage(t *testing.T, client *http.Client, roomID, body string) string {
	t.Helper()
	transactionID := fmt.Sprintf("txn-%d", time.Now().UnixNano())
	messageJSON, _ := json.Marshal(messaging.NewTextMessage(body))
	requestURL := fmt.Sprintf("http://proxy/http/matrix/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(roomID), url.PathEscape(transactionID))
	request, err := http.NewRequest("PUT", requestURL, strings.NewReader(string(messageJSON)))
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
func proxySyncRoomTimeline(t *testing.T, client *http.Client, roomID string) []messaging.Event {
	t.Helper()
	response, err := client.Get("http://proxy/http/matrix/_matrix/client/v3/sync?timeout=0")
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
		if event.Type != "m.room.message" {
			continue
		}
		body, _ := event.Content["body"].(string)
		if event.Sender == sender && body == expectedBody {
			return
		}
	}
	t.Errorf("message from %s with body %q not found in %d events", sender, expectedBody, len(events))
	for _, event := range events {
		if event.Type == "m.room.message" {
			body, _ := event.Content["body"].(string)
			t.Logf("  event %s from %s: %q", event.EventID, event.Sender, body)
		}
	}
}
