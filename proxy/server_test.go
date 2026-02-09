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
