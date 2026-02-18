// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

func TestHTTPServiceBasicProxy(t *testing.T) {
	// Create a mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back request info
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"method":   r.Method,
			"path":     r.URL.Path,
			"query":    r.URL.RawQuery,
			"auth":     r.Header.Get("Authorization"),
			"x-custom": r.Header.Get("X-Custom-Header"),
		})
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "api-key",
		},
		Credential: testCredentials(t, map[string]string{
			"api-key": "Bearer sk-test-12345",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Create a request
	req := httptest.NewRequest("POST", "/v1/chat/completions?model=gpt-4", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "custom-value")

	// Record the response
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var result map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Check the request was proxied correctly
	if result["method"] != "POST" {
		t.Errorf("expected method POST, got %v", result["method"])
	}
	if result["path"] != "/v1/chat/completions" {
		t.Errorf("expected path /v1/chat/completions, got %v", result["path"])
	}
	if result["query"] != "model=gpt-4" {
		t.Errorf("expected query model=gpt-4, got %v", result["query"])
	}
	// Check credential was injected
	if result["auth"] != "Bearer sk-test-12345" {
		t.Errorf("expected auth header to be injected, got %v", result["auth"])
	}
	// Check custom header was passed through
	if result["x-custom"] != "custom-value" {
		t.Errorf("expected custom header to be passed, got %v", result["x-custom"])
	}
}

func TestHTTPServiceStripHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"auth": r.Header.Get("Authorization"),
		})
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "real-key",
		},
		StripHeaders: []string{"Authorization"}, // Strip incoming auth
		Credential: testCredentials(t, map[string]string{
			"real-key": "Bearer real-secret",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Request with a dummy auth header
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer dummy-key-from-agent")

	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)

	// The real key should have replaced the dummy key
	if result["auth"] != "Bearer real-secret" {
		t.Errorf("expected real auth to be injected, got %v", result["auth"])
	}
}

func TestHTTPServiceSSEStreaming(t *testing.T) {
	// Create a mock SSE upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		// Send a few SSE events
		events := []string{
			`data: {"id":"1","choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"id":"2","choices":[{"delta":{"content":" world"}}]}`,
			`data: {"id":"3","choices":[{"delta":{"content":"!"}}]}`,
			`data: [DONE]`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "openai",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "api-key",
		},
		Credential: testCredentials(t, map[string]string{
			"api-key": "Bearer sk-test",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Check content type
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Errorf("expected SSE content type, got %s", contentType)
	}

	// Parse the SSE events
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) {
		t.Errorf("expected Hello in response, got: %s", body)
	}
	if !strings.Contains(body, `"content":" world"`) {
		t.Errorf("expected world in response, got: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("expected [DONE] in response, got: %s", body)
	}
}

func TestHTTPServiceMissingCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "missing-credential",
		},
		Credential: testCredentials(t, map[string]string{}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing credentials") {
		t.Errorf("expected missing credentials error, got: %s", rec.Body.String())
	}
}

func TestHTTPServicePathFilter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Only allow POST to /v1/chat/completions
	filter := &GlobFilter{
		Allowed: []string{"POST /v1/chat/*"},
		Blocked: []string{"* /v1/admin/*"},
	}

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
		Filter:   filter,
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	tests := []struct {
		method       string
		path         string
		expectStatus int
	}{
		{"POST", "/v1/chat/completions", http.StatusOK},
		{"GET", "/v1/chat/completions", http.StatusForbidden}, // Wrong method
		{"POST", "/v1/admin/users", http.StatusForbidden},     // Blocked path
		{"POST", "/v1/embeddings", http.StatusForbidden},      // Not allowed
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			service.ServeHTTP(rec, req)

			if rec.Code != tc.expectStatus {
				t.Errorf("expected status %d, got %d", tc.expectStatus, rec.Code)
			}
		})
	}
}

func TestHTTPServiceUpstreamError(t *testing.T) {
	// Create a server that immediately closes connections
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close() // Close immediately so connections fail

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: "http://" + listener.Addr().String(),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", rec.Code)
	}
}

func TestHTTPServiceRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"received": string(body),
		})
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-api",
		Upstream: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)

	if result["received"] != reqBody {
		t.Errorf("expected request body to be forwarded, got: %s", result["received"])
	}
}

// Test HTTP service through the full server stack
func TestHTTPServiceIntegration(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	// Create mock upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"auth": r.Header.Get("Authorization"),
			"path": r.URL.Path,
		})
	}))
	defer upstream.Close()

	// Create server
	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Create and register HTTP service
	httpService, err := NewHTTPService(HTTPServiceConfig{
		Name:     "openai",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "openai-key",
		},
		StripHeaders: []string{"Authorization"},
		Credential: testCredentials(t, map[string]string{
			"openai-key": "Bearer sk-real-key",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create http service: %v", err)
	}

	server.RegisterHTTPService("openai", httpService)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	// Create client that connects via Unix socket
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	// Make request through HTTP proxy endpoint
	req, _ := http.NewRequest("POST", "http://localhost/http/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer dummy-agent-key")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	// Should have injected the real key
	if result["auth"] != "Bearer sk-real-key" {
		t.Errorf("expected real auth, got: %s", result["auth"])
	}
	// Path should be correctly rewritten
	if result["path"] != "/v1/chat/completions" {
		t.Errorf("expected path /v1/chat/completions, got: %s", result["path"])
	}
}

// Test SSE streaming through the full server stack
func TestHTTPServiceSSEIntegration(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	// Create mock SSE upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher := w.(http.Flusher)

		// Simulate OpenAI streaming response
		chunks := []string{
			`data: {"id":"chatcmpl-123","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"id":"chatcmpl-123","choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"id":"chatcmpl-123","choices":[{"delta":{"content":"!"}}]}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	httpService, err := NewHTTPService(HTTPServiceConfig{
		Name:     "openai",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "key",
		},
		Credential: testCredentials(t, map[string]string{
			"key": "Bearer sk-test",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create http service: %v", err)
	}

	server.RegisterHTTPService("openai", httpService)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", "http://localhost/http/openai/v1/chat/completions",
		strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	// Verify it's SSE
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected SSE content type, got: %s", resp.Header.Get("Content-Type"))
	}

	// Read all SSE events
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}

	if len(events) != 4 {
		t.Errorf("expected 4 events, got %d: %v", len(events), events)
	}

	// Check we got the streamed content
	allEvents := strings.Join(events, "\n")
	if !strings.Contains(allEvents, "Hello") {
		t.Errorf("expected Hello in events, got: %s", allEvents)
	}
	if !strings.Contains(allEvents, "[DONE]") {
		t.Errorf("expected [DONE] in events, got: %s", allEvents)
	}
}

func TestHTTPServiceUnknownService(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Don't register any HTTP services

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	resp, err := client.Get("http://localhost/http/unknown/v1/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unknown http service") {
		t.Errorf("expected unknown service error, got: %s", body)
	}
}

// Benchmark SSE streaming throughput
func BenchmarkHTTPServiceSSE(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		// Send 100 events
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"%d\"}\n\n", i)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	service, _ := NewHTTPService(HTTPServiceConfig{
		Name:     "bench",
		Upstream: upstream.URL,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/stream", nil)
		rec := httptest.NewRecorder()
		service.ServeHTTP(rec, req)

		// Consume the response
		io.Copy(io.Discard, rec.Body)
	}
}

// TestMatrixProxyIntegration tests the complete agent → proxy → Matrix
// homeserver flow. An agent sends a Matrix client-server API request to
// /http/matrix/... and the proxy forwards it to the mock homeserver with
// the injected Bearer token, stripping any agent-provided Authorization header.
func TestMatrixProxyIntegration(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	// Mock Matrix homeserver that verifies the Authorization header and
	// echoes request details.
	homeserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer syt_real_agent_token" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"errcode": "M_UNKNOWN_TOKEN",
				"error":   fmt.Sprintf("got %q, want Bearer syt_real_agent_token", authHeader),
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{
			"event_id": "$abc123:bureau.local",
			"method":   r.Method,
			"path":     r.URL.Path,
		})
	}))
	defer homeserver.Close()

	// Create proxy server with Matrix service configured the same way
	// createMatrixService in cmd/bureau-proxy/main.go does.
	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	matrixService, err := NewHTTPService(HTTPServiceConfig{
		Name:     "matrix",
		Upstream: homeserver.URL,
		InjectHeaders: map[string]string{
			"Authorization": "MATRIX_BEARER",
		},
		StripHeaders: []string{"Authorization"},
		Credential: testCredentials(t, map[string]string{
			"MATRIX_BEARER": "Bearer syt_real_agent_token",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create matrix service: %v", err)
	}

	server.RegisterHTTPService("matrix", matrixService)

	// Grant raw Matrix API passthrough access for this test. Without this,
	// the proxy rejects all /http/matrix/ requests with 403 (agents must
	// use the structured /v1/matrix/* endpoints by default).
	server.SetGrants([]schema.Grant{{Actions: []string{"matrix/raw-api"}}})

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	t.Run("send message", func(t *testing.T) {
		body := `{"msgtype":"m.text","body":"hello from agent"}`
		req, _ := http.NewRequest("PUT",
			"http://localhost/http/matrix/_matrix/client/v3/rooms/!room:bureau.local/send/m.room.message/txn1",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// Agent tries to set its own auth — proxy must strip it and inject the real one.
		req.Header.Set("Authorization", "Bearer sk-agent-attempt-to-override")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)

		if result["event_id"] != "$abc123:bureau.local" {
			t.Errorf("event_id = %q, want $abc123:bureau.local", result["event_id"])
		}
		if result["method"] != "PUT" {
			t.Errorf("method = %q, want PUT", result["method"])
		}
		if result["path"] != "/_matrix/client/v3/rooms/!room:bureau.local/send/m.room.message/txn1" {
			t.Errorf("path = %q, want /_matrix/client/v3/rooms/!room:bureau.local/send/m.room.message/txn1", result["path"])
		}
	})

	t.Run("room messages", func(t *testing.T) {
		req, _ := http.NewRequest("GET",
			"http://localhost/http/matrix/_matrix/client/v3/rooms/!room:bureau.local/messages?dir=b&limit=10",
			nil)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)

		if result["method"] != "GET" {
			t.Errorf("method = %q, want GET", result["method"])
		}
	})

	t.Run("sync", func(t *testing.T) {
		req, _ := http.NewRequest("GET",
			"http://localhost/http/matrix/_matrix/client/v3/sync?timeout=0",
			nil)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}
	})
}

// TestMatrixAPIFilter tests the Matrix API surface restriction. The proxy
// only allows specific Matrix client-server API endpoints that agents need.
// Administrative, discovery, and account management endpoints are blocked.
func TestMatrixAPIFilter(t *testing.T) {
	// Mock homeserver that returns 200 for everything (we're testing the filter,
	// not the homeserver).
	homeserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"path":   r.URL.Path,
		})
	}))
	defer homeserver.Close()

	// The exact filter from cmd/bureau-proxy/main.go's matrixAPIFilter().
	filter := &GlobFilter{
		Allowed: []string{
			"PUT /_matrix/client/v3/rooms/*/send/*",
			"GET /_matrix/client/v3/rooms/*/messages*",
			"GET /_matrix/client/v3/rooms/*/state*",
			"PUT /_matrix/client/v3/rooms/*/state/*",
			"GET /_matrix/client/v3/rooms/*/relations/*",
			"GET /_matrix/client/v3/sync*",
			"GET /_matrix/client/v3/account/whoami",
			"GET /_matrix/client/v3/directory/room/*",
			"POST /_matrix/client/v3/join/*",
			"GET /_matrix/client/v3/joined_rooms",
		},
	}

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "matrix",
		Upstream: homeserver.URL,
		Filter:   filter,
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		// Allowed endpoints.
		{
			name:       "send message",
			method:     "PUT",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/send/m.room.message/txn1",
			wantStatus: http.StatusOK,
		},
		{
			name:       "read messages",
			method:     "GET",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/messages?dir=b&limit=10",
			wantStatus: http.StatusOK,
		},
		{
			name:       "read state",
			method:     "GET",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/state",
			wantStatus: http.StatusOK,
		},
		{
			name:       "read state event",
			method:     "GET",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/state/m.bureau.machine_config/machine/workstation",
			wantStatus: http.StatusOK,
		},
		{
			name:       "write state event",
			method:     "PUT",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/state/m.bureau.service/service/stt/whisper",
			wantStatus: http.StatusOK,
		},
		{
			name:       "read thread",
			method:     "GET",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/relations/$event123/m.thread",
			wantStatus: http.StatusOK,
		},
		{
			name:       "sync",
			method:     "GET",
			path:       "/_matrix/client/v3/sync?timeout=30000",
			wantStatus: http.StatusOK,
		},
		{
			name:       "sync without params",
			method:     "GET",
			path:       "/_matrix/client/v3/sync",
			wantStatus: http.StatusOK,
		},
		{
			name:       "whoami",
			method:     "GET",
			path:       "/_matrix/client/v3/account/whoami",
			wantStatus: http.StatusOK,
		},
		{
			name:       "join room",
			method:     "POST",
			path:       "/_matrix/client/v3/join/!room:bureau.local",
			wantStatus: http.StatusOK,
		},
		{
			name:       "joined rooms",
			method:     "GET",
			path:       "/_matrix/client/v3/joined_rooms",
			wantStatus: http.StatusOK,
		},
		{
			name:       "resolve room alias",
			method:     "GET",
			path:       "/_matrix/client/v3/directory/room/%23bureau%2Fmachines:bureau.local",
			wantStatus: http.StatusOK,
		},

		// Blocked endpoints — administrative/discovery operations.
		{
			name:       "public rooms blocked",
			method:     "GET",
			path:       "/_matrix/client/v3/publicRooms",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "user directory search blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/user_directory/search",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "create room blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/createRoom",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "invite user blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/invite",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "kick user blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/kick",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "register account blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/register",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "login blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/login",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "media upload blocked",
			method:     "POST",
			path:       "/_matrix/media/v3/upload",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "profile read blocked",
			method:     "GET",
			path:       "/_matrix/client/v3/profile/@agent:bureau.local",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "set displayname blocked",
			method:     "PUT",
			path:       "/_matrix/client/v3/profile/@agent:bureau.local/displayname",
			wantStatus: http.StatusForbidden,
		},

		// Method mismatch — right path, wrong method.
		{
			name:       "POST to sync blocked",
			method:     "POST",
			path:       "/_matrix/client/v3/sync",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "GET to send blocked",
			method:     "GET",
			path:       "/_matrix/client/v3/rooms/!room:bureau.local/send/m.room.message/txn1",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()
			service.ServeHTTP(recorder, req)

			if recorder.Code != test.wantStatus {
				t.Errorf("%s %s: got status %d, want %d (body: %s)",
					test.method, test.path, recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}

// Test that large SSE payloads stream correctly
func TestHTTPServiceLargeSSEPayload(t *testing.T) {
	// Create a large payload (simulating base64-encoded audio or images)
	largeData := bytes.Repeat([]byte("x"), 100*1024) // 100KB

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		// Send large payload in chunks
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "data: %s\n\n", largeData)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test",
		Upstream: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	req := httptest.NewRequest("POST", "/stream", nil)
	rec := httptest.NewRecorder()
	service.ServeHTTP(rec, req)

	// Should have received all the data
	expectedSize := 10 * (len("data: ") + len(largeData) + len("\n\n"))
	if rec.Body.Len() < expectedSize-1000 { // Allow some slack
		t.Errorf("expected at least %d bytes, got %d", expectedSize, rec.Body.Len())
	}
}
