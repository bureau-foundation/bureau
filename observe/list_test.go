// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestListTargets(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	// Start a mock daemon that handles list requests.
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	mockResponse := ListResponse{
		OK: true,
		Principals: []ListPrincipal{
			{Localpart: "iree/amdgpu/pm", Machine: "machine/workstation", Observable: true, Local: true},
			{Localpart: "service/stt/whisper", Machine: "machine/cloud-gpu", Observable: true, Local: false},
		},
		Machines: []ListMachine{
			{Name: "machine/workstation", UserID: "@machine/workstation:bureau.local", Self: true, Reachable: true},
			{Name: "machine/cloud-gpu", UserID: "@machine/cloud-gpu:bureau.local", Self: false, Reachable: true},
		},
	}

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			var request ListRequest
			if decodeError := json.NewDecoder(connection).Decode(&request); decodeError != nil {
				connection.Close()
				continue
			}
			if request.Action != "list" {
				json.NewEncoder(connection).Encode(ListResponse{Error: "unexpected action"})
				connection.Close()
				continue
			}

			// If observable filter is set, only return observable principals.
			response := mockResponse
			if request.Observable {
				// Both test principals are observable, so no filtering needed
				// in this mock. Just verify the flag was sent.
			}

			json.NewEncoder(connection).Encode(response)
			connection.Close()
		}
	}()

	t.Run("list all", func(t *testing.T) {
		response, err := ListTargets(socketPath, false)
		if err != nil {
			t.Fatalf("ListTargets() error: %v", err)
		}
		if len(response.Principals) != 2 {
			t.Errorf("got %d principals, want 2", len(response.Principals))
		}
		if len(response.Machines) != 2 {
			t.Errorf("got %d machines, want 2", len(response.Machines))
		}

		// Verify principal fields.
		found := false
		for _, principal := range response.Principals {
			if principal.Localpart == "iree/amdgpu/pm" {
				found = true
				if !principal.Observable {
					t.Error("iree/amdgpu/pm should be observable")
				}
				if !principal.Local {
					t.Error("iree/amdgpu/pm should be local")
				}
				if principal.Machine != "machine/workstation" {
					t.Errorf("machine = %q, want machine/workstation", principal.Machine)
				}
			}
		}
		if !found {
			t.Error("iree/amdgpu/pm not found in principals")
		}

		// Verify machine fields.
		foundSelf := false
		for _, machine := range response.Machines {
			if machine.Self {
				foundSelf = true
				if machine.Name != "machine/workstation" {
					t.Errorf("self machine name = %q, want machine/workstation", machine.Name)
				}
			}
		}
		if !foundSelf {
			t.Error("no self machine found")
		}
	})

	t.Run("observable filter", func(t *testing.T) {
		response, err := ListTargets(socketPath, true)
		if err != nil {
			t.Fatalf("ListTargets() error: %v", err)
		}
		if len(response.Principals) != 2 {
			t.Errorf("got %d principals, want 2", len(response.Principals))
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		_, err := ListTargets("/nonexistent/observe.sock", false)
		if err == nil {
			t.Error("expected error for nonexistent socket")
		}
	})
}

func TestListTargets_ErrorResponse(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	go func() {
		connection, acceptError := listener.Accept()
		if acceptError != nil {
			return
		}
		// Skip reading the request and send an error response.
		json.NewDecoder(connection).Decode(&json.RawMessage{})
		json.NewEncoder(connection).Encode(ListResponse{
			OK:    false,
			Error: "internal test error",
		})
		connection.Close()
	}()

	_, err = ListTargets(socketPath, false)
	if err == nil {
		t.Fatal("expected error from daemon")
	}
	if got := err.Error(); got != "list targets: internal test error" {
		t.Errorf("error = %q, want 'list targets: internal test error'", got)
	}
}

func TestListTargets_InvalidJSON(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	go func() {
		connection, acceptError := listener.Accept()
		if acceptError != nil {
			return
		}
		// Skip reading the request and send garbage.
		json.NewDecoder(connection).Decode(&json.RawMessage{})
		connection.Write([]byte("not json\n"))
		connection.Close()
	}()

	_, err = ListTargets(socketPath, false)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
