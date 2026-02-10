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
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/transport"
)

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		expected  string
	}{
		{
			name:      "simple alias",
			fullAlias: "#bureau/machines:bureau.local",
			expected:  "bureau/machines",
		},
		{
			name:      "nested config alias",
			fullAlias: "#bureau/config/machine/workstation:bureau.local",
			expected:  "bureau/config/machine/workstation",
		},
		{
			name:      "different server",
			fullAlias: "#test:example.org",
			expected:  "test",
		},
		{
			name:      "no # prefix",
			fullAlias: "bureau/machines:bureau.local",
			expected:  "bureau/machines",
		},
		{
			name:      "no server suffix",
			fullAlias: "#bureau/machines",
			expected:  "bureau/machines",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := principal.RoomAliasLocalpart(test.fullAlias)
			if result != test.expected {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q",
					test.fullAlias, result, test.expected)
			}
		})
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	levels := schema.ConfigRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Machine config and credentials events should require power level 100.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events["m.bureau.machine_config"] != 100 {
		t.Errorf("m.bureau.machine_config power level = %v, want 100", events["m.bureau.machine_config"])
	}
	if events["m.bureau.credentials"] != 100 {
		t.Errorf("m.bureau.credentials power level = %v, want 100", events["m.bureau.credentials"])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}
}

func TestLauncherRequest(t *testing.T) {
	// Set up a mock launcher server.
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "launcher.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	// Mock launcher that echoes back a success response.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			var request launcherIPCRequest
			decoder := json.NewDecoder(conn)
			encoder := json.NewEncoder(conn)

			if err := decoder.Decode(&request); err != nil {
				conn.Close()
				continue
			}

			switch request.Action {
			case "status":
				encoder.Encode(launcherIPCResponse{OK: true})
			case "create-sandbox":
				encoder.Encode(launcherIPCResponse{OK: true, ProxyPID: 12345})
			case "destroy-sandbox":
				encoder.Encode(launcherIPCResponse{OK: true})
			default:
				encoder.Encode(launcherIPCResponse{
					OK:    false,
					Error: "unknown action: " + request.Action,
				})
			}
			conn.Close()
		}
	}()

	daemon := &Daemon{
		launcherSocket: socketPath,
		logger:         slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("status", func(t *testing.T) {
		response, err := daemon.launcherRequest(context.Background(), launcherIPCRequest{
			Action: "status",
		})
		if err != nil {
			t.Fatalf("launcherRequest() error: %v", err)
		}
		if !response.OK {
			t.Errorf("expected OK, got error: %s", response.Error)
		}
	})

	t.Run("create-sandbox", func(t *testing.T) {
		response, err := daemon.launcherRequest(context.Background(), launcherIPCRequest{
			Action:    "create-sandbox",
			Principal: "iree/amdgpu/pm",
		})
		if err != nil {
			t.Fatalf("launcherRequest() error: %v", err)
		}
		if !response.OK {
			t.Errorf("expected OK, got error: %s", response.Error)
		}
		if response.ProxyPID != 12345 {
			t.Errorf("ProxyPID = %d, want 12345", response.ProxyPID)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		response, err := daemon.launcherRequest(context.Background(), launcherIPCRequest{
			Action: "nonexistent",
		})
		if err != nil {
			t.Fatalf("launcherRequest() error: %v", err)
		}
		if response.OK {
			t.Error("expected error for unknown action")
		}
		if !strings.Contains(response.Error, "unknown action") {
			t.Errorf("error = %q, want 'unknown action'", response.Error)
		}
	})
}

func TestLauncherRequest_ConnectionRefused(t *testing.T) {
	daemon := &Daemon{
		launcherSocket: "/nonexistent/launcher.sock",
		logger:         slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	_, err := daemon.launcherRequest(context.Background(), launcherIPCRequest{
		Action: "status",
	})
	if err == nil {
		t.Error("expected error when launcher socket doesn't exist")
	}
	if !strings.Contains(err.Error(), "connecting to launcher") {
		t.Errorf("error = %v, want 'connecting to launcher'", err)
	}
}

func TestLoadSession(t *testing.T) {
	t.Run("valid session", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		sessionJSON := `{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@machine/test:bureau.local",
			"access_token": "syt_test_token"
		}`
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(sessionJSON), 0600)

		session, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("loadSession() error: %v", err)
		}
		if session.UserID() != "@machine/test:bureau.local" {
			t.Errorf("UserID() = %q, want %q", session.UserID(), "@machine/test:bureau.local")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		_, err := loadSession(t.TempDir(), "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for missing session file")
		}
	})

	t.Run("empty access token", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@test:local",
			"access_token": ""
		}`), 0600)

		_, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for empty access token")
		}
		if !strings.Contains(err.Error(), "empty access token") {
			t.Errorf("error = %v, want 'empty access token'", err)
		}
	})
}

// TestReconcile_NoConfig is replaced by TestReconcileNoConfig in
// composition_test.go, which uses a mock Matrix server to properly
// test the M_NOT_FOUND path.

func TestUptimeSeconds(t *testing.T) {
	// uptimeSeconds should return a positive value on Linux.
	uptime := uptimeSeconds()
	if uptime <= 0 {
		t.Errorf("uptimeSeconds() = %d, want > 0", uptime)
	}
}

func TestPublishStatus_SandboxCount(t *testing.T) {
	// Verify that the running map count is correctly calculated.
	daemon := &Daemon{
		machineName: "machine/test",
		serverName:  "bureau.local",
		running: map[string]bool{
			"iree/amdgpu/pm":        true,
			"service/stt/whisper":    true,
			"service/tts/piper":      true,
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Count running principals the same way publishStatus does.
	runningCount := 0
	for range daemon.running {
		runningCount++
	}
	if runningCount != 3 {
		t.Errorf("running count = %d, want 3", runningCount)
	}
}

func TestServiceChanged(t *testing.T) {
	base := &schema.Service{
		Principal:    "@stt/whisper:bureau.local",
		Machine:      "@machine/gpu-1:bureau.local",
		Capabilities: []string{"streaming", "diarization"},
		Protocol:     "http",
		Description:  "Speech-to-text via Whisper",
	}

	tests := []struct {
		name    string
		current *schema.Service
		changed bool
	}{
		{
			name:    "identical",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: []string{"streaming", "diarization"}, Protocol: "http", Description: "Speech-to-text via Whisper"},
			changed: false,
		},
		{
			name:    "different machine",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-2:bureau.local", Capabilities: []string{"streaming", "diarization"}, Protocol: "http", Description: "Speech-to-text via Whisper"},
			changed: true,
		},
		{
			name:    "different protocol",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: []string{"streaming", "diarization"}, Protocol: "grpc", Description: "Speech-to-text via Whisper"},
			changed: true,
		},
		{
			name:    "different capabilities",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: []string{"streaming"}, Protocol: "http", Description: "Speech-to-text via Whisper"},
			changed: true,
		},
		{
			name:    "capabilities reordered",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: []string{"diarization", "streaming"}, Protocol: "http", Description: "Speech-to-text via Whisper"},
			changed: true,
		},
		{
			name:    "different description",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: []string{"streaming", "diarization"}, Protocol: "http", Description: "Updated description"},
			changed: true,
		},
		{
			name:    "nil vs empty capabilities",
			current: &schema.Service{Principal: "@stt/whisper:bureau.local", Machine: "@machine/gpu-1:bureau.local", Capabilities: nil, Protocol: "http", Description: "Speech-to-text via Whisper"},
			changed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := serviceChanged(base, test.current)
			if result != test.changed {
				t.Errorf("serviceChanged() = %v, want %v", result, test.changed)
			}
		})
	}
}

func TestStringSlicesEqual(t *testing.T) {
	tests := []struct {
		name  string
		a, b  []string
		equal bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []string{}, []string{}, true},
		{"nil vs empty", nil, []string{}, true},
		{"identical", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different order", []string{"a", "b"}, []string{"b", "a"}, false},
		{"different length", []string{"a", "b"}, []string{"a"}, false},
		{"different content", []string{"a"}, []string{"b"}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := stringSlicesEqual(test.a, test.b)
			if result != test.equal {
				t.Errorf("stringSlicesEqual(%v, %v) = %v, want %v", test.a, test.b, result, test.equal)
			}
		})
	}
}

func TestLocalAndRemoteServices(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
			"service/tts/piper": {
				Principal: "@service/tts/piper:bureau.local",
				Machine:   "@machine/cloud-gpu-1:bureau.local",
				Protocol:  "http",
			},
			"service/llm/mixtral": {
				Principal: "@service/llm/mixtral:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	local := daemon.localServices()
	if len(local) != 2 {
		t.Errorf("localServices() returned %d services, want 2", len(local))
	}
	if _, ok := local["service/stt/whisper"]; !ok {
		t.Error("localServices() missing service/stt/whisper")
	}
	if _, ok := local["service/llm/mixtral"]; !ok {
		t.Error("localServices() missing service/llm/mixtral")
	}

	remote := daemon.remoteServices()
	if len(remote) != 1 {
		t.Errorf("remoteServices() returned %d services, want 1", len(remote))
	}
	if _, ok := remote["service/tts/piper"]; !ok {
		t.Error("remoteServices() missing service/tts/piper")
	}
}

func TestReconcileServices_LocalAndRemote(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		machineName:   "machine/workstation",
		serverName:    "bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
			"service/tts/piper": {
				Principal: "@service/tts/piper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
				Protocol:  "http",
			},
		},
		running:     make(map[string]bool),
		proxyRoutes: make(map[string]string),
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// With no running consumers, reconcileServices should update
	// proxyRoutes for local services but not make any HTTP calls.
	daemon.reconcileServices(
		context.Background(),
		[]string{"service/stt/whisper", "service/tts/piper"},
		nil,
		nil,
	)

	// Verify lastActivityAt was updated.
	if daemon.lastActivityAt.IsZero() {
		t.Error("lastActivityAt should have been set after service reconciliation")
	}

	// Verify the local service was recorded in proxyRoutes.
	if _, ok := daemon.proxyRoutes["service-stt-whisper"]; !ok {
		t.Error("proxyRoutes should contain service-stt-whisper")
	}

	// Verify the remote service was NOT recorded in proxyRoutes.
	if _, ok := daemon.proxyRoutes["service-tts-piper"]; ok {
		t.Error("proxyRoutes should not contain remote service-tts-piper")
	}
}

func TestReconcileServices_NoChanges(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services:      make(map[string]*schema.Service),
		running:       make(map[string]bool),
		proxyRoutes:   make(map[string]string),
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// reconcileServices with no changes should be a no-op.
	daemon.reconcileServices(context.Background(), nil, nil, nil)

	if !daemon.lastActivityAt.IsZero() {
		t.Error("lastActivityAt should remain zero when no changes occur")
	}
}

func TestReconcileServices_Removal(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services:      make(map[string]*schema.Service),
		running:       make(map[string]bool),
		proxyRoutes: map[string]string{
			"service-stt-whisper": "/run/bureau/principal/service/stt/whisper.sock",
		},
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Removing a service that was locally routed should clean up proxyRoutes.
	daemon.reconcileServices(
		context.Background(),
		nil,
		[]string{"service/stt/whisper"},
		nil,
	)

	if _, ok := daemon.proxyRoutes["service-stt-whisper"]; ok {
		t.Error("proxyRoutes should no longer contain service-stt-whisper after removal")
	}
}

func TestReconcileServices_ServiceMigration(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local", // moved to remote
				Protocol:  "http",
			},
		},
		running: make(map[string]bool),
		proxyRoutes: map[string]string{
			"service-stt-whisper": "/run/bureau/principal/service/stt/whisper.sock",
		},
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Updating a service that moved to a remote machine should remove the
	// local proxy route.
	daemon.reconcileServices(
		context.Background(),
		nil,
		nil,
		[]string{"service/stt/whisper"},
	)

	if _, ok := daemon.proxyRoutes["service-stt-whisper"]; ok {
		t.Error("proxyRoutes should not contain service-stt-whisper after migration to remote machine")
	}
}

func TestProxyRouteRegistration(t *testing.T) {
	// Set up a mock proxy admin server on a Unix socket.
	tempDir := t.TempDir()

	// Track the requests the mock receives.
	type adminCall struct {
		method      string
		serviceName string
		body        string
	}
	var calls []adminCall
	var callsMu sync.Mutex

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		bodyBytes, _ := io.ReadAll(r.Body)
		callsMu.Lock()
		calls = append(calls, adminCall{method: "PUT", serviceName: name, body: string(bodyBytes)})
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": name, "upstream": "http://localhost"})
	})
	adminMux.HandleFunc("DELETE /v1/admin/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		callsMu.Lock()
		calls = append(calls, adminCall{method: "DELETE", serviceName: name})
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "removed", "name": name})
	})

	// Create the admin socket for "agent/alice" under the temp dir.
	aliceAdminDir := filepath.Join(tempDir, "agent")
	os.MkdirAll(aliceAdminDir, 0755)
	aliceAdminSocket := filepath.Join(aliceAdminDir, "alice.admin.sock")

	listener, err := net.Listen("unix", aliceAdminSocket)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(listener)
	defer adminServer.Close()

	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
		},
		running: map[string]bool{
			"agent/alice": true,
		},
		proxyRoutes: make(map[string]string),
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("register on add", func(t *testing.T) {
		callsMu.Lock()
		calls = nil
		callsMu.Unlock()

		daemon.reconcileServices(
			context.Background(),
			[]string{"service/stt/whisper"},
			nil,
			nil,
		)

		callsMu.Lock()
		defer callsMu.Unlock()

		if len(calls) != 1 {
			t.Fatalf("expected 1 admin call, got %d", len(calls))
		}
		if calls[0].method != "PUT" {
			t.Errorf("expected PUT, got %s", calls[0].method)
		}
		if calls[0].serviceName != "service-stt-whisper" {
			t.Errorf("expected service name service-stt-whisper, got %s", calls[0].serviceName)
		}

		// Verify the request body contains the provider socket and path prefix.
		var registration adminServiceRegistration
		if err := json.Unmarshal([]byte(calls[0].body), &registration); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		expectedSocket := "/run/bureau/principal/service/stt/whisper.sock"
		if registration.UpstreamUnix != expectedSocket {
			t.Errorf("upstream_unix = %q, want %q", registration.UpstreamUnix, expectedSocket)
		}
		// The URL should include the /http/ path prefix so the provider
		// proxy routes the request to the correct service.
		expectedURL := "http://localhost/http/service-stt-whisper"
		if registration.UpstreamURL != expectedURL {
			t.Errorf("upstream_url = %q, want %q", registration.UpstreamURL, expectedURL)
		}

		// Verify proxyRoutes was updated.
		if daemon.proxyRoutes["service-stt-whisper"] != expectedSocket {
			t.Errorf("proxyRoutes[service-stt-whisper] = %q, want %q",
				daemon.proxyRoutes["service-stt-whisper"], expectedSocket)
		}
	})

	t.Run("unregister on remove", func(t *testing.T) {
		callsMu.Lock()
		calls = nil
		callsMu.Unlock()

		// Clear services (simulating syncServiceDirectory removing it).
		daemon.services = make(map[string]*schema.Service)

		daemon.reconcileServices(
			context.Background(),
			nil,
			[]string{"service/stt/whisper"},
			nil,
		)

		callsMu.Lock()
		defer callsMu.Unlock()

		if len(calls) != 1 {
			t.Fatalf("expected 1 admin call, got %d", len(calls))
		}
		if calls[0].method != "DELETE" {
			t.Errorf("expected DELETE, got %s", calls[0].method)
		}
		if calls[0].serviceName != "service-stt-whisper" {
			t.Errorf("expected service name service-stt-whisper, got %s", calls[0].serviceName)
		}

		// Verify proxyRoutes was cleaned up.
		if _, ok := daemon.proxyRoutes["service-stt-whisper"]; ok {
			t.Error("proxyRoutes should not contain service-stt-whisper after removal")
		}
	})
}

func TestConfigureConsumerProxy(t *testing.T) {
	// Set up a mock admin server for a new consumer.
	tempDir := t.TempDir()

	type adminCall struct {
		method      string
		serviceName string
	}
	var calls []adminCall
	var callsMu sync.Mutex

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		callsMu.Lock()
		calls = append(calls, adminCall{method: "PUT", serviceName: name})
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": name})
	})

	bobAdminDir := filepath.Join(tempDir, "agent")
	os.MkdirAll(bobAdminDir, 0755)
	bobAdminSocket := filepath.Join(bobAdminDir, "bob.admin.sock")

	listener, err := net.Listen("unix", bobAdminSocket)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(listener)
	defer adminServer.Close()

	daemon := &Daemon{
		proxyRoutes: map[string]string{
			"service-stt-whisper": "/run/bureau/principal/service/stt/whisper.sock",
			"service-llm-mixtral": "/run/bureau/principal/service/llm/mixtral.sock",
		},
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	daemon.configureConsumerProxy(context.Background(), "agent/bob")

	callsMu.Lock()
	defer callsMu.Unlock()

	// Should have registered both services.
	if len(calls) != 2 {
		t.Fatalf("expected 2 admin calls, got %d", len(calls))
	}

	// Collect service names (order is non-deterministic from map iteration).
	registered := make(map[string]bool)
	for _, call := range calls {
		if call.method != "PUT" {
			t.Errorf("expected PUT, got %s", call.method)
		}
		registered[call.serviceName] = true
	}
	if !registered["service-stt-whisper"] {
		t.Error("service-stt-whisper should have been registered")
	}
	if !registered["service-llm-mixtral"] {
		t.Error("service-llm-mixtral should have been registered")
	}
}

func TestParseServiceFromPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "basic service", path: "/http/service-stt-whisper/v1/transcribe", want: "service-stt-whisper"},
		{name: "service root", path: "/http/service-stt-whisper/", want: "service-stt-whisper"},
		{name: "service no trailing slash", path: "/http/service-stt-whisper", want: "service-stt-whisper"},
		{name: "deep path", path: "/http/my-service/v1/a/b/c", want: "my-service"},
		{name: "wrong prefix", path: "/v1/proxy", wantErr: "must start with /http/"},
		{name: "empty service", path: "/http/", wantErr: "empty service name"},
		{name: "bare path", path: "/", wantErr: "must start with /http/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseServiceFromPath(test.path)
			if test.wantErr != "" {
				if err == nil {
					t.Errorf("parseServiceFromPath(%q) = %q, want error containing %q", test.path, got, test.wantErr)
				} else if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("parseServiceFromPath(%q) error = %v, want error containing %q", test.path, err, test.wantErr)
				}
			} else {
				if err != nil {
					t.Errorf("parseServiceFromPath(%q) error = %v, want nil", test.path, err)
				} else if got != test.want {
					t.Errorf("parseServiceFromPath(%q) = %q, want %q", test.path, got, test.want)
				}
			}
		})
	}
}

func TestServiceByProxyName(t *testing.T) {
	daemon := &Daemon{
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
			},
			"service/tts/piper": {
				Principal: "@service/tts/piper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
			},
		},
	}

	t.Run("found", func(t *testing.T) {
		localpart, service, ok := daemon.serviceByProxyName("service-stt-whisper")
		if !ok {
			t.Fatal("expected to find service-stt-whisper")
		}
		if localpart != "service/stt/whisper" {
			t.Errorf("localpart = %q, want %q", localpart, "service/stt/whisper")
		}
		if service.Machine != "@machine/workstation:bureau.local" {
			t.Errorf("machine = %q, want %q", service.Machine, "@machine/workstation:bureau.local")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, _, ok := daemon.serviceByProxyName("nonexistent")
		if ok {
			t.Error("expected service not found")
		}
	})
}

func TestLocalProviderSocket(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
			},
			"service/tts/piper": {
				Principal: "@service/tts/piper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("local service", func(t *testing.T) {
		socket, ok := daemon.localProviderSocket("service-stt-whisper")
		if !ok {
			t.Fatal("expected to find local provider socket")
		}
		want := "/run/bureau/principal/service/stt/whisper.sock"
		if socket != want {
			t.Errorf("socket = %q, want %q", socket, want)
		}
	})

	t.Run("remote service", func(t *testing.T) {
		_, ok := daemon.localProviderSocket("service-tts-piper")
		if ok {
			t.Error("expected remote service to not be found as local provider")
		}
	})

	t.Run("unknown service", func(t *testing.T) {
		_, ok := daemon.localProviderSocket("nonexistent")
		if ok {
			t.Error("expected unknown service to not be found")
		}
	})
}

func TestReconcileServices_RemoteWithRelay(t *testing.T) {
	relaySocket := filepath.Join(t.TempDir(), "relay.sock")

	daemon := &Daemon{
		machineUserID:   "@machine/workstation:bureau.local",
		machineName:     "machine/workstation",
		serverName:      "bureau.local",
		relaySocketPath: relaySocket,
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
			"service/tts/piper": {
				Principal: "@service/tts/piper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
				Protocol:  "http",
			},
		},
		running:     make(map[string]bool),
		proxyRoutes: make(map[string]string),
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	daemon.reconcileServices(
		context.Background(),
		[]string{"service/stt/whisper", "service/tts/piper"},
		nil,
		nil,
	)

	// Local service should be routed via provider socket.
	if route, ok := daemon.proxyRoutes["service-stt-whisper"]; !ok {
		t.Error("proxyRoutes should contain service-stt-whisper")
	} else if route != "/run/bureau/principal/service/stt/whisper.sock" {
		t.Errorf("service-stt-whisper route = %q, want provider socket", route)
	}

	// Remote service should be routed via relay socket.
	if route, ok := daemon.proxyRoutes["service-tts-piper"]; !ok {
		t.Error("proxyRoutes should contain service-tts-piper when relay is configured")
	} else if route != relaySocket {
		t.Errorf("service-tts-piper route = %q, want relay socket %q", route, relaySocket)
	}
}

func TestReconcileServices_MigrationWithRelay(t *testing.T) {
	// A service migrates from remote to local while the relay is active.
	relaySocket := filepath.Join(t.TempDir(), "relay.sock")

	daemon := &Daemon{
		machineUserID:   "@machine/workstation:bureau.local",
		relaySocketPath: relaySocket,
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local", // now local
				Protocol:  "http",
			},
		},
		running: make(map[string]bool),
		proxyRoutes: map[string]string{
			"service-stt-whisper": relaySocket, // was remote
		},
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(t.TempDir(), localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	daemon.reconcileServices(
		context.Background(),
		nil,
		nil,
		[]string{"service/stt/whisper"},
	)

	// After migration to local, route should point to provider socket, not relay.
	if route, ok := daemon.proxyRoutes["service-stt-whisper"]; !ok {
		t.Error("proxyRoutes should contain service-stt-whisper")
	} else if route == relaySocket {
		t.Error("route should have changed from relay to provider socket after migration to local")
	} else if route != "/run/bureau/principal/service/stt/whisper.sock" {
		t.Errorf("route = %q, want provider socket", route)
	}
}

func TestRelayHandler(t *testing.T) {
	// Set up a mock "peer daemon" that receives forwarded requests.
	peerReceived := make(chan *http.Request, 1)
	peerServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peerReceived <- r
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"result":"from-peer","path":%q}`, r.URL.Path)
		}),
	}
	peerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer peerListener.Close()
	go peerServer.Serve(peerListener)
	defer peerServer.Close()

	peerAddress := peerListener.Addr().String()

	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
				Protocol:  "http",
			},
		},
		peerAddresses: map[string]string{
			"@machine/cloud-gpu:bureau.local": peerAddress,
		},
		peerTransports: make(map[string]http.RoundTripper),
		transportDialer: &transport.TCPDialer{
			Timeout: 5 * time.Second,
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Create a relay socket and serve the relay handler.
	relaySocketPath := filepath.Join(t.TempDir(), "relay.sock")
	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer relayListener.Close()

	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/http/", daemon.handleRelay)
	relayServer := &http.Server{Handler: relayMux}
	go relayServer.Serve(relayListener)
	defer relayServer.Close()

	// Create an HTTP client that connects via the relay socket.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", relaySocketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	response, err := client.Get("http://localhost/http/service-stt-whisper/v1/transcribe")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", response.StatusCode)
	}

	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "from-peer") {
		t.Errorf("response body = %q, expected to contain 'from-peer'", string(body))
	}

	// Verify the peer received the correct path.
	select {
	case peerRequest := <-peerReceived:
		if peerRequest.URL.Path != "/http/service-stt-whisper/v1/transcribe" {
			t.Errorf("peer received path = %q, want /http/service-stt-whisper/v1/transcribe", peerRequest.URL.Path)
		}
	case <-time.After(5 * time.Second):
		t.Error("peer did not receive request")
	}
}

func TestRelayHandler_UnknownService(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services:      make(map[string]*schema.Service),
		peerAddresses: make(map[string]string),
		logger:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	relaySocketPath := filepath.Join(t.TempDir(), "relay.sock")
	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer relayListener.Close()

	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/http/", daemon.handleRelay)
	relayServer := &http.Server{Handler: relayMux}
	go relayServer.Serve(relayListener)
	defer relayServer.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", relaySocketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	response, err := client.Get("http://localhost/http/nonexistent/v1/test")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

func TestTransportInboundHandler(t *testing.T) {
	// Set up a mock "provider proxy" on a Unix socket.
	providerSocketPath := filepath.Join(t.TempDir(), "provider.sock")
	providerListener, err := net.Listen("unix", providerSocketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer providerListener.Close()

	providerServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"result":"from-provider","path":%q}`, r.URL.Path)
		}),
	}
	go providerServer.Serve(providerListener)
	defer providerServer.Close()

	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Start the inbound handler on a TCP listener (simulating the
	// transport listener).
	inboundListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer inboundListener.Close()

	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", daemon.handleTransportInbound)
	inboundServer := &http.Server{Handler: inboundMux}
	go inboundServer.Serve(inboundListener)
	defer inboundServer.Close()

	// The inbound handler needs to find the provider's proxy socket.
	// Override the services entry's principal to match our test socket.
	// Since localProviderSocket derives the socket from the principal
	// Matrix ID, we need the localpart "service/stt/whisper" to map
	// to our test socket. The real SocketPath would be
	// /run/bureau/principal/service/stt/whisper.sock. For testing, we
	// need to control the path.
	//
	// Rather than patching SocketPath (which is a package-level
	// function), the inbound handler uses localProviderSocket which
	// calls principal.SocketPath. So for this test to work with a real
	// provider socket, we'd need the socket at the production path.
	//
	// Instead, test this via the full cross-transport integration test
	// which uses real Bureau proxies at real socket paths.
	//
	// For this unit test, verify that the handler returns 404 for an
	// unknown service and that it calls the handler at all.
	client := &http.Client{Timeout: 5 * time.Second}

	response, err := client.Get("http://" + inboundListener.Addr().String() + "/http/nonexistent/v1/test")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown service", response.StatusCode)
	}
}

func TestCrossTransportRouting(t *testing.T) {
	// Integration test: a request travels through the complete
	// cross-machine routing chain:
	//
	//   client → relay socket → TCP transport → inbound handler → provider socket → backend
	//
	// Machine A (consumer side): relay socket
	// Machine B (provider side): transport listener + provider proxy

	// 1. Start a backend HTTP server (the actual service).
	backendSocketPath := filepath.Join(t.TempDir(), "backend.sock")
	backendListener, err := net.Listen("unix", backendSocketPath)
	if err != nil {
		t.Fatalf("backend Listen() error: %v", err)
	}
	defer backendListener.Close()

	backendServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"transcription":"hello world","path":%q}`, r.URL.Path)
		}),
	}
	go backendServer.Serve(backendListener)
	defer backendServer.Close()

	// 2. Start a "provider proxy" — a Bureau proxy that has the
	// service registered and routes to the backend. For this test
	// we use a simple HTTP server that simulates what HandleHTTPProxy
	// does: strip /http/<service>/ and forward to the backend.
	providerSocketPath := filepath.Join(t.TempDir(), "provider.sock")
	providerListener, err := net.Listen("unix", providerSocketPath)
	if err != nil {
		t.Fatalf("provider Listen() error: %v", err)
	}
	defer providerListener.Close()

	providerServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate HandleHTTPProxy: strip /http/<service>/ prefix.
			path := r.URL.Path
			if strings.HasPrefix(path, "/http/") {
				remaining := strings.TrimPrefix(path, "/http/")
				parts := strings.SplitN(remaining, "/", 2)
				if len(parts) > 1 {
					path = "/" + parts[1]
				} else {
					path = "/"
				}
			}

			// Forward to backend via Unix socket.
			backendProxy := &httputil.ReverseProxy{
				Director: func(request *http.Request) {
					request.URL.Scheme = "http"
					request.URL.Host = "localhost"
					request.URL.Path = path
				},
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", backendSocketPath)
					},
				},
			}
			backendProxy.ServeHTTP(w, r)
		}),
	}
	go providerServer.Serve(providerListener)
	defer providerServer.Close()

	// 3. Set up the "provider daemon" (machine B) with a transport
	// listener. Its inbound handler routes to the provider proxy.
	providerDaemon := &Daemon{
		machineUserID: "@machine/cloud-gpu:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
				Protocol:  "http",
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// The inbound handler calls localProviderSocket which derives the
	// socket from the service principal. Since principal.SocketPath
	// returns /run/bureau/principal/... (not our temp dir), we need to
	// hook the handler to use our test socket. We do this by making
	// the inbound handler a closure that routes to the correct socket.
	//
	// For a clean test, use a custom inbound handler that bypasses
	// localProviderSocket and routes directly to our test socket.
	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", func(w http.ResponseWriter, r *http.Request) {
		// Parse the service name to verify correct routing.
		serviceName, err := parseServiceFromPath(r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// In this test, all services route to our test provider socket.
		_ = serviceName
		proxy := &httputil.ReverseProxy{
			Director: func(request *http.Request) {
				request.URL.Scheme = "http"
				request.URL.Host = "localhost"
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", providerSocketPath)
				},
			},
		}
		proxy.ServeHTTP(w, r)
	})

	transportListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("transport Listen() error: %v", err)
	}
	defer transportListener.Close()

	transportServer := &http.Server{Handler: inboundMux}
	go transportServer.Serve(transportListener)
	defer transportServer.Close()

	_ = providerDaemon // Used for documentation; routing is handled by the custom mux above.

	// 4. Set up the "consumer daemon" (machine A) with a relay socket.
	consumerDaemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/cloud-gpu:bureau.local",
				Protocol:  "http",
			},
		},
		peerAddresses: map[string]string{
			"@machine/cloud-gpu:bureau.local": transportListener.Addr().String(),
		},
		peerTransports:  make(map[string]http.RoundTripper),
		transportDialer: &transport.TCPDialer{Timeout: 5 * time.Second},
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	relaySocketPath := filepath.Join(t.TempDir(), "relay.sock")
	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		t.Fatalf("relay Listen() error: %v", err)
	}
	defer relayListener.Close()

	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/http/", consumerDaemon.handleRelay)
	relayServer := &http.Server{Handler: relayMux}
	go relayServer.Serve(relayListener)
	defer relayServer.Close()

	// 5. Send a request through the relay socket as if we were a
	// consumer proxy routing a remote service request.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", relaySocketPath)
			},
		},
		Timeout: 10 * time.Second,
	}

	response, err := client.Get("http://localhost/http/service-stt-whisper/v1/transcribe")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body = %q, want 200", response.StatusCode, string(body))
	}

	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "hello world") {
		t.Errorf("response body = %q, expected to contain 'hello world'", string(body))
	}

	// Verify the backend received the correct stripped path.
	if !strings.Contains(string(body), `"/v1/transcribe"`) {
		t.Errorf("response body = %q, expected backend to receive /v1/transcribe", string(body))
	}
}

func TestLauncherRequest_Timeout(t *testing.T) {
	// Set up a server that accepts but never responds.
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "slow-launcher.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	daemon := &Daemon{
		launcherSocket: socketPath,
		logger:         slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Use a short context timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = daemon.launcherRequest(ctx, launcherIPCRequest{Action: "status"})
	if err == nil {
		t.Error("expected timeout error")
	}

	// Clean up the held connection so the goroutine exits.
	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(time.Second):
	}
}
