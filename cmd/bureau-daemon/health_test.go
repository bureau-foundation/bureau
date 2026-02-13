// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestHealthCheckDefaults(t *testing.T) {
	t.Parallel()

	t.Run("all defaults applied", func(t *testing.T) {
		t.Parallel()
		config := healthCheckDefaults(&schema.HealthCheck{
			Endpoint:        "/health",
			IntervalSeconds: 10,
		})
		if config.TimeoutSeconds != 5 {
			t.Errorf("TimeoutSeconds = %d, want 5", config.TimeoutSeconds)
		}
		if config.FailureThreshold != 3 {
			t.Errorf("FailureThreshold = %d, want 3", config.FailureThreshold)
		}
		if config.GracePeriodSeconds != 30 {
			t.Errorf("GracePeriodSeconds = %d, want 30", config.GracePeriodSeconds)
		}
	})

	t.Run("explicit values preserved", func(t *testing.T) {
		t.Parallel()
		config := healthCheckDefaults(&schema.HealthCheck{
			Endpoint:           "/status",
			IntervalSeconds:    5,
			TimeoutSeconds:     2,
			FailureThreshold:   5,
			GracePeriodSeconds: 60,
		})
		if config.TimeoutSeconds != 2 {
			t.Errorf("TimeoutSeconds = %d, want 2", config.TimeoutSeconds)
		}
		if config.FailureThreshold != 5 {
			t.Errorf("FailureThreshold = %d, want 5", config.FailureThreshold)
		}
		if config.GracePeriodSeconds != 60 {
			t.Errorf("GracePeriodSeconds = %d, want 60", config.GracePeriodSeconds)
		}
	})

	t.Run("does not mutate original", func(t *testing.T) {
		t.Parallel()
		original := &schema.HealthCheck{
			Endpoint:        "/health",
			IntervalSeconds: 10,
		}
		_ = healthCheckDefaults(original)
		if original.TimeoutSeconds != 0 {
			t.Errorf("original TimeoutSeconds mutated to %d", original.TimeoutSeconds)
		}
		if original.FailureThreshold != 0 {
			t.Errorf("original FailureThreshold mutated to %d", original.FailureThreshold)
		}
	})
}

func TestCheckHealth(t *testing.T) {
	t.Parallel()

	socketDir := testutil.SocketDir(t)

	t.Run("healthy proxy returns true", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(socketDir, "healthy.sock")
		startMockAdminServer(t, socketPath, http.StatusOK)

		daemon, _ := newTestDaemon(t)
		daemon.runDir = principal.DefaultRunDir
		daemon.adminSocketPathFunc = func(localpart string) string { return socketPath }
		daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

		if !daemon.checkHealth(context.Background(), "test/agent", "/health", 5) {
			t.Error("checkHealth returned false for a healthy proxy")
		}
	})

	t.Run("unhealthy proxy returns false", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(socketDir, "unhealthy.sock")
		startMockAdminServer(t, socketPath, http.StatusServiceUnavailable)

		daemon, _ := newTestDaemon(t)
		daemon.runDir = principal.DefaultRunDir
		daemon.adminSocketPathFunc = func(localpart string) string { return socketPath }
		daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

		if daemon.checkHealth(context.Background(), "test/agent", "/health", 5) {
			t.Error("checkHealth returned true for an unhealthy proxy (HTTP 503)")
		}
	})

	t.Run("missing socket returns false", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.runDir = principal.DefaultRunDir
		daemon.adminSocketPathFunc = func(localpart string) string { return "/nonexistent/proxy.sock" }
		daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

		if daemon.checkHealth(context.Background(), "test/agent", "/health", 1) {
			t.Error("checkHealth returned true for a missing socket")
		}
	})

	t.Run("cancelled context returns false", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(socketDir, "cancelled.sock")
		startMockAdminServer(t, socketPath, http.StatusOK)

		daemon, _ := newTestDaemon(t)
		daemon.runDir = principal.DefaultRunDir
		daemon.adminSocketPathFunc = func(localpart string) string { return socketPath }
		daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if daemon.checkHealth(ctx, "test/agent", "/health", 5) {
			t.Error("checkHealth returned true with a cancelled context")
		}
	})
}

func TestHealthMonitorStopDuringGracePeriod(t *testing.T) {
	t.Parallel()

	daemon, fakeClock := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a health monitor with a long grace period so it's still
	// waiting when we stop it.
	daemon.startHealthMonitor(ctx, "test/agent", &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		GracePeriodSeconds: 3600, // 1 hour — we'll stop it long before this
	})

	// Wait for the grace period timer to be registered.
	fakeClock.WaitForTimers(1)

	// Verify the monitor is registered.
	daemon.healthMonitorsMu.Lock()
	if _, exists := daemon.healthMonitors["test/agent"]; !exists {
		t.Fatal("health monitor not registered after startHealthMonitor")
	}
	daemon.healthMonitorsMu.Unlock()

	// Stop should cancel the goroutine and return promptly (via
	// context cancellation, not grace period expiry).
	done := make(chan struct{})
	go func() {
		daemon.stopHealthMonitor("test/agent")
		close(done)
	}()

	testutil.RequireClosed(t, done, 5*time.Second, "stopHealthMonitor did not return")

	// Verify the monitor is deregistered.
	daemon.healthMonitorsMu.Lock()
	if _, exists := daemon.healthMonitors["test/agent"]; exists {
		t.Error("health monitor still registered after stopHealthMonitor")
	}
	daemon.healthMonitorsMu.Unlock()
}

func TestHealthMonitorIdempotentStart(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	healthCheck := &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		GracePeriodSeconds: 3600,
	}

	daemon.startHealthMonitor(ctx, "test/agent", healthCheck)
	daemon.startHealthMonitor(ctx, "test/agent", healthCheck) // Second call is a no-op.

	daemon.healthMonitorsMu.Lock()
	count := len(daemon.healthMonitors)
	daemon.healthMonitorsMu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 health monitor, got %d", count)
	}

	daemon.stopAllHealthMonitors()
}

func TestHealthMonitorStopNonexistent(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Should be a no-op, not a panic or hang.
	daemon.stopHealthMonitor("nonexistent/agent")
}

func TestStopAllHealthMonitors(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, localpart := range []string{"agent/one", "agent/two", "agent/three"} {
		daemon.startHealthMonitor(ctx, localpart, &schema.HealthCheck{
			Endpoint:           "/health",
			IntervalSeconds:    1,
			GracePeriodSeconds: 3600,
		})
	}

	daemon.healthMonitorsMu.Lock()
	if len(daemon.healthMonitors) != 3 {
		t.Fatalf("expected 3 health monitors, got %d", len(daemon.healthMonitors))
	}
	daemon.healthMonitorsMu.Unlock()

	done := make(chan struct{})
	go func() {
		daemon.stopAllHealthMonitors()
		close(done)
	}()

	testutil.RequireClosed(t, done, 5*time.Second, "stopAllHealthMonitors did not return")

	daemon.healthMonitorsMu.Lock()
	if len(daemon.healthMonitors) != 0 {
		t.Errorf("expected 0 health monitors after stopAll, got %d", len(daemon.healthMonitors))
	}
	daemon.healthMonitorsMu.Unlock()
}

func TestHealthMonitorThresholdTriggersRollback(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
		localpart    = "agent/test"
	)

	// Mock Matrix: principal still in config with credentials.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: localpart,
			AutoStart: true,
		}},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, localpart, schema.Credentials{
		Ciphertext: "encrypted-creds",
	})

	// Capture messages sent to the config room.
	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				messages = append(messages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	// Mock launcher tracks IPC calls. Signals rollbackDone when the
	// create-sandbox (the final IPC in a rollback sequence) arrives.
	rollbackDone := make(chan struct{}, 1)
	var (
		launcherMu    sync.Mutex
		ipcActions    []string
		ipcPrincipals []string
		lastSpec      *schema.SandboxSpec
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, request.Action)
		ipcPrincipals = append(ipcPrincipals, request.Principal)
		lastSpec = request.SandboxSpec
		launcherMu.Unlock()
		if request.Action == "create-sandbox" {
			select {
			case rollbackDone <- struct{}{}:
			default:
			}
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	// Mock admin server: always unhealthy. Signals after each request
	// so the test knows when the health check HTTP call has completed.
	adminSocket := filepath.Join(socketDir, "admin.sock")
	checkHandled := make(chan struct{}, 10)
	adminListener, err := net.Listen("unix", adminSocket)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", adminSocket, err)
	}
	adminServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			select {
			case checkHandled <- struct{}{}:
			default:
			}
		}),
	}
	go adminServer.Serve(adminListener)
	t.Cleanup(func() { adminServer.Close(); adminListener.Close() })

	previousSpec := &schema.SandboxSpec{Command: []string{"/bin/old-agent"}}

	daemon, fakeClock := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/new-agent"}}
	daemon.previousSpecs[localpart] = previousSpec
	daemon.lastTemplates[localpart] = &schema.TemplateContent{
		HealthCheck: &schema.HealthCheck{
			Endpoint:        "/health",
			IntervalSeconds: 1,
		},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return adminSocket }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startHealthMonitor(ctx, localpart, &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		FailureThreshold:   2,
		GracePeriodSeconds: 1,
		TimeoutSeconds:     1,
	})

	// Advance past grace period.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(1 * time.Second)

	// Wait for the ticker to be registered after grace period.
	fakeClock.WaitForTimers(1)

	// First tick → first health check (failure 1).
	fakeClock.Advance(1 * time.Second)
	testutil.RequireReceive(t, checkHandled, 5*time.Second, "waiting for first health check")

	// Second tick → second health check (failure 2 → rollback).
	fakeClock.Advance(1 * time.Second)

	// Wait for rollback to complete (destroy + create IPC).
	testutil.RequireReceive(t, rollbackDone, 5*time.Second, "waiting for rollback")

	// Verify the create-sandbox used the previous spec.
	launcherMu.Lock()
	if lastSpec == nil {
		t.Error("expected create-sandbox to have a SandboxSpec")
	} else if len(lastSpec.Command) == 0 || lastSpec.Command[0] != "/bin/old-agent" {
		t.Errorf("rollback used wrong spec: got command %v, want [/bin/old-agent]", lastSpec.Command)
	}

	// Verify IPC sequence: destroy then create.
	destroyIndex := -1
	createIndex := -1
	for index, action := range ipcActions {
		if action == "destroy-sandbox" && ipcPrincipals[index] == localpart && destroyIndex == -1 {
			destroyIndex = index
		}
		if action == "create-sandbox" && ipcPrincipals[index] == localpart && createIndex == -1 {
			createIndex = index
		}
	}
	launcherMu.Unlock()

	if destroyIndex >= createIndex {
		t.Errorf("destroy (index %d) should come before create (index %d)", destroyIndex, createIndex)
	}

	// Verify principal is running again.
	daemon.reconcileMu.RLock()
	isRunning := daemon.running[localpart]
	daemon.reconcileMu.RUnlock()
	if !isRunning {
		t.Error("principal should be running after rollback")
	}

	// Verify previousSpecs cleared after rollback (prevents double-rollback).
	daemon.reconcileMu.RLock()
	hasPreviousSpec := daemon.previousSpecs[localpart] != nil
	daemon.reconcileMu.RUnlock()
	if hasPreviousSpec {
		t.Error("previousSpecs should be cleared after successful rollback")
	}

	// Verify a rollback message was sent to the config room.
	messagesMu.Lock()
	foundRollbackMessage := false
	for _, message := range messages {
		if strings.Contains(message, "Rolled back") && strings.Contains(message, localpart) {
			foundRollbackMessage = true
			break
		}
	}
	messagesMu.Unlock()
	if !foundRollbackMessage {
		t.Error("expected rollback message in config room")
	}
}

func TestRollbackNoPreviousSpec(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
		localpart    = "agent/test"
	)

	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: localpart,
			AutoStart: true,
		}},
	})

	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				messages = append(messages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var (
		launcherMu sync.Mutex
		ipcActions []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, request.Action)
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/agent"}}
	daemon.adminSocketPathFunc = func(localpart string) string {
		return filepath.Join(socketDir, localpart+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// Call rollbackPrincipal directly — no previous spec.
	daemon.rollbackPrincipal(context.Background(), localpart)

	// Should have destroyed but NOT recreated.
	launcherMu.Lock()
	foundDestroy := false
	foundCreate := false
	for _, action := range ipcActions {
		if action == "destroy-sandbox" {
			foundDestroy = true
		}
		if action == "create-sandbox" {
			foundCreate = true
		}
	}
	launcherMu.Unlock()

	if !foundDestroy {
		t.Error("expected destroy-sandbox call")
	}
	if foundCreate {
		t.Error("should NOT call create-sandbox when no previous spec exists")
	}

	// Principal should NOT be running.
	if daemon.running[localpart] {
		t.Error("principal should not be running after rollback with no previous spec")
	}

	// Should have sent CRITICAL message.
	messagesMu.Lock()
	foundCritical := false
	for _, message := range messages {
		if strings.Contains(message, "CRITICAL") && strings.Contains(message, localpart) {
			foundCritical = true
			break
		}
	}
	messagesMu.Unlock()
	if !foundCritical {
		t.Error("expected CRITICAL message when rollback has no previous spec")
	}
}

func TestRollbackPrincipalAlreadyStopped(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Principal is not running — rollback should be a no-op.
	daemon.rollbackPrincipal(context.Background(), "nonexistent/agent")

	// No panic, no IPC calls needed — success.
}

func TestReconcileStartsHealthMonitorForTemplate(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	state := newMockMatrixState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent"},
		HealthCheck: &schema.HealthCheck{
			Endpoint:        "/health",
			IntervalSeconds: 10,
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "agent/test",
			Template:  "bureau/template:test-template",
			AutoStart: true,
		}},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["agent/test"] {
		t.Fatal("principal should be running after reconcile")
	}

	// Verify a health monitor was started.
	daemon.healthMonitorsMu.Lock()
	_, hasMonitor := daemon.healthMonitors["agent/test"]
	daemon.healthMonitorsMu.Unlock()

	if !hasMonitor {
		t.Error("expected health monitor to be started for principal with HealthCheck in template")
	}

	// Verify lastTemplates was populated.
	if daemon.lastTemplates["agent/test"] == nil {
		t.Error("expected lastTemplates to be populated after reconcile")
	}
}

func TestReconcileNoHealthMonitorWithoutHealthCheck(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	state := newMockMatrixState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent"},
		// No HealthCheck
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "agent/test",
			Template:  "bureau/template:test-template",
			AutoStart: true,
		}},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["agent/test"] {
		t.Fatal("principal should be running")
	}

	// No HealthCheck in template → no health monitor.
	daemon.healthMonitorsMu.Lock()
	_, hasMonitor := daemon.healthMonitors["agent/test"]
	daemon.healthMonitorsMu.Unlock()

	if hasMonitor {
		t.Error("health monitor should NOT be started for template without HealthCheck")
	}
}

func TestReconcileStopsHealthMonitorOnDestroy(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	// Config with NO principals — the running one should be destroyed.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		// No principals — agent/test should be stopped.
	})

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastSpecs["agent/test"] = &schema.SandboxSpec{Command: []string{"/bin/agent"}}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Start a health monitor manually (simulating one that was started
	// during previous reconcile when the principal was created).
	ctx := context.Background()
	daemon.startHealthMonitor(ctx, "agent/test", &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		GracePeriodSeconds: 3600,
	})

	daemon.healthMonitorsMu.Lock()
	if _, exists := daemon.healthMonitors["agent/test"]; !exists {
		t.Fatal("health monitor should exist before reconcile")
	}
	daemon.healthMonitorsMu.Unlock()

	// Reconcile: principal removed from config → destroy → health monitor stopped.
	if err := daemon.reconcile(ctx); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if daemon.running["agent/test"] {
		t.Error("principal should not be running after removal from config")
	}

	daemon.healthMonitorsMu.Lock()
	_, stillExists := daemon.healthMonitors["agent/test"]
	daemon.healthMonitorsMu.Unlock()

	if stillExists {
		t.Error("health monitor should be stopped after principal is destroyed")
	}
}

func TestHealthMonitorRecoveryResetsCounter(t *testing.T) {
	t.Parallel()

	const localpart = "agent/test"

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	socketDir := testutil.SocketDir(t)
	adminSocket := filepath.Join(socketDir, "admin.sock")

	// Track health check requests. The response pattern verifies that
	// transient failures below the threshold don't accumulate across
	// recovery events — the counter resets on each successful check.
	var requestCount atomic.Int32
	checkHandled := make(chan struct{}, 10)

	listener, err := net.Listen("unix", adminSocket)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", adminSocket, err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := requestCount.Add(1)
			// Pattern: fail, fail, succeed, fail, fail, succeed
			// With threshold=3, the reset at check 3 prevents reaching
			// the threshold. Max consecutive failures = 2.
			switch {
			case count <= 2:
				w.WriteHeader(http.StatusServiceUnavailable)
			case count == 3:
				w.WriteHeader(http.StatusOK)
			case count <= 5:
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				w.WriteHeader(http.StatusOK)
			}
			select {
			case checkHandled <- struct{}{}:
			default:
			}
		}),
	}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})

	daemon, _ := newTestDaemon(t)
	daemon.clock = fakeClock
	daemon.runDir = principal.DefaultRunDir
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/agent"}}
	daemon.adminSocketPathFunc = func(string) string { return adminSocket }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllHealthMonitors)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.startHealthMonitor(ctx, localpart, &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		FailureThreshold:   3,
		GracePeriodSeconds: 1,
		TimeoutSeconds:     1,
	})

	// Advance past grace period.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(1 * time.Second)

	// Wait for ticker registration after grace period expires.
	fakeClock.WaitForTimers(1)

	// Drive 6 health checks through the pattern:
	// fail, fail, succeed, fail, fail, succeed
	for check := 1; check <= 6; check++ {
		fakeClock.Advance(1 * time.Second)
		testutil.RequireReceive(t, checkHandled, 5*time.Second, "waiting for health check %d", check)
	}

	// If rollback had been triggered, rollbackPrincipal would attempt
	// launcherRequest (which would fail with no launcher socket) and
	// delete running[localpart]. The principal still being marked as
	// running proves the recovery resets prevented false rollback.
	if !daemon.running[localpart] {
		t.Error("principal should still be running: transient failures with recovery should not trigger rollback")
	}
}

func TestHealthMonitorCancelDuringPolling(t *testing.T) {
	t.Parallel()

	const localpart = "agent/test"

	fakeClock := clock.Fake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	socketDir := testutil.SocketDir(t)
	adminSocket := filepath.Join(socketDir, "admin.sock")

	// Signal when the monitor has completed at least one health check,
	// proving it is past the grace period and in the active polling loop.
	checkHandled := make(chan struct{}, 10)

	listener, err := net.Listen("unix", adminSocket)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", adminSocket, err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			select {
			case checkHandled <- struct{}{}:
			default:
			}
		}),
	}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})

	daemon, _ := newTestDaemon(t)
	daemon.clock = fakeClock
	daemon.runDir = principal.DefaultRunDir
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/agent"}}
	daemon.adminSocketPathFunc = func(string) string { return adminSocket }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())

	daemon.startHealthMonitor(ctx, localpart, &schema.HealthCheck{
		Endpoint:           "/health",
		IntervalSeconds:    1,
		FailureThreshold:   3,
		GracePeriodSeconds: 1,
		TimeoutSeconds:     1,
	})

	// Advance past grace period.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(1 * time.Second)

	// Wait for ticker registration after grace period expires.
	fakeClock.WaitForTimers(1)

	// Drive one health check to confirm the monitor is in the polling loop.
	fakeClock.Advance(1 * time.Second)
	testutil.RequireReceive(t, checkHandled, 5*time.Second, "waiting for first health check")

	// Cancel the context while the monitor is in its polling loop.
	// This exercises the ctx.Done branch in the ticker select
	// (health.go:141-145), which is a different code path from the
	// grace period cancellation tested in TestHealthMonitorStopDuringGracePeriod.
	cancel()

	// The monitor's goroutine should exit promptly via ctx.Done.
	done := make(chan struct{})
	go func() {
		daemon.stopHealthMonitor(localpart)
		close(done)
	}()

	testutil.RequireClosed(t, done, 5*time.Second, "health monitor did not stop after context cancellation")

	// No rollback should have occurred — ctx.Done exits cleanly.
	if !daemon.running[localpart] {
		t.Error("principal should still be running: context cancellation should not trigger rollback")
	}
}

func TestRollbackLauncherRejectsCreate(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
		localpart    = "agent/test"
	)

	// Matrix state: principal in config with credentials (so rollback
	// reaches the create-sandbox step before the launcher rejects it).
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: localpart,
			AutoStart: true,
		}},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, localpart, schema.Credentials{
		Ciphertext: "encrypted-creds",
	})

	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				messages = append(messages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var (
		launcherMu sync.Mutex
		ipcActions []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, request.Action)
		launcherMu.Unlock()
		switch request.Action {
		case "destroy-sandbox":
			return launcherIPCResponse{OK: true}
		case "create-sandbox":
			return launcherIPCResponse{OK: false, Error: "sandbox limit exceeded"}
		default:
			return launcherIPCResponse{OK: true}
		}
	})
	t.Cleanup(func() { listener.Close() })

	previousSpec := &schema.SandboxSpec{Command: []string{"/bin/old-agent"}}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/new-agent"}}
	daemon.previousSpecs[localpart] = previousSpec
	daemon.lastTemplates[localpart] = &schema.TemplateContent{
		HealthCheck: &schema.HealthCheck{Endpoint: "/health", IntervalSeconds: 10},
	}
	daemon.adminSocketPathFunc = func(string) string { return filepath.Join(socketDir, "nonexistent.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	daemon.rollbackPrincipal(context.Background(), localpart)

	// Verify IPC sequence: destroy succeeded, create attempted.
	launcherMu.Lock()
	foundDestroy := false
	foundCreate := false
	for _, action := range ipcActions {
		if action == "destroy-sandbox" {
			foundDestroy = true
		}
		if action == "create-sandbox" {
			foundCreate = true
		}
	}
	launcherMu.Unlock()

	if !foundDestroy {
		t.Error("expected destroy-sandbox call")
	}
	if !foundCreate {
		t.Error("expected create-sandbox call (even though it was rejected)")
	}

	// Principal should NOT be running — create was rejected.
	if daemon.running[localpart] {
		t.Error("principal should not be running after failed rollback create")
	}

	// previousSpecs and lastTemplates should be cleaned up.
	if daemon.previousSpecs[localpart] != nil {
		t.Error("previousSpecs should be cleaned up after failed rollback")
	}
	if daemon.lastTemplates[localpart] != nil {
		t.Error("lastTemplates should be cleaned up after failed rollback")
	}

	// CRITICAL message should have been sent.
	messagesMu.Lock()
	foundCritical := false
	for _, message := range messages {
		if strings.Contains(message, "CRITICAL") && strings.Contains(message, "rollback") {
			foundCritical = true
			break
		}
	}
	messagesMu.Unlock()
	if !foundCritical {
		t.Error("expected CRITICAL message when rollback create-sandbox is rejected")
	}
}

func TestRollbackCredentialsMissing(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
		localpart    = "agent/test"
	)

	// Matrix state: principal in config but NO credentials. The
	// readCredentials call during rollback returns M_NOT_FOUND.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: localpart,
			AutoStart: true,
		}},
	})

	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			var content struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&content); err == nil {
				messagesMu.Lock()
				messages = append(messages, content.Body)
				messagesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var (
		launcherMu sync.Mutex
		ipcActions []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, request.Action)
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	previousSpec := &schema.SandboxSpec{Command: []string{"/bin/old-agent"}}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running[localpart] = true
	daemon.lastSpecs[localpart] = &schema.SandboxSpec{Command: []string{"/bin/new-agent"}}
	daemon.previousSpecs[localpart] = previousSpec
	daemon.lastTemplates[localpart] = &schema.TemplateContent{
		HealthCheck: &schema.HealthCheck{Endpoint: "/health", IntervalSeconds: 10},
	}
	daemon.adminSocketPathFunc = func(string) string { return filepath.Join(socketDir, "nonexistent.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	daemon.rollbackPrincipal(context.Background(), localpart)

	// Destroy should have been called, but create should NOT — the
	// credential read failure aborts before reaching create-sandbox.
	launcherMu.Lock()
	foundDestroy := false
	foundCreate := false
	for _, action := range ipcActions {
		if action == "destroy-sandbox" {
			foundDestroy = true
		}
		if action == "create-sandbox" {
			foundCreate = true
		}
	}
	launcherMu.Unlock()

	if !foundDestroy {
		t.Error("expected destroy-sandbox call")
	}
	if foundCreate {
		t.Error("create-sandbox should NOT be called when credentials are missing")
	}

	// Principal should NOT be running.
	if daemon.running[localpart] {
		t.Error("principal should not be running after failed credential read")
	}

	// previousSpecs and lastTemplates should be cleaned up.
	if daemon.previousSpecs[localpart] != nil {
		t.Error("previousSpecs should be cleaned up")
	}
	if daemon.lastTemplates[localpart] != nil {
		t.Error("lastTemplates should be cleaned up")
	}

	// CRITICAL message about credentials should have been sent.
	messagesMu.Lock()
	foundCritical := false
	for _, message := range messages {
		if strings.Contains(message, "CRITICAL") && strings.Contains(message, "credentials") {
			foundCritical = true
			break
		}
	}
	messagesMu.Unlock()
	if !foundCritical {
		t.Error("expected CRITICAL message about missing credentials")
	}
}

func TestDestroyExtrasCleansPreviousSpecs(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	// Config with NO principals — the running one should be destroyed.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{})

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+schema.MatrixEventTypeMessage+"/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$msg1"})
			return
		}
		state.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	// Principal has a populated previousSpecs entry (from a prior
	// structural restart). When the principal is destroyed (removed from
	// config), the destroy-extras pass should clean up all associated
	// map entries: running, lastSpecs, previousSpecs, lastTemplates.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastSpecs["agent/test"] = &schema.SandboxSpec{Command: []string{"/bin/agent-v2"}}
	daemon.previousSpecs["agent/test"] = &schema.SandboxSpec{Command: []string{"/bin/agent-v1"}}
	daemon.lastTemplates["agent/test"] = &schema.TemplateContent{
		HealthCheck: &schema.HealthCheck{Endpoint: "/health", IntervalSeconds: 10},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if daemon.running["agent/test"] {
		t.Error("principal should not be running after removal from config")
	}
	if daemon.lastSpecs["agent/test"] != nil {
		t.Error("lastSpecs should be cleaned up when principal is destroyed")
	}
	if daemon.previousSpecs["agent/test"] != nil {
		t.Error("previousSpecs should be cleaned up when principal is destroyed")
	}
	if daemon.lastTemplates["agent/test"] != nil {
		t.Error("lastTemplates should be cleaned up when principal is destroyed")
	}
}

// --- Test helpers ---

// startMockAdminServer starts an HTTP server on a Unix socket that responds
// to all requests with the given status code. Used to simulate a proxy's
// admin socket for health check testing.
func startMockAdminServer(t *testing.T, socketPath string, statusCode int) {
	t.Helper()

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", socketPath, err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(statusCode)
		}),
	}

	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})
}
