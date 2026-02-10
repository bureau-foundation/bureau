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
)

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
