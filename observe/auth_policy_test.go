// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
)

func TestQueryGrants(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	mockResponse := GrantsResponse{
		OK:        true,
		Principal: "iree/amdgpu/pm",
		Grants: []schema.Grant{
			{Actions: []string{observation.ActionAll}, Targets: []string{"iree/**"}},
			{Actions: []string{"command/pipeline/list"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{schema.ActionInterruptAll}},
		},
	}

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			var request GrantsRequest
			if decodeError := json.NewDecoder(connection).Decode(&request); decodeError != nil {
				connection.Close()
				continue
			}
			if request.Action != "query_grants" {
				json.NewEncoder(connection).Encode(GrantsResponse{Error: "unexpected action"})
				connection.Close()
				continue
			}

			json.NewEncoder(connection).Encode(mockResponse)
			connection.Close()
		}
	}()

	response, err := QueryGrants(socketPath, GrantsRequest{
		Principal: "iree/amdgpu/pm",
		Observer:  "@test:bureau.local",
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("QueryGrants() error: %v", err)
	}

	if response.Principal != "iree/amdgpu/pm" {
		t.Errorf("Principal = %q, want iree/amdgpu/pm", response.Principal)
	}
	if len(response.Grants) != 2 {
		t.Errorf("Grants = %d, want 2", len(response.Grants))
	}
	if len(response.Denials) != 1 {
		t.Errorf("Denials = %d, want 1", len(response.Denials))
	}
}

func TestQueryGrantsDialError(t *testing.T) {
	_, err := QueryGrants("/nonexistent/observe.sock", GrantsRequest{})
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}

func TestQueryAllowances(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	mockResponse := AllowancesResponse{
		OK:        true,
		Principal: "iree/amdgpu/compiler",
		Allowances: []schema.Allowance{
			{Actions: []string{observation.ActionAll}, Actors: []string{"iree/**"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{observation.ActionReadWrite}, Actors: []string{"untrusted/**"}},
		},
	}

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			var request AllowancesRequest
			if decodeError := json.NewDecoder(connection).Decode(&request); decodeError != nil {
				connection.Close()
				continue
			}
			if request.Action != "query_allowances" {
				json.NewEncoder(connection).Encode(AllowancesResponse{Error: "unexpected action"})
				connection.Close()
				continue
			}

			json.NewEncoder(connection).Encode(mockResponse)
			connection.Close()
		}
	}()

	response, err := QueryAllowances(socketPath, AllowancesRequest{
		Principal: "iree/amdgpu/compiler",
		Observer:  "@test:bureau.local",
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("QueryAllowances() error: %v", err)
	}

	if response.Principal != "iree/amdgpu/compiler" {
		t.Errorf("Principal = %q, want iree/amdgpu/compiler", response.Principal)
	}
	if len(response.Allowances) != 1 {
		t.Errorf("Allowances = %d, want 1", len(response.Allowances))
	}
	if len(response.AllowanceDenials) != 1 {
		t.Errorf("AllowanceDenials = %d, want 1", len(response.AllowanceDenials))
	}
}

func TestQueryAllowancesDialError(t *testing.T) {
	_, err := QueryAllowances("/nonexistent/observe.sock", AllowancesRequest{})
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}
