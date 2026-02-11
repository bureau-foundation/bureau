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
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestLauncherRequest(t *testing.T) {
	// Set up a mock launcher server.
	socketDir := testutil.SocketDir(t)
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
		runDir:         principal.DefaultRunDir,
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
		runDir:         principal.DefaultRunDir,
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

func TestPayloadChanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		old, new *schema.SandboxSpec
		want     bool
	}{
		{
			name: "identical nil payloads",
			old:  &schema.SandboxSpec{Command: []string{"/bin/sh"}},
			new:  &schema.SandboxSpec{Command: []string{"/bin/sh"}},
			want: false,
		},
		{
			name: "identical non-nil payloads",
			old:  &schema.SandboxSpec{Payload: map[string]any{"key": "value"}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"key": "value"}},
			want: false,
		},
		{
			name: "different values",
			old:  &schema.SandboxSpec{Payload: map[string]any{"key": "old"}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"key": "new"}},
			want: true,
		},
		{
			name: "added key",
			old:  &schema.SandboxSpec{Payload: map[string]any{"a": 1}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"a": 1, "b": 2}},
			want: true,
		},
		{
			name: "removed key",
			old:  &schema.SandboxSpec{Payload: map[string]any{"a": 1, "b": 2}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"a": 1}},
			want: true,
		},
		{
			name: "nil to non-nil",
			old:  &schema.SandboxSpec{},
			new:  &schema.SandboxSpec{Payload: map[string]any{"key": "value"}},
			want: true,
		},
		{
			name: "non-nil to nil",
			old:  &schema.SandboxSpec{Payload: map[string]any{"key": "value"}},
			new:  &schema.SandboxSpec{},
			want: true,
		},
		{
			name: "nested map identical",
			old:  &schema.SandboxSpec{Payload: map[string]any{"nested": map[string]any{"deep": true}}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"nested": map[string]any{"deep": true}}},
			want: false,
		},
		{
			name: "nested map different",
			old:  &schema.SandboxSpec{Payload: map[string]any{"nested": map[string]any{"deep": true}}},
			new:  &schema.SandboxSpec{Payload: map[string]any{"nested": map[string]any{"deep": false}}},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := payloadChanged(test.old, test.new)
			if got != test.want {
				t.Errorf("payloadChanged() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestStructurallyChanged(t *testing.T) {
	t.Parallel()

	baseSpec := &schema.SandboxSpec{
		Command:    []string{"/bin/agent", "--mode=auto"},
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
		Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
		EnvironmentVariables: map[string]string{
			"HOME": "/workspace",
			"TERM": "xterm-256color",
		},
		Payload: map[string]any{"model": "gpt-4"},
	}

	tests := []struct {
		name     string
		old, new *schema.SandboxSpec
		want     bool
	}{
		{
			name: "identical specs",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: false,
		},
		{
			name: "payload differs but structure identical",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "claude-4"},
			},
			want: false,
		},
		{
			name: "command changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=manual"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "namespace changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: false},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "environment variable added",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME":    "/workspace",
					"TERM":    "xterm-256color",
					"NEW_VAR": "new_value",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "security option changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true, DieWithParent: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "filesystem changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				Filesystem: []schema.TemplateMount{{Dest: "/tmp", Type: "tmpfs"}},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "environment path changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:         []string{"/bin/agent", "--mode=auto"},
				Namespaces:      &schema.TemplateNamespaces{PID: true, Net: true},
				Security:        &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentPath: "/nix/store/abc123",
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
		{
			name: "both nil payloads with identical structure",
			old: &schema.SandboxSpec{
				Command: []string{"/bin/sh"},
			},
			new: &schema.SandboxSpec{
				Command: []string{"/bin/sh"},
			},
			want: false,
		},
		{
			name: "roles changed",
			old:  baseSpec,
			new: &schema.SandboxSpec{
				Command:    []string{"/bin/agent", "--mode=auto"},
				Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
				Security:   &schema.TemplateSecurity{NewSession: true, NoNewPrivs: true},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Roles:   map[string][]string{"agent": {"/bin/agent"}},
				Payload: map[string]any{"model": "gpt-4"},
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := structurallyChanged(test.old, test.new)
			if got != test.want {
				t.Errorf("structurallyChanged() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestQueryLauncherStatus(t *testing.T) {
	t.Parallel()

	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")

	listener := startMockLauncher(t, socketPath, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action != "status" {
			return launcherIPCResponse{OK: false, Error: "unexpected action: " + request.Action}
		}
		return launcherIPCResponse{
			OK:              true,
			BinaryHash:      "abc123def456",
			ProxyBinaryPath: "/nix/store/xyz-bureau-proxy/bin/bureau-proxy",
		}
	})
	t.Cleanup(func() { listener.Close() })

	daemon := &Daemon{
		runDir:         principal.DefaultRunDir,
		launcherSocket: socketPath,
		logger:         slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	hash, proxyPath, err := daemon.queryLauncherStatus(context.Background())
	if err != nil {
		t.Fatalf("queryLauncherStatus() error: %v", err)
	}
	if hash != "abc123def456" {
		t.Errorf("launcher hash = %q, want %q", hash, "abc123def456")
	}
	if proxyPath != "/nix/store/xyz-bureau-proxy/bin/bureau-proxy" {
		t.Errorf("proxy binary path = %q, want /nix/store/xyz-bureau-proxy/bin/bureau-proxy", proxyPath)
	}
}

func TestReconcileBureauVersion_NilVersion(t *testing.T) {
	t.Parallel()

	// nil BureauVersion in MachineConfig means no version management.
	// reconcile() should not call reconcileBureauVersion at all.
	// We test this indirectly: if BureauVersion is nil, the launcher
	// status should not be queried (no IPC call).

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		// No BureauVersion set, no principals.
	})

	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	// If any IPC request is sent, it means reconcileBureauVersion was
	// called when it shouldn't have been. We don't start a launcher at
	// all — an IPC attempt would error, and we check that reconcile
	// succeeds without error.

	daemon := &Daemon{
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}
}

func TestReconcileBureauVersion_ProxyChanged(t *testing.T) {
	t.Parallel()

	// Create temp files to act as "binaries" with different content.
	directory := t.TempDir()

	desiredProxy := filepath.Join(directory, "bureau-proxy-new")
	if err := os.WriteFile(desiredProxy, []byte("new proxy binary v2"), 0755); err != nil {
		t.Fatalf("WriteFile desired proxy: %v", err)
	}

	currentProxy := filepath.Join(directory, "bureau-proxy-current")
	if err := os.WriteFile(currentProxy, []byte("old proxy binary v1"), 0755); err != nil {
		t.Fatalf("WriteFile current proxy: %v", err)
	}

	// Also create daemon/launcher files that match current (no change).
	daemonBinary := filepath.Join(directory, "bureau-daemon")
	if err := os.WriteFile(daemonBinary, []byte("daemon binary"), 0755); err != nil {
		t.Fatalf("WriteFile daemon: %v", err)
	}

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	// Configure MachineConfig with BureauVersion pointing to the desired paths.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		BureauVersion: &schema.BureauVersion{
			DaemonStorePath: daemonBinary,
			ProxyStorePath:  desiredProxy,
		},
	})

	// Track messages sent to config room.
	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/m.room.message/") {
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
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	// Mock launcher: "status" returns current proxy path, "update-proxy-binary"
	// is tracked.
	var (
		launcherMu       sync.Mutex
		updateProxyCalls []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		switch request.Action {
		case "status":
			return launcherIPCResponse{
				OK:              true,
				BinaryHash:      "",
				ProxyBinaryPath: currentProxy,
			}
		case "update-proxy-binary":
			launcherMu.Lock()
			updateProxyCalls = append(updateProxyCalls, request.BinaryPath)
			launcherMu.Unlock()
			return launcherIPCResponse{OK: true}
		default:
			return launcherIPCResponse{OK: true}
		}
	})
	t.Cleanup(func() { listener.Close() })

	// Compute daemon hash from the daemon binary file (it matches desired,
	// so DaemonChanged should be false).
	daemonHash := ""
	if digest, hashErr := binhash.HashFile(daemonBinary); hashErr == nil {
		daemonHash = binhash.FormatDigest(digest)
	}

	daemon := &Daemon{
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		daemonBinaryHash:    daemonHash,
		running:             make(map[string]bool),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	// Verify update-proxy-binary was called with the desired path.
	launcherMu.Lock()
	defer launcherMu.Unlock()
	if len(updateProxyCalls) != 1 {
		t.Fatalf("expected 1 update-proxy-binary call, got %d", len(updateProxyCalls))
	}
	if updateProxyCalls[0] != desiredProxy {
		t.Errorf("update-proxy-binary path = %q, want %q", updateProxyCalls[0], desiredProxy)
	}

	// Verify a message was sent to the config room about the update.
	messagesMu.Lock()
	defer messagesMu.Unlock()
	foundProxyMessage := false
	for _, message := range messages {
		if strings.Contains(message, "proxy") {
			foundProxyMessage = true
			break
		}
	}
	if !foundProxyMessage {
		t.Errorf("expected config room message about proxy update, got %v", messages)
	}
}

func TestReconcileStructuralChangeTriggersRestart(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Start with one command, then change the template to a different command.
	state := newMockMatrixState()
	state.setRoomAlias("#bureau/templates:test.local", templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Intercept messages for verification.
	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/m.room.message/") {
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
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	// Track IPC calls.
	var (
		launcherMu    sync.Mutex
		ipcActions    []string
		ipcPrincipals []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, request.Action)
		ipcPrincipals = append(ipcPrincipals, request.Principal)
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	daemon := &Daemon{
		runDir:         principal.DefaultRunDir,
		session:        session,
		machineName:    machineName,
		serverName:     serverName,
		configRoomID:   configRoomID,
		launcherSocket: launcherSocket,
		running:        map[string]bool{"agent/test": true},
		lastSpecs: map[string]*schema.SandboxSpec{
			"agent/test": {
				Command: []string{"/bin/agent", "--mode=v1"},
			},
		},
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// Reconcile: template command changed from v1 to v2.
	// Should see: destroy-sandbox (structural restart) + create-sandbox (recreate).
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	defer launcherMu.Unlock()

	// Should have exactly: destroy-sandbox then create-sandbox.
	if len(ipcActions) < 2 {
		t.Fatalf("expected at least 2 IPC calls, got %d: %v", len(ipcActions), ipcActions)
	}

	destroyIndex := -1
	createIndex := -1
	for index, action := range ipcActions {
		if action == "destroy-sandbox" && ipcPrincipals[index] == "agent/test" {
			destroyIndex = index
		}
		if action == "create-sandbox" && ipcPrincipals[index] == "agent/test" {
			createIndex = index
		}
	}

	if destroyIndex == -1 {
		t.Error("expected destroy-sandbox call for agent/test")
	}
	if createIndex == -1 {
		t.Error("expected create-sandbox call for agent/test")
	}
	if destroyIndex != -1 && createIndex != -1 && destroyIndex >= createIndex {
		t.Errorf("destroy-sandbox (index %d) should come before create-sandbox (index %d)",
			destroyIndex, createIndex)
	}

	// Principal should be running again after the cycle.
	if !daemon.running["agent/test"] {
		t.Error("principal should be running after structural restart cycle")
	}

	// Structural restart should save the old spec as a rollback target.
	// After the "create missing" pass recreates, previousSpecs should
	// still hold the pre-restart spec (it's only cleared on rollback or
	// principal removal).
	if daemon.previousSpecs["agent/test"] == nil {
		t.Error("expected previousSpecs to be populated after structural restart")
	}
}

// TestReconcileStructuralChangeOnly verifies that a structural change is
// detected even when the payload has NOT changed. This was a bug: the
// previous code only checked structurallyChanged() inside the
// payloadChanged() branch, silently ignoring structural-only changes.
func TestReconcileStructuralChangeOnly(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Template with changed command but same payload.
	state := newMockMatrixState()
	state.setRoomAlias("#bureau/templates:test.local", templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/m.room.message/") {
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
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
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
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	// Payload is nil in both old and new spec — only command differs.
	daemon := &Daemon{
		runDir:         principal.DefaultRunDir,
		session:        session,
		machineName:    machineName,
		serverName:     serverName,
		configRoomID:   configRoomID,
		launcherSocket: launcherSocket,
		running:        map[string]bool{"agent/test": true},
		lastSpecs: map[string]*schema.SandboxSpec{
			"agent/test": {
				Command: []string{"/bin/agent", "--mode=v1"},
				// No payload — verifying structural-only detection.
			},
		},
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	defer launcherMu.Unlock()

	// Should see destroy-sandbox (structural change detected) then
	// create-sandbox (recreate with new spec).
	foundDestroy := false
	for _, action := range ipcActions {
		if action == "destroy-sandbox" {
			foundDestroy = true
			break
		}
	}
	if !foundDestroy {
		t.Errorf("expected destroy-sandbox call (structural change without payload change), got: %v", ipcActions)
	}
}

func TestReconcilePayloadOnlyChangeHotReloads(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Template with same command but different payload.
	state := newMockMatrixState()
	state.setRoomAlias("#bureau/templates:test.local", templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:        []string{"/bin/agent", "--mode=v1"},
		DefaultPayload: map[string]any{"model": "claude-4"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
			},
		},
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
	session, err := client.SessionFromToken("@machine/test:test.local", "test-token")
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

	// Old spec has same command but different payload.
	daemon := &Daemon{
		runDir:         principal.DefaultRunDir,
		session:        session,
		machineName:    machineName,
		serverName:     serverName,
		configRoomID:   configRoomID,
		launcherSocket: launcherSocket,
		running:        map[string]bool{"agent/test": true},
		lastSpecs: map[string]*schema.SandboxSpec{
			"agent/test": {
				Command: []string{"/bin/agent", "--mode=v1"},
				Payload: map[string]any{"model": "gpt-4"},
			},
		},
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	launcherMu.Lock()
	defer launcherMu.Unlock()

	// Should see update-payload (hot-reload), NOT destroy-sandbox.
	foundUpdatePayload := false
	foundDestroy := false
	for _, action := range ipcActions {
		if action == "update-payload" {
			foundUpdatePayload = true
		}
		if action == "destroy-sandbox" {
			foundDestroy = true
		}
	}
	if !foundUpdatePayload {
		t.Errorf("expected update-payload call for hot-reload, got: %v", ipcActions)
	}
	if foundDestroy {
		t.Errorf("should NOT destroy sandbox for payload-only change, got: %v", ipcActions)
	}

	// Principal should still be running (not restarted).
	if !daemon.running["agent/test"] {
		t.Error("principal should still be running after payload-only hot-reload")
	}
}

func TestLauncherRequest_Timeout(t *testing.T) {
	// Set up a server that accepts but never responds.
	socketDir := testutil.SocketDir(t)
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
		runDir:         principal.DefaultRunDir,
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
