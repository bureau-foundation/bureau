// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestDaemonLauncherIntegration exercises the full daemon → launcher → proxy
// composition: the daemon reads machine config from a mock Matrix server,
// sends IPC requests to a real launcher subprocess, and the launcher spawns
// real proxy processes. This verifies that the three components compose
// correctly — matching IPC wire formats, credential flow, socket paths, and
// process lifecycle management.
//
// The test runs three phases:
//   - Phase 1: Config assigns a principal → daemon creates sandbox → proxy is running
//   - Phase 2: Config removes the principal → daemon destroys sandbox → proxy is gone
//   - Phase 3: Config re-adds the principal → daemon recreates sandbox → proxy works again
func TestDaemonLauncherIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries and manages subprocesses")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available (sandbox creation requires namespace support)")
	}

	// Build both binaries.
	proxyBinary := buildBinary(t, "./cmd/bureau-proxy")
	launcherBinary := buildBinary(t, "./cmd/bureau-launcher")

	// Generate a machine keypair (the launcher decrypts credentials with this).
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	defer keypair.Close()

	// Prepare the launcher's state directory with the keypair and a session.
	// Having the keypair files present tells the launcher this isn't first boot,
	// so it skips registration and just loads the session.
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.txt"), keypair.PrivateKey.Bytes(), 0600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.pub"), []byte(keypair.PublicKey), 0644); err != nil {
		t.Fatalf("writing public key: %v", err)
	}

	// All runtime sockets live under a short temp directory (--run-dir)
	// to stay within the Unix domain socket 108-byte path limit.
	runDir := testutil.SocketDir(t)
	launcherSocket := principal.LauncherSocketPath(runDir)

	// Set up the mock Matrix server.
	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	// Room IDs used throughout the test. The mock matches on these exactly
	// (after URL-decoding the request path).
	const (
		configRoomID  = "!config:test"
		machineRoomID = "!machine:test"
		serviceRoomID = "!service:test"
	)

	// Configure mock Matrix: assign one principal with AutoStart.
	// The template reference "templates:echo" maps to room alias
	// #templates:bureau.local, template state key "echo". This exercises
	// the full template resolution pipeline during reconcile.
	const templateRoomID = "!templates:test"
	principalLocalpart := "test/echo"
	matrixState.setRoomAlias("#templates:bureau.local", templateRoomID)
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "echo", schema.TemplateContent{
		Description: "Echo test template",
		Command:     []string{"/bin/echo", "hello"},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: principalLocalpart,
			Template:  "templates:echo",
			AutoStart: true,
		}},
	})

	// Encrypt credentials for the test principal. The launcher will decrypt
	// these with its keypair and pipe them to the proxy's stdin.
	credentialMap := map[string]string{
		"MATRIX_TOKEN":          "syt_test_echo_token",
		"MATRIX_HOMESERVER_URL": matrixServer.URL,
		"MATRIX_USER_ID":        "@test/echo:bureau.local",
		"API_KEY":               "test-api-key-value",
	}
	credentialJSON, err := json.Marshal(credentialMap)
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}
	ciphertext, err := sealed.Encrypt(credentialJSON, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}

	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, principalLocalpart, schema.Credentials{
		Version:       1,
		Principal:     "@test/echo:bureau.local",
		EncryptedFor:  []string{"@machine/test:bureau.local"},
		Keys:          []string{"MATRIX_TOKEN", "MATRIX_HOMESERVER_URL", "MATRIX_USER_ID", "API_KEY"},
		Ciphertext:    ciphertext,
		ProvisionedBy: "@bureau-admin:bureau.local",
		ProvisionedAt: "2026-01-01T00:00:00Z",
	})

	// Write session.json for the launcher. The launcher loads this on startup
	// but doesn't use the session after first boot — it only listens for IPC.
	// The token and URL don't need to reach a real server.
	sessionJSON, _ := json.Marshal(service.SessionData{
		HomeserverURL: matrixServer.URL,
		UserID:        "@machine/test:bureau.local",
		AccessToken:   "syt_launcher_session_token",
	})
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("writing session.json: %v", err)
	}

	// Start the launcher subprocess.
	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	launcherCmd := exec.Command(launcherBinary,
		"--machine-name", "machine/test",
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--proxy-binary", proxyBinary,
		"--homeserver", matrixServer.URL,
		"--server-name", "bureau.local",
		"--workspace-root", workspaceRoot,
		"--cache-root", cacheRoot,
	)
	launcherCmd.Stderr = os.Stderr
	if err := launcherCmd.Start(); err != nil {
		t.Fatalf("starting launcher: %v", err)
	}
	t.Cleanup(func() {
		terminateProcess(t, launcherCmd)
	})

	// Wait for the launcher's IPC socket to appear.
	waitForFile(t, launcherSocket, 10*time.Second)

	// Create a messaging session for the daemon pointing at the mock Matrix.
	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_daemon_session_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Construct the daemon directly (not via run()) so we control the lifecycle
	// and avoid signal handling, polling loops, etc.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = runDir
	daemon.session = session
	daemon.machineName = "machine/test"
	daemon.machineUserID = "@machine/test:bureau.local"
	daemon.serverName = "bureau.local"
	daemon.configRoomID = configRoomID
	daemon.machineRoomID = machineRoomID
	daemon.serviceRoomID = serviceRoomID
	daemon.launcherSocket = launcherSocket
	daemon.statusInterval = time.Hour
	daemon.tmuxServer = tmux.NewServer(principal.TmuxSocketPath(runDir), "")
	daemon.adminSocketPathFunc = func(localpart string) string {
		return principal.RunDirAdminSocketPath(runDir, localpart)
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	ctx := context.Background()
	agentSocket := principal.RunDirSocketPath(runDir, principalLocalpart)
	adminSocket := principal.RunDirAdminSocketPath(runDir, principalLocalpart)

	// --- Phase 1: Reconcile should create a sandbox for test/echo. ---

	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile (create): %v", err)
	}

	if !daemon.running[principalLocalpart] {
		t.Fatalf("%s should be running after reconcile", principalLocalpart)
	}

	// Verify the proxy's agent socket is functional.
	agentClient := unixHTTPClient(agentSocket)
	healthResponse, err := agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check on agent socket: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health check status = %d, want 200", healthResponse.StatusCode)
	}

	// Verify the proxy's admin socket is functional.
	adminClient := unixHTTPClient(adminSocket)
	adminHealthResponse, err := adminClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check on admin socket: %v", err)
	}
	adminHealthResponse.Body.Close()
	if adminHealthResponse.StatusCode != http.StatusOK {
		t.Errorf("admin health check status = %d, want 200", adminHealthResponse.StatusCode)
	}

	// Verify the proxy received the correct identity from the credential
	// payload. This confirms the full credential flow: mock Matrix →
	// daemon reads encrypted ciphertext → launcher decrypts → pipes JSON
	// to proxy stdin → proxy parses Matrix user ID.
	identityResponse, err := agentClient.Get("http://localhost/v1/identity")
	if err != nil {
		t.Fatalf("identity request: %v", err)
	}
	identityBody, _ := io.ReadAll(identityResponse.Body)
	identityResponse.Body.Close()

	var identity struct {
		UserID     string `json:"user_id"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal(identityBody, &identity); err != nil {
		t.Fatalf("parsing identity response %q: %v", string(identityBody), err)
	}
	if identity.UserID != "@test/echo:bureau.local" {
		t.Errorf("identity user_id = %q, want @test/echo:bureau.local", identity.UserID)
	}

	// --- Phase 2: Remove principal from config → destroy sandbox. ---

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: nil,
	})

	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile (destroy): %v", err)
	}

	if daemon.running[principalLocalpart] {
		t.Errorf("%s should not be running after config removal", principalLocalpart)
	}

	// Verify the proxy sockets were cleaned up by the launcher.
	if _, err := os.Stat(agentSocket); !os.IsNotExist(err) {
		t.Errorf("agent socket should be removed after destroy (stat error: %v)", err)
	}
	if _, err := os.Stat(adminSocket); !os.IsNotExist(err) {
		t.Errorf("admin socket should be removed after destroy (stat error: %v)", err)
	}

	// --- Phase 3: Re-add principal → recreate sandbox. ---

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: principalLocalpart,
			Template:  "templates:echo",
			AutoStart: true,
		}},
	})

	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile (recreate): %v", err)
	}

	if !daemon.running[principalLocalpart] {
		t.Fatalf("%s should be running after re-add", principalLocalpart)
	}

	// Verify the recreated proxy is functional (fresh client — old socket is gone).
	agentClient = unixHTTPClient(agentSocket)
	healthResponse, err = agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check after recreate: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health check after recreate status = %d, want 200", healthResponse.StatusCode)
	}
}

// TestDaemonLauncherServiceMounts exercises the full direct service socket
// chain: daemon resolves RequiredServices from Matrix state events into
// ServiceMounts, sends them to a real launcher subprocess via IPC, and the
// launcher includes the bind-mounts in the sandbox build. This proves that
// ServiceMounts survive JSON serialization across the daemon-launcher boundary
// (different Go types, same wire format) and that the launcher processes them.
//
// The test creates a mock service socket on the host filesystem at the path
// the daemon will resolve, verifies that the sandbox is created successfully
// (not rejected due to ServiceMounts), and checks that the proxy is functional.
func TestDaemonLauncherServiceMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries and manages subprocesses")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available (sandbox creation requires namespace support)")
	}

	proxyBinary := buildBinary(t, "./cmd/bureau-proxy")
	launcherBinary := buildBinary(t, "./cmd/bureau-launcher")

	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	defer keypair.Close()

	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.txt"), keypair.PrivateKey.Bytes(), 0600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.pub"), []byte(keypair.PublicKey), 0644); err != nil {
		t.Fatalf("writing public key: %v", err)
	}

	runDir := testutil.SocketDir(t)
	launcherSocket := principal.LauncherSocketPath(runDir)

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const (
		configRoomID   = "!config:test"
		machineRoomID  = "!machine:test"
		serviceRoomID  = "!service:test"
		templateRoomID = "!templates:test"
	)

	principalLocalpart := "test/consumer"

	// Template that requires the "mock" service.
	matrixState.setRoomAlias("#templates:bureau.local", templateRoomID)
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "service-consumer", schema.TemplateContent{
		Description:      "Service consumer test template",
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"mock"},
	})

	// Bind the "mock" service role to a provider principal in the config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "mock", schema.RoomServiceContent{
		Principal: "@service/mock:bureau.local",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: principalLocalpart,
			Template:  "templates:service-consumer",
			AutoStart: true,
		}},
	})

	credentialMap := map[string]string{
		"MATRIX_TOKEN":          "syt_test_consumer_token",
		"MATRIX_HOMESERVER_URL": matrixServer.URL,
		"MATRIX_USER_ID":        "@test/consumer:bureau.local",
	}
	credentialJSON, err := json.Marshal(credentialMap)
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}
	ciphertext, err := sealed.Encrypt(credentialJSON, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, principalLocalpart, schema.Credentials{
		Version:       1,
		Principal:     "@test/consumer:bureau.local",
		EncryptedFor:  []string{"@machine/test:bureau.local"},
		Keys:          []string{"MATRIX_TOKEN", "MATRIX_HOMESERVER_URL", "MATRIX_USER_ID"},
		Ciphertext:    ciphertext,
		ProvisionedBy: "@bureau-admin:bureau.local",
		ProvisionedAt: "2026-01-01T00:00:00Z",
	})

	// Create a mock service socket at the path the daemon will resolve.
	// The daemon resolves "service/mock" → RunDirSocketPath(runDir, "service/mock").
	// This socket must exist for the launcher's bwrap to bind-mount it.
	serviceSocketPath := principal.RunDirSocketPath(runDir, "service/mock")
	serviceSocketDir := filepath.Dir(serviceSocketPath)
	if err := os.MkdirAll(serviceSocketDir, 0755); err != nil {
		t.Fatalf("creating service socket directory: %v", err)
	}
	serviceListener, err := net.Listen("unix", serviceSocketPath)
	if err != nil {
		t.Fatalf("creating mock service socket: %v", err)
	}
	t.Cleanup(func() { serviceListener.Close() })

	// Accept connections on the mock service socket (drain them so
	// nothing blocks). A real service would process CBOR requests;
	// here we just accept and close to prove the socket is reachable.
	go func() {
		for {
			conn, err := serviceListener.Accept()
			if err != nil {
				return // Listener closed.
			}
			conn.Close()
		}
	}()

	sessionJSON, _ := json.Marshal(service.SessionData{
		HomeserverURL: matrixServer.URL,
		UserID:        "@machine/test:bureau.local",
		AccessToken:   "syt_launcher_session_token",
	})
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("writing session.json: %v", err)
	}

	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	launcherCmd := exec.Command(launcherBinary,
		"--machine-name", "machine/test",
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--proxy-binary", proxyBinary,
		"--homeserver", matrixServer.URL,
		"--server-name", "bureau.local",
		"--workspace-root", workspaceRoot,
		"--cache-root", cacheRoot,
	)
	launcherCmd.Stderr = os.Stderr
	if err := launcherCmd.Start(); err != nil {
		t.Fatalf("starting launcher: %v", err)
	}
	t.Cleanup(func() {
		terminateProcess(t, launcherCmd)
	})

	waitForFile(t, launcherSocket, 10*time.Second)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_daemon_session_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Generate a token signing keypair for the daemon. Required for
	// minting service tokens when RequiredServices are present.
	tokenSigningPublicKey, tokenSigningPrivateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = runDir
	daemon.stateDir = stateDir
	daemon.session = session
	daemon.machineName = "machine/test"
	daemon.machineUserID = "@machine/test:bureau.local"
	daemon.serverName = "bureau.local"
	daemon.configRoomID = configRoomID
	daemon.machineRoomID = machineRoomID
	daemon.serviceRoomID = serviceRoomID
	daemon.launcherSocket = launcherSocket
	daemon.statusInterval = time.Hour
	daemon.tmuxServer = tmux.NewServer(principal.TmuxSocketPath(runDir), "")
	daemon.tokenSigningPublicKey = tokenSigningPublicKey
	daemon.tokenSigningPrivateKey = tokenSigningPrivateKey
	daemon.adminSocketPathFunc = func(localpart string) string {
		return principal.RunDirAdminSocketPath(runDir, localpart)
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	ctx := context.Background()
	agentSocket := principal.RunDirSocketPath(runDir, principalLocalpart)

	// Reconcile: daemon resolves RequiredServices, sends ServiceMounts
	// (and mints service tokens) in the IPC request, launcher builds
	// sandbox with bind-mounts.
	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if !daemon.running[principalLocalpart] {
		t.Fatalf("%s should be running after reconcile (service resolution + IPC succeeded)", principalLocalpart)
	}

	// Verify the proxy is functional — this proves the full daemon → launcher
	// IPC chain worked, including ServiceMounts serialization.
	agentClient := unixHTTPClient(agentSocket)
	healthResponse, err := agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check on agent socket: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health check status = %d, want 200", healthResponse.StatusCode)
	}

	// Verify the proxy received the correct identity.
	identityResponse, err := agentClient.Get("http://localhost/v1/identity")
	if err != nil {
		t.Fatalf("identity request: %v", err)
	}
	identityBody, _ := io.ReadAll(identityResponse.Body)
	identityResponse.Body.Close()

	var identity struct {
		UserID     string `json:"user_id"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal(identityBody, &identity); err != nil {
		t.Fatalf("parsing identity response %q: %v", string(identityBody), err)
	}
	if identity.UserID != "@test/consumer:bureau.local" {
		t.Errorf("identity user_id = %q, want @test/consumer:bureau.local", identity.UserID)
	}
}

// TestReconcileNoConfig verifies that reconcile handles the M_NOT_FOUND
// case gracefully — when no MachineConfig state event exists in the config
// room, the daemon should succeed (treating it as "nothing to do") rather
// than failing.
func TestReconcileNoConfig(t *testing.T) {
	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = "machine/test"
	daemon.machineUserID = "@machine/test:bureau.local"
	daemon.serverName = "bureau.local"
	daemon.configRoomID = "!config:test"
	daemon.machineRoomID = "!machine:test"
	daemon.serviceRoomID = "!service:test"
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// The mock has no state event for m.bureau.machine_config, so the
	// mock returns M_NOT_FOUND. Reconcile should treat this as "no config yet"
	// and return nil.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile with no config should succeed, got: %v", err)
	}

	if len(daemon.running) != 0 {
		t.Errorf("no principals should be running, got %d", len(daemon.running))
	}
}

// TestDaemonJoinsGlobalRooms verifies that the daemon startup path (as
// implemented in run()) explicitly joins the machines and service rooms.
// This is a unit-level test that exercises joinGlobalRooms directly with a
// mock Matrix server, verifying that both rooms receive JoinRoom calls.
func TestDaemonJoinsGlobalRooms(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const (
		machineRoomID = "!machine:test"
		serviceRoomID = "!service:test"
	)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	ctx := context.Background()

	// Join machine room.
	if _, err := session.JoinRoom(ctx, machineRoomID); err != nil {
		t.Fatalf("JoinRoom machines: %v", err)
	}
	if !matrixState.hasJoined(machineRoomID) {
		t.Error("machine room should have been joined")
	}

	// Join service room.
	if _, err := session.JoinRoom(ctx, serviceRoomID); err != nil {
		t.Fatalf("JoinRoom services: %v", err)
	}
	if !matrixState.hasJoined(serviceRoomID) {
		t.Error("service room should have been joined")
	}
}

// TestProxyCrashRecovery exercises the full proxy crash detection and recovery
// path. It creates a real sandbox via the launcher, kills the proxy process
// with SIGKILL, and verifies that the daemon's watchProxyExit goroutine
// detects the crash, destroys the orphaned sandbox, and re-reconciles to
// create a fresh one.
//
// This test validates that proxy crash detection works without health checks
// — the wait-proxy IPC provides event-driven detection within milliseconds
// of proxy death, compared to the polling-based health check path.
func TestProxyCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries and manages subprocesses")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available (sandbox creation requires namespace support)")
	}

	proxyBinary := buildBinary(t, "./cmd/bureau-proxy")
	launcherBinary := buildBinary(t, "./cmd/bureau-launcher")

	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	defer keypair.Close()

	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.txt"), keypair.PrivateKey.Bytes(), 0600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "machine-key.pub"), []byte(keypair.PublicKey), 0644); err != nil {
		t.Fatalf("writing public key: %v", err)
	}

	runDir := testutil.SocketDir(t)
	launcherSocket := principal.LauncherSocketPath(runDir)

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const (
		configRoomID   = "!config:test"
		machineRoomID  = "!machine:test"
		serviceRoomID  = "!service:test"
		templateRoomID = "!templates:test"
	)

	principalLocalpart := "test/echo"
	matrixState.setRoomAlias("#templates:bureau.local", templateRoomID)
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "echo", schema.TemplateContent{
		Description: "Echo test template",
		Command:     []string{"/bin/sleep", "3600"},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: principalLocalpart,
			Template:  "templates:echo",
			AutoStart: true,
		}},
	})

	credentialMap := map[string]string{
		"MATRIX_TOKEN":          "syt_test_echo_token",
		"MATRIX_HOMESERVER_URL": matrixServer.URL,
		"MATRIX_USER_ID":        "@test/echo:bureau.local",
	}
	credentialJSON, err := json.Marshal(credentialMap)
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}
	ciphertext, err := sealed.Encrypt(credentialJSON, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, principalLocalpart, schema.Credentials{
		Version:       1,
		Principal:     "@test/echo:bureau.local",
		EncryptedFor:  []string{"@machine/test:bureau.local"},
		Keys:          []string{"MATRIX_TOKEN", "MATRIX_HOMESERVER_URL", "MATRIX_USER_ID"},
		Ciphertext:    ciphertext,
		ProvisionedBy: "@bureau-admin:bureau.local",
		ProvisionedAt: "2026-01-01T00:00:00Z",
	})

	sessionJSON, _ := json.Marshal(service.SessionData{
		HomeserverURL: matrixServer.URL,
		UserID:        "@machine/test:bureau.local",
		AccessToken:   "syt_launcher_session_token",
	})
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), sessionJSON, 0600); err != nil {
		t.Fatalf("writing session.json: %v", err)
	}

	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	launcherCmd := exec.Command(launcherBinary,
		"--machine-name", "machine/test",
		"--run-dir", runDir,
		"--state-dir", stateDir,
		"--proxy-binary", proxyBinary,
		"--homeserver", matrixServer.URL,
		"--server-name", "bureau.local",
		"--workspace-root", workspaceRoot,
		"--cache-root", cacheRoot,
	)
	launcherCmd.Stderr = os.Stderr
	if err := launcherCmd.Start(); err != nil {
		t.Fatalf("starting launcher: %v", err)
	}
	t.Cleanup(func() {
		terminateProcess(t, launcherCmd)
	})

	waitForFile(t, launcherSocket, 10*time.Second)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_daemon_session_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// shutdownCtx enables exit watchers. The daemon starts watchSandboxExit
	// and watchProxyExit goroutines only when shutdownCtx is non-nil.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	t.Cleanup(shutdownCancel)

	daemon, _ := newTestDaemon(t)
	daemon.reconcileNotify = make(chan struct{}, 10)
	daemon.runDir = runDir
	daemon.session = session
	daemon.machineName = "machine/test"
	daemon.machineUserID = "@machine/test:bureau.local"
	daemon.serverName = "bureau.local"
	daemon.configRoomID = configRoomID
	daemon.machineRoomID = machineRoomID
	daemon.serviceRoomID = serviceRoomID
	daemon.launcherSocket = launcherSocket
	daemon.statusInterval = time.Hour
	daemon.tmuxServer = tmux.NewServer(principal.TmuxSocketPath(runDir), "")
	daemon.adminSocketPathFunc = func(localpart string) string {
		return principal.RunDirAdminSocketPath(runDir, localpart)
	}
	daemon.shutdownCtx = shutdownCtx
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(func() {
		shutdownCancel()
		daemon.stopAllLayoutWatchers()
		daemon.stopAllHealthMonitors()
	})

	ctx := context.Background()
	adminSocket := principal.RunDirAdminSocketPath(runDir, principalLocalpart)

	// --- Phase 1: Create sandbox and verify proxy is running. ---

	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile (create): %v", err)
	}

	if !daemon.running[principalLocalpart] {
		t.Fatalf("%s should be running after reconcile", principalLocalpart)
	}

	// Verify proxy is functional via admin socket health check.
	adminClient := unixHTTPClient(adminSocket)
	healthResponse, err := adminClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check on admin socket: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("health check status = %d, want 200", healthResponse.StatusCode)
	}

	// Verify exit watchers are running.
	daemon.reconcileMu.Lock()
	hasSandboxWatcher := daemon.exitWatchers[principalLocalpart] != nil
	hasProxyWatcher := daemon.proxyExitWatchers[principalLocalpart] != nil
	daemon.reconcileMu.Unlock()
	if !hasSandboxWatcher {
		t.Fatal("sandbox exit watcher should be running")
	}
	if !hasProxyWatcher {
		t.Fatal("proxy exit watcher should be running")
	}

	// --- Phase 2: Kill the proxy process with SIGKILL. ---

	// Get the proxy PID from the launcher via list-sandboxes IPC.
	listResponse, err := daemon.launcherRequest(ctx, launcherIPCRequest{
		Action: "list-sandboxes",
	})
	if err != nil {
		t.Fatalf("list-sandboxes IPC: %v", err)
	}
	if !listResponse.OK {
		t.Fatalf("list-sandboxes rejected: %s", listResponse.Error)
	}

	var proxyPID int
	for _, entry := range listResponse.Sandboxes {
		if entry.Localpart == principalLocalpart {
			proxyPID = entry.ProxyPID
			break
		}
	}
	if proxyPID == 0 {
		t.Fatalf("proxy PID not found in list-sandboxes for %s", principalLocalpart)
	}

	t.Logf("killing proxy (PID %d) with SIGKILL", proxyPID)
	if err := syscall.Kill(proxyPID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing proxy PID %d: %v", proxyPID, err)
	}

	// --- Phase 3: Wait for detection and recovery. ---

	// The watchProxyExit goroutine detects the proxy death via the
	// launcher's wait-proxy IPC (blocking read that returns when the
	// process exits). It then destroys the orphaned sandbox, clears
	// running state, and calls reconcile() which recreates the sandbox.
	// The reconcileNotify channel fires after the reconcile completes,
	// so we can wait on it instead of polling daemon.running.
	testutil.RequireReceive(t, daemon.reconcileNotify, 15*time.Second, "waiting for proxy crash recovery reconcile")

	daemon.reconcileMu.Lock()
	running := daemon.running[principalLocalpart]
	daemon.reconcileMu.Unlock()
	if !running {
		t.Fatal("principal should be running after proxy crash recovery")
	}

	// Verify the new proxy is functional.
	newAdminClient := unixHTTPClient(adminSocket)
	newHealthResponse, err := newAdminClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check after recovery: %v", err)
	}
	newHealthResponse.Body.Close()
	if newHealthResponse.StatusCode != http.StatusOK {
		t.Fatalf("health check after recovery = %d, want 200", newHealthResponse.StatusCode)
	}

	// Verify new exit watchers are running for the recreated sandbox.
	daemon.reconcileMu.Lock()
	hasSandboxWatcher = daemon.exitWatchers[principalLocalpart] != nil
	hasProxyWatcher = daemon.proxyExitWatchers[principalLocalpart] != nil
	daemon.reconcileMu.Unlock()
	if !hasSandboxWatcher {
		t.Error("sandbox exit watcher should be running after recovery")
	}
	if !hasProxyWatcher {
		t.Error("proxy exit watcher should be running after recovery")
	}

	t.Log("proxy crash recovery completed successfully")
}

// --- Helpers ---

// buildBinary returns the path to a pre-built binary for the given
// package. Binaries are provided as Bazel data dependencies.
func buildBinary(t *testing.T, pkg string) string {
	t.Helper()

	envVars := map[string]string{
		"./cmd/bureau-proxy":    "BUREAU_PROXY_BINARY",
		"./cmd/bureau-launcher": "BUREAU_LAUNCHER_BINARY",
	}
	envName, ok := envVars[pkg]
	if !ok {
		t.Fatalf("no data dependency configured for package %q", pkg)
	}
	return testutil.DataBinary(t, envName)
}

// waitForFile blocks until a file appears at the given path. Polls in
// a background goroutine and uses testutil.RequireClosed for the
// timeout safety valve.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	found := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(path); err == nil {
				close(found)
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	testutil.RequireClosed(t, found, timeout, "waiting for file %s", path)
}

// terminateProcess sends SIGTERM and waits for the process to exit.
// If it doesn't exit within 5 seconds (bounded by testutil.RequireClosed),
// the test fails. A deferred SIGKILL ensures cleanup even on timeout.
func terminateProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	command.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { command.Wait(); close(done) }()
	testutil.RequireClosed(t, done, 5*time.Second, "waiting for process %d to exit after SIGTERM", command.Process.Pid)
}

// unixHTTPClient creates an HTTP client that connects via the given Unix socket.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// --- Mock Matrix Server ---

// mockMatrixState holds configurable state for a mock Matrix homeserver.
// Thread-safe: state can be updated between reconcile calls from the test
// goroutine while the mock server handles requests.
type mockMatrixState struct {
	mu sync.Mutex

	// stateEvents stores individual state event content. Key format:
	// "roomID\x00eventType\x00stateKey" → JSON content bytes.
	stateEvents map[string]json.RawMessage

	// roomStates stores arrays of state events for GetRoomState responses.
	// Key: roomID.
	roomStates map[string][]mockRoomStateEvent

	// roomAliases maps room aliases to room IDs for ResolveAlias.
	// Key: full alias (e.g., "#iree/amdgpu/general:bureau.local").
	roomAliases map[string]string

	// roomMembers maps room IDs to member lists for GetRoomMembers.
	roomMembers map[string][]mockRoomMember

	// stateEventWritten is signaled (non-blocking) whenever a PUT state
	// event arrives. Tests can wait on this to detect when production
	// code publishes a state event. Nil means no notification.
	stateEventWritten chan string

	// joinedRooms tracks which rooms each user has joined via the JoinRoom
	// endpoint. Key: room ID, value: set of user IDs that called join.
	joinedRooms map[string]map[string]bool

	// syncBatch is a counter for generating unique next_batch tokens.
	syncBatch int

	// pendingSyncEvents holds events to include in the next incremental
	// /sync response. Keyed by room ID. Cleared after each sync response.
	pendingSyncEvents map[string][]mockRoomStateEvent

	// invites tracks rooms with pending invites. Included in the /sync
	// response's rooms.invite section. Cleared when handleJoinRoom is
	// called for the room (accepting the invite moves it to joined).
	invites map[string]bool
}

// mockRoomMember represents a member for the /members endpoint.
type mockRoomMember struct {
	UserID      string `json:"user_id"`
	Membership  string `json:"membership"`
	DisplayName string `json:"displayname,omitempty"`
}

// mockRoomStateEvent represents a single state event in a GetRoomState response.
// The Content field uses map[string]any because that's what the messaging
// library's Event type uses after JSON unmarshaling.
type mockRoomStateEvent struct {
	Type     string         `json:"type"`
	StateKey *string        `json:"state_key"`
	Content  map[string]any `json:"content"`
}

func newMockMatrixState() *mockMatrixState {
	return &mockMatrixState{
		stateEvents:       make(map[string]json.RawMessage),
		roomStates:        make(map[string][]mockRoomStateEvent),
		roomAliases:       make(map[string]string),
		roomMembers:       make(map[string][]mockRoomMember),
		joinedRooms:       make(map[string]map[string]bool),
		pendingSyncEvents: make(map[string][]mockRoomStateEvent),
		invites:           make(map[string]bool),
	}
}

// enqueueSyncEvent queues a state event to appear in the next incremental
// /sync response for the given room. Used by tests to simulate state changes
// arriving via /sync.
func (m *mockMatrixState) enqueueSyncEvent(roomID string, event mockRoomStateEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingSyncEvents[roomID] = append(m.pendingSyncEvents[roomID], event)
}

// addInvite marks a room as having a pending invite. The room will appear
// in the /sync response's rooms.invite section until the daemon calls
// JoinRoom (which clears the invite).
func (m *mockMatrixState) addInvite(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[roomID] = true
}

func (m *mockMatrixState) setStateEvent(roomID, eventType, stateKey string, content any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := json.Marshal(content)
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	m.stateEvents[key] = data
}

func (m *mockMatrixState) setRoomState(roomID string, events []mockRoomStateEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomStates[roomID] = events
}

func (m *mockMatrixState) setRoomAlias(alias, roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomAliases[alias] = roomID
}

func (m *mockMatrixState) setRoomMembers(roomID string, members []mockRoomMember) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomMembers[roomID] = members
}

// handler returns an http.Handler that implements the subset of the Matrix
// client-server API used by the daemon: GetStateEvent, GetRoomState,
// SendStateEvent, ResolveAlias, GetRoomMembers, and Sync.
//
// URL path parsing handles percent-encoded room IDs and state keys (the
// messaging library uses url.PathEscape which encodes /, :, ! etc.). The
// handler splits on literal "/" in the raw path and decodes each segment
// individually, so an encoded state key like "test%2Fecho" is correctly
// decoded to "test/echo".
func (m *mockMatrixState) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use RawPath to preserve percent-encoded slashes in state keys.
		rawPath := r.URL.RawPath
		if rawPath == "" {
			rawPath = r.URL.Path
		}

		// GET /_matrix/client/v3/sync — sync.
		if rawPath == "/_matrix/client/v3/sync" && r.Method == "GET" {
			since := r.URL.Query().Get("since")
			m.handleSync(w, since)
			return
		}

		// POST /_matrix/client/v3/join/{roomIdOrAlias} — join room.
		const joinPrefix = "/_matrix/client/v3/join/"
		if strings.HasPrefix(rawPath, joinPrefix) && r.Method == "POST" {
			encoded := rawPath[len(joinPrefix):]
			roomIDOrAlias, _ := url.PathUnescape(encoded)
			m.handleJoinRoom(w, r, roomIDOrAlias)
			return
		}

		// GET /_matrix/client/v3/directory/room/{alias} — resolve alias.
		const directoryPrefix = "/_matrix/client/v3/directory/room/"
		if strings.HasPrefix(rawPath, directoryPrefix) && r.Method == "GET" {
			encodedAlias := rawPath[len(directoryPrefix):]
			alias, _ := url.PathUnescape(encodedAlias)
			m.handleResolveAlias(w, alias)
			return
		}

		const roomsPrefix = "/_matrix/client/v3/rooms/"
		if !strings.HasPrefix(rawPath, roomsPrefix) {
			http.NotFound(w, r)
			return
		}

		// Split into room ID and the rest of the path.
		rest := rawPath[len(roomsPrefix):]
		segments := strings.SplitN(rest, "/", 2)
		if len(segments) < 2 {
			http.NotFound(w, r)
			return
		}

		roomID, _ := url.PathUnescape(segments[0])
		pathAfterRoom := segments[1]

		// GET /rooms/{roomId}/state — return all state events.
		if pathAfterRoom == "state" && r.Method == "GET" {
			m.handleGetRoomState(w, roomID)
			return
		}

		// GET or PUT /rooms/{roomId}/state/{eventType}/{stateKey}
		if strings.HasPrefix(pathAfterRoom, "state/") {
			stateRest := pathAfterRoom[len("state/"):]
			typeAndKey := strings.SplitN(stateRest, "/", 2)
			if len(typeAndKey) < 2 {
				http.NotFound(w, r)
				return
			}

			eventType, _ := url.PathUnescape(typeAndKey[0])
			stateKey, _ := url.PathUnescape(typeAndKey[1])

			switch r.Method {
			case "GET":
				m.handleGetStateEvent(w, roomID, eventType, stateKey)
			case "PUT":
				m.handlePutStateEvent(w, r, roomID, eventType, stateKey)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// POST /rooms/{roomId}/invite — invite user.
		if pathAfterRoom == "invite" && r.Method == "POST" {
			m.handleInvite(w, r, roomID)
			return
		}

		// PUT /rooms/{roomId}/send/{eventType}/{txnId} — send event.
		if strings.HasPrefix(pathAfterRoom, "send/") && r.Method == "PUT" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"event_id": "$mock-send-event-id",
			})
			return
		}

		// GET /rooms/{roomId}/members — return room members.
		if pathAfterRoom == "members" && r.Method == "GET" {
			m.handleGetRoomMembers(w, roomID)
			return
		}

		http.NotFound(w, r)
	})
}

func (m *mockMatrixState) handleJoinRoom(w http.ResponseWriter, r *http.Request, roomIDOrAlias string) {
	// Resolve alias to room ID if it looks like an alias.
	roomID := roomIDOrAlias
	if strings.HasPrefix(roomIDOrAlias, "#") {
		m.mu.Lock()
		resolved, ok := m.roomAliases[roomIDOrAlias]
		m.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   fmt.Sprintf("room alias %q not found", roomIDOrAlias),
			})
			return
		}
		roomID = resolved
	}

	// Extract user ID from the Authorization header (Bearer token lookup
	// is not needed for mock — just record the join).
	m.mu.Lock()
	if m.joinedRooms[roomID] == nil {
		m.joinedRooms[roomID] = make(map[string]bool)
	}
	// Track by room ID. We use "any" since the mock doesn't verify tokens.
	m.joinedRooms[roomID]["_joined"] = true
	// Accepting an invite clears it from the pending set.
	delete(m.invites, roomID)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"room_id": roomID,
	})
}

// hasJoined returns true if any client called JoinRoom on the given room ID.
func (m *mockMatrixState) hasJoined(roomID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.joinedRooms[roomID]["_joined"]
}

func (m *mockMatrixState) handleGetStateEvent(w http.ResponseWriter, roomID, eventType, stateKey string) {
	m.mu.Lock()
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	data, ok := m.stateEvents[key]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_NOT_FOUND",
			"error":   fmt.Sprintf("state event not found: %s/%s in %s", eventType, stateKey, roomID),
		})
		return
	}

	w.Write(data)
}

func (m *mockMatrixState) handleGetRoomState(w http.ResponseWriter, roomID string) {
	m.mu.Lock()
	events, ok := m.roomStates[roomID]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok || len(events) == 0 {
		w.Write([]byte("[]"))
		return
	}

	json.NewEncoder(w).Encode(events)
}

func (m *mockMatrixState) handlePutStateEvent(w http.ResponseWriter, r *http.Request, roomID, eventType, stateKey string) {
	body, _ := io.ReadAll(r.Body)

	// Store the event (so it can be read back via GetStateEvent).
	m.mu.Lock()
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	m.stateEvents[key] = json.RawMessage(body)
	writeNotify := m.stateEventWritten
	m.mu.Unlock()

	// Signal listeners that a state event was written (non-blocking).
	if writeNotify != nil {
		select {
		case writeNotify <- key:
		default:
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"event_id": "$mock-event-id",
	})
}

func (m *mockMatrixState) handleInvite(w http.ResponseWriter, r *http.Request, roomID string) {
	// Accept all invite requests. In a real homeserver, this would
	// check power levels and membership state.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{})
}

func (m *mockMatrixState) handleResolveAlias(w http.ResponseWriter, alias string) {
	m.mu.Lock()
	roomID, ok := m.roomAliases[alias]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_NOT_FOUND",
			"error":   fmt.Sprintf("room alias %q not found", alias),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"room_id": roomID,
		"servers": []string{"bureau.local"},
	})
}

func (m *mockMatrixState) handleGetRoomMembers(w http.ResponseWriter, roomID string) {
	m.mu.Lock()
	members, ok := m.roomMembers[roomID]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		json.NewEncoder(w).Encode(map[string]any{"chunk": []any{}})
		return
	}

	// Build the member events in the format that GetRoomMembers expects.
	var chunk []map[string]any
	for _, member := range members {
		chunk = append(chunk, map[string]any{
			"type":      schema.MatrixEventTypeRoomMember,
			"state_key": member.UserID,
			"sender":    member.UserID,
			"content": map[string]any{
				"membership":  member.Membership,
				"displayname": member.DisplayName,
			},
		})
	}

	json.NewEncoder(w).Encode(map[string]any{"chunk": chunk})
}

// handleSync returns a /sync response. On initial sync (empty since), it
// returns all rooms that have roomStates configured as joined rooms with
// their state events. On incremental sync (non-empty since), it returns
// any pending events queued via enqueueSyncEvent.
func (m *mockMatrixState) handleSync(w http.ResponseWriter, since string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.syncBatch++
	nextBatch := fmt.Sprintf("batch_%d", m.syncBatch)

	joinedRooms := make(map[string]any)

	if since == "" {
		// Initial sync: return all configured room state.
		for roomID, events := range m.roomStates {
			joinedRooms[roomID] = map[string]any{
				"state": map[string]any{
					"events": events,
				},
				"timeline": map[string]any{
					"events":     []any{},
					"prev_batch": "",
					"limited":    false,
				},
			}
		}
	} else {
		// Incremental sync: return pending events and clear the queue.
		for roomID, events := range m.pendingSyncEvents {
			// State events in incremental sync appear as timeline events
			// with state_key set (matching real Matrix server behavior).
			timelineEvents := make([]any, 0, len(events))
			for _, event := range events {
				timelineEvents = append(timelineEvents, map[string]any{
					"type":      event.Type,
					"state_key": event.StateKey,
					"content":   event.Content,
					"event_id":  fmt.Sprintf("$sync_%d", m.syncBatch),
					"sender":    "@admin:bureau.local",
				})
			}
			joinedRooms[roomID] = map[string]any{
				"state": map[string]any{
					"events": []any{},
				},
				"timeline": map[string]any{
					"events":     timelineEvents,
					"prev_batch": since,
					"limited":    false,
				},
			}
		}
		m.pendingSyncEvents = make(map[string][]mockRoomStateEvent)
	}

	// Build invite section from pending invites.
	invitedRooms := make(map[string]any)
	for roomID := range m.invites {
		invitedRooms[roomID] = map[string]any{
			"invite_state": map[string]any{
				"events": []any{},
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"next_batch": nextBatch,
		"rooms": map[string]any{
			"join":   joinedRooms,
			"invite": invitedRooms,
		},
	})
}
