// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"test-credential": "secret-value-12345",
			},
		},
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"test-cred": `{"type":"service_account","project_id":"test"}`,
			},
		},
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
		Credential: &MapCredentialSource{
			Credentials: map[string]string{
				"test-cred": "secret-content",
			},
		},
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
