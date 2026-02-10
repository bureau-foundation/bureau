// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
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
