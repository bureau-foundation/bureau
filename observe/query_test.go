// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// mockDaemonServer creates a unix socket that accepts one connection and
// responds with the given QueryLayoutResponse. Returns the socket path.
func mockDaemonServer(t *testing.T, response QueryLayoutResponse) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()

		// Read the request line (we don't validate it in this mock).
		buffer := make([]byte, 4096)
		bytesRead, readErr := connection.Read(buffer)
		if readErr != nil {
			return
		}

		// Verify it's a valid query_layout request.
		var request QueryLayoutRequest
		if unmarshalErr := json.Unmarshal(buffer[:bytesRead], &request); unmarshalErr != nil {
			return
		}

		// Send the response.
		json.NewEncoder(connection).Encode(response)
	}()

	return socketPath
}

func TestQueryLayoutSuccess(t *testing.T) {
	t.Parallel()
	expectedLayout := &Layout{
		Windows: []Window{
			{
				Name: "agents",
				Panes: []Pane{
					{Observe: "iree/amdgpu/pm"},
					{Observe: "iree/amdgpu/compiler", Split: "horizontal"},
				},
			},
		},
	}

	socketPath := mockDaemonServer(t, QueryLayoutResponse{
		OK:     true,
		Layout: expectedLayout,
	})

	layout, err := QueryLayout(socketPath, "#iree/amdgpu/general:bureau.local")
	if err != nil {
		t.Fatalf("QueryLayout failed: %v", err)
	}

	if len(layout.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(layout.Windows))
	}
	if layout.Windows[0].Name != "agents" {
		t.Errorf("window name = %q, want %q", layout.Windows[0].Name, "agents")
	}
	if len(layout.Windows[0].Panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(layout.Windows[0].Panes))
	}
	if layout.Windows[0].Panes[0].Observe != "iree/amdgpu/pm" {
		t.Errorf("pane 0 observe = %q, want %q",
			layout.Windows[0].Panes[0].Observe, "iree/amdgpu/pm")
	}
	if layout.Windows[0].Panes[1].Observe != "iree/amdgpu/compiler" {
		t.Errorf("pane 1 observe = %q, want %q",
			layout.Windows[0].Panes[1].Observe, "iree/amdgpu/compiler")
	}
}

func TestQueryLayoutDaemonError(t *testing.T) {
	t.Parallel()
	socketPath := mockDaemonServer(t, QueryLayoutResponse{
		OK:    false,
		Error: "channel not found",
	})

	_, err := QueryLayout(socketPath, "#nonexistent:bureau.local")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "channel not found") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "channel not found")
	}
}

func TestQueryLayoutEmptyLayout(t *testing.T) {
	t.Parallel()
	socketPath := mockDaemonServer(t, QueryLayoutResponse{
		OK:     true,
		Layout: nil,
	})

	_, err := QueryLayout(socketPath, "#empty:bureau.local")
	if err == nil {
		t.Fatal("expected error for nil layout, got nil")
	}
	if !strings.Contains(err.Error(), "empty layout") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "empty layout")
	}
}

func TestQueryLayoutConnectionRefused(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	_, err := QueryLayout(socketPath, "#test:bureau.local")
	if err == nil {
		t.Fatal("expected error for nonexistent socket, got nil")
	}
}

func TestQueryLayoutSocketClosed(t *testing.T) {
	t.Parallel()
	// Create a socket that accepts but immediately closes the connection.
	socketPath := filepath.Join(t.TempDir(), "observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		connection.Close()
	}()

	_, err = QueryLayout(socketPath, "#test:bureau.local")
	if err == nil {
		t.Fatal("expected error for closed connection, got nil")
	}
}

func TestQueryLayoutRequestFormat(t *testing.T) {
	t.Parallel()
	// Verify the wire format of the query request by capturing it at
	// the socket level.
	socketPath := filepath.Join(t.TempDir(), "observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	requestChannel := make(chan QueryLayoutRequest, 1)

	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()

		var request QueryLayoutRequest
		json.NewDecoder(connection).Decode(&request)
		requestChannel <- request

		// Send a valid response so the client doesn't hang.
		json.NewEncoder(connection).Encode(QueryLayoutResponse{
			OK:     true,
			Layout: &Layout{Windows: []Window{{Name: "test", Panes: []Pane{{Command: "echo"}}}}},
		})
	}()

	_, err = QueryLayout(socketPath, "#iree/amdgpu/general:bureau.local")
	if err != nil {
		t.Fatalf("QueryLayout failed: %v", err)
	}

	request := <-requestChannel
	if request.Action != "query_layout" {
		t.Errorf("action = %q, want %q", request.Action, "query_layout")
	}
	if request.Channel != "#iree/amdgpu/general:bureau.local" {
		t.Errorf("channel = %q, want %q",
			request.Channel, "#iree/amdgpu/general:bureau.local")
	}
}
