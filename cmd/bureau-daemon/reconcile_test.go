// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
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
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
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
			case ipc.ActionStatus:
				encoder.Encode(launcherIPCResponse{OK: true})
			case ipc.ActionCreateSandbox:
				encoder.Encode(launcherIPCResponse{OK: true, ProxyPID: 12345})
			case ipc.ActionDestroySandbox:
				encoder.Encode(launcherIPCResponse{OK: true})
			default:
				encoder.Encode(launcherIPCResponse{
					OK:    false,
					Error: "unknown action: " + string(request.Action),
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
			Action: ipc.ActionStatus,
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
			Action:    ipc.ActionCreateSandbox,
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
		Action: ipc.ActionStatus,
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
		if request.Action != ipc.ActionStatus {
			return launcherIPCResponse{OK: false, Error: "unexpected action: " + string(request.Action)}
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

	const configRoomID = "!config:test.local"

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()

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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	const configRoomID = "!config:test.local"

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()

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
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+string(schema.MatrixEventTypeMessage)+"/") {
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		case ipc.ActionStatus:
			return launcherIPCResponse{
				OK:              true,
				BinaryHash:      "",
				ProxyBinaryPath: currentProxy,
			}
		case ipc.ActionUpdateProxyBinary:
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

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.daemonBinaryHash = daemonHash
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()
	fleetPrefix := daemon.fleet.Localpart() + "/"

	// Start with one command, then change the template to a different command.
	state := newMockMatrixState()
	state.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "agent/test"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Intercept messages for verification.
	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+string(schema.MatrixEventTypeMessage)+"/") {
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		ipcActions = append(ipcActions, string(request.Action))
		ipcPrincipals = append(ipcPrincipals, request.Principal)
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	agentTestEntity := testEntity(t, daemon.fleet, "agent/test")
	daemon.running[agentTestEntity] = true
	daemon.lastSpecs[agentTestEntity] = &schema.SandboxSpec{
		Command: []string{"/bin/agent", "--mode=v1"},
	}
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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
		t.Errorf("expected destroy-sandbox call for agent/test, got principals: %v", ipcPrincipals)
	}
	if createIndex == -1 {
		t.Errorf("expected create-sandbox call for agent/test, got principals: %v", ipcPrincipals)
	}
	if destroyIndex != -1 && createIndex != -1 && destroyIndex >= createIndex {
		t.Errorf("destroy-sandbox (index %d) should come before create-sandbox (index %d)",
			destroyIndex, createIndex)
	}

	// Principal should be running again after the cycle.
	if !daemon.running[agentTestEntity] {
		t.Error("principal should be running after structural restart cycle")
	}

	// Structural restart should save the old spec as a rollback target.
	// After the "create missing" pass recreates, previousSpecs should
	// still hold the pre-restart spec (it's only cleared on rollback or
	// principal removal).
	if daemon.previousSpecs[agentTestEntity] == nil {
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()
	fleetPrefix := daemon.fleet.Localpart() + "/"

	// Template with changed command but same payload.
	state := newMockMatrixState()
	state.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/agent", "--mode=v2"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "agent/test"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+string(schema.MatrixEventTypeMessage)+"/") {
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		ipcActions = append(ipcActions, string(request.Action))
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	// Payload is nil in both old and new spec — only command differs.
	agentTestEntity := testEntity(t, daemon.fleet, "agent/test")
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.running[agentTestEntity] = true
	daemon.lastSpecs[agentTestEntity] = &schema.SandboxSpec{
		Command: []string{"/bin/agent", "--mode=v1"},
	}
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()
	fleetPrefix := daemon.fleet.Localpart() + "/"

	agentTestEntity := testEntity(t, daemon.fleet, "agent/test")

	// Template with a command and a default payload.
	state := newMockMatrixState()
	state.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:        []string{"/bin/agent", "--mode=v1"},
		DefaultPayload: map[string]any{"model": "gpt-4"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: agentTestEntity,
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
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

	var (
		launcherMu sync.Mutex
		ipcActions []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, string(request.Action))
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true}
	})
	t.Cleanup(func() { listener.Close() })

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// First reconcile: creates the sandbox through the production path,
	// populating lastSpecs with whatever the create path produces.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}
	if !daemon.running[agentTestEntity] {
		t.Fatal("principal should be running after first reconcile")
	}

	// Clear tracked actions so we only see the second reconcile's IPC.
	launcherMu.Lock()
	ipcActions = nil
	launcherMu.Unlock()

	// Change the template's default payload. The command stays the same,
	// so the only difference is in Payload → hot-reload, not restart.
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command:        []string{"/bin/agent", "--mode=v1"},
		DefaultPayload: map[string]any{"model": "claude-4"},
	})

	// Second reconcile: should hot-reload the payload, not destroy.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
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
	if !daemon.running[agentTestEntity] {
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

	_, err = daemon.launcherRequest(ctx, launcherIPCRequest{Action: ipc.ActionStatus})
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
				Grants:     []schema.Grant{{Actions: []string{schema.ActionServiceDiscover}}},
				Allowances: []schema.Allowance{{Actions: []string{"observe/attach"}, Actors: []string{"admin/**:**"}}},
			},
			wantGrants:     1,
			wantAllowances: 1,
		},
		{
			name: "both policies merge additively",
			defaultPolicy: &schema.AuthorizationPolicy{
				Grants:           []schema.Grant{{Actions: []string{"observe/*"}}},
				Denials:          []schema.Denial{{Actions: []string{"admin/*"}}},
				Allowances:       []schema.Allowance{{Actions: []string{"observe/attach"}, Actors: []string{"**:**"}}},
				AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{schema.ActionObserveResize}, Actors: []string{"untrusted/**:**"}}},
			},
			principalPolicy: &schema.AuthorizationPolicy{
				Grants:           []schema.Grant{{Actions: []string{schema.ActionServiceDiscover}}, {Actions: []string{schema.ActionMatrixJoin}}},
				Denials:          []schema.Denial{{Actions: []string{schema.ActionServiceRegister}}},
				Allowances:       []schema.Allowance{{Actions: []string{schema.ActionObserveInput}, Actors: []string{"admin/**:**"}}},
				AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{schema.ActionObserveInput}, Actors: []string{"untrusted/**:**"}}},
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
	if merged.Grants[0].Source != schema.SourceMachineDefault {
		t.Errorf("Grants[0].Source = %q, want %q", merged.Grants[0].Source, schema.SourceMachineDefault)
	}
	if merged.Grants[1].Actions[0] != "principal-action" {
		t.Errorf("Grants[1].Actions[0] = %q, want %q", merged.Grants[1].Actions[0], "principal-action")
	}
	if merged.Grants[1].Source != schema.SourcePrincipal {
		t.Errorf("Grants[1].Source = %q, want %q", merged.Grants[1].Source, schema.SourcePrincipal)
	}
}

// TestMergeAuthorizationPolicy_SourceProvenance verifies that all four
// policy types (grants, denials, allowances, allowance denials) are
// stamped with the correct source during merge.
func TestMergeAuthorizationPolicy_SourceProvenance(t *testing.T) {
	t.Parallel()

	merged := mergeAuthorizationPolicy(
		&schema.AuthorizationPolicy{
			Grants:           []schema.Grant{{Actions: []string{"default-grant"}}},
			Denials:          []schema.Denial{{Actions: []string{"default-denial"}}},
			Allowances:       []schema.Allowance{{Actions: []string{"default-allowance"}, Actors: []string{"**:**"}}},
			AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{"default-allow-denial"}, Actors: []string{"bad/**:**"}}},
		},
		&schema.AuthorizationPolicy{
			Grants:           []schema.Grant{{Actions: []string{"principal-grant"}}},
			Denials:          []schema.Denial{{Actions: []string{"principal-denial"}}},
			Allowances:       []schema.Allowance{{Actions: []string{"principal-allowance"}, Actors: []string{"admin/**:**"}}},
			AllowanceDenials: []schema.AllowanceDenial{{Actions: []string{"principal-allow-denial"}, Actors: []string{"evil/**:**"}}},
		},
	)

	// Grants.
	if merged.Grants[0].Source != schema.SourceMachineDefault {
		t.Errorf("Grants[0].Source = %q, want %q", merged.Grants[0].Source, schema.SourceMachineDefault)
	}
	if merged.Grants[1].Source != schema.SourcePrincipal {
		t.Errorf("Grants[1].Source = %q, want %q", merged.Grants[1].Source, schema.SourcePrincipal)
	}

	// Denials.
	if merged.Denials[0].Source != schema.SourceMachineDefault {
		t.Errorf("Denials[0].Source = %q, want %q", merged.Denials[0].Source, schema.SourceMachineDefault)
	}
	if merged.Denials[1].Source != schema.SourcePrincipal {
		t.Errorf("Denials[1].Source = %q, want %q", merged.Denials[1].Source, schema.SourcePrincipal)
	}

	// Allowances.
	if merged.Allowances[0].Source != schema.SourceMachineDefault {
		t.Errorf("Allowances[0].Source = %q, want %q", merged.Allowances[0].Source, schema.SourceMachineDefault)
	}
	if merged.Allowances[1].Source != schema.SourcePrincipal {
		t.Errorf("Allowances[1].Source = %q, want %q", merged.Allowances[1].Source, schema.SourcePrincipal)
	}

	// Allowance denials.
	if merged.AllowanceDenials[0].Source != schema.SourceMachineDefault {
		t.Errorf("AllowanceDenials[0].Source = %q, want %q", merged.AllowanceDenials[0].Source, schema.SourceMachineDefault)
	}
	if merged.AllowanceDenials[1].Source != schema.SourcePrincipal {
		t.Errorf("AllowanceDenials[1].Source = %q, want %q", merged.AllowanceDenials[1].Source, schema.SourcePrincipal)
	}
}

// TestMergeAuthorizationPolicy_DoesNotMutateInput verifies that the merge
// stamps Source on copies, not on the original config data.
func TestMergeAuthorizationPolicy_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	original := &schema.AuthorizationPolicy{
		Grants: []schema.Grant{{Actions: []string{"observe/*"}}},
	}

	_ = mergeAuthorizationPolicy(original, nil)

	if original.Grants[0].Source != "" {
		t.Errorf("merge mutated input: Source = %q, want empty", original.Grants[0].Source)
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
				Principal: testEntity(t, daemon.fleet, "agent/alpha"),
				AutoStart: true,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{schema.ActionServiceDiscover}}},
				},
			},
			{
				Principal: testEntity(t, daemon.fleet, "agent/beta"),
				AutoStart: true,
				// No per-principal policy — only inherits defaults.
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config, nil, nil)

	// Both principals should be in the index.
	principals := daemon.authorizationIndex.Principals()
	if len(principals) != 2 {
		t.Fatalf("Principals() count = %d, want 2", len(principals))
	}

	// agent/alpha should have 2 grants (1 default + 1 per-principal).
	alphaGrants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/alpha").UserID())
	if len(alphaGrants) != 2 {
		t.Fatalf("agent/alpha grants = %d, want 2", len(alphaGrants))
	}
	if alphaGrants[0].Source != schema.SourceMachineDefault {
		t.Errorf("agent/alpha grants[0].Source = %q, want %q", alphaGrants[0].Source, schema.SourceMachineDefault)
	}
	if alphaGrants[1].Source != schema.SourcePrincipal {
		t.Errorf("agent/alpha grants[1].Source = %q, want %q", alphaGrants[1].Source, schema.SourcePrincipal)
	}

	// agent/beta should have 1 grant (default only).
	betaGrants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/beta").UserID())
	if len(betaGrants) != 1 {
		t.Fatalf("agent/beta grants = %d, want 1", len(betaGrants))
	}
	if betaGrants[0].Source != schema.SourceMachineDefault {
		t.Errorf("agent/beta grants[0].Source = %q, want %q", betaGrants[0].Source, schema.SourceMachineDefault)
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
			{Principal: testEntity(t, daemon.fleet, "agent/alpha")},
			{Principal: testEntity(t, daemon.fleet, "agent/beta")},
		},
	}, nil, nil)

	if len(daemon.authorizationIndex.Principals()) != 2 {
		t.Fatalf("after first rebuild: principals = %d, want 2", len(daemon.authorizationIndex.Principals()))
	}

	// Second reconcile: only agent/alpha remains.
	daemon.rebuildAuthorizationIndex(&schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: testEntity(t, daemon.fleet, "agent/alpha")},
		},
	}, nil, nil)

	principals := daemon.authorizationIndex.Principals()
	if len(principals) != 1 {
		t.Fatalf("after second rebuild: principals = %d, want 1", len(principals))
	}
	expectedPrincipal := testEntity(t, daemon.fleet, "agent/alpha").UserID()
	if principals[0] != expectedPrincipal {
		t.Errorf("remaining principal = %q, want %q", principals[0], expectedPrincipal)
	}

	// agent/beta should have no grants (fully removed).
	if grants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/beta").UserID()); grants != nil {
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
			{Principal: testEntity(t, daemon.fleet, "agent/alpha")},
		},
	}

	// First rebuild establishes the principal.
	daemon.rebuildAuthorizationIndex(config, nil, nil)

	// Add a temporal grant between rebuilds.
	temporalGrant := schema.Grant{
		Actions:   []string{schema.ActionServiceRegister},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "test-temporal-grant",
	}
	alphaUserID := testEntity(t, daemon.fleet, "agent/alpha").UserID()
	if !daemon.authorizationIndex.AddTemporalGrant(alphaUserID, temporalGrant) {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Verify it's there.
	if grants := daemon.authorizationIndex.Grants(alphaUserID); len(grants) != 1 {
		t.Fatalf("before rebuild: grants = %d, want 1 (temporal)", len(grants))
	}

	// Second rebuild with the same config. The temporal grant should survive
	// because SetPrincipal preserves existing temporal entries.
	daemon.rebuildAuthorizationIndex(config, nil, nil)

	grants := daemon.authorizationIndex.Grants(alphaUserID)
	if len(grants) != 1 {
		t.Fatalf("after rebuild: grants = %d, want 1 (temporal preserved)", len(grants))
	}
	if grants[0].Ticket != "test-temporal-grant" {
		t.Errorf("preserved grant ticket = %q, want %q", grants[0].Ticket, "test-temporal-grant")
	}
}

func TestRebuildAuthorizationIndex_RoomLevelMemberGrants(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	workspaceRoomID := mustRoomID("!workspace:test")
	daemon.configRoomID = mustRoomID("!config:test")

	alpha := testEntity(t, daemon.fleet, "agent/alpha")
	beta := testEntity(t, daemon.fleet, "agent/beta")

	config := &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: alpha, AutoStart: true},
			{Principal: beta, AutoStart: true},
		},
	}

	// alpha is in the workspace room; beta is not.
	conditionRoomIDs := map[ref.Entity]ref.RoomID{
		alpha: workspaceRoomID,
	}

	// The workspace room has a room-level authorization policy with
	// MemberGrants. The config room does not.
	roomPolicies := map[ref.RoomID]*fetchedRoomPolicy{
		workspaceRoomID: {
			policy: schema.RoomAuthorizationPolicy{
				MemberGrants: []schema.Grant{
					{Actions: []string{schema.ActionTicketCreate}, Targets: []string{"**:**"}},
				},
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config, conditionRoomIDs, roomPolicies)

	// alpha should have the room-level MemberGrants (config room has no
	// policy, workspace room does).
	alphaGrants := daemon.authorizationIndex.Grants(alpha.UserID())
	if len(alphaGrants) != 1 {
		t.Fatalf("alpha grants = %d, want 1 (room-level MemberGrant)", len(alphaGrants))
	}
	if alphaGrants[0].Actions[0] != schema.ActionTicketCreate {
		t.Errorf("alpha grant action = %q, want %q", alphaGrants[0].Actions[0], schema.ActionTicketCreate)
	}
	expectedSource := schema.SourceRoom(workspaceRoomID.String())
	if alphaGrants[0].Source != expectedSource {
		t.Errorf("alpha grant source = %q, want %q", alphaGrants[0].Source, expectedSource)
	}

	// beta is only in the config room (which has no policy), so no
	// room-level grants.
	betaGrants := daemon.authorizationIndex.Grants(beta.UserID())
	if len(betaGrants) != 0 {
		t.Fatalf("beta grants = %d, want 0 (no room-level grants)", len(betaGrants))
	}
}

func TestRebuildAuthorizationIndex_PowerLevelGrants(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	workspaceRoomID := mustRoomID("!workspace:test")
	daemon.configRoomID = mustRoomID("!config:test")

	alpha := testEntity(t, daemon.fleet, "agent/alpha")
	beta := testEntity(t, daemon.fleet, "agent/beta")

	config := &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Principal: alpha, AutoStart: true},
			{Principal: beta, AutoStart: true},
		},
	}

	// Both principals are in the workspace room.
	conditionRoomIDs := map[ref.Entity]ref.RoomID{
		alpha: workspaceRoomID,
		beta:  workspaceRoomID,
	}

	// Room has MemberGrants (PL 0) and PowerLevelGrants at PL 50.
	// alpha has PL 100, beta has PL 0 (default).
	roomPolicies := map[ref.RoomID]*fetchedRoomPolicy{
		workspaceRoomID: {
			policy: schema.RoomAuthorizationPolicy{
				MemberGrants: []schema.Grant{
					{Actions: []string{"observe/*"}},
				},
				PowerLevelGrants: map[string][]schema.Grant{
					"50": {
						{Actions: []string{schema.ActionInterrupt}, Targets: []string{"**:**"}},
					},
				},
			},
			powerLevels: schema.PowerLevels{
				Users: map[string]int{
					alpha.UserID().String(): 100,
				},
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config, conditionRoomIDs, roomPolicies)

	// alpha (PL 100) gets both MemberGrants and PL 50 grants.
	alphaGrants := daemon.authorizationIndex.Grants(alpha.UserID())
	if len(alphaGrants) != 2 {
		t.Fatalf("alpha grants = %d, want 2 (MemberGrant + PowerLevelGrant)", len(alphaGrants))
	}
	// First grant: MemberGrant (observe/*).
	if alphaGrants[0].Actions[0] != "observe/*" {
		t.Errorf("alpha grants[0] action = %q, want %q", alphaGrants[0].Actions[0], "observe/*")
	}
	// Second grant: PowerLevelGrant at PL 50 (interrupt).
	if alphaGrants[1].Actions[0] != schema.ActionInterrupt {
		t.Errorf("alpha grants[1] action = %q, want %q", alphaGrants[1].Actions[0], schema.ActionInterrupt)
	}

	// beta (PL 0, below 50) gets only MemberGrants.
	betaGrants := daemon.authorizationIndex.Grants(beta.UserID())
	if len(betaGrants) != 1 {
		t.Fatalf("beta grants = %d, want 1 (MemberGrant only)", len(betaGrants))
	}
	if betaGrants[0].Actions[0] != "observe/*" {
		t.Errorf("beta grants[0] action = %q, want %q", betaGrants[0].Actions[0], "observe/*")
	}
}

func TestRebuildAuthorizationIndex_RoomGrantsMergeWithPrincipalPolicy(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	workspaceRoomID := mustRoomID("!workspace:test")
	daemon.configRoomID = mustRoomID("!config:test")

	alpha := testEntity(t, daemon.fleet, "agent/alpha")

	config := &schema.MachineConfig{
		DefaultPolicy: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{{Actions: []string{schema.ActionServiceDiscover}}},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Principal: alpha,
				AutoStart: true,
				Authorization: &schema.AuthorizationPolicy{
					Grants: []schema.Grant{{Actions: []string{schema.ActionMatrixJoin}}},
				},
			},
		},
	}

	conditionRoomIDs := map[ref.Entity]ref.RoomID{
		alpha: workspaceRoomID,
	}

	roomPolicies := map[ref.RoomID]*fetchedRoomPolicy{
		workspaceRoomID: {
			policy: schema.RoomAuthorizationPolicy{
				MemberGrants: []schema.Grant{
					{Actions: []string{"observe/*"}},
				},
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config, conditionRoomIDs, roomPolicies)

	// alpha should have all three grant sources:
	// 1. machine-default: service/discover
	// 2. per-principal: matrix/join
	// 3. room-level: observe/*
	grants := daemon.authorizationIndex.Grants(alpha.UserID())
	if len(grants) != 3 {
		t.Fatalf("grants = %d, want 3 (default + principal + room)", len(grants))
	}

	// Verify source provenance and ordering.
	if grants[0].Source != schema.SourceMachineDefault {
		t.Errorf("grants[0].Source = %q, want %q", grants[0].Source, schema.SourceMachineDefault)
	}
	if grants[1].Source != schema.SourcePrincipal {
		t.Errorf("grants[1].Source = %q, want %q", grants[1].Source, schema.SourcePrincipal)
	}
	expectedRoomSource := schema.SourceRoom(workspaceRoomID.String())
	if grants[2].Source != expectedRoomSource {
		t.Errorf("grants[2].Source = %q, want %q", grants[2].Source, expectedRoomSource)
	}
}

func TestRebuildAuthorizationIndex_PayloadWorkspaceRoomID(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	workspaceRoomID := mustRoomID("!workspace-payload:test")
	daemon.configRoomID = mustRoomID("!config:test")

	alpha := testEntity(t, daemon.fleet, "agent/alpha")

	config := &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: alpha,
				AutoStart: true,
				Payload: map[string]any{
					"WORKSPACE_ROOM_ID": workspaceRoomID.String(),
				},
			},
		},
	}

	// No conditionRoomIDs — the workspace room comes from the payload.
	roomPolicies := map[ref.RoomID]*fetchedRoomPolicy{
		workspaceRoomID: {
			policy: schema.RoomAuthorizationPolicy{
				MemberGrants: []schema.Grant{
					{Actions: []string{"ticket/view"}},
				},
			},
		},
	}

	daemon.rebuildAuthorizationIndex(config, nil, roomPolicies)

	grants := daemon.authorizationIndex.Grants(alpha.UserID())
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1 (room-level from payload WORKSPACE_ROOM_ID)", len(grants))
	}
	if grants[0].Actions[0] != "ticket/view" {
		t.Errorf("grant action = %q, want %q", grants[0].Actions[0], "ticket/view")
	}
}

func TestFilterGrantsForService(t *testing.T) {
	t.Parallel()

	grants := []schema.Grant{
		{Actions: []string{schema.ActionTicketCreate, "ticket/assign"}},
		{Actions: []string{"observe/*"}, Targets: []string{"**:**"}},
		{Actions: []string{"**"}},
		{Actions: []string{schema.ActionArtifactFetch}},
		{Actions: []string{"ticket/*"}, Targets: []string{"**/iree/**:**"}},
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
			wantAction: schema.ActionTicketCreate,
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
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	daemon.stateDir = stateDir
	daemon.tokenSigningPrivateKey = privateKey

	// Set up the principal with grants covering the ticket namespace.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "agent/alpha").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionTicketCreate, "ticket/assign"}},
			{Actions: []string{"observe/*"}, Targets: []string{"**:**"}},
		},
	})

	tokenDir, minted, err := daemon.mintServiceTokens(testEntity(t, daemon.fleet, "agent/alpha"), []string{"ticket"})
	if err != nil {
		t.Fatalf("mintServiceTokens: %v", err)
	}

	if tokenDir == "" {
		t.Fatal("tokenDir should not be empty")
	}

	// Verify minted token entries are returned.
	if len(minted) != 1 {
		t.Fatalf("minted = %d entries, want 1", len(minted))
	}
	if minted[0].serviceRole != "ticket" {
		t.Errorf("minted[0].serviceRole = %q, want %q", minted[0].serviceRole, "ticket")
	}
	if minted[0].id == "" {
		t.Error("minted[0].id should not be empty")
	}

	// Verify the token file exists.
	tokenPath := filepath.Join(tokenDir, "ticket.token")
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

	wantSubject := testEntity(t, daemon.fleet, "agent/alpha").UserID()
	if token.Subject != wantSubject {
		t.Errorf("Subject = %q, want %q", token.Subject, wantSubject)
	}
	if token.Machine != daemon.machine {
		t.Errorf("Machine = %q, want %q", token.Machine, daemon.machine)
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

	tokenDir, minted, err := daemon.mintServiceTokens(testEntity(t, daemon.fleet, "agent/alpha"), nil)
	if err != nil {
		t.Fatalf("mintServiceTokens with nil services: %v", err)
	}
	if tokenDir != "" {
		t.Errorf("tokenDir should be empty for no required services, got %q", tokenDir)
	}
	if len(minted) != 0 {
		t.Errorf("minted should be empty for no required services, got %d", len(minted))
	}
}

func TestMintServiceTokens_MissingPrivateKey(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.stateDir = t.TempDir()

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "agent/alpha").UserID(), schema.AuthorizationPolicy{})

	_, _, err := daemon.mintServiceTokens(testEntity(t, daemon.fleet, "agent/alpha"), []string{"ticket"})
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
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	daemon.stateDir = stateDir
	daemon.tokenSigningPrivateKey = privateKey

	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "agent/alpha").UserID(), schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{schema.ActionTicketAll}},
			{Actions: []string{schema.ActionArtifactFetch}},
		},
	})

	tokenDir, minted, err := daemon.mintServiceTokens(testEntity(t, daemon.fleet, "agent/alpha"), []string{"ticket", "artifact"})
	if err != nil {
		t.Fatalf("mintServiceTokens: %v", err)
	}

	// Verify both token files exist.
	for _, role := range []string{"ticket", "artifact"} {
		tokenPath := filepath.Join(tokenDir, role+".token")
		if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
			t.Errorf("token file for %q should exist", role)
		}
	}

	// Verify minted entries cover both services.
	if len(minted) != 2 {
		t.Fatalf("minted = %d entries, want 2", len(minted))
	}
	roles := map[string]bool{}
	for _, entry := range minted {
		roles[entry.serviceRole] = true
	}
	if !roles["ticket"] || !roles["artifact"] {
		t.Errorf("minted roles = %v, want ticket and artifact", roles)
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
				{Actions: []string{schema.ActionMatrixJoin}},
			},
		},
		{
			name:   "invite only",
			policy: &schema.MatrixPolicy{AllowInvite: true},
			expected: []schema.Grant{
				{Actions: []string{schema.ActionMatrixInvite}},
			},
		},
		{
			name:   "create-room only",
			policy: &schema.MatrixPolicy{AllowRoomCreate: true},
			expected: []schema.Grant{
				{Actions: []string{schema.ActionMatrixCreateRoom}},
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
				{Actions: []string{schema.ActionMatrixJoin}},
				{Actions: []string{schema.ActionMatrixInvite}},
				{Actions: []string{schema.ActionMatrixCreateRoom}},
			},
		},
		{
			name:       "visibility only",
			visibility: []string{"service/stt/*:**", "service/embedding/**:**"},
			expected: []schema.Grant{
				{Actions: []string{schema.ActionServiceDiscover}, Targets: []string{"service/stt/*:**", "service/embedding/**:**"}},
			},
		},
		{
			name:       "policy and visibility combined",
			policy:     &schema.MatrixPolicy{AllowJoin: true},
			visibility: []string{"service/**:**"},
			expected: []schema.Grant{
				{Actions: []string{schema.ActionMatrixJoin}},
				{Actions: []string{schema.ActionServiceDiscover}, Targets: []string{"service/**:**"}},
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

	const configRoomID = "!config:test.local"

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()
	fleetPrefix := daemon.fleet.Localpart() + "/"

	// MachineConfig with a MatrixPolicy and ServiceVisibility — these get
	// synthesized into grants since there's no explicit Authorization field.
	state := newMockMatrixState()
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal:         testEntity(t, daemon.fleet, "agent/test"),
				AutoStart:         true,
				MatrixPolicy:      &schema.MatrixPolicy{AllowJoin: true, AllowInvite: true},
				ServiceVisibility: []string{"service/stt/**"},
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
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

	agentTestEntity := testEntity(t, daemon.fleet, "agent/test")
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.running[agentTestEntity] = true
	daemon.lastCredentials[agentTestEntity] = "encrypted-test-credentials"
	// Old grants differ from what the config now produces.
	daemon.lastGrants[agentTestEntity] = []schema.Grant{
		{Actions: []string{schema.ActionMatrixJoin}},
	}
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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
		{Actions: []string{schema.ActionMatrixJoin}},
		{Actions: []string{schema.ActionMatrixInvite}},
		{Actions: []string{schema.ActionServiceDiscover}, Targets: []string{"service/stt/**"}},
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
	stored := daemon.lastGrants[agentTestEntity]
	if len(stored) != 3 {
		t.Errorf("lastGrants has %d entries, want 3", len(stored))
	}
}

func TestApplyPipelineExecutorOverlay(t *testing.T) {
	t.Parallel()

	const (
		executorBinary  = "/nix/store/abc-executor/bin/bureau-pipeline-executor"
		workspaceRoot   = "/var/bureau/workspace"
		pipelineEnvPath = "/nix/store/xyz-env"
	)

	tests := []struct {
		name                   string
		pipelineExecutorBinary string
		pipelineEnvironment    string
		workspaceRoot          string
		spec                   *schema.SandboxSpec
		wantApplied            bool
		wantCommand            []string
		wantFilesystemLength   int
		wantBureauSandbox      string
		wantEnvironmentPath    string
	}{
		{
			name:                   "pipeline_ref with empty command",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				Filesystem: []schema.TemplateMount{
					{Dest: "/tmp", Type: "tmpfs"},
				},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
				Payload: map[string]any{
					"pipeline_ref":      "bureau/pipeline:dev-workspace-init",
					"WORKSPACE_ROOM_ID": "!room:test",
					"PROJECT":           "test",
				},
			},
			wantApplied:          true,
			wantCommand:          []string{executorBinary},
			wantFilesystemLength: 3, // original /tmp + executor binary + workspace root
			wantBureauSandbox:    "1",
			wantEnvironmentPath:  pipelineEnvPath,
		},
		{
			name:                   "existing command not overwritten",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				Command: []string{"/bin/my-agent"},
				Payload: map[string]any{
					"pipeline_ref": "bureau/pipeline:dev-workspace-init",
				},
			},
			wantApplied: false,
		},
		{
			name:                   "no pipeline keys in payload",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				Payload: map[string]any{
					"some_other_key": "value",
				},
			},
			wantApplied: false,
		},
		{
			name:                   "nil payload",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec:                   &schema.SandboxSpec{},
			wantApplied:            false,
		},
		{
			name:                   "no pipeline executor binary configured",
			pipelineExecutorBinary: "",
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				Payload: map[string]any{
					"pipeline_ref": "bureau/pipeline:dev-workspace-init",
				},
			},
			wantApplied: false,
		},
		{
			name:                   "existing environment path preserved",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    pipelineEnvPath,
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				EnvironmentPath: "/nix/store/existing-env",
				Payload: map[string]any{
					"pipeline_ref": "bureau/pipeline:dev-workspace-init",
				},
			},
			wantApplied:          true,
			wantCommand:          []string{executorBinary},
			wantFilesystemLength: 2,
			wantBureauSandbox:    "1",
			wantEnvironmentPath:  "/nix/store/existing-env",
		},
		{
			name:                   "nil environment variables initialized",
			pipelineExecutorBinary: executorBinary,
			pipelineEnvironment:    "",
			workspaceRoot:          workspaceRoot,
			spec: &schema.SandboxSpec{
				Payload: map[string]any{
					"pipeline_ref": "bureau/pipeline:dev-workspace-init",
				},
			},
			wantApplied:          true,
			wantCommand:          []string{executorBinary},
			wantFilesystemLength: 2,
			wantBureauSandbox:    "1",
			wantEnvironmentPath:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			daemon, _ := newTestDaemon(t)
			daemon.pipelineExecutorBinary = test.pipelineExecutorBinary
			daemon.pipelineEnvironment = test.pipelineEnvironment
			daemon.workspaceRoot = test.workspaceRoot

			applied := daemon.applyPipelineExecutorOverlay(test.spec)

			if applied != test.wantApplied {
				t.Fatalf("applyPipelineExecutorOverlay() = %v, want %v", applied, test.wantApplied)
			}

			if !test.wantApplied {
				return
			}

			// Verify command.
			if len(test.spec.Command) != len(test.wantCommand) {
				t.Errorf("Command = %v, want %v", test.spec.Command, test.wantCommand)
			} else {
				for index, value := range test.wantCommand {
					if test.spec.Command[index] != value {
						t.Errorf("Command[%d] = %q, want %q", index, test.spec.Command[index], value)
					}
				}
			}

			// Verify filesystem mounts.
			if len(test.spec.Filesystem) != test.wantFilesystemLength {
				t.Errorf("Filesystem length = %d, want %d", len(test.spec.Filesystem), test.wantFilesystemLength)
			}

			// The last two mounts should be the executor binary and workspace root.
			mounts := test.spec.Filesystem
			if len(mounts) >= 2 {
				executorMount := mounts[len(mounts)-2]
				if executorMount.Source != executorBinary || executorMount.Dest != executorBinary || executorMount.Mode != "ro" {
					t.Errorf("executor mount = {Source:%q Dest:%q Mode:%q}, want {Source:%q Dest:%q Mode:ro}",
						executorMount.Source, executorMount.Dest, executorMount.Mode, executorBinary, executorBinary)
				}
				workspaceMount := mounts[len(mounts)-1]
				if workspaceMount.Source != workspaceRoot || workspaceMount.Dest != "/workspace" || workspaceMount.Mode != "rw" {
					t.Errorf("workspace mount = {Source:%q Dest:%q Mode:%q}, want {Source:%q Dest:/workspace Mode:rw}",
						workspaceMount.Source, workspaceMount.Dest, workspaceMount.Mode, workspaceRoot)
				}
			}

			// Verify BUREAU_SANDBOX env var.
			if test.spec.EnvironmentVariables["BUREAU_SANDBOX"] != test.wantBureauSandbox {
				t.Errorf("BUREAU_SANDBOX = %q, want %q",
					test.spec.EnvironmentVariables["BUREAU_SANDBOX"], test.wantBureauSandbox)
			}

			// Verify environment path.
			if test.spec.EnvironmentPath != test.wantEnvironmentPath {
				t.Errorf("EnvironmentPath = %q, want %q",
					test.spec.EnvironmentPath, test.wantEnvironmentPath)
			}

			// Verify WORKSPACE_PATH injection when PROJECT is in the payload.
			if project, ok := test.spec.Payload["PROJECT"].(string); ok && project != "" {
				wantPath := test.workspaceRoot + "/" + project
				gotPath, _ := test.spec.Payload["WORKSPACE_PATH"].(string)
				if gotPath != wantPath {
					t.Errorf("WORKSPACE_PATH = %q, want %q", gotPath, wantPath)
				}
			}
		})
	}
}

// TestReconcileNoConfig verifies that reconcile handles the M_NOT_FOUND
// case gracefully — when no MachineConfig state event exists in the config
// room, the daemon should succeed (treating it as "nothing to do") rather
// than failing.
func TestReconcileNoConfig(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken(daemon.machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID("!config:test")
	daemon.machineRoomID = mustRoomID("!machine:test")
	daemon.serviceRoomID = mustRoomID("!service:test")
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

func TestTailLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{
			name:  "fewer lines than limit",
			input: "line1\nline2\nline3",
			n:     10,
			want:  "line1\nline2\nline3",
		},
		{
			name:  "exact number of lines",
			input: "line1\nline2\nline3",
			n:     3,
			want:  "line1\nline2\nline3",
		},
		{
			name:  "more lines than limit",
			input: "line1\nline2\nline3\nline4\nline5",
			n:     2,
			want:  "line4\nline5",
		},
		{
			name:  "single line no newline",
			input: "only line",
			n:     5,
			want:  "only line",
		},
		{
			name:  "empty string",
			input: "",
			n:     5,
			want:  "",
		},
		{
			name:  "trailing newline",
			input: "line1\nline2\nline3\n",
			n:     2,
			want:  "line2\nline3\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := tailLines(test.input, test.n)
			if got != test.want {
				t.Errorf("tailLines(%q, %d) = %q, want %q",
					test.input, test.n, got, test.want)
			}
		})
	}
}

func TestCountLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty", input: "", want: 0},
		{name: "single line no newline", input: "hello", want: 1},
		{name: "single line with newline", input: "hello\n", want: 1},
		{name: "two lines", input: "a\nb", want: 2},
		{name: "two lines trailing newline", input: "a\nb\n", want: 2},
		{name: "three lines", input: "a\nb\nc", want: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := countLines(test.input)
			if got != test.want {
				t.Errorf("countLines(%q) = %d, want %d",
					test.input, got, test.want)
			}
		})
	}
}

func TestValidateCommandBinary(t *testing.T) {
	t.Parallel()

	// Create a temporary "environment" with a bin directory containing
	// a test binary, simulating a Nix environment path.
	environmentDir := t.TempDir()
	binDir := filepath.Join(environmentDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	testBinary := filepath.Join(binDir, "test-agent")
	if err := os.WriteFile(testBinary, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("writing test binary: %v", err)
	}

	tests := []struct {
		name            string
		command         string
		environmentPath string
		wantError       bool
		wantSubstring   string
	}{
		{
			name:          "empty command",
			command:       "",
			wantError:     true,
			wantSubstring: "empty",
		},
		{
			name:      "shell interpreter sh",
			command:   "sh",
			wantError: false,
		},
		{
			name:      "shell interpreter /bin/bash",
			command:   "/bin/bash",
			wantError: false,
		},
		{
			name:      "shell interpreter /usr/bin/env",
			command:   "/usr/bin/env",
			wantError: false,
		},
		{
			name:      "absolute path exists",
			command:   "/bin/sh",
			wantError: false,
		},
		{
			name:          "absolute path missing",
			command:       "/nonexistent/binary",
			wantError:     true,
			wantSubstring: "not found",
		},
		{
			name:            "bare name found in environment",
			command:         "test-agent",
			environmentPath: environmentDir,
			wantError:       false,
		},
		{
			name:            "bare name not in environment and not on PATH",
			command:         "nonexistent-bureau-binary-xyz",
			environmentPath: environmentDir,
			wantError:       true,
			wantSubstring:   "not found in environment",
		},
		{
			name:          "bare name not on PATH and no environment",
			command:       "nonexistent-bureau-binary-xyz",
			wantError:     true,
			wantSubstring: "not found on PATH",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateCommandBinary(test.command, test.environmentPath)
			if test.wantError && err == nil {
				t.Errorf("validateCommandBinary(%q, %q) = nil, want error containing %q",
					test.command, test.environmentPath, test.wantSubstring)
			}
			if !test.wantError && err != nil {
				t.Errorf("validateCommandBinary(%q, %q) = %v, want nil",
					test.command, test.environmentPath, err)
			}
			if test.wantError && err != nil && test.wantSubstring != "" {
				if !strings.Contains(err.Error(), test.wantSubstring) {
					t.Errorf("error %q does not contain %q",
						err.Error(), test.wantSubstring)
				}
			}
		})
	}
}

func TestEnsureCommandBinaryMounted(t *testing.T) {
	t.Parallel()

	// Create a temporary binary to use for absolute path tests.
	tempDir := t.TempDir()
	binaryPath := filepath.Join(tempDir, "bureau-test-binary")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("writing test binary: %v", err)
	}

	t.Run("shell interpreter unchanged", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{Command: []string{"bash", "-c", "echo hi"}}
		ensureCommandBinaryMounted(spec)
		if spec.Command[0] != "bash" {
			t.Errorf("command = %q, want bash", spec.Command[0])
		}
		if len(spec.Filesystem) != 0 {
			t.Errorf("expected no filesystem mounts for shell interpreter, got %d", len(spec.Filesystem))
		}
	})

	t.Run("nix environment skipped", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{
			Command:         []string{"bureau-test-binary"},
			EnvironmentPath: "/nix/store/abc123-env",
		}
		ensureCommandBinaryMounted(spec)
		// Command should remain bare — Nix handles resolution.
		if spec.Command[0] != "bureau-test-binary" {
			t.Errorf("command = %q, want bureau-test-binary", spec.Command[0])
		}
		if len(spec.Filesystem) != 0 {
			t.Errorf("expected no filesystem mounts for nix environment, got %d", len(spec.Filesystem))
		}
	})

	t.Run("absolute path adds mount", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{Command: []string{binaryPath}}
		ensureCommandBinaryMounted(spec)
		if spec.Command[0] != binaryPath {
			t.Errorf("command = %q, want %q", spec.Command[0], binaryPath)
		}
		found := false
		for _, mount := range spec.Filesystem {
			if mount.Source == binaryPath && mount.Dest == binaryPath && mount.Mode == "ro" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected filesystem mount for %q, got %v", binaryPath, spec.Filesystem)
		}
	})

	t.Run("absolute path with existing mount skips", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{
			Command: []string{binaryPath},
			Filesystem: []schema.TemplateMount{
				{Source: binaryPath, Dest: binaryPath, Mode: schema.MountModeRO},
			},
		}
		ensureCommandBinaryMounted(spec)
		if len(spec.Filesystem) != 1 {
			t.Errorf("expected exactly 1 filesystem mount, got %d", len(spec.Filesystem))
		}
	})

	t.Run("absolute path under directory mount skips", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{
			Command: []string{binaryPath},
			Filesystem: []schema.TemplateMount{
				{Source: tempDir, Dest: tempDir, Mode: schema.MountModeRO},
			},
		}
		ensureCommandBinaryMounted(spec)
		if len(spec.Filesystem) != 1 {
			t.Errorf("expected exactly 1 filesystem mount, got %d", len(spec.Filesystem))
		}
	})

	t.Run("empty command no panic", func(t *testing.T) {
		t.Parallel()
		spec := &schema.SandboxSpec{}
		ensureCommandBinaryMounted(spec) // should not panic
	})
}

// TestReconcileCommandBinaryValidationBlocksCreate verifies that when the
// command binary validation fails, the daemon records a command_binary failure
// and does NOT send a create-sandbox IPC to the launcher.
func TestReconcileCommandBinaryValidationBlocksCreate(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.Localpart()
	fleetPrefix := daemon.fleet.Localpart() + "/"

	state := newMockMatrixState()
	state.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"missing-agent-binary"},
	})
	state.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "agent/test"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	state.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Track messages posted to Matrix for notification verification.
	var (
		messagesMu sync.Mutex
		messages   []string
	)
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/"+string(schema.MatrixEventTypeMessage)+"/") {
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	// Track IPC calls — there should be zero.
	var (
		launcherMu sync.Mutex
		ipcActions []string
	)
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		launcherMu.Lock()
		ipcActions = append(ipcActions, string(request.Action))
		launcherMu.Unlock()
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)
	t.Cleanup(daemon.stopAllHealthMonitors)

	// Override the validator to reject the binary.
	daemon.validateCommandFunc = func(command string, environmentPath string) error {
		if command == "missing-agent-binary" {
			return fmt.Errorf("%q not found on PATH", command)
		}
		return nil
	}

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	// No create-sandbox IPC should have been sent.
	launcherMu.Lock()
	defer launcherMu.Unlock()
	for _, action := range ipcActions {
		if action == "create-sandbox" {
			t.Error("create-sandbox IPC was sent despite command binary validation failure")
		}
	}

	// A command_binary failure should be recorded.
	failure := daemon.startFailures[testEntity(t, daemon.fleet, "agent/test")]
	if failure == nil {
		t.Fatal("expected a start failure for agent/test")
	}
	if failure.category != failureCategoryCommandBinary {
		t.Errorf("failure category = %q, want %q", failure.category, failureCategoryCommandBinary)
	}

	// A notification message should have been posted to the config room.
	messagesMu.Lock()
	defer messagesMu.Unlock()
	found := false
	for _, message := range messages {
		if strings.Contains(message, "missing-agent-binary") && strings.Contains(message, "not found") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a notification message mentioning 'missing-agent-binary', got: %v", messages)
	}
}
