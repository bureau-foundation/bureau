// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// mockService is a test service that returns predictable output.
type mockService struct {
	name       string
	stdout     string
	stderr     string
	exitCode   int
	sleepChunk time.Duration // Sleep between chunks for streaming test
}

func (s *mockService) Name() string {
	return s.name
}

func (s *mockService) Execute(ctx context.Context, args []string, input string) (*ExecutionResult, error) {
	return &ExecutionResult{
		Stdout:   s.stdout,
		Stderr:   s.stderr,
		ExitCode: s.exitCode,
	}, nil
}

func (s *mockService) ExecuteStream(ctx context.Context, args []string, input string, stdout, stderr io.Writer) (int, error) {
	// Write stdout in chunks
	if s.stdout != "" {
		lines := strings.Split(s.stdout, "\n")
		for i, line := range lines {
			if i > 0 {
				stdout.Write([]byte("\n"))
			}
			stdout.Write([]byte(line))
			if s.sleepChunk > 0 {
				time.Sleep(s.sleepChunk)
			}
		}
	}

	// Write stderr in chunks
	if s.stderr != "" {
		lines := strings.Split(s.stderr, "\n")
		for i, line := range lines {
			if i > 0 {
				stderr.Write([]byte("\n"))
			}
			stderr.Write([]byte(line))
			if s.sleepChunk > 0 {
				time.Sleep(s.sleepChunk)
			}
		}
	}

	return s.exitCode, nil
}

// Verify mockService implements StreamingService
var _ StreamingService = (*mockService)(nil)

// blockingService is a test service that doesn't implement StreamingService
type blockingService struct {
	name   string
	result *ExecutionResult
}

func (s *blockingService) Name() string {
	return s.name
}

func (s *blockingService) Execute(ctx context.Context, args []string, input string) (*ExecutionResult, error) {
	return s.result, nil
}

// Verify blockingService implements only Service (not StreamingService)
var _ Service = (*blockingService)(nil)

func TestServerIntegration(t *testing.T) {
	// Create temp socket
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	// Create server
	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Register test service
	server.RegisterService("test", &mockService{
		name:     "test",
		stdout:   "hello stdout",
		stderr:   "hello stderr",
		exitCode: 0,
	})

	// Start server
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	// Give server time to start
	time.Sleep(10 * time.Millisecond)

	// Create client
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	t.Run("buffered request", func(t *testing.T) {
		req := Request{
			Service: "test",
			Args:    []string{"arg1", "arg2"},
			Stream:  false,
		}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		var result Response
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if result.Stdout != "hello stdout" {
			t.Errorf("expected stdout 'hello stdout', got %q", result.Stdout)
		}
		if result.Stderr != "hello stderr" {
			t.Errorf("expected stderr 'hello stderr', got %q", result.Stderr)
		}
		if result.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", result.ExitCode)
		}
	})

	t.Run("streaming request", func(t *testing.T) {
		req := Request{
			Service: "test",
			Args:    []string{"arg1"},
			Stream:  true,
		}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		contentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "application/x-ndjson") {
			t.Errorf("expected content-type application/x-ndjson, got %q", contentType)
		}

		// Read all chunks
		body, _ := io.ReadAll(resp.Body)
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")

		var stdoutData, stderrData string
		var exitCode int

		for _, line := range lines {
			if line == "" {
				continue
			}
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				t.Errorf("failed to parse chunk: %v", err)
				continue
			}
			switch chunk.Type {
			case "stdout":
				stdoutData += chunk.Data
			case "stderr":
				stderrData += chunk.Data
			case "exit":
				exitCode = chunk.Code
			}
		}

		if stdoutData != "hello stdout" {
			t.Errorf("expected stdout 'hello stdout', got %q", stdoutData)
		}
		if stderrData != "hello stderr" {
			t.Errorf("expected stderr 'hello stderr', got %q", stderrData)
		}
		if exitCode != 0 {
			t.Errorf("expected exit code 0, got %d", exitCode)
		}
	})

	t.Run("unknown service", func(t *testing.T) {
		req := Request{
			Service: "nonexistent",
			Args:    []string{},
		}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", resp.StatusCode)
		}
	})

	t.Run("health check", func(t *testing.T) {
		resp, err := client.Get("http://localhost/health")
		if err != nil {
			t.Fatalf("health check failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		var result map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if result["status"] != "ok" {
			t.Errorf("expected status 'ok', got %q", result["status"])
		}
	})
}

func TestServerStreamingFallback(t *testing.T) {
	// Test that streaming falls back to buffered when service doesn't support it

	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Register a service that doesn't support streaming
	server.RegisterService("blocking", &blockingService{
		name: "blocking",
		result: &ExecutionResult{
			Stdout:   "buffered output",
			Stderr:   "",
			ExitCode: 0,
		},
	})

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

	// Request streaming, but service doesn't support it
	req := Request{
		Service: "blocking",
		Args:    []string{},
		Stream:  true,
	}
	reqData, _ := json.Marshal(req)

	resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should fall back to buffered mode (application/json)
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("expected content-type application/json (fallback), got %q", contentType)
	}

	var result Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.Stdout != "buffered output" {
		t.Errorf("expected stdout 'buffered output', got %q", result.Stdout)
	}
}

func TestServerWithCLIService(t *testing.T) {
	// Test with a real CLI service (echo command)

	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Create a CLI service using echo
	echoService, err := NewCLIService(CLIServiceConfig{
		Name:   "echo",
		Binary: "/bin/echo",
	})
	if err != nil {
		t.Fatalf("failed to create echo service: %v", err)
	}

	server.RegisterService("echo", echoService)

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

	t.Run("buffered echo", func(t *testing.T) {
		req := Request{
			Service: "echo",
			Args:    []string{"hello", "world"},
			Stream:  false,
		}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var result Response
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		expected := "hello world\n"
		if result.Stdout != expected {
			t.Errorf("expected stdout %q, got %q", expected, result.Stdout)
		}
		if result.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", result.ExitCode)
		}
	})

	t.Run("streaming echo", func(t *testing.T) {
		req := Request{
			Service: "echo",
			Args:    []string{"streaming", "test"},
			Stream:  true,
		}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")

		var stdoutData string
		for _, line := range lines {
			if line == "" {
				continue
			}
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}
			if chunk.Type == "stdout" {
				stdoutData += chunk.Data
			}
		}

		expected := "streaming test\n"
		if stdoutData != expected {
			t.Errorf("expected stdout %q, got %q", expected, stdoutData)
		}
	})
}

func TestServerWithFilter(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Create a CLI service with filter
	echoService, err := NewCLIService(CLIServiceConfig{
		Name:   "filtered-echo",
		Binary: "/bin/echo",
		Filter: &GlobFilter{
			Allowed: []string{"hello *"},
			Blocked: []string{"* dangerous *"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	server.RegisterService("filtered-echo", echoService)

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

	t.Run("allowed command", func(t *testing.T) {
		req := Request{Service: "filtered-echo", Args: []string{"hello", "world"}}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("blocked command", func(t *testing.T) {
		req := Request{Service: "filtered-echo", Args: []string{"hello", "dangerous", "world"}}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		// Blocked commands should return 403 Forbidden
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected status 403 (blocked), got %d", resp.StatusCode)
		}

		var result Response
		json.NewDecoder(resp.Body).Decode(&result)
		if !strings.Contains(result.Error, "blocked") {
			t.Errorf("expected error to contain 'blocked', got %q", result.Error)
		}
	})

	t.Run("not allowed command", func(t *testing.T) {
		req := Request{Service: "filtered-echo", Args: []string{"goodbye", "world"}}
		reqData, _ := json.Marshal(req)

		resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		// Commands not matching allowed patterns should also return 403
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected status 403 (not allowed), got %d", resp.StatusCode)
		}
	})
}

func TestServerWithCredentials(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Create a service that uses printenv to check injected credentials
	printenvPath := "/usr/bin/printenv"
	if _, err := os.Stat(printenvPath); os.IsNotExist(err) {
		t.Skip("printenv not available")
	}

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "printenv",
		Binary: printenvPath,
		EnvVars: map[string]EnvVarConfig{
			"TEST_SECRET": {Credential: "test-credential", Type: "value"},
		},
		Credential: testCredentials(t, map[string]string{
			"test-credential": "secret-value-12345",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	server.RegisterService("printenv", service)

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

	req := Request{
		Service: "printenv",
		Args:    []string{"TEST_SECRET"},
	}
	reqData, _ := json.Marshal(req)

	resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result Response
	json.NewDecoder(resp.Body).Decode(&result)

	expected := "secret-value-12345\n"
	if result.Stdout != expected {
		t.Errorf("expected stdout %q (credential injected), got %q", expected, result.Stdout)
	}
}

func TestFileBasedCredentials(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Use cat to read the file path from an env var
	// This tests that the credential is written to a temp file
	// and the file path is injected as the env var value
	catPath := "/bin/cat"
	if _, err := os.Stat(catPath); os.IsNotExist(err) {
		t.Skip("cat not available")
	}

	// Shell script to read the file path from env and cat its contents
	shPath := "/bin/sh"
	if _, err := os.Stat(shPath); os.IsNotExist(err) {
		t.Skip("sh not available")
	}

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "file-cred-test",
		Binary: shPath,
		EnvVars: map[string]EnvVarConfig{
			"CREDENTIAL_FILE": {Credential: "test-cred", Type: "file"},
		},
		Credential: testCredentials(t, map[string]string{
			"test-cred": `{"type":"service_account","project_id":"test"}`,
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	server.RegisterService("file-cred-test", service)

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

	// Use sh -c 'cat "$CREDENTIAL_FILE"' to read the file
	req := Request{
		Service: "file-cred-test",
		Args:    []string{"-c", `cat "$CREDENTIAL_FILE"`},
	}
	reqData, _ := json.Marshal(req)

	resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result Response
	json.NewDecoder(resp.Body).Decode(&result)

	expected := `{"type":"service_account","project_id":"test"}`
	if result.Stdout != expected {
		t.Errorf("expected stdout %q (credential from file), got %q", expected, result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d (stderr: %s)", result.ExitCode, result.Stderr)
	}
}

func TestFileCredentialCleanup(t *testing.T) {
	// Test that temp files are cleaned up after command execution
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	shPath := "/bin/sh"
	if _, err := os.Stat(shPath); os.IsNotExist(err) {
		t.Skip("sh not available")
	}

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "cleanup-test",
		Binary: shPath,
		EnvVars: map[string]EnvVarConfig{
			"CREDENTIAL_FILE": {Credential: "test-cred", Type: "file"},
		},
		Credential: testCredentials(t, map[string]string{
			"test-cred": "secret-content",
		}),
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	server.RegisterService("cleanup-test", service)

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

	// Echo the file path so we can check if it exists after
	req := Request{
		Service: "cleanup-test",
		Args:    []string{"-c", `echo "$CREDENTIAL_FILE"`},
	}
	reqData, _ := json.Marshal(req)

	resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result Response
	json.NewDecoder(resp.Body).Decode(&result)

	if result.ExitCode != 0 {
		t.Fatalf("command failed: %s", result.Stderr)
	}

	credFilePath := strings.TrimSpace(result.Stdout)
	if credFilePath == "" {
		t.Fatal("expected credential file path in output")
	}

	// The temp file should have been cleaned up
	if _, err := os.Stat(credFilePath); !os.IsNotExist(err) {
		t.Errorf("credential temp file %q should have been cleaned up, but still exists", credFilePath)
	}
}

func TestIdentityEndpoint(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

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

	t.Run("identity not configured", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/identity")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", resp.StatusCode)
		}

		var result Response
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if !strings.Contains(result.Error, "identity not configured") {
			t.Errorf("expected error about identity not configured, got %q", result.Error)
		}
	})

	t.Run("identity configured", func(t *testing.T) {
		server.SetIdentity(IdentityInfo{
			UserID:     "@iree/amdgpu/pm:bureau.local",
			ServerName: "bureau.local",
		})

		resp, err := client.Get("http://localhost/v1/identity")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		contentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "application/json") {
			t.Errorf("expected content-type application/json, got %q", contentType)
		}

		var identity IdentityInfo
		if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if identity.UserID != "@iree/amdgpu/pm:bureau.local" {
			t.Errorf("expected user_id %q, got %q", "@iree/amdgpu/pm:bureau.local", identity.UserID)
		}
		if identity.ServerName != "bureau.local" {
			t.Errorf("expected server_name %q, got %q", "bureau.local", identity.ServerName)
		}
	})

	t.Run("identity without server name", func(t *testing.T) {
		server.SetIdentity(IdentityInfo{
			UserID: "@agent:other.host",
		})

		resp, err := client.Get("http://localhost/v1/identity")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		var identity IdentityInfo
		if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if identity.UserID != "@agent:other.host" {
			t.Errorf("expected user_id %q, got %q", "@agent:other.host", identity.UserID)
		}
		if identity.ServerName != "" {
			t.Errorf("expected empty server_name, got %q", identity.ServerName)
		}
	})

	t.Run("wrong method returns 405", func(t *testing.T) {
		reqData := []byte(`{}`)
		resp, err := client.Post("http://localhost/v1/identity", "application/json", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", resp.StatusCode)
		}
	})
}

// errorService always returns an error
type errorService struct {
	name string
	err  error
}

func (s *errorService) Name() string { return s.name }
func (s *errorService) Execute(ctx context.Context, args []string, input string) (*ExecutionResult, error) {
	return nil, s.err
}

func TestServerErrorHandling(t *testing.T) {
	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	server.RegisterService("error", &errorService{
		name: "error",
		err:  fmt.Errorf("something went wrong"),
	})

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

	req := Request{Service: "error", Args: []string{}}
	reqData, _ := json.Marshal(req)

	resp, err := client.Post("http://localhost/v1/proxy", "application/json", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var result Response
	json.NewDecoder(resp.Body).Decode(&result)
	if !strings.Contains(result.Error, "something went wrong") {
		t.Errorf("expected error message, got %q", result.Error)
	}
}

func TestAdminEndpoints(t *testing.T) {
	tempDir := t.TempDir()
	agentSocket := filepath.Join(tempDir, "proxy.sock")
	adminSocket := filepath.Join(tempDir, "admin.sock")

	server, err := NewServer(ServerConfig{
		SocketPath:      agentSocket,
		AdminSocketPath: adminSocket,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

	adminClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", adminSocket)
			},
		},
	}

	agentClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	t.Run("list services empty", func(t *testing.T) {
		resp, err := adminClient.Get("http://localhost/v1/admin/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		var services []AdminServiceInfo
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 0 {
			t.Errorf("expected 0 services, got %d", len(services))
		}
	})

	t.Run("register service with upstream URL", func(t *testing.T) {
		body, _ := json.Marshal(AdminServiceRequest{
			UpstreamURL: "http://localhost:9999",
		})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/test-api", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 201, got %d: %s", resp.StatusCode, respBody)
		}

		var info AdminServiceInfo
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if info.Name != "test-api" {
			t.Errorf("expected name 'test-api', got %q", info.Name)
		}
	})

	t.Run("register service with unix socket", func(t *testing.T) {
		body, _ := json.Marshal(AdminServiceRequest{
			UpstreamUnix: "/run/bureau/principal/service/stt/whisper.sock",
		})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/stt-whisper", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 201, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("list services after registration", func(t *testing.T) {
		resp, err := adminClient.Get("http://localhost/v1/admin/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []AdminServiceInfo
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 2 {
			t.Errorf("expected 2 services, got %d", len(services))
		}
	})

	t.Run("unregister service", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "http://localhost/v1/admin/services/test-api", nil)
		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		// Verify it was removed.
		listResp, err := adminClient.Get("http://localhost/v1/admin/services")
		if err != nil {
			t.Fatalf("list request failed: %v", err)
		}
		defer listResp.Body.Close()
		var services []AdminServiceInfo
		json.NewDecoder(listResp.Body).Decode(&services)
		if len(services) != 1 {
			t.Errorf("expected 1 service after removal, got %d", len(services))
		}
	})

	t.Run("unregister nonexistent service", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "http://localhost/v1/admin/services/nonexistent", nil)
		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", resp.StatusCode)
		}
	})

	t.Run("register rejects missing upstream", func(t *testing.T) {
		body, _ := json.Marshal(AdminServiceRequest{})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/bad", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", resp.StatusCode)
		}
	})

	t.Run("register with both URL and unix socket", func(t *testing.T) {
		// Both can be provided: the URL gives the path prefix for routing
		// (e.g., /http/stt-whisper on the provider proxy) and the Unix
		// socket provides the physical transport.
		body, _ := json.Marshal(AdminServiceRequest{
			UpstreamURL:  "http://localhost/http/stt-whisper",
			UpstreamUnix: "/run/bureau/principal/service/stt/whisper.sock",
		})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/service-stt-whisper", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 201, got %d: %s", resp.StatusCode, respBody)
		}

		var info AdminServiceInfo
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		// The upstream URL should include the path prefix.
		if info.Upstream != "http://localhost/http/stt-whisper" {
			t.Errorf("expected upstream 'http://localhost/http/stt-whisper', got %q", info.Upstream)
		}

		// Clean up for the list count in subsequent tests.
		delReq, _ := http.NewRequest("DELETE", "http://localhost/v1/admin/services/service-stt-whisper", nil)
		adminClient.Do(delReq)
	})

	t.Run("admin endpoints not on agent socket", func(t *testing.T) {
		// Admin endpoints should not be accessible via the agent socket.
		resp, err := agentClient.Get("http://localhost/v1/admin/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		// The agent mux doesn't have /v1/admin/services, so Go's default
		// ServeMux returns 404.
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected admin endpoint to return 404 on agent socket, got %d", resp.StatusCode)
		}
	})

	t.Run("agent endpoints work via admin socket", func(t *testing.T) {
		resp, err := adminClient.Get("http://localhost/health")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})
}

func TestUnixSocketUpstream(t *testing.T) {
	// Test end-to-end: agent → consumer proxy → provider proxy (via Unix socket)
	//
	// This simulates the real local service routing path where both the
	// consumer and provider are Bureau proxy instances. The provider proxy
	// has the actual service backend registered; the consumer proxy
	// forwards through the provider's proxy socket with a path prefix
	// that routes to the correct service on the provider.
	tempDir := t.TempDir()

	// --- Service backend: a plain HTTP server simulating Whisper STT ---
	backendSocket := filepath.Join(tempDir, "whisper-backend.sock")
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/v1/transcribe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"text":   "hello world",
			"method": r.Method,
		})
	})
	backendListener, err := net.Listen("unix", backendSocket)
	if err != nil {
		t.Fatalf("failed to listen on backend socket: %v", err)
	}
	backendServer := &http.Server{Handler: backendMux}
	go backendServer.Serve(backendListener)
	defer backendServer.Close()

	// --- Provider proxy: the principal's proxy with the STT service registered ---
	providerSocket := filepath.Join(tempDir, "provider.sock")
	providerAdmin := filepath.Join(tempDir, "provider-admin.sock")

	provider, err := NewServer(ServerConfig{
		SocketPath:      providerSocket,
		AdminSocketPath: providerAdmin,
	})
	if err != nil {
		t.Fatalf("failed to create provider server: %v", err)
	}

	// Register the backend service on the provider proxy. By convention,
	// the service name matches the flat form of the service localpart.
	backendService, err := NewHTTPService(HTTPServiceConfig{
		Name:         "service-stt-whisper",
		UpstreamUnix: backendSocket,
	})
	if err != nil {
		t.Fatalf("failed to create backend service: %v", err)
	}
	provider.RegisterHTTPService("service-stt-whisper", backendService)

	if err := provider.Start(); err != nil {
		t.Fatalf("failed to start provider server: %v", err)
	}
	defer provider.Shutdown(context.Background())

	// --- Consumer proxy: forwards to provider proxy via Unix socket ---
	consumerSocket := filepath.Join(tempDir, "consumer.sock")
	consumerAdmin := filepath.Join(tempDir, "consumer-admin.sock")

	consumer, err := NewServer(ServerConfig{
		SocketPath:      consumerSocket,
		AdminSocketPath: consumerAdmin,
	})
	if err != nil {
		t.Fatalf("failed to create consumer server: %v", err)
	}

	if err := consumer.Start(); err != nil {
		t.Fatalf("failed to start consumer server: %v", err)
	}
	defer consumer.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

	// Register the service route on the consumer proxy via admin API.
	// The upstream URL includes the /http/service-stt-whisper path prefix
	// so requests are forwarded to the correct service on the provider.
	adminClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", consumerAdmin)
			},
		},
	}

	registerBody, _ := json.Marshal(AdminServiceRequest{
		UpstreamURL:  "http://localhost/http/service-stt-whisper",
		UpstreamUnix: providerSocket,
	})
	registerReq, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/service-stt-whisper", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerResp, err := adminClient.Do(registerReq)
	if err != nil {
		t.Fatalf("register request failed: %v", err)
	}
	registerResp.Body.Close()
	if registerResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", registerResp.StatusCode)
	}

	// --- Agent: connects to consumer proxy and calls the service ---
	// The full path: agent → consumer proxy (/http/service-stt-whisper/v1/transcribe)
	//   → consumer strips /http/service-stt-whisper/, forwards /v1/transcribe
	//     to upstream http://localhost/http/service-stt-whisper via provider socket
	//   → provider proxy receives /http/service-stt-whisper/v1/transcribe
	//   → provider strips /http/service-stt-whisper/, forwards /v1/transcribe
	//     to backend socket → backend responds
	agentClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", consumerSocket)
			},
		},
	}

	resp, err := agentClient.Post("http://localhost/http/service-stt-whisper/v1/transcribe", "application/json", nil)
	if err != nil {
		t.Fatalf("agent request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["text"] != "hello world" {
		t.Errorf("expected text 'hello world', got %q", result["text"])
	}
	if result["method"] != "POST" {
		t.Errorf("expected method 'POST', got %q", result["method"])
	}
}

func TestMatrixPolicy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(logger)

	// checkMatrixPolicy is a method on Handler — test it directly with
	// table-driven subtests covering every gated endpoint.
	tests := []struct {
		name    string
		policy  *schema.MatrixPolicy
		method  string
		path    string
		blocked bool
	}{
		// Default-deny (nil policy): all self-service operations blocked.
		{
			name:    "nil policy blocks join by alias",
			policy:  nil,
			method:  "POST",
			path:    "/_matrix/client/v3/join/%23room:bureau.local",
			blocked: true,
		},
		{
			name:    "nil policy blocks join by room ID",
			policy:  nil,
			method:  "POST",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/join",
			blocked: true,
		},
		{
			name:    "nil policy blocks invite",
			policy:  nil,
			method:  "POST",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/invite",
			blocked: true,
		},
		{
			name:    "nil policy blocks createRoom",
			policy:  nil,
			method:  "POST",
			path:    "/_matrix/client/v3/createRoom",
			blocked: true,
		},
		{
			name:    "nil policy allows GET messages",
			policy:  nil,
			method:  "GET",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/messages",
			blocked: false,
		},
		{
			name:    "nil policy allows PUT send",
			policy:  nil,
			method:  "PUT",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/send/m.room.message/txn1",
			blocked: false,
		},
		{
			name:    "nil policy allows GET sync",
			policy:  nil,
			method:  "GET",
			path:    "/_matrix/client/v3/sync",
			blocked: false,
		},

		// Zero-valued policy (all false): same as nil.
		{
			name:    "zero policy blocks join",
			policy:  &schema.MatrixPolicy{},
			method:  "POST",
			path:    "/_matrix/client/v3/join/%23room:bureau.local",
			blocked: true,
		},

		// AllowJoin = true: join and accept-invite unblocked.
		{
			name:    "AllowJoin unblocks join by alias",
			policy:  &schema.MatrixPolicy{AllowJoin: true},
			method:  "POST",
			path:    "/_matrix/client/v3/join/%23iree:bureau.local",
			blocked: false,
		},
		{
			name:    "AllowJoin unblocks rooms/*/join",
			policy:  &schema.MatrixPolicy{AllowJoin: true},
			method:  "POST",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/join",
			blocked: false,
		},
		{
			name:    "AllowJoin still blocks invite",
			policy:  &schema.MatrixPolicy{AllowJoin: true},
			method:  "POST",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/invite",
			blocked: true,
		},
		{
			name:    "AllowJoin still blocks createRoom",
			policy:  &schema.MatrixPolicy{AllowJoin: true},
			method:  "POST",
			path:    "/_matrix/client/v3/createRoom",
			blocked: true,
		},

		// AllowInvite = true: invite unblocked, join still blocked.
		{
			name:    "AllowInvite unblocks invite",
			policy:  &schema.MatrixPolicy{AllowInvite: true},
			method:  "POST",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/invite",
			blocked: false,
		},
		{
			name:    "AllowInvite still blocks join",
			policy:  &schema.MatrixPolicy{AllowInvite: true},
			method:  "POST",
			path:    "/_matrix/client/v3/join/%23room:bureau.local",
			blocked: true,
		},

		// AllowRoomCreate = true: createRoom unblocked.
		{
			name:    "AllowRoomCreate unblocks createRoom",
			policy:  &schema.MatrixPolicy{AllowRoomCreate: true},
			method:  "POST",
			path:    "/_matrix/client/v3/createRoom",
			blocked: false,
		},
		{
			name:    "AllowRoomCreate still blocks join",
			policy:  &schema.MatrixPolicy{AllowRoomCreate: true},
			method:  "POST",
			path:    "/_matrix/client/v3/join/%23room:bureau.local",
			blocked: true,
		},

		// Coordinator policy (all true): everything allowed.
		{
			name: "full policy allows everything",
			policy: &schema.MatrixPolicy{
				AllowJoin:       true,
				AllowInvite:     true,
				AllowRoomCreate: true,
			},
			method:  "POST",
			path:    "/_matrix/client/v3/createRoom",
			blocked: false,
		},

		// Non-POST methods are never blocked.
		{
			name:    "GET join endpoint is allowed (not POST)",
			policy:  nil,
			method:  "GET",
			path:    "/_matrix/client/v3/join/%23room:bureau.local",
			blocked: false,
		},

		// Non-Matrix paths are never checked.
		{
			name:    "non-matrix path ignored",
			policy:  nil,
			method:  "POST",
			path:    "/v1/proxy",
			blocked: false,
		},

		// PUT state events are always allowed (admin sets state).
		{
			name:    "PUT state event allowed even with nil policy",
			policy:  nil,
			method:  "PUT",
			path:    "/_matrix/client/v3/rooms/!abc:bureau.local/state/m.room.topic/",
			blocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler.SetMatrixPolicy(tt.policy)
			blocked, reason := handler.checkMatrixPolicy(tt.method, tt.path)
			if blocked != tt.blocked {
				t.Errorf("checkMatrixPolicy(%s, %s) blocked = %v, want %v (reason: %s)",
					tt.method, tt.path, blocked, tt.blocked, reason)
			}
			if blocked && reason == "" {
				t.Error("blocked request should have a reason")
			}
			if !blocked && reason != "" {
				t.Errorf("allowed request should not have a reason, got %q", reason)
			}
		})
	}
}

func TestServiceDirectory(t *testing.T) {
	tempDir := t.TempDir()
	agentSocket := filepath.Join(tempDir, "proxy.sock")
	adminSocket := filepath.Join(tempDir, "admin.sock")

	server, err := NewServer(ServerConfig{
		SocketPath:      agentSocket,
		AdminSocketPath: adminSocket,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Set wildcard visibility so the directory is fully visible. Tests
	// specifically for visibility filtering are in TestServiceVisibility.
	server.SetServiceVisibility([]string{"**"})

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

	adminClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", adminSocket)
			},
		},
	}

	agentClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	t.Run("empty directory", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 0 {
			t.Errorf("expected 0 services, got %d", len(services))
		}
	})

	// Push a directory via the admin endpoint.
	directory := []ServiceDirectoryEntry{
		{
			Localpart:    "service/stt/whisper",
			Principal:    "@service/stt/whisper:bureau.local",
			Machine:      "@machine/gpu-1:bureau.local",
			Protocol:     "http",
			Description:  "Speech-to-text via Whisper",
			Capabilities: []string{"streaming", "speaker-diarization"},
		},
		{
			Localpart:    "service/tts/piper",
			Principal:    "@service/tts/piper:bureau.local",
			Machine:      "@machine/gpu-1:bureau.local",
			Protocol:     "http",
			Description:  "Text-to-speech via Piper",
			Capabilities: []string{"streaming"},
		},
		{
			Localpart:   "service/embedding/e5",
			Principal:   "@service/embedding/e5:bureau.local",
			Machine:     "@machine/cpu-1:bureau.local",
			Protocol:    "grpc",
			Description: "Text embeddings via E5",
			Metadata:    map[string]any{"max_tokens": float64(512)},
		},
	}

	t.Run("push directory via admin", func(t *testing.T) {
		body, _ := json.Marshal(directory)
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/directory", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if result["status"] != "ok" {
			t.Errorf("expected status ok, got %v", result["status"])
		}
		if result["services"] != float64(3) {
			t.Errorf("expected 3 services, got %v", result["services"])
		}
	})

	t.Run("list all services", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 3 {
			t.Fatalf("expected 3 services, got %d", len(services))
		}
	})

	t.Run("filter by protocol", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services?protocol=http")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 2 {
			t.Errorf("expected 2 http services, got %d", len(services))
		}
		for _, service := range services {
			if service.Protocol != "http" {
				t.Errorf("expected protocol http, got %q", service.Protocol)
			}
		}
	})

	t.Run("filter by capability", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services?capability=speaker-diarization")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 1 {
			t.Errorf("expected 1 service with speaker-diarization, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/stt/whisper" {
			t.Errorf("expected whisper service, got %q", services[0].Localpart)
		}
	})

	t.Run("filter by protocol and capability", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services?protocol=http&capability=streaming")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 2 {
			t.Errorf("expected 2 http+streaming services, got %d", len(services))
		}
	})

	t.Run("filter returns empty for no matches", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services?protocol=websocket")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 0 {
			t.Errorf("expected 0 services, got %d", len(services))
		}
	})

	t.Run("metadata preserved", func(t *testing.T) {
		resp, err := agentClient.Get("http://localhost/v1/services?protocol=grpc")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(services) != 1 {
			t.Fatalf("expected 1 grpc service, got %d", len(services))
		}
		if services[0].Metadata == nil {
			t.Fatal("expected metadata to be preserved")
		}
		maxTokens, ok := services[0].Metadata["max_tokens"]
		if !ok {
			t.Fatal("expected max_tokens in metadata")
		}
		if maxTokens != float64(512) {
			t.Errorf("expected max_tokens=512, got %v", maxTokens)
		}
	})

	t.Run("admin directory not on agent socket", func(t *testing.T) {
		body, _ := json.Marshal(directory)
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/directory", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := agentClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		// The agent mux doesn't have /v1/admin/directory, so Go's
		// default ServeMux returns 405 (PUT method not allowed on the
		// catch-all pattern).
		if resp.StatusCode == http.StatusOK {
			t.Error("admin directory endpoint should not be accessible on agent socket")
		}
	})

	t.Run("services endpoint via admin socket", func(t *testing.T) {
		// GET /v1/services should work via admin socket too (falls
		// through to agent mux).
		resp, err := adminClient.Get("http://localhost/v1/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("push replaces entire directory", func(t *testing.T) {
		// Push a smaller directory — should replace, not append.
		newDirectory := []ServiceDirectoryEntry{
			{
				Localpart: "service/llm/claude",
				Principal: "@service/llm/claude:bureau.local",
				Machine:   "@machine/cpu-1:bureau.local",
				Protocol:  "http",
			},
		}
		body, _ := json.Marshal(newDirectory)
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/directory", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("push failed: %v", err)
		}
		resp.Body.Close()

		// Query to verify replacement.
		listResp, err := agentClient.Get("http://localhost/v1/services")
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		defer listResp.Body.Close()

		var services []ServiceDirectoryEntry
		json.NewDecoder(listResp.Body).Decode(&services)
		if len(services) != 1 {
			t.Errorf("expected 1 service after replacement, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/llm/claude" {
			t.Errorf("expected claude service, got %q", services[0].Localpart)
		}
	})

	t.Run("push invalid body", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/directory", bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", resp.StatusCode)
		}
	})

	t.Run("SetServiceDirectory programmatic", func(t *testing.T) {
		// Test the programmatic setter (used internally, not via HTTP).
		server.SetServiceDirectory([]ServiceDirectoryEntry{
			{
				Localpart: "service/test",
				Principal: "@service/test:bureau.local",
				Machine:   "@machine/dev:bureau.local",
				Protocol:  "http",
			},
		})

		resp, err := agentClient.Get("http://localhost/v1/services")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var services []ServiceDirectoryEntry
		json.NewDecoder(resp.Body).Decode(&services)
		if len(services) != 1 {
			t.Errorf("expected 1 service, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/test" {
			t.Errorf("expected test service, got %q", services[0].Localpart)
		}
	})
}

func TestServiceVisibility(t *testing.T) {
	tempDir := t.TempDir()
	agentSocket := filepath.Join(tempDir, "proxy.sock")
	adminSocket := filepath.Join(tempDir, "admin.sock")

	server, err := NewServer(ServerConfig{
		SocketPath:      agentSocket,
		AdminSocketPath: adminSocket,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(10 * time.Millisecond)

	adminClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", adminSocket)
			},
		},
	}

	agentClient := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	// Push a directory with services in different namespaces.
	directory := []ServiceDirectoryEntry{
		{
			Localpart:    "service/stt/whisper",
			Principal:    "@service/stt/whisper:bureau.local",
			Machine:      "@machine/gpu-1:bureau.local",
			Protocol:     "http",
			Capabilities: []string{"streaming"},
		},
		{
			Localpart: "service/stt/deepgram",
			Principal: "@service/stt/deepgram:bureau.local",
			Machine:   "@machine/gpu-1:bureau.local",
			Protocol:  "http",
		},
		{
			Localpart:    "service/tts/piper",
			Principal:    "@service/tts/piper:bureau.local",
			Machine:      "@machine/gpu-1:bureau.local",
			Protocol:     "http",
			Capabilities: []string{"streaming"},
		},
		{
			Localpart: "service/embedding/e5",
			Principal: "@service/embedding/e5:bureau.local",
			Machine:   "@machine/cpu-1:bureau.local",
			Protocol:  "grpc",
		},
	}

	body, _ := json.Marshal(directory)
	req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/directory", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := adminClient.Do(req)
	if err != nil {
		t.Fatalf("push directory failed: %v", err)
	}
	resp.Body.Close()

	getServices := func(t *testing.T, query string) []ServiceDirectoryEntry {
		t.Helper()
		url := "http://localhost/v1/services"
		if query != "" {
			url += "?" + query
		}
		resp, err := agentClient.Get(url)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}

		var services []ServiceDirectoryEntry
		if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		return services
	}

	t.Run("default-deny: no visibility returns empty", func(t *testing.T) {
		// No visibility patterns set — agent sees nothing.
		services := getServices(t, "")
		if len(services) != 0 {
			t.Errorf("expected 0 services with no visibility, got %d", len(services))
		}
	})

	t.Run("wildcard visibility shows all", func(t *testing.T) {
		server.SetServiceVisibility([]string{"**"})
		services := getServices(t, "")
		if len(services) != 4 {
			t.Errorf("expected 4 services with ** visibility, got %d", len(services))
		}
	})

	t.Run("namespace pattern filters to subtree", func(t *testing.T) {
		server.SetServiceVisibility([]string{"service/stt/*"})
		services := getServices(t, "")
		if len(services) != 2 {
			t.Errorf("expected 2 STT services, got %d", len(services))
		}
		for _, service := range services {
			if service.Localpart != "service/stt/whisper" && service.Localpart != "service/stt/deepgram" {
				t.Errorf("unexpected service: %q", service.Localpart)
			}
		}
	})

	t.Run("multiple patterns are unioned", func(t *testing.T) {
		server.SetServiceVisibility([]string{"service/stt/*", "service/embedding/*"})
		services := getServices(t, "")
		if len(services) != 3 {
			t.Errorf("expected 3 services (2 STT + 1 embedding), got %d", len(services))
		}
	})

	t.Run("exact localpart match", func(t *testing.T) {
		server.SetServiceVisibility([]string{"service/tts/piper"})
		services := getServices(t, "")
		if len(services) != 1 {
			t.Errorf("expected 1 service, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/tts/piper" {
			t.Errorf("expected piper, got %q", services[0].Localpart)
		}
	})

	t.Run("no matching patterns returns empty", func(t *testing.T) {
		server.SetServiceVisibility([]string{"service/llm/*"})
		services := getServices(t, "")
		if len(services) != 0 {
			t.Errorf("expected 0 services for non-matching pattern, got %d", len(services))
		}
	})

	t.Run("visibility combined with query param filtering", func(t *testing.T) {
		// Can see all services, but filter by capability.
		server.SetServiceVisibility([]string{"**"})
		services := getServices(t, "capability=streaming")
		if len(services) != 2 {
			t.Errorf("expected 2 streaming services, got %d", len(services))
		}
	})

	t.Run("visibility restricts before query param filtering", func(t *testing.T) {
		// Can only see STT services; even though TTS/piper also has streaming,
		// it should not appear because it's not visible.
		server.SetServiceVisibility([]string{"service/stt/*"})
		services := getServices(t, "capability=streaming")
		if len(services) != 1 {
			t.Errorf("expected 1 visible streaming service, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/stt/whisper" {
			t.Errorf("expected whisper, got %q", services[0].Localpart)
		}
	})

	t.Run("push visibility via admin API", func(t *testing.T) {
		// Reset to default-deny first.
		server.SetServiceVisibility(nil)

		// Push patterns via admin API.
		patterns, _ := json.Marshal([]string{"service/embedding/*"})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/visibility", bytes.NewReader(patterns))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("admin request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		if result["status"] != "ok" {
			t.Errorf("expected status ok, got %v", result["status"])
		}
		if result["patterns"] != float64(1) {
			t.Errorf("expected 1 pattern, got %v", result["patterns"])
		}

		// Verify the agent now sees only embedding services.
		services := getServices(t, "")
		if len(services) != 1 {
			t.Errorf("expected 1 service after visibility push, got %d", len(services))
		}
		if len(services) > 0 && services[0].Localpart != "service/embedding/e5" {
			t.Errorf("expected e5, got %q", services[0].Localpart)
		}
	})

	t.Run("admin visibility not on agent socket", func(t *testing.T) {
		patterns, _ := json.Marshal([]string{"**"})
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/visibility", bytes.NewReader(patterns))
		req.Header.Set("Content-Type", "application/json")

		resp, err := agentClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Error("admin visibility endpoint should not be accessible on agent socket")
		}
	})

	t.Run("push invalid visibility body", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", "http://localhost/v1/admin/visibility", bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := adminClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", resp.StatusCode)
		}
	})

	t.Run("deep wildcard matches nested paths", func(t *testing.T) {
		server.SetServiceVisibility([]string{"service/**"})
		services := getServices(t, "")
		if len(services) != 4 {
			t.Errorf("expected 4 services with service/** visibility, got %d", len(services))
		}
	})

	t.Run("empty patterns resets to default-deny", func(t *testing.T) {
		server.SetServiceVisibility([]string{"**"})
		// Verify agent can see services.
		services := getServices(t, "")
		if len(services) != 4 {
			t.Fatalf("expected 4 services with ** visibility, got %d", len(services))
		}

		// Reset to empty.
		server.SetServiceVisibility([]string{})
		services = getServices(t, "")
		if len(services) != 0 {
			t.Errorf("expected 0 services after clearing visibility, got %d", len(services))
		}
	})
}
