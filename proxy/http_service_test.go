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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"api-key": "Bearer sk-test-12345",
			},
		},
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"real-key": "Bearer real-secret",
			},
		},
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
			time.Sleep(10 * time.Millisecond) // Simulate streaming delay
		}
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "openai",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "api-key",
		},
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"api-key": "Bearer sk-test",
			},
		},
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{}, // Empty - credential not provided
		},
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"openai-key": "Bearer sk-real-key",
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create http service: %v", err)
	}

	server.RegisterHTTPService("openai", httpService)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

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
			time.Sleep(5 * time.Millisecond)
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"key": "Bearer sk-test",
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create http service: %v", err)
	}

	server.RegisterHTTPService("openai", httpService)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

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

	time.Sleep(10 * time.Millisecond)

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
