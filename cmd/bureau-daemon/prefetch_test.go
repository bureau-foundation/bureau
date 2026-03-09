// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// validTestHash is a 32-character lowercase alphanumeric string matching
// the Nix store path hash format for use in test store paths.
const validTestHash = "abcdefghijklmnopqrstuvwxyz012345"

// findExistingStoreDirectory returns a real Nix store directory path that
// exists on the current machine. Skips the test if Nix is not available.
func findExistingStoreDirectory(t *testing.T) string {
	t.Helper()
	nixBinary, err := nix.FindBinary("nix")
	if err != nil {
		t.Skipf("nix not available: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(nixBinary)
	if err != nil {
		t.Skipf("cannot resolve nix binary symlinks: %v", err)
	}
	storeDirectory, err := nix.StoreDirectory(resolved)
	if err != nil {
		t.Skipf("cannot extract store directory from %q: %v", resolved, err)
	}
	return storeDirectory
}

func TestPrefetchEnvironment_ExistingPath(t *testing.T) {
	t.Parallel()

	existingPath := t.TempDir()
	called := false
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.validateStorePathFunc = func(path string) error {
		return nil // Test uses temp paths, not real Nix store paths.
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		called = true
		return fmt.Errorf("should not be called")
	}

	err := daemon.prefetchEnvironment(context.Background(), existingPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("prefetchFunc should not be called when path exists on disk")
	}
}

func TestPrefetchEnvironment_MissingPathSuccess(t *testing.T) {
	t.Parallel()

	var capturedPath string
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.validateStorePathFunc = func(path string) error {
		return nil // Test uses fake paths, not real Nix store paths.
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		capturedPath = storePath
		return nil
	}

	const wantPath = "/nix/store/nonexistent-test-path"
	err := daemon.prefetchEnvironment(context.Background(), wantPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedPath != wantPath {
		t.Errorf("prefetchFunc called with %q, want %q", capturedPath, wantPath)
	}
}

func TestPrefetchEnvironment_MissingPathFailure(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.validateStorePathFunc = func(path string) error {
		return nil // Test uses fake paths, not real Nix store paths.
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return fmt.Errorf("substituter unreachable")
	}

	err := daemon.prefetchEnvironment(context.Background(), "/nix/store/nonexistent-test-path")
	if err == nil {
		t.Fatal("expected error when prefetchFunc fails")
	}
	if !strings.Contains(err.Error(), "substituter unreachable") {
		t.Errorf("error = %v, want containing 'substituter unreachable'", err)
	}
}

func TestPrefetchEnvironment_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.validateStorePathFunc = func(path string) error {
		return nil // Test uses fake paths, not real Nix store paths.
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return ctx.Err()
	}

	err := daemon.prefetchEnvironment(ctx, "/nix/store/nonexistent-test-path")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestPrefetchEnvironment_RejectsPathTraversal(t *testing.T) {
	t.Parallel()

	// Simulates a compromised template publishing an EnvironmentPath
	// with path traversal. prefetchEnvironment must reject before any
	// filesystem access (os.Stat or nix-store --realise).
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		t.Fatal("prefetchFunc should not be called for invalid paths")
		return nil
	}

	traversalPaths := []string{
		"/nix/store/../../etc/shadow",
		"/nix/store/../etc/passwd",
		"/usr/local/bin/bash",
		"/nix/store/",
		"",
	}

	for _, path := range traversalPaths {
		err := daemon.prefetchEnvironment(context.Background(), path)
		if err == nil {
			t.Errorf("expected error for path %q, got nil", path)
		}
	}
}

func TestPrefetchBureauVersion_AllPaths(t *testing.T) {
	t.Parallel()

	// All store paths point to the same real Nix store directory that
	// exists on disk, so no actual prefetch should be needed (os.Stat
	// fast path).
	existingPath := findExistingStoreDirectory(t)

	called := false
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		called = true
		return fmt.Errorf("should not be called for existing paths")
	}

	version := &schema.BureauVersion{
		DaemonStorePath:   existingPath,
		LauncherStorePath: existingPath,
		ProxyStorePath:    existingPath,
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("prefetchFunc should not be called when all paths exist")
	}
}

func TestPrefetchBureauVersion_PartialPaths(t *testing.T) {
	t.Parallel()

	// Only DaemonStorePath is set; others are empty and should be skipped.
	existingPath := findExistingStoreDirectory(t)

	var prefetchedPaths []string
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		prefetchedPaths = append(prefetchedPaths, storePath)
		return nil
	}

	version := &schema.BureauVersion{
		DaemonStorePath: existingPath,
		// LauncherStorePath and ProxyStorePath intentionally empty.
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prefetchedPaths) != 0 {
		t.Errorf("expected no prefetchFunc calls (path exists), got calls for %v", prefetchedPaths)
	}
}

func TestPrefetchBureauVersion_PrefetchFailure(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return fmt.Errorf("substituter unavailable")
	}

	version := &schema.BureauVersion{
		DaemonStorePath: "/nix/store/" + validTestHash + "-nonexistent-daemon-binary",
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err == nil {
		t.Fatal("expected error when prefetchFunc fails")
	}
	if !strings.Contains(err.Error(), "daemon") || !strings.Contains(err.Error(), "substituter unavailable") {
		t.Errorf("error = %v, want error mentioning daemon and substituter unavailable", err)
	}
}

func TestPrefetchBureauVersion_MissingPathTriggersFetch(t *testing.T) {
	t.Parallel()

	var fetchedPaths []string
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		fetchedPaths = append(fetchedPaths, storePath)
		return nil
	}

	version := &schema.BureauVersion{
		DaemonStorePath:   "/nix/store/" + validTestHash + "-nonexistent-daemon",
		LauncherStorePath: "/nix/store/" + validTestHash + "-nonexistent-launcher",
		ProxyStorePath:    "/nix/store/" + validTestHash + "-nonexistent-proxy",
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fetchedPaths) != 3 {
		t.Errorf("expected 3 prefetch calls, got %d: %v", len(fetchedPaths), fetchedPaths)
	}
}

func TestPrefetchBureauVersion_PathTraversalRejected(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		t.Fatal("prefetchFunc should not be called for invalid paths")
		return nil
	}

	tests := []struct {
		name string
		path string
	}{
		{
			name: "dotdot traversal to bin",
			path: "/nix/store/../../bin/bash",
		},
		{
			name: "dotdot traversal to etc",
			path: "/nix/store/../etc/passwd",
		},
		{
			name: "short hash basename",
			path: "/nix/store/abc-bureau-daemon/bin/bureau-daemon",
		},
		{
			name: "non-nix path",
			path: "/usr/local/bin/evil-binary",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			version := &schema.BureauVersion{
				DaemonStorePath: testCase.path,
			}
			err := daemon.prefetchBureauVersion(context.Background(), version)
			if err == nil {
				t.Fatalf("expected error for path %q", testCase.path)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Errorf("error = %v, want error containing 'invalid'", err)
			}
		})
	}
}

func TestFindNixStore(t *testing.T) {
	t.Parallel()

	path, err := nix.FindBinary("nix-store")
	if err != nil {
		t.Skipf("nix-store not available: %v", err)
	}
	if !strings.Contains(path, "nix-store") {
		t.Errorf("nix.FindBinary(\"nix-store\") = %q, expected path containing 'nix-store'", path)
	}
}

// TestReconcile_PrefetchFailureSkipsPrincipal verifies that a failed
// Nix prefetch prevents sandbox creation and posts an error message to
// the config room. On the next reconcile (with prefetch succeeding),
// the principal starts normally.
func TestReconcile_PrefetchFailureSkipsPrincipal(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
	)

	daemon, fakeClock := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	// Track messages sent to the config room (prefetch error messages).
	var (
		messagesMu         sync.Mutex
		configRoomMessages []string
	)

	// Mock Matrix server with template and config state.
	matrixState := newPrefetchTestMatrixState(t, daemon.fleet, configRoomID, templateRoomID, machineName)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intercept message sends to the config room. SendEvent uses
		// PUT with url.PathEscape on room ID and event type.
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+string(schema.MatrixEventTypeMessage)+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				configRoomMessages = append(configRoomMessages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		matrixState.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Mock launcher that tracks create-sandbox calls.
	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var (
		launcherMu        sync.Mutex
		createdPrincipals []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action == ipc.ActionCreateSandbox {
			launcherMu.Lock()
			createdPrincipals = append(createdPrincipals, request.Principal)
			launcherMu.Unlock()
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		}
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	// Start with prefetch that always fails.
	prefetchError := fmt.Errorf("connection to attic refused")
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return prefetchError
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// First reconcile: prefetch fails, principal should NOT start.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	if len(createdPrincipals) != 0 {
		t.Errorf("expected no create-sandbox calls after prefetch failure, got %v", createdPrincipals)
	}
	launcherMu.Unlock()

	if daemon.isAlive(testEntity(t, daemon.fleet, "agent/test")) {
		t.Error("principal should not be marked as running after prefetch failure")
	}

	// Verify error message was posted to the config room.
	messagesMu.Lock()
	if len(configRoomMessages) == 0 {
		t.Error("expected error message in config room after prefetch failure")
	} else {
		found := false
		for _, message := range configRoomMessages {
			if strings.Contains(message, "agent/test") && strings.Contains(message, "attic refused") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("config room messages = %v, want message mentioning agent/test and attic refused", configRoomMessages)
		}
	}
	messagesMu.Unlock()

	// Second reconcile: prefetch succeeds, principal should start.
	// Advance past the start failure backoff so the principal is retried.
	fakeClock.Advance(2 * time.Second)
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	if len(createdPrincipals) != 1 || createdPrincipals[0] != "agent/test" {
		t.Errorf("expected create-sandbox for agent/test, got %v", createdPrincipals)
	}
	launcherMu.Unlock()

	if !daemon.isAlive(testEntity(t, daemon.fleet, "agent/test")) {
		t.Error("principal should be marked as running after successful prefetch")
	}
}

// TestReconcile_NoPrefetchWithoutEnvironmentPath verifies that
// principals with templates that have no EnvironmentPath skip the
// prefetch step entirely.
func TestReconcile_NoPrefetchWithoutEnvironmentPath(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newPrefetchTestMatrixState(t, daemon.fleet, configRoomID, templateRoomID, machineName)
	// Override template to have no environment path.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	prefetchCalled := false
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		prefetchCalled = true
		return fmt.Errorf("should not be called")
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if prefetchCalled {
		t.Error("prefetchFunc should not be called when template has no EnvironmentPath")
	}

	if !daemon.isAlive(testEntity(t, daemon.fleet, "agent/test")) {
		t.Error("principal should be running (no prefetch needed)")
	}
}

// --- Test helpers ---

// newPrefetchTestMatrixState creates a mock Matrix state with a
// template that has an EnvironmentPath and a MachineConfig that
// references it. Used by the reconcile prefetch tests.
func newPrefetchTestMatrixState(t *testing.T, fleet ref.Fleet, configRoomID, templateRoomID, machineName string) *mockMatrixState {
	t.Helper()

	agentEntity := testEntity(t, fleet, "agent/test")
	state := newMockMatrixState()

	state.setRoomAlias(fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:     []string{"/bin/echo", "hello"},
		Environment: "/nix/store/abcdefghijklmnopqrstuvwxyz012345-test-env",
	})

	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: agentEntity,
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})

	state.setStateEvent(configRoomID, schema.EventTypeCredentials, agentEntity.UserID().StateKey(), schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	return state
}

// startMockLauncher starts a mock launcher that accepts CBOR IPC on a
// Unix socket and responds using the provided handler function. Returns
// the listener (caller should defer Close).
func startMockLauncher(t *testing.T, socketPath string, handler func(launcherIPCRequest) launcherIPCResponse) net.Listener {
	t.Helper()

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", socketPath, err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			var request launcherIPCRequest
			if err := codec.NewDecoder(conn).Decode(&request); err != nil {
				conn.Close()
				continue
			}

			response := handler(request)
			codec.NewEncoder(conn).Encode(response)
			conn.Close()
		}
	}()

	return listener
}

// Fixed timestamps for test certificate generation. Using constants
// avoids wall-clock dependency (check-real-clock hook) while providing
// a validity window wide enough for any test execution.
var ( //nolint:realclock // These are fixed timestamps, not wall-clock usage.
	testCertNotBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	testCertNotAfter  = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
)

// generateTestPEM creates synthetic CA and Rekor PEM material for test
// verifiers. The material is cryptographically valid but won't match
// any real Sigstore bundle — Verify() always returns StatusRejected.
func generateTestPEM(t *testing.T) (caPEM, rekorPEM []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             testCertNotBefore,
		NotAfter:              testCertNotAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	rekorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating rekor key: %v", err)
	}
	rekorPubDER, err := x509.MarshalPKIXPublicKey(&rekorKey.PublicKey)
	if err != nil {
		t.Fatalf("marshaling rekor public key: %v", err)
	}
	rekorPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rekorPubDER})

	return caPEM, rekorPEM
}

// newTestVerifier creates a provenance.Verifier with the given
// enforcement level for "nix_store_paths". Uses synthetic PEM material
// (self-signed CA + ECDSA key) that won't match any real bundle. This
// means Verify() will always return StatusRejected for any bundle
// passed to it — which is what most daemon tests need (testing the
// enforcement gate, not cryptographic verification).
func newTestVerifier(t *testing.T, enforcement schema.EnforcementLevel) *provenance.Verifier {
	t.Helper()

	caPEM, rekorPEM := generateTestPEM(t)

	roots := schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			"test": {
				FulcioRootPEM:     string(caPEM),
				RekorPublicKeyPEM: string(rekorPEM),
			},
		},
	}

	policy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "test-ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"nix_store_paths": enforcement,
		},
	}

	verifier, err := provenance.NewVerifier(roots, policy)
	if err != nil {
		t.Fatalf("creating test verifier: %v", err)
	}
	return verifier
}

func TestVerifyStorePathProvenance_NilVerifier(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	// No provenanceVerifier set — should skip verification entirely.
	err := daemon.verifyStorePathProvenance(context.Background(), "daemon", "/nix/store/abc-test")
	if err != nil {
		t.Fatalf("expected nil error with nil verifier, got %v", err)
	}
}

func TestVerifyStorePathProvenance_NoCacheURL(t *testing.T) {
	t.Parallel()

	t.Run("require", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
		// No fleetCacheConfig → no cache URL.

		err := daemon.verifyStorePathProvenance(context.Background(), "daemon", "/nix/store/abc-test")
		if err == nil {
			t.Fatal("expected error with enforcement=require and no cache URL")
		}
		if !strings.Contains(err.Error(), "no binary cache URL") {
			t.Errorf("error = %v, want containing 'no binary cache URL'", err)
		}
	})

	t.Run("warn", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)

		err := daemon.verifyStorePathProvenance(context.Background(), "daemon", "/nix/store/abc-test")
		if err != nil {
			t.Fatalf("expected nil error with enforcement=warn and no cache URL, got %v", err)
		}
	})

	t.Run("log", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementLog)

		err := daemon.verifyStorePathProvenance(context.Background(), "daemon", "/nix/store/abc-test")
		if err != nil {
			t.Fatalf("expected nil error with enforcement=log and no cache URL, got %v", err)
		}
	})
}

func TestVerifyStorePathProvenance_BundleNotFound(t *testing.T) {
	t.Parallel()

	// HTTP server that returns 404 for all requests.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	t.Run("require", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err == nil {
			t.Fatal("expected error with enforcement=require and missing bundle")
		}
		if !strings.Contains(err.Error(), "no provenance bundle found") {
			t.Errorf("error = %v, want containing 'no provenance bundle found'", err)
		}
	})

	t.Run("warn", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err != nil {
			t.Fatalf("expected nil error with enforcement=warn and missing bundle, got %v", err)
		}
	})
}

func TestVerifyStorePathProvenance_VerificationRejected(t *testing.T) {
	t.Parallel()

	// HTTP server that returns a syntactically valid but
	// cryptographically bogus Sigstore bundle. The verifier will
	// reject it because the certificate chain and signatures don't
	// match the test trust roots.
	fakeBundleJSON := `{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": {
			"x509CertificateChain": {"certificates": [{"rawBytes": ""}]},
			"tlogEntries": []
		},
		"dsseEnvelope": {
			"payload": "",
			"payloadType": "application/vnd.in-toto+json",
			"signatures": [{"sig": "", "keyid": ""}]
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeBundleJSON))
	}))
	t.Cleanup(server.Close)

	// Mock NAR digest — returns a fixed 32-byte hash.
	fakeDigest := sha256.Sum256([]byte("test-nar-content"))

	t.Run("require", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
		daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
			return fakeDigest[:], nil
		}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err == nil {
			t.Fatal("expected error with enforcement=require and rejected bundle")
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Errorf("error = %v, want containing 'rejected'", err)
		}
	})

	t.Run("warn", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
		daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
			return fakeDigest[:], nil
		}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err != nil {
			t.Fatalf("expected nil error with enforcement=warn and rejected bundle, got %v", err)
		}
	})
}

func TestVerifyStorePathProvenance_NARDigestFailure(t *testing.T) {
	t.Parallel()

	// HTTP server that returns a bundle (content doesn't matter since
	// we'll fail before verification).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}`))
	}))
	t.Cleanup(server.Close)

	t.Run("require", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
		daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
			return nil, fmt.Errorf("nix-store not found")
		}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err == nil {
			t.Fatal("expected error with enforcement=require and NAR digest failure")
		}
		if !strings.Contains(err.Error(), "NAR digest") {
			t.Errorf("error = %v, want containing 'NAR digest'", err)
		}
	})

	t.Run("warn", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)
		daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
		daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
			return nil, fmt.Errorf("nix-store not found")
		}

		err := daemon.verifyStorePathProvenance(
			context.Background(), "daemon",
			"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
		if err != nil {
			t.Fatalf("expected nil error with enforcement=warn and NAR digest failure, got %v", err)
		}
	})
}

func TestPrefetchBureauVersion_ExistingPathSkipsVerificationUnderWarn(t *testing.T) {
	t.Parallel()

	// Under warn enforcement, existing paths skip verification
	// entirely (fast path).
	existingPath := findExistingStoreDirectory(t)

	narDigestCalled := false
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: "http://cache.example.com"}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		narDigestCalled = true
		return nil, fmt.Errorf("should not be called")
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		t.Fatal("prefetchFunc should not be called for existing paths")
		return nil
	}

	version := &schema.BureauVersion{
		DaemonStorePath: existingPath,
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if narDigestCalled {
		t.Error("narDigestFunc should not be called for existing paths under warn")
	}
}

func TestPrefetchBureauVersion_ExistingPathVerifiedUnderRequire(t *testing.T) {
	t.Parallel()

	// Under require enforcement, existing paths that haven't been
	// verified in this session MUST be verified. The fast path is
	// only for paths already in verifiedStorePaths.
	existingPath := findExistingStoreDirectory(t)

	bundleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(bundleServer.Close)

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: bundleServer.URL}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		t.Fatal("prefetchFunc should not be called for existing paths")
		return nil
	}

	version := &schema.BureauVersion{
		DaemonStorePath: existingPath,
	}

	// First call: path exists but not yet verified — should trigger
	// verification, which fails because no bundle exists. The bundle
	// fetch returns 404, which under require enforcement returns an
	// error before reaching narDigest computation.
	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err == nil {
		t.Fatal("expected error for unverified existing path under require")
	}
	if !strings.Contains(err.Error(), "provenance") {
		t.Errorf("error = %v, want containing 'provenance'", err)
	}
}

func TestPrefetchBureauVersion_VerifiedPathSkipsFastPath(t *testing.T) {
	t.Parallel()

	// Once a path is in verifiedStorePaths, it should skip
	// verification even under require enforcement.
	existingPath := findExistingStoreDirectory(t)

	narDigestCalled := false
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: "http://cache.example.com"}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		narDigestCalled = true
		return nil, fmt.Errorf("should not be called")
	}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		t.Fatal("prefetchFunc should not be called for existing paths")
		return nil
	}
	// Pre-populate the verified map.
	daemon.verifiedStorePaths = map[string]bool{existingPath: true}

	version := &schema.BureauVersion{
		DaemonStorePath: existingPath,
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if narDigestCalled {
		t.Error("narDigestFunc should not be called for already-verified path")
	}
}

func TestPrefetchBureauVersion_VerificationBlocksUpdate(t *testing.T) {
	t.Parallel()

	// Bundle server returns 404 — no bundle available.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil // prefetch succeeds
	}

	version := &schema.BureauVersion{
		DaemonStorePath: "/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon/bin/bureau-daemon",
	}

	err := daemon.prefetchBureauVersion(context.Background(), version)
	if err == nil {
		t.Fatal("expected error when enforcement=require and no bundle exists")
	}
	if !strings.Contains(err.Error(), "provenance") {
		t.Errorf("error = %v, want containing 'provenance'", err)
	}
}

func TestVerifyStorePathProvenance_RedirectRejected(t *testing.T) {
	t.Parallel()

	// Server returns 302 redirect to an "attacker" server. The client
	// should NOT follow the redirect — it should treat the 302
	// response as a non-200 error.
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect was followed to attacker server")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"fake":"bundle"}`))
	}))
	t.Cleanup(attacker.Close)

	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(cache.Close)

	daemon, _ := newTestDaemon(t)
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: cache.URL}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		return []byte("fake-digest-for-test-only-32-by"), nil
	}

	err := daemon.verifyStorePathProvenance(
		context.Background(), "daemon",
		"/nix/store/"+validTestHash+"-bureau-daemon")
	if err == nil {
		t.Fatal("expected error when cache returns redirect under require enforcement")
	}
	// The redirect is not followed, so provenance.FetchBundle sees the
	// 302 status and returns an HTTP error (not ErrNoBundleFound).
	if !strings.Contains(err.Error(), "provenance") {
		t.Errorf("error = %v, want containing 'provenance'", err)
	}
}

func TestVerifyStorePathProvenance_UnexpectedStatusRequireBlocks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"fake":"bundle"}`))
	}))
	t.Cleanup(server.Close)

	daemon, _ := newTestDaemon(t)
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementRequire)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		return []byte("fake-digest-for-test-only-32-by"), nil
	}
	daemon.verifyBundleFunc = func(bundleBytes []byte, digestAlgorithm string, artifactDigest []byte) provenance.Result {
		return provenance.Result{Status: provenance.Status(99)}
	}

	err := daemon.verifyStorePathProvenance(
		context.Background(), "daemon",
		"/nix/store/"+validTestHash+"-bureau-daemon")
	if err == nil {
		t.Fatal("expected error for unexpected status under require enforcement")
	}
	if !strings.Contains(err.Error(), "unexpected verification status") {
		t.Errorf("error = %v, want containing 'unexpected verification status'", err)
	}
}

func TestVerifyStorePathProvenance_UnexpectedStatusWarnAllows(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"fake":"bundle"}`))
	}))
	t.Cleanup(server.Close)

	daemon, _ := newTestDaemon(t)
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementWarn)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		return []byte("fake-digest-for-test-only-32-by"), nil
	}
	daemon.verifyBundleFunc = func(bundleBytes []byte, digestAlgorithm string, artifactDigest []byte) provenance.Result {
		return provenance.Result{Status: provenance.Status(99)}
	}

	err := daemon.verifyStorePathProvenance(
		context.Background(), "daemon",
		"/nix/store/"+validTestHash+"-bureau-daemon")
	if err != nil {
		t.Fatalf("expected no error for unexpected status under warn enforcement, got %v", err)
	}
}

func TestVerifyStorePathProvenance_UnexpectedStatusLogAllows(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"fake":"bundle"}`))
	}))
	t.Cleanup(server.Close)

	daemon, _ := newTestDaemon(t)
	daemon.provenanceVerifier = newTestVerifier(t, schema.EnforcementLog)
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}
	daemon.narDigestFunc = func(ctx context.Context, storePath string) ([]byte, error) {
		return []byte("fake-digest-for-test-only-32-by"), nil
	}
	daemon.verifyBundleFunc = func(bundleBytes []byte, digestAlgorithm string, artifactDigest []byte) provenance.Result {
		return provenance.Result{Status: provenance.Status(99)}
	}

	err := daemon.verifyStorePathProvenance(
		context.Background(), "daemon",
		"/nix/store/"+validTestHash+"-bureau-daemon")
	if err != nil {
		t.Fatalf("expected no error for unexpected status under log enforcement, got %v", err)
	}
}

// --- Adversarial tests ---
//
// The tests below verify that provenance verification cannot be
// bypassed through error injection, malformed state events, or
// enforcement level manipulation. Each test represents a specific
// attack scenario identified during security review.

// newSyncProvenanceDaemon creates a daemon with a mock Matrix server
// and session, suitable for testing syncProvenance. The fleetRoomID
// is wired to the mock state so GetState calls resolve correctly.
func newSyncProvenanceDaemon(t *testing.T, state *mockMatrixState) *Daemon {
	t.Helper()

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: server.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon.session = session
	daemon.fleetRoomID = mustRoomID("!fleet:test.local")
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return daemon
}

// validProvenanceRoots returns a ProvenanceRootsContent with synthetic
// but parseable PEM material, suitable for NewVerifier construction.
func validProvenanceRoots(t *testing.T) schema.ProvenanceRootsContent {
	t.Helper()

	caPEM, rekorPEM := generateTestPEM(t)
	return schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			"test": {
				FulcioRootPEM:     string(caPEM),
				RekorPublicKeyPEM: string(rekorPEM),
			},
		},
	}
}

// validProvenancePolicy returns a ProvenancePolicyContent that
// references the "test" root set from validProvenanceRoots.
func validProvenancePolicy() schema.ProvenancePolicyContent {
	return schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"nix_store_paths": schema.EnforcementRequire,
		},
	}
}

// publishProvenanceState sets both roots and policy state events in
// the mock state for the fleet room.
func publishProvenanceState(t *testing.T, state *mockMatrixState, roots schema.ProvenanceRootsContent, policy schema.ProvenancePolicyContent) {
	t.Helper()
	state.setStateEvent("!fleet:test.local", schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent("!fleet:test.local", schema.EventTypeProvenancePolicy, "", policy)
}

// TestSyncProvenance_RetainsVerifierOnTransientError verifies that a
// transient Matrix API error does not disable the provenance verifier.
// Attack scenario: an attacker induces a network partition or
// homeserver restart, causing GetState to fail, which previously
// would nil the verifier and disable all verification until the next
// successful sync.
func TestSyncProvenance_RetainsVerifierOnTransientError(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier should be configured.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync with valid roots and policy")
	}
	if daemon.provenanceStatus == nil {
		t.Fatal("expected status after initial sync")
	}
	initialVerifier := daemon.provenanceVerifier

	// Simulate a transient error by pointing the daemon at a server
	// that returns 500 for all requests.
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_UNKNOWN",
			"error":   "internal server error",
		})
	}))
	t.Cleanup(errorServer.Close)

	errorClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: errorServer.URL,
	})
	if err != nil {
		t.Fatalf("creating error client: %v", err)
	}
	errorSession, err := errorClient.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating error session: %v", err)
	}
	t.Cleanup(func() { errorSession.Close() })
	daemon.session = errorSession

	// Sync with errors: verifier must be retained.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("verifier was cleared on transient error — verification bypass")
	}
	if daemon.provenanceVerifier != initialVerifier {
		t.Error("verifier was replaced on transient error — should retain previous")
	}
	if daemon.provenanceStatus == nil {
		t.Fatal("status was cleared on transient error")
	}
}

// TestSyncProvenance_RetainsVerifierOnMalformedRoots verifies that
// publishing empty roots (which causes NewVerifier to fail) does not
// disable the previously configured verifier.
// Attack scenario: an attacker with fleet room write access publishes
// empty ProvenanceRootsContent to disable verification.
func TestSyncProvenance_RetainsVerifierOnMalformedRoots(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier configured.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync")
	}
	initialVerifier := daemon.provenanceVerifier

	// Attacker publishes empty roots. NewVerifier will reject
	// this ("no root sets defined").
	state.setStateEvent("!fleet:test.local", schema.EventTypeProvenanceRoots, "",
		schema.ProvenanceRootsContent{Roots: map[string]schema.ProvenanceTrustRoot{}})

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("verifier was cleared when roots became empty — verification bypass")
	}
	if daemon.provenanceVerifier != initialVerifier {
		t.Error("verifier was replaced — should retain previous on malformed config")
	}
}

// TestSyncProvenance_RetainsVerifierOnPolicyDeleted verifies that
// deleting the policy state event (while roots still exist) does not
// disable the previously configured verifier.
// Attack scenario: an attacker deletes the policy event to bypass
// verification while leaving roots in place to avoid suspicion.
func TestSyncProvenance_RetainsVerifierOnPolicyDeleted(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier configured.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync")
	}
	initialVerifier := daemon.provenanceVerifier

	// Attacker deletes the policy. mockMatrixState returns 404
	// for the policy event but roots still exist.
	state.mu.Lock()
	policyKey := "!fleet:test.local\x00" + string(schema.EventTypeProvenancePolicy) + "\x00"
	delete(state.stateEvents, policyKey)
	state.mu.Unlock()

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("verifier was cleared when policy was deleted but roots exist — verification bypass")
	}
	if daemon.provenanceVerifier != initialVerifier {
		t.Error("verifier was replaced — should retain previous when only policy is deleted")
	}
}

// TestSyncProvenance_RetainsVerifierOnDanglingRootRef verifies that
// publishing a policy with a TrustedIdentity referencing a
// non-existent root set does not disable the previously configured
// verifier.
// Attack scenario: an attacker publishes a malformed policy to trigger
// NewVerifier failure, which previously would nil the verifier.
func TestSyncProvenance_RetainsVerifierOnDanglingRootRef(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier configured.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync")
	}
	initialVerifier := daemon.provenanceVerifier

	// Attacker publishes policy referencing non-existent root set.
	maliciousPolicy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "fake",
				Roots:          "nonexistent_root_set",
				Issuer:         "https://evil.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"nix_store_paths": schema.EnforcementRequire,
		},
	}
	state.setStateEvent("!fleet:test.local", schema.EventTypeProvenancePolicy, "", maliciousPolicy)

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("verifier was cleared on dangling root ref — verification bypass")
	}
	if daemon.provenanceVerifier != initialVerifier {
		t.Error("verifier was replaced — should retain previous on invalid policy")
	}
}

// TestSyncProvenance_ClearsVerifierWhenBothAbsent verifies that
// syncProvenance correctly clears the verifier when both roots and
// policy are absent (404). This is the legitimate "no provenance
// configured" state — the only path that should nil the verifier.
func TestSyncProvenance_ClearsVerifierWhenBothAbsent(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier configured.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Admin removes both roots and policy (legitimate deconfiguration).
	state.mu.Lock()
	rootsKey := "!fleet:test.local\x00" + string(schema.EventTypeProvenanceRoots) + "\x00"
	policyKey := "!fleet:test.local\x00" + string(schema.EventTypeProvenancePolicy) + "\x00"
	delete(state.stateEvents, rootsKey)
	delete(state.stateEvents, policyKey)
	state.mu.Unlock()

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier != nil {
		t.Error("verifier should be nil when both roots and policy are absent")
	}
	if daemon.provenanceStatus != nil {
		t.Error("status should be nil when both roots and policy are absent")
	}
}

// TestSyncProvenance_NilVerifierNeverConfigured verifies that
// syncProvenance does not create a verifier when no roots or policy
// have ever been published. This is the initial fleet state.
func TestSyncProvenance_NilVerifierNeverConfigured(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	// No provenance state events published.
	daemon := newSyncProvenanceDaemon(t, state)

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier != nil {
		t.Error("verifier should be nil when no provenance is configured")
	}
	if daemon.provenanceStatus != nil {
		t.Error("status should be nil when no provenance is configured")
	}
}

// TestNewVerifier_RejectsUnknownEnforcementLevel verifies that
// NewVerifier rejects policies with unrecognized enforcement level
// strings. This prevents typos ("requre") or attacker-crafted
// strings ("disabled") from silently downgrading enforcement.
func TestNewVerifier_RejectsUnknownEnforcementLevel(t *testing.T) {
	t.Parallel()

	roots := validProvenanceRoots(t)

	testCases := []struct {
		name  string
		level schema.EnforcementLevel
	}{
		{"empty_string", ""},
		{"typo_requre", "requre"},
		{"attacker_disabled", "disabled"},
		{"uppercase_REQUIRE", "REQUIRE"},
		{"random_garbage", "xyzzy"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			policy := schema.ProvenancePolicyContent{
				TrustedIdentities: []schema.TrustedIdentity{
					{
						Name:           "ci",
						Roots:          "test",
						Issuer:         "https://test.example.com",
						SubjectPattern: "*",
					},
				},
				Enforcement: map[string]schema.EnforcementLevel{
					"nix_store_paths": testCase.level,
				},
			}

			verifier, err := provenance.NewVerifier(roots, policy)
			if err == nil {
				t.Fatalf("expected error for unknown enforcement level %q, got verifier", testCase.level)
			}
			if verifier != nil {
				t.Error("verifier should be nil on error")
			}
			if !strings.Contains(err.Error(), "unknown level") {
				t.Errorf("error = %v, want containing 'unknown level'", err)
			}
		})
	}
}

// TestSyncProvenance_UnknownEnforcementRetainsPrevious verifies that
// publishing a policy with a typo in the enforcement level does not
// disable the previously configured verifier.
// Attack scenario: an attacker publishes enforcement="requre"
// (typo), which previously would silently default to log (non-blocking).
// Now NewVerifier rejects it, and the daemon retains the previous
// verifier with the correct enforcement.
func TestSyncProvenance_UnknownEnforcementRetainsPrevious(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	roots := validProvenanceRoots(t)
	policy := validProvenancePolicy()
	publishProvenanceState(t, state, roots, policy)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: verifier configured with enforcement=require.
	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("expected verifier after initial sync")
	}
	initialVerifier := daemon.provenanceVerifier

	// Attacker publishes policy with typo enforcement level.
	maliciousPolicy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"nix_store_paths": "requre",
		},
	}
	state.setStateEvent("!fleet:test.local", schema.EventTypeProvenancePolicy, "", maliciousPolicy)

	daemon.syncProvenance(context.Background())
	if daemon.provenanceVerifier == nil {
		t.Fatal("verifier was cleared on unknown enforcement level — verification bypass")
	}
	if daemon.provenanceVerifier != initialVerifier {
		t.Error("verifier was replaced — should retain previous on invalid enforcement")
	}
	// Verify the original enforcement level is still active.
	enforcement := daemon.provenanceVerifier.Enforcement("nix_store_paths")
	if enforcement != schema.EnforcementRequire {
		t.Errorf("enforcement = %q, want %q — enforcement was downgraded", enforcement, schema.EnforcementRequire)
	}
}

// TestSyncFleetCache_RetainsCacheConfigOnTransientError verifies that
// a transient Matrix API error does not clear the fleet cache config.
// Attack scenario: an attacker induces a transient Matrix error, which
// previously would nil fleetCacheConfig. When fleetCacheConfig is nil,
// verifyStorePathProvenance skips bundle fetches entirely — bypassing
// provenance verification under warn/log enforcement.
func TestSyncFleetCache_RetainsCacheConfigOnTransientError(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	cacheConfig := schema.FleetCacheContent{
		URL:  "https://cache.infra.bureau.foundation",
		Name: "main",
	}
	state.setStateEvent("!fleet:test.local", schema.EventTypeFleetCache, "", cacheConfig)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: cache config should be set.
	daemon.syncFleetCache(context.Background())
	if daemon.fleetCacheConfig == nil {
		t.Fatal("expected fleet cache config after initial sync")
	}
	if daemon.fleetCacheConfig.URL != "https://cache.infra.bureau.foundation" {
		t.Errorf("cache URL = %q, want %q", daemon.fleetCacheConfig.URL, "https://cache.infra.bureau.foundation")
	}

	// Simulate a transient error by pointing the daemon at a server
	// that returns 500.
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"errcode": "M_UNKNOWN",
			"error":   "internal server error",
		})
	}))
	t.Cleanup(errorServer.Close)

	errorClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: errorServer.URL,
	})
	if err != nil {
		t.Fatalf("creating error client: %v", err)
	}
	errorSession, err := errorClient.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating error session: %v", err)
	}
	t.Cleanup(func() { errorSession.Close() })
	daemon.session = errorSession

	// Sync with transient error: cache config must be retained.
	daemon.syncFleetCache(context.Background())
	if daemon.fleetCacheConfig == nil {
		t.Fatal("fleet cache config was cleared on transient error — provenance verification bypass")
	}
	if daemon.fleetCacheConfig.URL != "https://cache.infra.bureau.foundation" {
		t.Errorf("cache URL changed to %q on transient error — should retain previous", daemon.fleetCacheConfig.URL)
	}
}

// TestSyncFleetCache_ClearsOnAbsent verifies that syncFleetCache
// correctly clears the cache config when the fleet cache event is
// absent (404). This is the legitimate "no cache configured" state.
func TestSyncFleetCache_ClearsOnAbsent(t *testing.T) {
	t.Parallel()

	state := newMockMatrixState()
	cacheConfig := schema.FleetCacheContent{URL: "https://cache.example.com"}
	state.setStateEvent("!fleet:test.local", schema.EventTypeFleetCache, "", cacheConfig)

	daemon := newSyncProvenanceDaemon(t, state)

	// Initial sync: cache config set.
	daemon.syncFleetCache(context.Background())
	if daemon.fleetCacheConfig == nil {
		t.Fatal("expected fleet cache config after initial sync")
	}

	// Remove the fleet cache event (admin deconfigures cache).
	state.mu.Lock()
	cacheKey := "!fleet:test.local\x00" + string(schema.EventTypeFleetCache) + "\x00"
	delete(state.stateEvents, cacheKey)
	state.mu.Unlock()

	// Sync again: cache config should be cleared.
	daemon.syncFleetCache(context.Background())
	if daemon.fleetCacheConfig != nil {
		t.Error("fleet cache config should be nil when event is absent")
	}
}

// TestVerifyStorePathProvenance_EmptyEnforcementMap verifies that
// when the enforcement map has no entry for "nix_store_paths", the
// daemon defaults to log enforcement (non-blocking). This is
// documented behavior but worth testing at the daemon level to
// ensure we know the fail-open surface.
func TestVerifyStorePathProvenance_EmptyEnforcementMap(t *testing.T) {
	t.Parallel()

	// Create a verifier with an empty enforcement map.
	roots := validProvenanceRoots(t)
	policy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{},
	}
	verifier, err := provenance.NewVerifier(roots, policy)
	if err != nil {
		t.Fatalf("creating verifier with empty enforcement: %v", err)
	}

	// Bundle server returns 404 — no bundle.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	daemon, _ := newTestDaemon(t)
	daemon.provenanceVerifier = verifier
	daemon.fleetCacheConfig = &schema.FleetCacheContent{URL: server.URL}

	// With empty enforcement map, nix_store_paths defaults to log,
	// so verification failure should NOT block.
	err = daemon.verifyStorePathProvenance(
		context.Background(), "daemon",
		"/nix/store/abcdefghijklmnopqrstuvwxyz012345-bureau-daemon")
	if err != nil {
		t.Fatalf("expected no error with empty enforcement map (defaults to log), got %v", err)
	}

	// Verify the enforcement level the verifier reports.
	enforcement := verifier.Enforcement("nix_store_paths")
	if enforcement != schema.EnforcementLog {
		t.Errorf("enforcement for missing category = %q, want %q", enforcement, schema.EnforcementLog)
	}
}
