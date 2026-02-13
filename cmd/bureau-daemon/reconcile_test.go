// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
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
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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
			decoder := codec.NewDecoder(conn)
			encoder := codec.NewEncoder(conn)

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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.launcherSocket = socketPath
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

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
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.launcherSocket = socketPath
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.daemonBinaryHash = daemonHash
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Start with one command, then change the template to a different command.
	state := newMockMatrixState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/template:test-template",
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastSpecs["agent/test"] = &schema.SandboxSpec{
		Command: []string{"/bin/agent", "--mode=v1"},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Template with changed command but same payload.
	state := newMockMatrixState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
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
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastSpecs["agent/test"] = &schema.SandboxSpec{
		Command: []string{"/bin/agent", "--mode=v1"},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	// Template with same command but different payload.
	state := newMockMatrixState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:        []string{"/bin/agent", "--mode=v1"},
		DefaultPayload: map[string]any{"model": "claude-4"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/template:test-template",
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
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastSpecs["agent/test"] = &schema.SandboxSpec{
		Command: []string{"/bin/agent", "--mode=v1"},
		Payload: map[string]any{"model": "gpt-4"},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.launcherSocket = socketPath
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Use a short context timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = daemon.launcherRequest(ctx, launcherIPCRequest{Action: "status"})
	if err == nil {
		t.Error("expected timeout error")
	}

	// Clean up the held connection so the goroutine exits.
	conn := testutil.RequireReceive(t, accepted, time.Second, "waiting for accepted connection")
	conn.Close()
}

func TestMergeAuthorizationPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		defaultPolicy    *schema.AuthorizationPolicy
		principalPolicy  *schema.AuthorizationPolicy
		wantGrants       int
		wantDenials      int
		wantAllowances   int
		wantAllowDenials int
	}{
		{
			name:            "both nil",
			defaultPolicy:   nil,
			principalPolicy: nil,
		},
		{
			name: "only default policy",
			defaultPolicy: &schema.AuthorizationPolicy{
				Grants:  []schema.Grant{{Actions: []string{"observe/*"}}},
				Denials: []schema.Denial{{Actions: []string{"admin/*"}}},
			},
			principalPolicy: nil,
			wantGrants:      1,
			wantDenials:     1,
		},
		{
			name:          "only principal policy",
			defaultPolicy: nil,
			principalPolicy: &schema.AuthorizationPolicy{
				Grants:     []schema.Grant{{Actions: []string{"service/discover"}}},
				Allowances: []schema.Allowance{{Actions: []string{"observe/attach"}, Actors: []string{"admin/**"}}},
			},
			wantGrants:     1,
			wantAllowances: 1,
		},
		{
			name: "both policies merge additively",
			defaultPolicy: &schema.AuthorizationPolicy{
				Grants:           []schema.Grant{{Actions: []string{"observe/*"}}},
				Denials:          []schema.Denial{{Actions: []string{"admin/*"}}},
				Allowances:       []schema.Allowance{{Actions: []string{"observe/attach"}, Actors: []string{"**"}}},
				AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{"observe/resize"}, Actors: []string{"untrusted/**"}}},
			},
			principalPolicy: &schema.AuthorizationPolicy{
				Grants:           []schema.Grant{{Actions: []string{"service/discover"}}, {Actions: []string{"matrix/join"}}},
				Denials:          []schema.Denial{{Actions: []string{"service/register"}}},
				Allowances:       []schema.Allowance{{Actions: []string{"observe/input"}, Actors: []string{"admin/**"}}},
				AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{"observe/input"}, Actors: []string{"untrusted/**"}}},
			},
			wantGrants:       3,
			wantDenials:      2,
			wantAllowances:   2,
			wantAllowDenials: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			merged := mergeAuthorizationPolicy(test.defaultPolicy, test.principalPolicy)

			if got := len(merged.Grants); got != test.wantGrants {
				t.Errorf("Grants count = %d, want %d", got, test.wantGrants)
			}
			if got := len(merged.Denials); got != test.wantDenials {
				t.Errorf("Denials count = %d, want %d", got, test.wantDenials)
			}
			if got := len(merged.Allowances); got != test.wantAllowances {
				t.Errorf("Allowances count = %d, want %d", got, test.wantAllowances)
			}
			if got := len(merged.AllowanceDenials); got != test.wantAllowDenials {
				t.Errorf("AllowanceDenials count = %d, want %d", got, test.wantAllowDenials)
			}
		})
	}
}

// TestMergeAuthorizationPolicy_DefaultBeforePrincipal verifies that default
// policy entries appear before per-principal entries in the merged result.
// This ordering matters because evaluation may be order-sensitive (first
// match wins, etc.).
func TestMergeAuthorizationPolicy_DefaultBeforePrincipal(t *testing.T) {
	t.Parallel()

	merged := mergeAuthorizationPolicy(
		&schema.AuthorizationPolicy{
			Grants: []schema.Grant{{Actions: []string{"default-action"}}},
		},
		&schema.AuthorizationPolicy{
			Grants: []schema.Grant{{Actions: []string{"principal-action"}}},
		},
	)

	if len(merged.Grants) != 2 {
		t.Fatalf("Grants count = %d, want 2", len(merged.Grants))
	}
	if merged.Grants[0].Actions[0] != "default-action" {
		t.Errorf("Grants[0].Actions[0] = %q, want %q", merged.Grants[0].Actions[0], "default-action")
	}
	if merged.Grants[1].Actions[0] != "principal-action" {
		t.Errorf("Grants[1].Actions[0] = %q, want %q", merged.Grants[1].Actions[0], "principal-action")
	}
}

func TestRebuildAuthorizationIndex(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	config := &schema.MachineConfig{
		DefaultPolicy: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{{Actions: []string{"observe/*"}}},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/alpha",
				AutoStart: true,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{"service/discover"}}},
				},
			},
			{
				Localpart: "agent/beta",
				AutoStart: true,
				// No per-principal policy — only inherits defaults.
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config)

	// Both principals should be in the index.
	principals := daemon.authorizationIndex.Principals()
	if len(principals) != 2 {
		t.Fatalf("Principals() count = %d, want 2", len(principals))
	}

	// agent/alpha should have 2 grants (1 default + 1 per-principal).
	alphaGrants := daemon.authorizationIndex.Grants("agent/alpha")
	if len(alphaGrants) != 2 {
		t.Errorf("agent/alpha grants = %d, want 2", len(alphaGrants))
	}

	// agent/beta should have 1 grant (default only).
	betaGrants := daemon.authorizationIndex.Grants("agent/beta")
	if len(betaGrants) != 1 {
		t.Errorf("agent/beta grants = %d, want 1", len(betaGrants))
	}
}

// TestRebuildAuthorizationIndex_RemovesStalePrincipals verifies that
// principals removed from config are removed from the authorization index.
func TestRebuildAuthorizationIndex_RemovesStalePrincipals(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	// First reconcile: two principals.
	daemon.rebuildAuthorizationIndex(&schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "agent/alpha"},
			{Localpart: "agent/beta"},
		},
	})

	if len(daemon.authorizationIndex.Principals()) != 2 {
		t.Fatalf("after first rebuild: principals = %d, want 2", len(daemon.authorizationIndex.Principals()))
	}

	// Second reconcile: only agent/alpha remains.
	daemon.rebuildAuthorizationIndex(&schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "agent/alpha"},
		},
	})

	principals := daemon.authorizationIndex.Principals()
	if len(principals) != 1 {
		t.Fatalf("after second rebuild: principals = %d, want 1", len(principals))
	}
	if principals[0] != "agent/alpha" {
		t.Errorf("remaining principal = %q, want %q", principals[0], "agent/alpha")
	}

	// agent/beta should have no grants (fully removed).
	if grants := daemon.authorizationIndex.Grants("agent/beta"); grants != nil {
		t.Errorf("agent/beta grants should be nil, got %d entries", len(grants))
	}
}

// TestRebuildAuthorizationIndex_PreservesTemporalGrants verifies that
// temporal grants are preserved across rebuilds. SetPrincipal is designed
// to merge existing temporal grants into the new policy — this test
// confirms that the rebuild path doesn't accidentally drop them.
func TestRebuildAuthorizationIndex_PreservesTemporalGrants(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	config := &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "agent/alpha"},
		},
	}

	// First rebuild establishes the principal.
	daemon.rebuildAuthorizationIndex(config)

	// Add a temporal grant between rebuilds.
	temporalGrant := schema.Grant{
		Actions:   []string{"service/register"},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "test-temporal-grant",
	}
	if !daemon.authorizationIndex.AddTemporalGrant("agent/alpha", temporalGrant) {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Verify it's there.
	if grants := daemon.authorizationIndex.Grants("agent/alpha"); len(grants) != 1 {
		t.Fatalf("before rebuild: grants = %d, want 1 (temporal)", len(grants))
	}

	// Second rebuild with the same config. The temporal grant should survive
	// because SetPrincipal preserves existing temporal entries.
	daemon.rebuildAuthorizationIndex(config)

	grants := daemon.authorizationIndex.Grants("agent/alpha")
	if len(grants) != 1 {
		t.Fatalf("after rebuild: grants = %d, want 1 (temporal preserved)", len(grants))
	}
	if grants[0].Ticket != "test-temporal-grant" {
		t.Errorf("preserved grant ticket = %q, want %q", grants[0].Ticket, "test-temporal-grant")
	}
}

func TestFilterGrantsForService(t *testing.T) {
	t.Parallel()

	grants := []schema.Grant{
		{Actions: []string{"ticket/create", "ticket/assign"}},
		{Actions: []string{"observe/*"}, Targets: []string{"**"}},
		{Actions: []string{"**"}},
		{Actions: []string{"artifact/fetch"}},
		{Actions: []string{"ticket/*"}, Targets: []string{"iree/**"}},
	}

	tests := []struct {
		name       string
		role       string
		wantCount  int
		wantAction string // first action of first matched grant
	}{
		{
			name:       "ticket role matches ticket grants and wildcard",
			role:       "ticket",
			wantCount:  3, // ticket/create+assign, **, ticket/*
			wantAction: "ticket/create",
		},
		{
			name:       "observe role matches observe grant and wildcard",
			role:       "observe",
			wantCount:  2, // observe/*, **
			wantAction: "observe/*",
		},
		{
			name:       "artifact role matches artifact grant and wildcard",
			role:       "artifact",
			wantCount:  2, // **, artifact/fetch
			wantAction: "**",
		},
		{
			name:      "unknown role matches only wildcard",
			role:      "unknown",
			wantCount: 1, // **
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filtered := filterGrantsForService(grants, test.role)
			if len(filtered) != test.wantCount {
				t.Errorf("filterGrantsForService(%q) = %d grants, want %d", test.role, len(filtered), test.wantCount)
				for i, grant := range filtered {
					t.Logf("  [%d] actions=%v", i, grant.Actions)
				}
			}
			if test.wantAction != "" && len(filtered) > 0 {
				if filtered[0].Actions[0] != test.wantAction {
					t.Errorf("first grant action = %q, want %q", filtered[0].Actions[0], test.wantAction)
				}
			}
		})
	}
}

func TestFilterGrantsForService_EmptyGrants(t *testing.T) {
	t.Parallel()

	filtered := filterGrantsForService(nil, "ticket")
	if len(filtered) != 0 {
		t.Errorf("nil grants should return empty, got %d", len(filtered))
	}

	filtered = filterGrantsForService([]schema.Grant{}, "ticket")
	if len(filtered) != 0 {
		t.Errorf("empty grants should return empty, got %d", len(filtered))
	}
}

func TestMintServiceTokens(t *testing.T) {
	t.Parallel()

	_, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	stateDir := t.TempDir()

	daemon, _ := newTestDaemon(t)
	daemon.machineName = "machine/test"
	daemon.stateDir = stateDir
	daemon.tokenSigningPrivateKey = privateKey

	// Set up the principal with grants covering the ticket namespace.
	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/create", "ticket/assign"}},
			{Actions: []string{"observe/*"}, Targets: []string{"**"}},
		},
	})

	tokenDir, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err != nil {
		t.Fatalf("mintServiceTokens: %v", err)
	}

	if tokenDir == "" {
		t.Fatal("tokenDir should not be empty")
	}

	// Verify the token file exists.
	tokenPath := filepath.Join(tokenDir, "ticket")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}

	if len(tokenBytes) == 0 {
		t.Fatal("token file should not be empty")
	}

	// Verify the token is valid (can be decoded and verified). Use VerifyAt
	// with the daemon's fake clock so the token isn't rejected as expired.
	publicKey := privateKey.Public().(ed25519.PublicKey)
	token, err := servicetoken.VerifyAt(publicKey, tokenBytes, daemon.clock.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if token.Subject != "agent/alpha" {
		t.Errorf("Subject = %q, want %q", token.Subject, "agent/alpha")
	}
	if token.Machine != "machine/test" {
		t.Errorf("Machine = %q, want %q", token.Machine, "machine/test")
	}
	if token.Audience != "ticket" {
		t.Errorf("Audience = %q, want %q", token.Audience, "ticket")
	}
	if token.ID == "" {
		t.Error("ID should not be empty")
	}

	// Should have 1 grant (ticket/* matching, observe/* filtered out).
	if len(token.Grants) != 1 {
		t.Errorf("Grants = %d, want 1 (only ticket namespace)", len(token.Grants))
	}
}

func TestMintServiceTokens_NoRequiredServices(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	tokenDir, err := daemon.mintServiceTokens("agent/alpha", nil)
	if err != nil {
		t.Fatalf("mintServiceTokens with nil services: %v", err)
	}
	if tokenDir != "" {
		t.Errorf("tokenDir should be empty for no required services, got %q", tokenDir)
	}
}

func TestMintServiceTokens_MissingPrivateKey(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.stateDir = t.TempDir()

	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{})

	_, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket"})
	if err == nil {
		t.Fatal("expected error when private key is nil")
	}
	if !strings.Contains(err.Error(), "token signing keypair not initialized") {
		t.Errorf("error = %q, want mention of token signing keypair", err.Error())
	}
}

func TestMintServiceTokens_MultipleServices(t *testing.T) {
	t.Parallel()

	_, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	stateDir := t.TempDir()

	daemon, _ := newTestDaemon(t)
	daemon.machineName = "machine/test"
	daemon.stateDir = stateDir
	daemon.tokenSigningPrivateKey = privateKey

	daemon.authorizationIndex.SetPrincipal("agent/alpha", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"ticket/**"}},
			{Actions: []string{"artifact/fetch"}},
		},
	})

	tokenDir, err := daemon.mintServiceTokens("agent/alpha", []string{"ticket", "artifact"})
	if err != nil {
		t.Fatalf("mintServiceTokens: %v", err)
	}

	// Verify both token files exist.
	for _, role := range []string{"ticket", "artifact"} {
		tokenPath := filepath.Join(tokenDir, role)
		if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
			t.Errorf("token file for %q should exist", role)
		}
	}
}

func TestSynthesizeGrants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     *schema.MatrixPolicy
		visibility []string
		expected   []schema.Grant
	}{
		{
			name:     "nil policy and empty visibility",
			policy:   nil,
			expected: nil,
		},
		{
			name:   "join only",
			policy: &schema.MatrixPolicy{AllowJoin: true},
			expected: []schema.Grant{
				{Actions: []string{"matrix/join"}},
			},
		},
		{
			name:   "invite only",
			policy: &schema.MatrixPolicy{AllowInvite: true},
			expected: []schema.Grant{
				{Actions: []string{"matrix/invite"}},
			},
		},
		{
			name:   "create-room only",
			policy: &schema.MatrixPolicy{AllowRoomCreate: true},
			expected: []schema.Grant{
				{Actions: []string{"matrix/create-room"}},
			},
		},
		{
			name: "all matrix permissions",
			policy: &schema.MatrixPolicy{
				AllowJoin:       true,
				AllowInvite:     true,
				AllowRoomCreate: true,
			},
			expected: []schema.Grant{
				{Actions: []string{"matrix/join"}},
				{Actions: []string{"matrix/invite"}},
				{Actions: []string{"matrix/create-room"}},
			},
		},
		{
			name:       "visibility only",
			visibility: []string{"service/stt/*", "service/embedding/**"},
			expected: []schema.Grant{
				{Actions: []string{"service/discover"}, Targets: []string{"service/stt/*", "service/embedding/**"}},
			},
		},
		{
			name:       "policy and visibility combined",
			policy:     &schema.MatrixPolicy{AllowJoin: true},
			visibility: []string{"service/**"},
			expected: []schema.Grant{
				{Actions: []string{"matrix/join"}},
				{Actions: []string{"service/discover"}, Targets: []string{"service/**"}},
			},
		},
		{
			name:   "zero-valued policy produces no grants",
			policy: &schema.MatrixPolicy{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := synthesizeGrants(tt.policy, tt.visibility)
			if len(got) != len(tt.expected) {
				t.Fatalf("got %d grants, want %d: %v", len(got), len(tt.expected), got)
			}
			for i := range got {
				if !slicesEqual(got[i].Actions, tt.expected[i].Actions) {
					t.Errorf("grant[%d].Actions = %v, want %v", i, got[i].Actions, tt.expected[i].Actions)
				}
				if !slicesEqual(got[i].Targets, tt.expected[i].Targets) {
					t.Errorf("grant[%d].Targets = %v, want %v", i, got[i].Targets, tt.expected[i].Targets)
				}
			}
		})
	}
}

// slicesEqual compares two string slices for equality.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReconcileAuthorizationGrantsHotReload(t *testing.T) {
	t.Parallel()

	const (
		configRoomID = "!config:test.local"
		serverName   = "test.local"
		machineName  = "machine/test"
	)

	// MachineConfig with a MatrixPolicy and ServiceVisibility — these get
	// synthesized into grants since there's no explicit Authorization field.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart:         "agent/test",
				AutoStart:         true,
				MatrixPolicy:      &schema.MatrixPolicy{AllowJoin: true, AllowInvite: true},
				ServiceVisibility: []string{"service/stt/**"},
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

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	// Mock admin server that captures PUT /v1/admin/authorization calls.
	var (
		grantsMu        sync.Mutex
		receivedGrants  []schema.Grant
		grantsCallCount int
	)
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("PUT /v1/admin/authorization", func(w http.ResponseWriter, r *http.Request) {
		grantsMu.Lock()
		defer grantsMu.Unlock()
		grantsCallCount++

		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &receivedGrants)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})
	// Accept all other admin endpoints so they don't fail the reconcile.
	adminMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	adminSocketPath := filepath.Join(socketDir, "agent/test.admin.sock")
	os.MkdirAll(filepath.Dir(adminSocketPath), 0755)
	adminListener, err := net.Listen("unix", adminSocketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer adminListener.Close()
	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(adminListener)
	defer adminServer.Close()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.running["agent/test"] = true
	daemon.lastCredentials["agent/test"] = "encrypted-test-credentials"
	// Old grants differ from what the config now produces.
	daemon.lastGrants["agent/test"] = []schema.Grant{
		{Actions: []string{"matrix/join"}},
	}
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	grantsMu.Lock()
	defer grantsMu.Unlock()

	if grantsCallCount != 1 {
		t.Errorf("expected 1 grants push, got %d", grantsCallCount)
	}

	// The new config has AllowJoin + AllowInvite + ServiceVisibility,
	// which synthesizes to three grants.
	expectedGrants := []schema.Grant{
		{Actions: []string{"matrix/join"}},
		{Actions: []string{"matrix/invite"}},
		{Actions: []string{"service/discover"}, Targets: []string{"service/stt/**"}},
	}
	if len(receivedGrants) != len(expectedGrants) {
		t.Fatalf("proxy received %d grants, want %d: %v", len(receivedGrants), len(expectedGrants), receivedGrants)
	}
	for i := range expectedGrants {
		if !slicesEqual(receivedGrants[i].Actions, expectedGrants[i].Actions) {
			t.Errorf("grant[%d].Actions = %v, want %v", i, receivedGrants[i].Actions, expectedGrants[i].Actions)
		}
		if !slicesEqual(receivedGrants[i].Targets, expectedGrants[i].Targets) {
			t.Errorf("grant[%d].Targets = %v, want %v", i, receivedGrants[i].Targets, expectedGrants[i].Targets)
		}
	}

	// lastGrants should be updated to the new grants.
	stored := daemon.lastGrants["agent/test"]
	if len(stored) != 3 {
		t.Errorf("lastGrants has %d entries, want 3", len(stored))
	}
}
