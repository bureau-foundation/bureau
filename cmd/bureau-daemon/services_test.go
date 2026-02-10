// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

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

func TestBuildServiceDirectory(t *testing.T) {
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal:    "@service/stt/whisper:bureau.local",
				Machine:      "@machine/gpu-1:bureau.local",
				Capabilities: []string{"streaming", "speaker-diarization"},
				Protocol:     "http",
				Description:  "Speech-to-text via Whisper",
				Metadata:     map[string]any{"model": "large-v3"},
			},
			"service/tts/piper": {
				Principal:    "@service/tts/piper:bureau.local",
				Machine:      "@machine/gpu-1:bureau.local",
				Capabilities: []string{"streaming"},
				Protocol:     "http",
				Description:  "Text-to-speech via Piper",
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	directory := daemon.buildServiceDirectory()

	if len(directory) != 2 {
		t.Fatalf("buildServiceDirectory() returned %d entries, want 2", len(directory))
	}

	// Build a lookup map for easier assertions (order is non-deterministic).
	byLocalpart := make(map[string]adminDirectoryEntry, len(directory))
	for _, entry := range directory {
		byLocalpart[entry.Localpart] = entry
	}

	whisper, ok := byLocalpart["service/stt/whisper"]
	if !ok {
		t.Fatal("missing service/stt/whisper in directory")
	}
	if whisper.Principal != "@service/stt/whisper:bureau.local" {
		t.Errorf("whisper principal = %q, want @service/stt/whisper:bureau.local", whisper.Principal)
	}
	if whisper.Protocol != "http" {
		t.Errorf("whisper protocol = %q, want http", whisper.Protocol)
	}
	if len(whisper.Capabilities) != 2 {
		t.Errorf("whisper capabilities = %d, want 2", len(whisper.Capabilities))
	}
	if whisper.Metadata == nil || whisper.Metadata["model"] != "large-v3" {
		t.Errorf("whisper metadata not preserved: %v", whisper.Metadata)
	}

	piper, ok := byLocalpart["service/tts/piper"]
	if !ok {
		t.Fatal("missing service/tts/piper in directory")
	}
	if piper.Description != "Text-to-speech via Piper" {
		t.Errorf("piper description = %q", piper.Description)
	}
}

func TestBuildServiceDirectory_Empty(t *testing.T) {
	daemon := &Daemon{
		services: make(map[string]*schema.Service),
		logger:   slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	directory := daemon.buildServiceDirectory()

	if len(directory) != 0 {
		t.Errorf("buildServiceDirectory() returned %d entries for empty services, want 0", len(directory))
	}
}

func TestPushDirectoryToProxy(t *testing.T) {
	// Set up a mock admin server that captures the PUT /v1/admin/directory request.
	tempDir := t.TempDir()

	var receivedDirectory []adminDirectoryEntry
	var callCount int
	var callsMu sync.Mutex

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/directory", func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		defer callsMu.Unlock()
		callCount++

		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &receivedDirectory); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"services": len(receivedDirectory),
		})
	})

	consumerAdminDir := filepath.Join(tempDir, "agent")
	os.MkdirAll(consumerAdminDir, 0755)
	consumerAdminSocket := filepath.Join(consumerAdminDir, "alice.admin.sock")

	listener, err := net.Listen("unix", consumerAdminSocket)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(listener)
	defer adminServer.Close()

	daemon := &Daemon{
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	directory := []adminDirectoryEntry{
		{
			Localpart:    "service/stt/whisper",
			Principal:    "@service/stt/whisper:bureau.local",
			Machine:      "@machine/gpu-1:bureau.local",
			Protocol:     "http",
			Description:  "Speech-to-text",
			Capabilities: []string{"streaming"},
		},
		{
			Localpart:   "service/embedding/e5",
			Principal:   "@service/embedding/e5:bureau.local",
			Machine:     "@machine/cpu-1:bureau.local",
			Protocol:    "grpc",
			Description: "Text embeddings",
			Metadata:    map[string]any{"max_tokens": float64(512)},
		},
	}

	err = daemon.pushDirectoryToProxy(context.Background(), "agent/alice", directory)
	if err != nil {
		t.Fatalf("pushDirectoryToProxy: %v", err)
	}

	callsMu.Lock()
	defer callsMu.Unlock()

	if callCount != 1 {
		t.Errorf("expected 1 admin call, got %d", callCount)
	}
	if len(receivedDirectory) != 2 {
		t.Fatalf("proxy received %d entries, want 2", len(receivedDirectory))
	}

	// Verify the entries were correctly serialized on the wire.
	byLocalpart := make(map[string]adminDirectoryEntry, len(receivedDirectory))
	for _, entry := range receivedDirectory {
		byLocalpart[entry.Localpart] = entry
	}
	whisper, ok := byLocalpart["service/stt/whisper"]
	if !ok {
		t.Fatal("missing service/stt/whisper")
	}
	if whisper.Protocol != "http" {
		t.Errorf("whisper protocol = %q, want http", whisper.Protocol)
	}
	e5, ok := byLocalpart["service/embedding/e5"]
	if !ok {
		t.Fatal("missing service/embedding/e5")
	}
	if e5.Protocol != "grpc" {
		t.Errorf("e5 protocol = %q, want grpc", e5.Protocol)
	}
}

func TestPushServiceDirectory_AllConsumers(t *testing.T) {
	// Verify pushServiceDirectory pushes to all running consumers.
	tempDir := t.TempDir()

	var pushCounts sync.Map // map[string]int — consumer localpart → push count

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/directory", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "services": 1})
	})

	// Create admin sockets for two consumers.
	for _, consumer := range []string{"agent/alice", "agent/bob"} {
		consumerSocketDir := filepath.Join(tempDir, consumer)
		os.MkdirAll(filepath.Dir(consumerSocketDir+".admin.sock"), 0755)
		socketPath := consumerSocketDir + ".admin.sock"

		localConsumer := consumer
		wrappedMux := http.NewServeMux()
		wrappedMux.HandleFunc("PUT /v1/admin/directory", func(w http.ResponseWriter, r *http.Request) {
			count, _ := pushCounts.LoadOrStore(localConsumer, 0)
			pushCounts.Store(localConsumer, count.(int)+1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "services": 1})
		})

		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("Listen(%s): %v", socketPath, err)
		}
		t.Cleanup(func() { listener.Close() })

		server := &http.Server{Handler: wrappedMux}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
	}

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
			"agent/bob":   true,
		},
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	daemon.pushServiceDirectory(context.Background())

	// Verify both consumers received a push.
	for _, consumer := range []string{"agent/alice", "agent/bob"} {
		count, ok := pushCounts.Load(consumer)
		if !ok {
			t.Errorf("consumer %s did not receive a directory push", consumer)
		} else if count.(int) != 1 {
			t.Errorf("consumer %s received %d pushes, want 1", consumer, count.(int))
		}
	}
}

func TestPushServiceDirectory_NoRunning(t *testing.T) {
	// When no consumers are running, pushServiceDirectory should be a no-op.
	daemon := &Daemon{
		machineUserID: "@machine/workstation:bureau.local",
		services: map[string]*schema.Service{
			"service/stt/whisper": {
				Principal: "@service/stt/whisper:bureau.local",
				Machine:   "@machine/workstation:bureau.local",
				Protocol:  "http",
			},
		},
		running: make(map[string]bool),
		logger:  slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Should not panic or error — just return immediately.
	daemon.pushServiceDirectory(context.Background())
}

func TestPushVisibilityToProxy(t *testing.T) {
	// Set up a mock admin server that captures the PUT /v1/admin/visibility request.
	tempDir := t.TempDir()

	var receivedPatterns []string
	var callCount int
	var callsMu sync.Mutex

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/visibility", func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		defer callsMu.Unlock()
		callCount++

		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &receivedPatterns); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"patterns": len(receivedPatterns),
		})
	})

	consumerAdminDir := filepath.Join(tempDir, "agent")
	os.MkdirAll(consumerAdminDir, 0755)
	consumerAdminSocket := filepath.Join(consumerAdminDir, "alice.admin.sock")

	listener, err := net.Listen("unix", consumerAdminSocket)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(listener)
	defer adminServer.Close()

	daemon := &Daemon{
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	patterns := []string{"service/stt/*", "service/embedding/**"}
	err = daemon.pushVisibilityToProxy(context.Background(), "agent/alice", patterns)
	if err != nil {
		t.Fatalf("pushVisibilityToProxy: %v", err)
	}

	callsMu.Lock()
	defer callsMu.Unlock()

	if callCount != 1 {
		t.Errorf("expected 1 admin call, got %d", callCount)
	}
	if len(receivedPatterns) != 2 {
		t.Fatalf("proxy received %d patterns, want 2", len(receivedPatterns))
	}
	if receivedPatterns[0] != "service/stt/*" {
		t.Errorf("pattern[0] = %q, want service/stt/*", receivedPatterns[0])
	}
	if receivedPatterns[1] != "service/embedding/**" {
		t.Errorf("pattern[1] = %q, want service/embedding/**", receivedPatterns[1])
	}
}

func TestPushVisibilityToProxy_EmptyPatterns(t *testing.T) {
	// Pushing empty patterns should still work (resets to default-deny).
	tempDir := t.TempDir()

	var receivedPatterns []string
	var callsMu sync.Mutex

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/visibility", func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		defer callsMu.Unlock()

		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &receivedPatterns)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"patterns": len(receivedPatterns),
		})
	})

	consumerAdminDir := filepath.Join(tempDir, "agent")
	os.MkdirAll(consumerAdminDir, 0755)
	consumerAdminSocket := filepath.Join(consumerAdminDir, "bob.admin.sock")

	listener, err := net.Listen("unix", consumerAdminSocket)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(listener)
	defer adminServer.Close()

	daemon := &Daemon{
		adminSocketPathFunc: func(localpart string) string {
			return filepath.Join(tempDir, localpart+".admin.sock")
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Push nil patterns (default for a principal with no ServiceVisibility configured).
	err = daemon.pushVisibilityToProxy(context.Background(), "agent/bob", nil)
	if err != nil {
		t.Fatalf("pushVisibilityToProxy: %v", err)
	}

	callsMu.Lock()
	defer callsMu.Unlock()

	// JSON-marshaling nil []string produces "null", which json.Unmarshal
	// decodes as nil. The proxy's SetServiceVisibility handles nil correctly
	// (it becomes an empty visibility list = default-deny).
	if receivedPatterns != nil {
		t.Errorf("expected nil patterns for nil input, got %v", receivedPatterns)
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
