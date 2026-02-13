// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Client construction tests ---

func TestNewServiceClientReadsTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "ticket")
	tokenBytes := []byte("fake-token-bytes-for-testing")

	if err := os.WriteFile(tokenPath, tokenBytes, 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	client, err := NewServiceClient("/tmp/test.sock", tokenPath)
	if err != nil {
		t.Fatalf("NewServiceClient: %v", err)
	}
	if len(client.tokenBytes) != len(tokenBytes) {
		t.Fatalf("token bytes length: got %d, want %d", len(client.tokenBytes), len(tokenBytes))
	}
}

func TestNewServiceClientMissingFile(t *testing.T) {
	_, err := NewServiceClient("/tmp/test.sock", "/nonexistent/path/token")
	if err == nil {
		t.Fatal("expected error for missing token file")
	}
}

func TestNewServiceClientEmptyFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "ticket")

	if err := os.WriteFile(tokenPath, []byte{}, 0600); err != nil {
		t.Fatalf("writing empty file: %v", err)
	}

	_, err := NewServiceClient("/tmp/test.sock", tokenPath)
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestNewServiceClientFromTokenNil(t *testing.T) {
	client := NewServiceClientFromToken("/tmp/test.sock", nil)
	if client.tokenBytes != nil {
		t.Fatal("expected nil token bytes")
	}
}

// --- Unauthenticated call tests ---

func TestClientCallUnauthenticated(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
		return map[string]any{"uptime_seconds": 42}, nil
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

	client := NewServiceClientFromToken(socketPath, nil)

	var result map[string]any
	if err := client.Call(ctx, "status", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result["uptime_seconds"] != uint64(42) {
		t.Errorf("uptime_seconds: got %v (%T), want 42", result["uptime_seconds"], result["uptime_seconds"])
	}

	cancel()
	wg.Wait()
}

func TestClientCallNilResult(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("ping", func(ctx context.Context, raw []byte) (any, error) {
		return map[string]any{"pong": true}, nil
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

	client := NewServiceClientFromToken(socketPath, nil)

	// Call with nil result — should succeed, just discard data.
	if err := client.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatalf("Call with nil result: %v", err)
	}

	cancel()
	wg.Wait()
}

func TestClientCallNoResponseData(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("noop", func(ctx context.Context, raw []byte) (any, error) {
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

	client := NewServiceClientFromToken(socketPath, nil)

	// Call with a result target but server returns no data — should
	// succeed without decoding.
	var result map[string]any
	if err := client.Call(ctx, "noop", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != nil {
		t.Errorf("result should be nil when server returns no data, got %v", result)
	}

	cancel()
	wg.Wait()
}

// --- Authenticated call tests ---

func TestClientCallAuthenticated(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	var receivedSubject string
	server.HandleAuth("query", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		receivedSubject = token.Subject
		// Echo back the room field from the request.
		var request struct {
			Room string `cbor:"room"`
		}
		codec.Unmarshal(raw, &request)
		return map[string]any{"room": request.Room, "count": 5}, nil
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

	tokenBytes := mintTestToken(t, privateKey, "iree/amdgpu/pm")
	client := NewServiceClientFromToken(socketPath, tokenBytes)

	var result struct {
		Room  string `cbor:"room"`
		Count int    `cbor:"count"`
	}
	err := client.Call(ctx, "query", map[string]any{"room": "!abc:local"}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Room != "!abc:local" {
		t.Errorf("room: got %q, want !abc:local", result.Room)
	}
	if result.Count != 5 {
		t.Errorf("count: got %d, want 5", result.Count)
	}
	if receivedSubject != "iree/amdgpu/pm" {
		t.Errorf("subject: got %q, want iree/amdgpu/pm", receivedSubject)
	}

	cancel()
	wg.Wait()
}

func TestClientCallAuthenticatedMissingToken(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, _ := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("query", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called without a token")
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

	// Unauthenticated client calling an authenticated action.
	client := NewServiceClientFromToken(socketPath, nil)
	err := client.Call(ctx, "query", nil, nil)
	if err == nil {
		t.Fatal("expected error for unauthenticated request to authenticated action")
	}

	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	if serviceErr.Action != "query" {
		t.Errorf("error action: got %q, want query", serviceErr.Action)
	}

	cancel()
	wg.Wait()
}

// --- Error handling tests ---

func TestClientCallServiceError(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("fail", func(ctx context.Context, raw []byte) (any, error) {
		return nil, errors.New("something broke")
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

	client := NewServiceClientFromToken(socketPath, nil)
	err := client.Call(ctx, "fail", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	if serviceErr.Action != "fail" {
		t.Errorf("error action: got %q, want fail", serviceErr.Action)
	}
	if serviceErr.Message != "something broke" {
		t.Errorf("error message: got %q, want 'something broke'", serviceErr.Message)
	}
}

func TestClientCallUnknownAction(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("known", func(ctx context.Context, raw []byte) (any, error) {
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

	client := NewServiceClientFromToken(socketPath, nil)
	err := client.Call(ctx, "unknown", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}

	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
}

func TestClientCallConnectionRefused(t *testing.T) {
	// Socket path that doesn't exist.
	client := NewServiceClientFromToken("/tmp/nonexistent-bureau-test.sock", nil)

	err := client.Call(context.Background(), "status", nil, nil)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}

	// Should NOT be a ServiceError — it's a connection failure.
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		t.Fatalf("connection failure should not be *ServiceError, got %v", serviceErr)
	}
}

// --- Token file loading integration test ---

func TestClientCallWithTokenFromFile(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("whoami", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		return map[string]any{"subject": token.Subject}, nil
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

	// Write a real minted token to a file.
	tokenBytes := mintTestToken(t, privateKey, "agent/coder")
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "test-service")
	if err := os.WriteFile(tokenPath, tokenBytes, 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	client, err := NewServiceClient(socketPath, tokenPath)
	if err != nil {
		t.Fatalf("NewServiceClient: %v", err)
	}

	var result struct {
		Subject string `cbor:"subject"`
	}
	if err := client.Call(ctx, "whoami", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Subject != "agent/coder" {
		t.Errorf("subject: got %q, want agent/coder", result.Subject)
	}

	cancel()
	wg.Wait()
}

// --- Concurrent calls ---

func TestClientConcurrentCalls(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("echo", func(ctx context.Context, raw []byte) (any, error) {
		var request struct {
			Value int `cbor:"value"`
		}
		codec.Unmarshal(raw, &request)
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

	client := NewServiceClientFromToken(socketPath, nil)

	const concurrency = 20
	var clientWg sync.WaitGroup
	for i := range concurrency {
		clientWg.Add(1)
		go func() {
			defer clientWg.Done()
			var result map[string]any
			err := client.Call(ctx, "echo", map[string]any{"value": i}, &result)
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			if result["value"] != uint64(i) {
				t.Errorf("call %d: got value %v, want %d", i, result["value"], i)
			}
		}()
	}

	clientWg.Wait()
	cancel()
	serveWg.Wait()
}

// --- Expired token test ---

func TestClientCallExpiredToken(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("query", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with expired token")
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

	// Mint an already-expired token using fixed timestamps far in the
	// past. The server verifies expiry against wall time, so any token
	// with ExpiresAt in 2020 is unconditionally expired.
	expiredToken := &servicetoken.Token{
		Subject:   "agent/test",
		Machine:   "machine/test",
		Audience:  "test-service",
		ID:        "expired-token",
		IssuedAt:  1577836800, // 2020-01-01T00:00:00Z
		ExpiresAt: 1577840400, // 2020-01-01T01:00:00Z
	}
	tokenBytes, err := servicetoken.Mint(privateKey, expiredToken)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	client := NewServiceClientFromToken(socketPath, tokenBytes)
	err = client.Call(ctx, "query", nil, nil)
	if err == nil {
		t.Fatal("expected error for expired token")
	}

	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}

	cancel()
	wg.Wait()
}
