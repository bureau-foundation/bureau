// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// sendRequest connects to a Unix socket, sends a JSON request, and
// returns the parsed response.
func sendRequest(t *testing.T, socketPath string, request any) map[string]any {
	t.Helper()

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to socket: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	// Signal that we're done writing (half-close). The server reads
	// one line, so this isn't strictly necessary, but it's good hygiene.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("reading response: %v", scanner.Err())
	}

	var response map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatalf("parsing response: %v (raw: %s)", err, scanner.Bytes())
	}
	return response
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "test.sock")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

func TestSocketServerStatus(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	server.Handle("status", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return map[string]any{
			"uptime_seconds": 42,
			"rooms":          3,
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = server.Serve(ctx)
	}()

	// Wait for the socket to appear.
	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "status"})

	if response["ok"] != true {
		t.Errorf("expected ok=true, got %v", response["ok"])
	}
	if response["uptime_seconds"] != float64(42) {
		t.Errorf("expected uptime_seconds=42, got %v", response["uptime_seconds"])
	}
	if response["rooms"] != float64(3) {
		t.Errorf("expected rooms=3, got %v", response["rooms"])
	}

	cancel()
	wg.Wait()
	if serveErr != nil {
		t.Errorf("Serve returned error: %v", serveErr)
	}
}

func TestSocketServerUnknownAction(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	server.Handle("status", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "nonexistent"})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if errorMessage == "" {
		t.Error("expected error message for unknown action")
	}

	cancel()
	wg.Wait()
}

func TestSocketServerMissingAction(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"foo": "bar"})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestSocketServerInvalidJSON(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("not json at all\n"))

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("expected error response")
	}

	var response map[string]any
	json.Unmarshal(scanner.Bytes(), &response)
	if response["ok"] != false {
		t.Errorf("expected ok=false for invalid JSON, got %v", response["ok"])
	}
}

func TestSocketServerHandlerError(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	server.Handle("fail", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return nil, fmt.Errorf("something broke")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "fail"})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	if response["error"] != "something broke" {
		t.Errorf("expected error='something broke', got %v", response["error"])
	}

	cancel()
	wg.Wait()
}

func TestSocketServerNilResult(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	server.Handle("noop", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "noop"})

	if response["ok"] != true {
		t.Errorf("expected ok=true, got %v", response["ok"])
	}
	// Should only have "ok", nothing else.
	if len(response) != 1 {
		t.Errorf("expected 1 field in response, got %d: %v", len(response), response)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerConcurrentRequests(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	server.Handle("echo", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var request struct {
			Value int `json:"value"`
		}
		json.Unmarshal(raw, &request)
		return map[string]any{"value": request.Value}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() {
		defer serveWg.Done()
		server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	const concurrency = 20
	var clientWg sync.WaitGroup
	for i := range concurrency {
		clientWg.Add(1)
		go func() {
			defer clientWg.Done()
			response := sendRequest(t, socketPath, map[string]any{
				"action": "echo",
				"value":  i,
			})
			if response["ok"] != true {
				t.Errorf("request %d: expected ok=true", i)
			}
			if int(response["value"].(float64)) != i {
				t.Errorf("request %d: expected value=%d, got %v", i, i, response["value"])
			}
		}()
	}

	clientWg.Wait()
	cancel()
	serveWg.Wait()
}

func TestSocketServerGracefulShutdown(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger())

	// Handler that takes a moment to complete.
	handlerStarted := make(chan struct{})
	server.Handle("slow", func(ctx context.Context, raw json.RawMessage) (any, error) {
		close(handlerStarted)
		time.Sleep(100 * time.Millisecond)
		return map[string]any{"completed": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx)
	}()

	waitForSocket(t, socketPath)

	// Start a slow request.
	responseChan := make(chan map[string]any, 1)
	go func() {
		responseChan <- sendRequest(t, socketPath, map[string]string{"action": "slow"})
	}()

	// Wait for the handler to start, then cancel.
	<-handlerStarted
	cancel()

	// The slow request should still complete.
	response := <-responseChan
	if response["ok"] != true {
		t.Errorf("expected ok=true for in-flight request, got %v", response["ok"])
	}
	if response["completed"] != true {
		t.Errorf("expected completed=true, got %v", response["completed"])
	}

	// Serve should return after the in-flight request completes.
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}

	// Socket file should be cleaned up.
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("socket file not cleaned up after Serve returned")
	}
}

func TestSocketServerDuplicateHandlerPanics(t *testing.T) {
	server := NewSocketServer("/tmp/test.sock", testLogger())
	server.Handle("foo", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return nil, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate handler registration")
		}
	}()

	server.Handle("foo", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return nil, nil
	})
}

// waitForSocket polls until the socket file exists. Fails the test
// after a timeout.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear within timeout", path)
}
