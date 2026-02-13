// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// sendRequest connects to a Unix socket, sends a CBOR request, and
// returns the decoded response envelope.
func sendRequest(t *testing.T, socketPath string, request any) Response {
	t.Helper()

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to socket: %v", err)
	}
	defer conn.Close()

	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	// Signal that we're done writing (half-close). CBOR is self-
	// delimiting so this isn't required by the protocol, but it's
	// good hygiene.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}

	var response Response
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return response
}

// decodeData unmarshals the Data field of a response into the given
// target. Fails the test if decoding fails.
func decodeData(t *testing.T, response Response, target any) {
	t.Helper()
	if len(response.Data) == 0 {
		t.Fatal("response has no data to decode")
	}
	if err := codec.Unmarshal(response.Data, target); err != nil {
		t.Fatalf("decoding response data: %v", err)
	}
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

// testKeypair generates an Ed25519 keypair for test use.
func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return public, private
}

// testAuthConfig creates an AuthConfig with a fresh keypair and
// blacklist for testing. Returns the config and the private key
// (for minting test tokens).
func testAuthConfig(t *testing.T) (*AuthConfig, ed25519.PrivateKey) {
	t.Helper()
	public, private := testKeypair(t)
	return &AuthConfig{
		PublicKey: public,
		Audience:  "test-service",
		Blacklist: servicetoken.NewBlacklist(),
	}, private
}

// mintTestToken creates a signed test token with the given subject.
// Uses fixed timestamps: issued 2025-01-01, expires 2099-01-01. The
// far-future expiry means the token is always valid without needing
// wall-clock access.
func mintTestToken(t *testing.T, privateKey ed25519.PrivateKey, subject string) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:  subject,
		Machine:  "machine/test",
		Audience: "test-service",
		Grants: []servicetoken.Grant{
			{Actions: []string{"test/read", "test/write"}},
		},
		ID:        "test-token-id",
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tokenBytes
}

func TestSocketServerStatus(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
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

	waitForSocket(t, socketPath)

	response := sendRequest(t, socketPath, map[string]string{"action": "status"})

	if !response.OK {
		t.Errorf("expected ok=true, got false")
	}

	var data map[string]any
	decodeData(t, response, &data)
	if data["uptime_seconds"] != uint64(42) {
		t.Errorf("expected uptime_seconds=42, got %v (%T)", data["uptime_seconds"], data["uptime_seconds"])
	}
	if data["rooms"] != uint64(3) {
		t.Errorf("expected rooms=3, got %v (%T)", data["rooms"], data["rooms"])
	}

	cancel()
	wg.Wait()
	if serveErr != nil {
		t.Errorf("Serve returned error: %v", serveErr)
	}
}

func TestSocketServerUnknownAction(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
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

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if response.Error == "" {
		t.Error("expected error message for unknown action")
	}

	cancel()
	wg.Wait()
}

func TestSocketServerMissingAction(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

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

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
}

func TestSocketServerInvalidCBOR(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

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

	// Send garbage bytes that aren't valid CBOR.
	conn.Write([]byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb})

	// Half-close so the server sees EOF after our bytes.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}

	var response Response
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if response.OK {
		t.Errorf("expected ok=false for invalid CBOR, got true")
	}
}

func TestSocketServerHandlerError(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	server.Handle("fail", func(ctx context.Context, raw []byte) (any, error) {
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

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if response.Error != "something broke" {
		t.Errorf("expected error='something broke', got %q", response.Error)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerNilResult(t *testing.T) {
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

	response := sendRequest(t, socketPath, map[string]string{"action": "noop"})

	if !response.OK {
		t.Errorf("expected ok=true, got false")
	}
	// Should have no data.
	if len(response.Data) != 0 {
		t.Errorf("expected no data in response, got %d bytes", len(response.Data))
	}

	cancel()
	wg.Wait()
}

func TestSocketServerConcurrentRequests(t *testing.T) {
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
			if !response.OK {
				t.Errorf("request %d: expected ok=true", i)
			}
			var data map[string]any
			decodeData(t, response, &data)
			if data["value"] != uint64(i) {
				t.Errorf("request %d: expected value=%d, got %v", i, i, data["value"])
			}
		}()
	}

	clientWg.Wait()
	cancel()
	serveWg.Wait()
}

func TestSocketServerGracefulShutdown(t *testing.T) {
	socketPath := testSocketPath(t)
	server := NewSocketServer(socketPath, testLogger(), nil)

	// Handler that takes a moment to complete.
	handlerStarted := make(chan struct{})
	server.Handle("slow", func(ctx context.Context, raw []byte) (any, error) {
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
	responseChan := make(chan Response, 1)
	go func() {
		responseChan <- sendRequest(t, socketPath, map[string]string{"action": "slow"})
	}()

	// Wait for the handler to start, then cancel.
	<-handlerStarted
	cancel()

	// The slow request should still complete.
	response := <-responseChan
	if !response.OK {
		t.Errorf("expected ok=true for in-flight request, got false")
	}
	var data map[string]any
	decodeData(t, response, &data)
	if data["completed"] != true {
		t.Errorf("expected completed=true, got %v", data["completed"])
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
	server := NewSocketServer("/tmp/test.sock", testLogger(), nil)
	server.Handle("foo", func(ctx context.Context, raw []byte) (any, error) {
		return nil, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate handler registration")
		}
	}()

	server.Handle("foo", func(ctx context.Context, raw []byte) (any, error) {
		return nil, nil
	})
}

// --- Authentication tests ---

func TestSocketServerHandleAuth(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	// Track what the handler receives.
	var receivedSubject string
	var receivedMachine string
	var receivedGrantCount int
	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		receivedSubject = token.Subject
		receivedMachine = token.Machine
		receivedGrantCount = len(token.Grants)
		return map[string]any{"created": true}, nil
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
	response := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
		"title":  "test ticket",
	})

	if !response.OK {
		t.Fatalf("expected ok=true, got false (error: %s)", response.Error)
	}

	var data map[string]any
	decodeData(t, response, &data)
	if data["created"] != true {
		t.Errorf("expected created=true, got %v", data["created"])
	}

	if receivedSubject != "iree/amdgpu/pm" {
		t.Errorf("handler received subject %q, want iree/amdgpu/pm", receivedSubject)
	}
	if receivedMachine != "machine/test" {
		t.Errorf("handler received machine %q, want machine/test", receivedMachine)
	}
	if receivedGrantCount != 1 {
		t.Errorf("handler received %d grants, want 1", receivedGrantCount)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerAuthMissingToken(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, _ := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
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

	// Send request without a token field.
	response := sendRequest(t, socketPath, map[string]string{"action": "create"})

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if !strings.Contains(response.Error, "missing token field") {
		t.Errorf("expected 'missing token field' in error, got %q", response.Error)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerAuthExpiredToken(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with an expired token")
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

	// Mint an already-expired token using fixed past timestamps.
	token := &servicetoken.Token{
		Subject:   "agent/test",
		Machine:   "machine/test",
		Audience:  "test-service",
		ID:        "expired-token",
		IssuedAt:  1577836800, // 2020-01-01T00:00:00Z
		ExpiresAt: 1577840400, // 2020-01-01T01:00:00Z
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	response := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
	})

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if !strings.Contains(response.Error, "token expired") {
		t.Errorf("expected 'token expired' in error, got %q", response.Error)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerAuthRevokedToken(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with a revoked token")
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

	// Mint a valid token, then blacklist it.
	tokenBytes := mintTestToken(t, privateKey, "agent/test")
	authConfig.Blacklist.Revoke("test-token-id", time.Unix(4070908800, 0)) // 2099-01-01: always in the future

	response := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
	})

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if !strings.Contains(response.Error, "token revoked") {
		t.Errorf("expected 'token revoked' in error, got %q", response.Error)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerAuthBadSignature(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with a tampered token")
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

	// Mint a valid token, then tamper with the payload.
	tokenBytes := mintTestToken(t, privateKey, "agent/test")
	tokenBytes[0] ^= 0xFF

	response := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
	})

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	if response.Error != "authentication failed" {
		t.Errorf("expected 'authentication failed', got %q", response.Error)
	}

	cancel()
	wg.Wait()
}

func TestSocketServerHandleAuthPanicsWithoutConfig(t *testing.T) {
	server := NewSocketServer("/tmp/test.sock", testLogger(), nil)

	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic when calling HandleAuth without AuthConfig")
		}
		message, ok := r.(string)
		if !ok || !strings.Contains(message, "HandleAuth requires AuthConfig") {
			t.Errorf("unexpected panic message: %v", r)
		}
	}()

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		return nil, nil
	})
}

func TestSocketServerDuplicateAuthAction(t *testing.T) {
	authConfig, _ := testAuthConfig(t)

	// Auth-auth duplicate.
	t.Run("auth-auth", func(t *testing.T) {
		server := NewSocketServer("/tmp/test.sock", testLogger(), authConfig)
		server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
			return nil, nil
		})

		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on duplicate auth handler")
			}
		}()

		server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
			return nil, nil
		})
	})

	// Auth-unauth duplicate: register auth first, then try unauth.
	t.Run("auth-then-unauth", func(t *testing.T) {
		server := NewSocketServer("/tmp/test.sock", testLogger(), authConfig)
		server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
			return nil, nil
		})

		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on auth-then-unauth duplicate")
			}
		}()

		server.Handle("create", func(ctx context.Context, raw []byte) (any, error) {
			return nil, nil
		})
	})

	// Unauth-auth duplicate: register unauth first, then try auth.
	t.Run("unauth-then-auth", func(t *testing.T) {
		server := NewSocketServer("/tmp/test.sock", testLogger(), authConfig)
		server.Handle("create", func(ctx context.Context, raw []byte) (any, error) {
			return nil, nil
		})

		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on unauth-then-auth duplicate")
			}
		}()

		server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
			return nil, nil
		})
	})
}

func TestSocketServerMixedHandlers(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	// Unauthenticated health check.
	server.Handle("status", func(ctx context.Context, raw []byte) (any, error) {
		return map[string]any{"healthy": true}, nil
	})

	// Authenticated mutation.
	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
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

	// Unauthenticated request should work without a token.
	statusResponse := sendRequest(t, socketPath, map[string]string{"action": "status"})
	if !statusResponse.OK {
		t.Fatalf("status: expected ok=true, got false (error: %s)", statusResponse.Error)
	}
	var statusData map[string]any
	decodeData(t, statusResponse, &statusData)
	if statusData["healthy"] != true {
		t.Errorf("status: expected healthy=true, got %v", statusData["healthy"])
	}

	// Authenticated request should work with a valid token.
	tokenBytes := mintTestToken(t, privateKey, "agent/coder")
	createResponse := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
	})
	if !createResponse.OK {
		t.Fatalf("create: expected ok=true, got false (error: %s)", createResponse.Error)
	}
	var createData map[string]any
	decodeData(t, createResponse, &createData)
	if createData["subject"] != "agent/coder" {
		t.Errorf("create: expected subject=agent/coder, got %v", createData["subject"])
	}

	// Authenticated endpoint without a token should fail.
	noTokenResponse := sendRequest(t, socketPath, map[string]string{"action": "create"})
	if noTokenResponse.OK {
		t.Errorf("create without token: expected ok=false, got true")
	}

	cancel()
	wg.Wait()
}

func TestSocketServerAuthWrongAudience(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuth("create", func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		t.Error("handler should not be called with wrong audience token")
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

	// Mint a token for a different service.
	token := &servicetoken.Token{
		Subject:   "agent/test",
		Machine:   "machine/test",
		Audience:  "wrong-service",
		ID:        "wrong-audience-token",
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	response := sendRequest(t, socketPath, map[string]any{
		"action": "create",
		"token":  tokenBytes,
	})

	if response.OK {
		t.Errorf("expected ok=false, got true")
	}
	// Audience mismatch should produce the generic "authentication failed"
	// (not leak which audience was expected).
	if response.Error != "authentication failed" {
		t.Errorf("expected 'authentication failed', got %q", response.Error)
	}

	cancel()
	wg.Wait()
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
