// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestQueryAuthorization(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	mockResponse := AuthorizationResponse{
		OK:       true,
		Decision: "allow",
		MatchedGrant: &schema.Grant{
			Actions: []string{"observe/**"},
			Targets: []string{"iree/**"},
		},
		MatchedAllowance: &schema.Allowance{
			Actions: []string{"observe/**"},
			Actors:  []string{"iree/**"},
		},
		ActorGrants: []schema.Grant{
			{Actions: []string{"observe/**"}, Targets: []string{"iree/**"}},
		},
		ActorDenials: []schema.Denial{},
		TargetAllowances: []schema.Allowance{
			{Actions: []string{"observe/**"}, Actors: []string{"iree/**"}},
		},
		TargetAllowanceDenials: []schema.AllowanceDenial{},
	}

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			var request AuthorizationRequest
			if decodeError := json.NewDecoder(connection).Decode(&request); decodeError != nil {
				connection.Close()
				continue
			}
			if request.Action != "query_authorization" {
				json.NewEncoder(connection).Encode(AuthorizationResponse{Error: "unexpected action"})
				connection.Close()
				continue
			}

			json.NewEncoder(connection).Encode(mockResponse)
			connection.Close()
		}
	}()

	response, err := QueryAuthorization(socketPath, AuthorizationRequest{
		Actor:      "iree/amdgpu/pm",
		AuthAction: "observe",
		Target:     "iree/amdgpu/compiler",
		Observer:   "@test:bureau.local",
		Token:      "test-token",
	})
	if err != nil {
		t.Fatalf("QueryAuthorization() error: %v", err)
	}

	if response.Decision != "allow" {
		t.Errorf("Decision = %q, want allow", response.Decision)
	}
	if response.MatchedGrant == nil {
		t.Error("MatchedGrant is nil")
	}
	if response.MatchedAllowance == nil {
		t.Error("MatchedAllowance is nil")
	}
	if len(response.ActorGrants) != 1 {
		t.Errorf("ActorGrants = %d, want 1", len(response.ActorGrants))
	}
	if len(response.TargetAllowances) != 1 {
		t.Errorf("TargetAllowances = %d, want 1", len(response.TargetAllowances))
	}
}

func TestQueryAuthorizationDeny(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			var request AuthorizationRequest
			json.NewDecoder(connection).Decode(&request)

			json.NewEncoder(connection).Encode(AuthorizationResponse{
				OK:           true,
				Decision:     "deny",
				Reason:       "no matching grant",
				ActorGrants:  []schema.Grant{},
				ActorDenials: []schema.Denial{},
			})
			connection.Close()
		}
	}()

	response, err := QueryAuthorization(socketPath, AuthorizationRequest{
		Actor:      "agent/alpha",
		AuthAction: "observe",
		Target:     "agent/beta",
		Observer:   "@test:bureau.local",
		Token:      "test-token",
	})
	if err != nil {
		t.Fatalf("QueryAuthorization() error: %v", err)
	}

	if response.Decision != "deny" {
		t.Errorf("Decision = %q, want deny", response.Decision)
	}
	if response.Reason != "no matching grant" {
		t.Errorf("Reason = %q, want 'no matching grant'", response.Reason)
	}
}

func TestQueryAuthorizationServerError(t *testing.T) {
	t.Parallel()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}

			json.NewDecoder(connection).Decode(&AuthorizationRequest{})
			json.NewEncoder(connection).Encode(AuthorizationResponse{
				OK:    false,
				Error: "actor is required for query_authorization",
			})
			connection.Close()
		}
	}()

	_, err = QueryAuthorization(socketPath, AuthorizationRequest{
		Observer: "@test:bureau.local",
		Token:    "test-token",
	})
	if err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestQueryAuthorizationDialError(t *testing.T) {
	_, err := QueryAuthorization("/nonexistent/observe.sock", AuthorizationRequest{})
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}
