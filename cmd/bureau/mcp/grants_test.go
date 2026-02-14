// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TestFetchGrants verifies that fetchGrants connects to a Unix socket,
// calls GET /v1/grants, and correctly decodes the response. This
// exercises the real Unix domain socket transport and JSON encoding
// without needing the full proxy server.
func TestFetchGrants(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "proxy.sock")

	expectedGrants := []schema.Grant{
		{Actions: []string{"command/pipeline/list"}},
		{Actions: []string{"command/template/list"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/grants", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expectedGrants)
	})

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)

	grants, err := fetchGrants()
	if err != nil {
		t.Fatalf("fetchGrants: %v", err)
	}

	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}
	if grants[0].Actions[0] != "command/pipeline/list" {
		t.Errorf("grants[0].Actions[0] = %q, want %q", grants[0].Actions[0], "command/pipeline/list")
	}
	if grants[1].Actions[0] != "command/template/list" {
		t.Errorf("grants[1].Actions[0] = %q, want %q", grants[1].Actions[0], "command/template/list")
	}
}

// TestFetchGrants_EmptyGrants verifies that an empty grant list from
// the proxy results in an empty (not nil) slice.
func TestFetchGrants_EmptyGrants(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "proxy.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/grants", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]schema.Grant{})
	})

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)

	grants, err := fetchGrants()
	if err != nil {
		t.Fatalf("fetchGrants: %v", err)
	}

	if grants == nil {
		t.Fatal("grants should be non-nil empty slice, got nil")
	}
	if len(grants) != 0 {
		t.Errorf("expected 0 grants, got %d", len(grants))
	}
}

// TestFetchGrants_ProxyError verifies that fetchGrants returns an error
// when the proxy returns a non-200 status.
func TestFetchGrants_ProxyError(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "proxy.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/grants", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)

	_, err = fetchGrants()
	if err == nil {
		t.Fatal("expected error from fetchGrants when proxy returns 500")
	}
}

// TestFetchGrants_NoSocket verifies that fetchGrants returns an error
// when the proxy socket does not exist.
func TestFetchGrants_NoSocket(t *testing.T) {
	t.Setenv("BUREAU_PROXY_SOCKET", "/nonexistent/proxy.sock")

	_, err := fetchGrants()
	if err == nil {
		t.Fatal("expected error from fetchGrants when socket does not exist")
	}
}
