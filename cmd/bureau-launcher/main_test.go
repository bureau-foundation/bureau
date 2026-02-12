// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/tmux"
)

func testBuffer(t *testing.T, value string) *secret.Buffer {
	t.Helper()
	buffer, err := secret.NewFromString(value)
	if err != nil {
		t.Fatalf("creating test buffer: %v", err)
	}
	t.Cleanup(func() { buffer.Close() })
	return buffer
}

func TestDerivePassword(t *testing.T) {
	// Deterministic: same inputs produce same output.
	password1, err := derivePassword(testBuffer(t, "token123"), "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password1.Close()
	password2, err := derivePassword(testBuffer(t, "token123"), "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password2.Close()
	if !password1.Equal(password2) {
		t.Errorf("derivePassword not deterministic: %q != %q", password1.String(), password2.String())
	}

	// Different tokens produce different passwords.
	password3, err := derivePassword(testBuffer(t, "different-token"), "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password3.Close()
	if password1.Equal(password3) {
		t.Errorf("different tokens should produce different passwords")
	}

	// Different machine names produce different passwords.
	password4, err := derivePassword(testBuffer(t, "token123"), "machine/other")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password4.Close()
	if password1.Equal(password4) {
		t.Errorf("different machine names should produce different passwords")
	}

	// Result should be a hex-encoded SHA-256 hash (64 hex chars).
	if password1.Len() != 64 {
		t.Errorf("password length = %d, want 64 (hex-encoded SHA-256)", password1.Len())
	}

	// All characters should be valid hex.
	for _, char := range password1.String() {
		if !strings.ContainsRune("0123456789abcdef", char) {
			t.Errorf("password contains non-hex character: %q", char)
			break
		}
	}
}

func TestReadSecret_File(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			name:     "plain value",
			content:  "my-secret-token",
			expected: "my-secret-token",
		},
		{
			name:     "trailing newline",
			content:  "my-secret-token\n",
			expected: "my-secret-token",
		},
		{
			name:     "trailing whitespace",
			content:  "my-secret-token  \n",
			expected: "my-secret-token",
		},
		{
			name:     "leading whitespace",
			content:  "  my-secret-token",
			expected: "my-secret-token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(tempDir, test.name)
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatalf("writing test file: %v", err)
			}

			result, err := readSecret(path)
			if err != nil {
				t.Fatalf("readSecret() error: %v", err)
			}
			defer result.Close()
			if result.String() != test.expected {
				t.Errorf("readSecret() = %q, want %q", result.String(), test.expected)
			}
		})
	}
}

func TestReadSecret_FileNotFound(t *testing.T) {
	_, err := readSecret("/nonexistent/path/to/secret")
	if err == nil {
		t.Error("readSecret() with nonexistent file should return error")
	}
}

func TestCredentialKeys(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]string
		expected    []string
	}{
		{
			name:        "nil map",
			credentials: nil,
			expected:    []string{},
		},
		{
			name:        "empty map",
			credentials: map[string]string{},
			expected:    []string{},
		},
		{
			name: "populated map",
			credentials: map[string]string{
				"OPENAI_API_KEY":    "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
				"GITHUB_PAT":        "ghp_test",
			},
			expected: []string{"ANTHROPIC_API_KEY", "GITHUB_PAT", "OPENAI_API_KEY"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := credentialKeys(test.credentials)
			sort.Strings(keys)
			sort.Strings(test.expected)

			if len(keys) != len(test.expected) {
				t.Fatalf("credentialKeys() returned %d keys, want %d", len(keys), len(test.expected))
			}
			for i, key := range keys {
				if key != test.expected[i] {
					t.Errorf("credentialKeys()[%d] = %q, want %q", i, key, test.expected[i])
				}
			}
		})
	}
}

func TestLoadOrGenerateKeypair_FirstBoot(t *testing.T) {
	stateDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	keypair, firstBoot, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		t.Fatalf("loadOrGenerateKeypair() error: %v", err)
	}
	defer keypair.Close()
	if !firstBoot {
		t.Error("expected firstBoot = true on first call")
	}
	if !strings.HasPrefix(keypair.PublicKey, "age1") {
		t.Errorf("public key = %q, want age1 prefix", keypair.PublicKey)
	}
	if !strings.HasPrefix(keypair.PrivateKey.String(), "AGE-SECRET-KEY-1") {
		t.Errorf("private key = %q, want AGE-SECRET-KEY-1 prefix", keypair.PrivateKey.String())
	}

	// Verify files were written.
	privateKeyPath := filepath.Join(stateDir, "machine-key.txt")
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")

	if _, err := os.Stat(privateKeyPath); err != nil {
		t.Errorf("private key file not created: %v", err)
	}
	if _, err := os.Stat(publicKeyPath); err != nil {
		t.Errorf("public key file not created: %v", err)
	}

	// Verify file permissions.
	info, err := os.Stat(privateKeyPath)
	if err == nil && info.Mode().Perm() != 0600 {
		t.Errorf("private key permissions = %o, want 0600", info.Mode().Perm())
	}
	info, err = os.Stat(publicKeyPath)
	if err == nil && info.Mode().Perm() != 0644 {
		t.Errorf("public key permissions = %o, want 0644", info.Mode().Perm())
	}
}

func TestLoadOrGenerateKeypair_SubsequentBoot(t *testing.T) {
	stateDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// First boot generates the keypair.
	keypair1, firstBoot1, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		t.Fatalf("first loadOrGenerateKeypair() error: %v", err)
	}
	defer keypair1.Close()
	if !firstBoot1 {
		t.Fatal("expected firstBoot = true on first call")
	}

	// Second call loads the existing keypair.
	keypair2, firstBoot2, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		t.Fatalf("second loadOrGenerateKeypair() error: %v", err)
	}
	defer keypair2.Close()
	if firstBoot2 {
		t.Error("expected firstBoot = false on second call")
	}

	if keypair2.PublicKey != keypair1.PublicKey {
		t.Errorf("public key changed: %q != %q", keypair2.PublicKey, keypair1.PublicKey)
	}
	if !keypair2.PrivateKey.Equal(keypair1.PrivateKey) {
		t.Errorf("private key changed on reload")
	}
}

func TestLoadOrGenerateKeypair_TrimsWhitespace(t *testing.T) {
	stateDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Generate a keypair normally first.
	keypair, _, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		t.Fatalf("loadOrGenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	// Overwrite the key files with trailing newlines (simulating
	// a text editor or echo command).
	privateKeyPath := filepath.Join(stateDir, "machine-key.txt")
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")
	keyWithNewline := make([]byte, keypair.PrivateKey.Len()+1)
	copy(keyWithNewline, keypair.PrivateKey.Bytes())
	keyWithNewline[len(keyWithNewline)-1] = '\n'
	os.WriteFile(privateKeyPath, keyWithNewline, 0600)
	os.WriteFile(publicKeyPath, []byte(keypair.PublicKey+"\n"), 0644)

	// Reload should still work and produce the same keys.
	keypair2, firstBoot, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		t.Fatalf("loadOrGenerateKeypair() after whitespace injection: %v", err)
	}
	defer keypair2.Close()
	if firstBoot {
		t.Error("expected firstBoot = false on reload")
	}
	if keypair2.PublicKey != keypair.PublicKey {
		t.Errorf("public key mismatch after whitespace trimming: %q != %q", keypair2.PublicKey, keypair.PublicKey)
	}
	if !keypair2.PrivateKey.Equal(keypair.PrivateKey) {
		t.Errorf("private key mismatch after whitespace trimming")
	}
}

func TestLoadOrGenerateKeypair_InvalidKeyFiles(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	t.Run("invalid private key", func(t *testing.T) {
		stateDir := t.TempDir()
		os.WriteFile(filepath.Join(stateDir, "machine-key.txt"), []byte("not-a-key"), 0600)
		os.WriteFile(filepath.Join(stateDir, "machine-key.pub"), []byte("age1abc"), 0644)

		_, _, err := loadOrGenerateKeypair(stateDir, logger)
		if err == nil {
			t.Error("expected error for invalid private key")
		}
		if !strings.Contains(err.Error(), "stored private key is invalid") {
			t.Errorf("error = %v, want 'stored private key is invalid'", err)
		}
	})

	t.Run("private key without public key", func(t *testing.T) {
		stateDir := t.TempDir()
		// Generate a valid keypair to get a real private key.
		keypair, err := sealed.GenerateKeypair()
		if err != nil {
			t.Fatalf("GenerateKeypair() error: %v", err)
		}
		defer keypair.Close()
		os.WriteFile(filepath.Join(stateDir, "machine-key.txt"), keypair.PrivateKey.Bytes(), 0600)
		// No public key file.

		_, _, err = loadOrGenerateKeypair(stateDir, logger)
		if err == nil {
			t.Error("expected error when public key file is missing")
		}
		if !strings.Contains(err.Error(), "public key missing") {
			t.Errorf("error = %v, want 'public key missing'", err)
		}
	})
}

func TestHandleCreateSandbox_Validation(t *testing.T) {
	launcher := &Launcher{
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	tests := []struct {
		name      string
		request   IPCRequest
		expectOK  bool
		expectErr string
	}{
		{
			name:      "missing principal",
			request:   IPCRequest{Action: "create-sandbox"},
			expectOK:  false,
			expectErr: "principal is required",
		},
		{
			name:      "invalid principal (path traversal)",
			request:   IPCRequest{Action: "create-sandbox", Principal: "../escape"},
			expectOK:  false,
			expectErr: "invalid principal",
		},
		{
			name:      "invalid principal (uppercase)",
			request:   IPCRequest{Action: "create-sandbox", Principal: "MACHINE"},
			expectOK:  false,
			expectErr: "invalid principal",
		},
		{
			name:      "valid principal but no proxy binary",
			request:   IPCRequest{Action: "create-sandbox", Principal: "iree/amdgpu/pm"},
			expectOK:  false,
			expectErr: "proxy binary path not configured",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := launcher.handleCreateSandbox(context.Background(), &test.request)
			if response.OK != test.expectOK {
				t.Errorf("OK = %v, want %v (error: %s)", response.OK, test.expectOK, response.Error)
			}
			if test.expectErr != "" && !strings.Contains(response.Error, test.expectErr) {
				t.Errorf("error = %q, want substring %q", response.Error, test.expectErr)
			}
		})
	}
}

func TestHandleCreateSandbox_CredentialDecryption(t *testing.T) {
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	launcher := &Launcher{
		keypair:   keypair,
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Encrypt a credential bundle that is missing MATRIX_TOKEN.
	// The request should fail at the spawnProxy stage (no proxy binary),
	// which means decryption succeeded.
	credentials := map[string]string{
		"OPENAI_API_KEY": "sk-test",
		"MATRIX_TOKEN":   "syt_test",
	}
	credentialJSON, _ := json.Marshal(credentials)
	ciphertext, err := sealed.Encrypt(credentialJSON, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "iree/amdgpu/pm",
		EncryptedCredentials: ciphertext,
	})

	// Should fail because proxy binary is not configured, but decryption
	// succeeded (error message references the proxy, not decryption).
	if response.OK {
		t.Error("expected error (no proxy binary)")
	}
	if strings.Contains(response.Error, "decrypting") {
		t.Errorf("error suggests decryption failed, but it should have succeeded: %s", response.Error)
	}
	if !strings.Contains(response.Error, "proxy binary") {
		t.Errorf("expected error about proxy binary, got: %s", response.Error)
	}
}

func TestHandleCreateSandbox_InvalidCiphertext(t *testing.T) {
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair() error: %v", err)
	}
	defer keypair.Close()

	launcher := &Launcher{
		keypair:   keypair,
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "iree/amdgpu/pm",
		EncryptedCredentials: "not-valid-base64!!!",
	})
	if response.OK {
		t.Error("expected error for invalid ciphertext")
	}
	if !strings.Contains(response.Error, "decrypting credentials") {
		t.Errorf("error = %q, want substring 'decrypting credentials'", response.Error)
	}
}

func TestHandleDestroySandbox_Validation(t *testing.T) {
	launcher := &Launcher{
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	tests := []struct {
		name      string
		request   IPCRequest
		expectOK  bool
		expectErr string
	}{
		{
			name:      "missing principal",
			request:   IPCRequest{Action: "destroy-sandbox"},
			expectOK:  false,
			expectErr: "principal is required",
		},
		{
			name:      "invalid principal",
			request:   IPCRequest{Action: "destroy-sandbox", Principal: ".."},
			expectOK:  false,
			expectErr: "invalid principal",
		},
		{
			name:      "valid principal but no sandbox running",
			request:   IPCRequest{Action: "destroy-sandbox", Principal: "iree/amdgpu/pm"},
			expectOK:  false,
			expectErr: "no sandbox running",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := launcher.handleDestroySandbox(context.Background(), &test.request)
			if response.OK != test.expectOK {
				t.Errorf("OK = %v, want %v (error: %s)", response.OK, test.expectOK, response.Error)
			}
			if test.expectErr != "" && !strings.Contains(response.Error, test.expectErr) {
				t.Errorf("error = %q, want substring %q", response.Error, test.expectErr)
			}
		})
	}
}

func TestHandleConnection_FullCycle(t *testing.T) {
	launcher := &Launcher{
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Create a socket pair for testing.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	tests := []struct {
		name     string
		request  IPCRequest
		expectOK bool
	}{
		{
			name:     "status",
			request:  IPCRequest{Action: "status"},
			expectOK: true,
		},
		{
			name:     "unknown action",
			request:  IPCRequest{Action: "explode"},
			expectOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Client side: connect and send request.
			done := make(chan IPCResponse, 1)
			go func() {
				conn, err := net.Dial("unix", socketPath)
				if err != nil {
					t.Errorf("Dial() error: %v", err)
					return
				}
				defer conn.Close()

				encoder := json.NewEncoder(conn)
				decoder := json.NewDecoder(conn)

				if err := encoder.Encode(test.request); err != nil {
					t.Errorf("Encode() error: %v", err)
					return
				}

				var response IPCResponse
				if err := decoder.Decode(&response); err != nil {
					t.Errorf("Decode() error: %v", err)
					return
				}
				done <- response
			}()

			// Server side: accept and handle.
			conn, err := listener.Accept()
			if err != nil {
				t.Fatalf("Accept() error: %v", err)
			}
			launcher.handleConnection(context.Background(), conn)

			response := <-done
			if response.OK != test.expectOK {
				t.Errorf("OK = %v, want %v (error: %s)", response.OK, test.expectOK, response.Error)
			}
		})
	}
}

func TestListenSocket(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "test.sock")

	listener, err := listenSocket(socketPath)
	if err != nil {
		t.Fatalf("listenSocket() error: %v", err)
	}
	defer listener.Close()

	// Verify the socket file exists.
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("socket file not created: %v", err)
	}

	// Verify permissions (0660).
	if info.Mode().Perm() != 0660 {
		t.Errorf("socket permissions = %o, want 0660", info.Mode().Perm())
	}

	// Calling listenSocket again should work (removes stale socket).
	listener.Close()
	listener2, err := listenSocket(socketPath)
	if err != nil {
		t.Fatalf("second listenSocket() error: %v", err)
	}
	listener2.Close()
}

func TestListenSocket_CreatesParentDirectory(t *testing.T) {
	tempDir := testutil.SocketDir(t)
	socketPath := filepath.Join(tempDir, "nested", "dir", "test.sock")

	listener, err := listenSocket(socketPath)
	if err != nil {
		t.Fatalf("listenSocket() error: %v", err)
	}
	listener.Close()
}

func TestSaveAndLoadSession(t *testing.T) {
	stateDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// saveSession requires a *messaging.Session which needs a real Client.
	// Instead, test the serialization by writing the JSON directly and
	// verifying loadSession can read it.
	sessionPath := filepath.Join(stateDir, "session.json")
	sessionJSON := `{
		"homeserver_url": "http://localhost:6167",
		"user_id": "@machine/test:bureau.local",
		"access_token": "syt_test_token_12345"
	}`
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0600); err != nil {
		t.Fatalf("writing session file: %v", err)
	}

	session, err := loadSession(stateDir, "http://localhost:6167", logger)
	if err != nil {
		t.Fatalf("loadSession() error: %v", err)
	}
	defer session.Close()

	if session.UserID() != "@machine/test:bureau.local" {
		t.Errorf("UserID() = %q, want %q", session.UserID(), "@machine/test:bureau.local")
	}
	if session.AccessToken() != "syt_test_token_12345" {
		t.Errorf("AccessToken() = %q, want %q", session.AccessToken(), "syt_test_token_12345")
	}
}

func TestLoadSession_Errors(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	t.Run("file not found", func(t *testing.T) {
		_, err := loadSession(t.TempDir(), "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error when session file doesn't exist")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		stateDir := t.TempDir()
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte("not json"), 0600)

		_, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("empty access token", func(t *testing.T) {
		stateDir := t.TempDir()
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@test:bureau.local",
			"access_token": ""
		}`), 0600)

		_, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for empty access token")
		}
		if !strings.Contains(err.Error(), "empty access token") {
			t.Errorf("error = %v, want 'empty access token'", err)
		}
	})
}

func TestBuildCredentialPayload(t *testing.T) {
	launcher := &Launcher{
		homeserverURL: "http://localhost:6167",
		serverName:    "bureau.local",
		logger:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("full credentials", func(t *testing.T) {
		credentials := map[string]string{
			"MATRIX_HOMESERVER_URL": "http://custom:6167",
			"MATRIX_TOKEN":          "syt_test_token",
			"MATRIX_USER_ID":        "@test/echo:bureau.local",
			"OPENAI_API_KEY":        "sk-test",
			"GITHUB_PAT":            "ghp_test",
		}

		payload, err := launcher.buildCredentialPayload("test/echo", credentials, nil)
		if err != nil {
			t.Fatalf("buildCredentialPayload() error: %v", err)
		}

		if payload.MatrixHomeserverURL != "http://custom:6167" {
			t.Errorf("MatrixHomeserverURL = %q, want %q", payload.MatrixHomeserverURL, "http://custom:6167")
		}
		if payload.MatrixToken != "syt_test_token" {
			t.Errorf("MatrixToken = %q, want %q", payload.MatrixToken, "syt_test_token")
		}
		if payload.MatrixUserID != "@test/echo:bureau.local" {
			t.Errorf("MatrixUserID = %q, want %q", payload.MatrixUserID, "@test/echo:bureau.local")
		}

		// Remaining credentials should not include Matrix fields.
		if _, exists := payload.Credentials["MATRIX_TOKEN"]; exists {
			t.Error("MATRIX_TOKEN should not be in remaining credentials")
		}
		if payload.Credentials["OPENAI_API_KEY"] != "sk-test" {
			t.Errorf("OPENAI_API_KEY = %q, want %q", payload.Credentials["OPENAI_API_KEY"], "sk-test")
		}
		if payload.Credentials["GITHUB_PAT"] != "ghp_test" {
			t.Errorf("GITHUB_PAT = %q, want %q", payload.Credentials["GITHUB_PAT"], "ghp_test")
		}
	})

	t.Run("missing MATRIX_TOKEN", func(t *testing.T) {
		credentials := map[string]string{
			"OPENAI_API_KEY": "sk-test",
		}

		_, err := launcher.buildCredentialPayload("test/echo", credentials, nil)
		if err == nil {
			t.Error("expected error for missing MATRIX_TOKEN")
		}
		if !strings.Contains(err.Error(), "MATRIX_TOKEN") {
			t.Errorf("error = %q, want mention of MATRIX_TOKEN", err.Error())
		}
	})

	t.Run("homeserver URL fallback", func(t *testing.T) {
		credentials := map[string]string{
			"MATRIX_TOKEN": "syt_test",
		}

		payload, err := launcher.buildCredentialPayload("test/echo", credentials, nil)
		if err != nil {
			t.Fatalf("buildCredentialPayload() error: %v", err)
		}

		// Should fall back to launcher's homeserver URL.
		if payload.MatrixHomeserverURL != "http://localhost:6167" {
			t.Errorf("MatrixHomeserverURL = %q, want fallback %q", payload.MatrixHomeserverURL, "http://localhost:6167")
		}
	})

	t.Run("user ID fallback", func(t *testing.T) {
		credentials := map[string]string{
			"MATRIX_TOKEN": "syt_test",
		}

		payload, err := launcher.buildCredentialPayload("test/echo", credentials, nil)
		if err != nil {
			t.Fatalf("buildCredentialPayload() error: %v", err)
		}

		// Should derive from principal localpart and server name.
		if payload.MatrixUserID != "@test/echo:bureau.local" {
			t.Errorf("MatrixUserID = %q, want %q", payload.MatrixUserID, "@test/echo:bureau.local")
		}
	})
}

func TestWaitForSocket(t *testing.T) {
	t.Run("socket appears", func(t *testing.T) {
		socketDir := testutil.SocketDir(t)
		socketPath := filepath.Join(socketDir, "test.sock")

		processDone := make(chan struct{})

		// Create the socket after a short delay.
		go func() {
			time.Sleep(50 * time.Millisecond)
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				return
			}
			defer listener.Close()
			// Keep the socket alive until the test finishes.
			<-processDone
		}()
		defer close(processDone)

		if err := waitForSocket(socketPath, make(chan struct{}), 5*time.Second); err != nil {
			t.Errorf("waitForSocket() error: %v", err)
		}
	})

	t.Run("process exits before socket", func(t *testing.T) {
		socketDir := testutil.SocketDir(t)
		socketPath := filepath.Join(socketDir, "nonexistent.sock")

		processDone := make(chan struct{})
		close(processDone) // process already exited

		err := waitForSocket(socketPath, processDone, 5*time.Second)
		if err == nil {
			t.Error("expected error when process exits before socket appears")
		}
		if !strings.Contains(err.Error(), "process exited") {
			t.Errorf("error = %q, want mention of process exit", err.Error())
		}
	})

	t.Run("timeout", func(t *testing.T) {
		socketDir := testutil.SocketDir(t)
		socketPath := filepath.Join(socketDir, "timeout.sock")

		err := waitForSocket(socketPath, make(chan struct{}), 50*time.Millisecond)
		if err == nil {
			t.Error("expected timeout error")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %q, want mention of timeout", err.Error())
		}
	})
}

// buildProxyBinary returns the path to a pre-built bureau-proxy binary.
// The binary is provided as a Bazel data dependency via BUREAU_PROXY_BINARY.
func buildProxyBinary(t *testing.T) string {
	t.Helper()
	return testutil.DataBinary(t, "BUREAU_PROXY_BINARY")
}

// newTestLauncher creates a Launcher with temp directories for sockets and
// state, a freshly generated keypair, and the given proxy binary. Suitable
// for integration tests that spawn real proxy processes.
func newTestLauncher(t *testing.T, proxyBinaryPath string) *Launcher {
	t.Helper()

	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	t.Cleanup(func() { keypair.Close() })

	tempDir := testutil.SocketDir(t)

	launcher := &Launcher{
		keypair:         keypair,
		machineName:     "machine/test",
		serverName:      "bureau.local",
		homeserverURL:   "http://localhost:9999",
		runDir:          tempDir,
		stateDir:        filepath.Join(tempDir, "state"),
		proxyBinaryPath: proxyBinaryPath,
		tmuxServer:      tmux.NewServer(principal.TmuxSocketPath(tempDir), "/dev/null"),
		sandboxes:       make(map[string]*managedSandbox),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Cleanup(launcher.shutdownAllSandboxes)

	return launcher
}

// encryptCredentials encrypts a credential map to the launcher's keypair,
// returning the ciphertext string for use in IPCRequest.EncryptedCredentials.
func encryptCredentials(t *testing.T, keypair *sealed.Keypair, credentials map[string]string) string {
	t.Helper()

	credentialJSON, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}

	ciphertext, err := sealed.Encrypt(credentialJSON, []string{keypair.PublicKey})
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}

	return ciphertext
}

// unixHTTPClient creates an http.Client that connects via the given Unix socket.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

func TestProxyLifecycle(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN":   "syt_fake_test_token",
		"MATRIX_USER_ID": "@test/echo:bureau.local",
		"OPENAI_API_KEY": "sk-test-fake",
	})

	// --- Create sandbox ---
	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/echo",
		EncryptedCredentials: ciphertext,
	})
	if !response.OK {
		t.Fatalf("create-sandbox failed: %s", response.Error)
	}
	if response.ProxyPID == 0 {
		t.Error("ProxyPID should be non-zero")
	}

	// --- Verify agent socket exists and serves health check ---
	socketPath := principal.RunDirSocketPath(launcher.runDir, "test/echo")
	agentClient := unixHTTPClient(socketPath)

	healthResponse, err := agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check through agent socket: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", healthResponse.StatusCode)
	}

	// --- Verify agent identity is set from credentials ---
	identityResponse, err := agentClient.Get("http://localhost/v1/identity")
	if err != nil {
		t.Fatalf("identity request: %v", err)
	}
	defer identityResponse.Body.Close()
	identityBody, _ := io.ReadAll(identityResponse.Body)

	var identity struct {
		UserID     string `json:"user_id"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal(identityBody, &identity); err != nil {
		t.Fatalf("parsing identity response: %v\nbody: %s", err, identityBody)
	}
	if identity.UserID != "@test/echo:bureau.local" {
		t.Errorf("identity user_id = %q, want %q", identity.UserID, "@test/echo:bureau.local")
	}
	if identity.ServerName != "bureau.local" {
		t.Errorf("identity server_name = %q, want %q", identity.ServerName, "bureau.local")
	}

	// --- Verify admin socket accepts service registration ---
	adminSocketPath := principal.RunDirAdminSocketPath(launcher.runDir, "test/echo")
	adminClient := unixHTTPClient(adminSocketPath)

	serviceRequest, _ := http.NewRequest("PUT", "http://localhost/v1/admin/services/test-svc",
		strings.NewReader(`{"upstream_url":"http://httpbin.org","upstream_unix":""}`))
	serviceRequest.Header.Set("Content-Type", "application/json")
	serviceResponse, err := adminClient.Do(serviceRequest)
	if err != nil {
		t.Fatalf("service registration through admin socket: %v", err)
	}
	serviceResponse.Body.Close()
	if serviceResponse.StatusCode != http.StatusCreated {
		t.Errorf("service registration status = %d, want 201", serviceResponse.StatusCode)
	}

	// --- Verify duplicate create is rejected ---
	duplicateResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/echo",
		EncryptedCredentials: ciphertext,
	})
	if duplicateResponse.OK {
		t.Error("expected error for duplicate create-sandbox")
	}
	if !strings.Contains(duplicateResponse.Error, "already has a running sandbox") {
		t.Errorf("duplicate error = %q, want mention of already running", duplicateResponse.Error)
	}

	// --- Destroy sandbox ---
	destroyResponse := launcher.handleDestroySandbox(context.Background(), &IPCRequest{
		Action:    "destroy-sandbox",
		Principal: "test/echo",
	})
	if !destroyResponse.OK {
		t.Fatalf("destroy-sandbox failed: %s", destroyResponse.Error)
	}

	// --- Verify cleanup ---
	if _, exists := launcher.sandboxes["test/echo"]; exists {
		t.Error("sandbox should be removed from map after destroy")
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("agent socket should be removed after destroy, stat error: %v", err)
	}
	if _, err := os.Stat(adminSocketPath); !os.IsNotExist(err) {
		t.Errorf("admin socket should be removed after destroy, stat error: %v", err)
	}

	// --- Verify re-creation works after destroy ---
	recreateResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/echo",
		EncryptedCredentials: ciphertext,
	})
	if !recreateResponse.OK {
		t.Fatalf("re-create after destroy failed: %s", recreateResponse.Error)
	}
	if recreateResponse.ProxyPID == 0 {
		t.Error("recreated ProxyPID should be non-zero")
	}

	// Verify the recreated proxy is functional.
	healthResponse2, err := agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check after recreate: %v", err)
	}
	healthResponse2.Body.Close()
	if healthResponse2.StatusCode != http.StatusOK {
		t.Errorf("recreated health status = %d, want 200", healthResponse2.StatusCode)
	}
}

func TestProxyLifecycle_WithoutCredentials(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	// Create a sandbox without credentials (no encrypted bundle).
	// The proxy starts without -credential-stdin, so Matrix service
	// won't be configured, but the proxy itself should function.
	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:    "create-sandbox",
		Principal: "test/nocreds",
	})
	if !response.OK {
		t.Fatalf("create-sandbox without credentials failed: %s", response.Error)
	}

	// Verify health endpoint works.
	socketPath := principal.RunDirSocketPath(launcher.runDir, "test/nocreds")
	client := unixHTTPClient(socketPath)

	healthResponse, err := client.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", healthResponse.StatusCode)
	}
}

func TestProxyLifecycle_CrashedProcessRecreation(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create a sandbox.
	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/crash",
		EncryptedCredentials: ciphertext,
	})
	if !response.OK {
		t.Fatalf("create-sandbox: %s", response.Error)
	}

	// Kill the tmux session to simulate a sandbox crash. In the unified
	// model, killing the tmux session is what ends the sandbox — the
	// session watcher detects it and closes the done channel.
	sandbox := launcher.sandboxes["test/crash"]
	launcher.destroyTmuxSession("test/crash")

	// Wait for the session watcher to detect the tmux session is gone.
	select {
	case <-sandbox.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session watcher to close done channel")
	}

	// Attempting to create the same sandbox should succeed because the
	// sandbox has exited (the launcher detects this and cleans up).
	response2 := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/crash",
		EncryptedCredentials: ciphertext,
	})
	if !response2.OK {
		t.Fatalf("recreate after crash: %s", response2.Error)
	}
	if response2.ProxyPID == response.ProxyPID {
		t.Error("recreated proxy should have a different PID")
	}
}

func TestShutdownAllSandboxes(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)
	// Remove the t.Cleanup since we'll call shutdown manually.
	// (t.Cleanup still runs, but shutdownAllSandboxes is idempotent.)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create multiple sandboxes.
	for _, name := range []string{"test/a", "test/b", "test/c"} {
		response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
			Action:               "create-sandbox",
			Principal:            name,
			EncryptedCredentials: ciphertext,
		})
		if !response.OK {
			t.Fatalf("create-sandbox %s: %s", name, response.Error)
		}
	}

	if len(launcher.sandboxes) != 3 {
		t.Fatalf("expected 3 sandboxes, got %d", len(launcher.sandboxes))
	}

	// Shutdown all.
	launcher.shutdownAllSandboxes()

	if len(launcher.sandboxes) != 0 {
		t.Errorf("expected 0 sandboxes after shutdown, got %d", len(launcher.sandboxes))
	}
}

// --- Tmux integration tests ---

// requireTmux skips the test if tmux is not available on the system.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
}

// newTestRunDir creates a short temp directory suitable for use as --run-dir
// in tests. Returns the runDir and a tmux.Server targeting the socket at
// <dir>/tmux.sock. The server is cleaned up on test teardown.
func newTestRunDir(t *testing.T) (string, *tmux.Server) {
	t.Helper()
	runDir := testutil.SocketDir(t)
	server := tmux.NewServer(principal.TmuxSocketPath(runDir), "/dev/null")
	t.Cleanup(func() {
		server.KillServer()
	})
	return runDir, server
}

func TestTmuxSessionName(t *testing.T) {
	tests := []struct {
		localpart string
		want      string
	}{
		{"iree/amdgpu/pm", "bureau/iree/amdgpu/pm"},
		{"service/stt/whisper", "bureau/service/stt/whisper"},
		{"test/echo", "bureau/test/echo"},
		{"simple", "bureau/simple"},
	}
	for _, test := range tests {
		got := tmuxSessionName(test.localpart)
		if got != test.want {
			t.Errorf("tmuxSessionName(%q) = %q, want %q", test.localpart, got, test.want)
		}
	}
}

func TestTmuxSessionLifecycle(t *testing.T) {
	requireTmux(t)

	runDir, tmuxServer := newTestRunDir(t)

	launcher := &Launcher{
		runDir:     runDir,
		tmuxServer: tmuxServer,
		sandboxes:  make(map[string]*managedSandbox),
		logger:     slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	localpart := "test/tmux"
	sessionName := tmuxSessionName(localpart)

	// Create the session.
	if err := launcher.createTmuxSession(localpart); err != nil {
		t.Fatalf("createTmuxSession: %v", err)
	}

	// Verify the session exists on the Bureau tmux server.
	if !tmuxServer.HasSession(sessionName) {
		t.Fatalf("tmux session %q should exist after creation", sessionName)
	}

	// Destroy the session.
	launcher.destroyTmuxSession(localpart)

	// Verify the session is gone.
	if tmuxServer.HasSession(sessionName) {
		t.Errorf("tmux session %q should not exist after destruction", sessionName)
	}
}

func TestTmuxSessionConfiguration(t *testing.T) {
	requireTmux(t)

	runDir, tmuxServer := newTestRunDir(t)

	launcher := &Launcher{
		runDir:     runDir,
		tmuxServer: tmuxServer,
		sandboxes:  make(map[string]*managedSandbox),
		logger:     slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	localpart := "test/config"
	sessionName := tmuxSessionName(localpart)

	if err := launcher.createTmuxSession(localpart); err != nil {
		t.Fatalf("createTmuxSession: %v", err)
	}

	// Verify Bureau-standard tmux options were applied.
	globalOptions := []struct {
		option string
		want   string
	}{
		{"prefix", "C-a"},
		{"mouse", "on"},
		{"history-limit", "50000"},
	}

	for _, test := range globalOptions {
		output, err := tmuxServer.Run("show-option", "-gv", test.option)
		if err != nil {
			t.Errorf("show-option -gv %s: %v", test.option, err)
			continue
		}
		got := strings.TrimSpace(output)
		if got != test.want {
			t.Errorf("global tmux option %s = %q, want %q", test.option, got, test.want)
		}
	}

	// Verify per-session status-left contains the principal name.
	output, err := tmuxServer.Run("show-option", "-t", sessionName, "-v", "status-left")
	if err != nil {
		t.Errorf("show-option status-left: %v", err)
	} else {
		got := strings.TrimSpace(output)
		if !strings.Contains(got, localpart) {
			t.Errorf("status-left = %q, should contain principal %q", got, localpart)
		}
	}
}

func TestTmuxSessionMultipleSessions(t *testing.T) {
	requireTmux(t)

	runDir, tmuxServer := newTestRunDir(t)

	launcher := &Launcher{
		runDir:     runDir,
		tmuxServer: tmuxServer,
		sandboxes:  make(map[string]*managedSandbox),
		logger:     slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	localparts := []string{"test/a", "test/b", "test/c"}

	// Create all sessions.
	for _, localpart := range localparts {
		if err := launcher.createTmuxSession(localpart); err != nil {
			t.Fatalf("createTmuxSession(%q): %v", localpart, err)
		}
	}

	// Verify all sessions exist.
	for _, localpart := range localparts {
		sessionName := tmuxSessionName(localpart)
		if !tmuxServer.HasSession(sessionName) {
			t.Errorf("session %q should exist", sessionName)
		}
	}

	// Destroy one session and verify the others survive.
	launcher.destroyTmuxSession("test/b")
	if tmuxServer.HasSession(tmuxSessionName("test/b")) {
		t.Error("session bureau/test/b should be gone after destroy")
	}
	if !tmuxServer.HasSession(tmuxSessionName("test/a")) {
		t.Error("session bureau/test/a should still exist")
	}
	if !tmuxServer.HasSession(tmuxSessionName("test/c")) {
		t.Error("session bureau/test/c should still exist")
	}
}

func TestTmuxSessionWithProxyLifecycle(t *testing.T) {
	requireTmux(t)

	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)
	runDir, tmuxServer := newTestRunDir(t)
	launcher.runDir = runDir
	launcher.tmuxServer = tmuxServer

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN":   "syt_fake_test_token",
		"MATRIX_USER_ID": "@test/tmuxproxy:bureau.local",
	})

	// Create a sandbox — this should spawn a proxy AND create a tmux session.
	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/tmuxproxy",
		EncryptedCredentials: ciphertext,
	})
	if !response.OK {
		t.Fatalf("create-sandbox failed: %s", response.Error)
	}

	sessionName := tmuxSessionName("test/tmuxproxy")
	if !tmuxServer.HasSession(sessionName) {
		t.Fatalf("tmux session %q should exist after sandbox creation", sessionName)
	}

	// Verify the proxy is also running (agent socket responds).
	socketPath := principal.RunDirSocketPath(launcher.runDir, "test/tmuxproxy")
	agentClient := unixHTTPClient(socketPath)
	healthResponse, err := agentClient.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", healthResponse.StatusCode)
	}

	// Destroy the sandbox — should kill both proxy and tmux session.
	destroyResponse := launcher.handleDestroySandbox(context.Background(), &IPCRequest{
		Action:    "destroy-sandbox",
		Principal: "test/tmuxproxy",
	})
	if !destroyResponse.OK {
		t.Fatalf("destroy-sandbox failed: %s", destroyResponse.Error)
	}

	if tmuxServer.HasSession(sessionName) {
		t.Errorf("tmux session %q should not exist after sandbox destruction", sessionName)
	}
	if _, exists := launcher.sandboxes["test/tmuxproxy"]; exists {
		t.Error("sandbox should be removed from map after destroy")
	}
}

func TestHandleUpdatePayload_Validation(t *testing.T) {
	launcher := &Launcher{
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("missing principal", func(t *testing.T) {
		response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
			Action: "update-payload",
		})
		if response.OK {
			t.Error("expected error for missing principal")
		}
		if !strings.Contains(response.Error, "principal is required") {
			t.Errorf("error = %q, want substring 'principal is required'", response.Error)
		}
	})

	t.Run("invalid principal", func(t *testing.T) {
		response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
			Action:    "update-payload",
			Principal: "../escape",
		})
		if response.OK {
			t.Error("expected error for invalid principal")
		}
		if !strings.Contains(response.Error, "invalid principal") {
			t.Errorf("error = %q, want substring 'invalid principal'", response.Error)
		}
	})

	t.Run("no sandbox running", func(t *testing.T) {
		response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
			Action:    "update-payload",
			Principal: "test/missing",
			Payload:   map[string]any{"key": "value"},
		})
		if response.OK {
			t.Error("expected error for non-existent sandbox")
		}
		if !strings.Contains(response.Error, "no sandbox running") {
			t.Errorf("error = %q, want substring 'no sandbox running'", response.Error)
		}
	})

	t.Run("sandbox already exited", func(t *testing.T) {
		done := make(chan struct{})
		close(done) // simulate exited process
		launcher.sandboxes["test/exited"] = &managedSandbox{
			localpart: "test/exited",
			configDir: t.TempDir(),
			done:      done,
		}
		defer delete(launcher.sandboxes, "test/exited")

		response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
			Action:    "update-payload",
			Principal: "test/exited",
			Payload:   map[string]any{"key": "value"},
		})
		if response.OK {
			t.Error("expected error for exited sandbox")
		}
		if !strings.Contains(response.Error, "already exited") {
			t.Errorf("error = %q, want substring 'already exited'", response.Error)
		}
	})

	t.Run("nil payload", func(t *testing.T) {
		done := make(chan struct{})
		launcher.sandboxes["test/nopayload"] = &managedSandbox{
			localpart: "test/nopayload",
			configDir: t.TempDir(),
			done:      done,
		}
		defer func() {
			delete(launcher.sandboxes, "test/nopayload")
			close(done)
		}()

		response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
			Action:    "update-payload",
			Principal: "test/nopayload",
		})
		if response.OK {
			t.Error("expected error for nil payload")
		}
		if !strings.Contains(response.Error, "payload is required") {
			t.Errorf("error = %q, want substring 'payload is required'", response.Error)
		}
	})
}

func TestHandleUpdatePayload_AtomicWrite(t *testing.T) {
	configDir := t.TempDir()

	// Write an initial payload file so we can verify it gets replaced.
	initialPayload := `{"initial":"data"}`
	payloadPath := filepath.Join(configDir, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(initialPayload), 0644); err != nil {
		t.Fatalf("writing initial payload: %v", err)
	}

	done := make(chan struct{})
	launcher := &Launcher{
		sandboxes: map[string]*managedSandbox{
			"test/update": {
				localpart: "test/update",
				configDir: configDir,
				done:      done,
			},
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	defer close(done)

	newPayload := map[string]any{
		"model":       "claude-4",
		"temperature": 0.7,
		"nested":      map[string]any{"key": "value"},
	}

	response := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
		Action:    "update-payload",
		Principal: "test/update",
		Payload:   newPayload,
	})
	if !response.OK {
		t.Fatalf("handleUpdatePayload failed: %s", response.Error)
	}

	// Verify the file was written with new content.
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("reading updated payload: %v", err)
	}

	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("parsing updated payload: %v", err)
	}

	if written["model"] != "claude-4" {
		t.Errorf("payload model = %v, want %q", written["model"], "claude-4")
	}
	if written["temperature"] != 0.7 {
		t.Errorf("payload temperature = %v, want 0.7", written["temperature"])
	}
	nested, ok := written["nested"].(map[string]any)
	if !ok {
		t.Fatal("payload nested is not a map")
	}
	if nested["key"] != "value" {
		t.Errorf("payload nested.key = %v, want %q", nested["key"], "value")
	}

	// Verify no temp file is left behind.
	tempPath := payloadPath + ".tmp"
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("temp file should not exist after successful update, stat error: %v", err)
	}
}

func TestHandleUpdatePayload_WithProxy(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN":   "syt_fake_test_token",
		"MATRIX_USER_ID": "@test/payload:bureau.local",
	})

	// Create a sandbox to get a real configDir and running process.
	createResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/payload",
		EncryptedCredentials: ciphertext,
	})
	if !createResponse.OK {
		t.Fatalf("create-sandbox failed: %s", createResponse.Error)
	}

	// Update the payload.
	updateResponse := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
		Action:    "update-payload",
		Principal: "test/payload",
		Payload: map[string]any{
			"prompt":     "You are a helpful assistant.",
			"max_tokens": 4096,
		},
	})
	if !updateResponse.OK {
		t.Fatalf("update-payload failed: %s", updateResponse.Error)
	}

	// Read back the payload file from the sandbox's configDir.
	sandbox := launcher.sandboxes["test/payload"]
	payloadPath := filepath.Join(sandbox.configDir, "payload.json")
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("reading payload file: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}

	if payload["prompt"] != "You are a helpful assistant." {
		t.Errorf("payload prompt = %v, want %q", payload["prompt"], "You are a helpful assistant.")
	}
	// JSON numbers are float64.
	if payload["max_tokens"] != float64(4096) {
		t.Errorf("payload max_tokens = %v, want 4096", payload["max_tokens"])
	}

	// Update again to verify overwrites work.
	update2 := launcher.handleUpdatePayload(context.Background(), &IPCRequest{
		Action:    "update-payload",
		Principal: "test/payload",
		Payload:   map[string]any{"replaced": true},
	})
	if !update2.OK {
		t.Fatalf("second update-payload failed: %s", update2.Error)
	}

	data2, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("reading payload after second update: %v", err)
	}
	var payload2 map[string]any
	if err := json.Unmarshal(data2, &payload2); err != nil {
		t.Fatalf("parsing second payload: %v", err)
	}
	if payload2["replaced"] != true {
		t.Errorf("second update payload = %v, want {replaced: true}", payload2)
	}
	// The old keys should be gone (full replacement, not merge).
	if _, exists := payload2["prompt"]; exists {
		t.Error("old payload key 'prompt' should not persist after full replacement")
	}
}

func TestTmuxSessionCleanedUpOnCrashedSandbox(t *testing.T) {
	requireTmux(t)

	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)
	runDir, tmuxServer := newTestRunDir(t)
	launcher.runDir = runDir
	launcher.tmuxServer = tmuxServer

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create a sandbox.
	response := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/crash",
		EncryptedCredentials: ciphertext,
	})
	if !response.OK {
		t.Fatalf("create-sandbox: %s", response.Error)
	}

	sessionName := tmuxSessionName("test/crash")
	if !tmuxServer.HasSession(sessionName) {
		t.Fatal("tmux session should exist after creation")
	}

	// Kill the tmux session to simulate a sandbox crash. The session
	// watcher detects the session is gone and closes the done channel.
	sandbox := launcher.sandboxes["test/crash"]
	launcher.destroyTmuxSession("test/crash")

	select {
	case <-sandbox.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session watcher to close done channel")
	}

	// Re-creating the sandbox should clean up the old sandbox
	// (via cleanupSandbox) and create a new one.
	response2 := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/crash",
		EncryptedCredentials: ciphertext,
	})
	if !response2.OK {
		t.Fatalf("recreate after crash: %s", response2.Error)
	}

	// The new tmux session should exist.
	if !tmuxServer.HasSession(sessionName) {
		t.Error("tmux session should exist after recreate")
	}
}

func TestHandleUpdateProxyBinary_Validation(t *testing.T) {
	launcher := &Launcher{
		proxyBinaryPath: "/original/path",
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	t.Run("missing binary path", func(t *testing.T) {
		response := launcher.handleUpdateProxyBinary(context.Background(), &IPCRequest{
			Action: "update-proxy-binary",
		})
		if response.OK {
			t.Error("expected error for missing binary_path")
		}
		if !strings.Contains(response.Error, "binary_path is required") {
			t.Errorf("error = %q, want substring 'binary_path is required'", response.Error)
		}
		if launcher.proxyBinaryPath != "/original/path" {
			t.Errorf("proxyBinaryPath changed to %q on error", launcher.proxyBinaryPath)
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		response := launcher.handleUpdateProxyBinary(context.Background(), &IPCRequest{
			Action:     "update-proxy-binary",
			BinaryPath: "/nonexistent/binary",
		})
		if response.OK {
			t.Error("expected error for nonexistent path")
		}
		if launcher.proxyBinaryPath != "/original/path" {
			t.Errorf("proxyBinaryPath changed to %q on error", launcher.proxyBinaryPath)
		}
	})

	t.Run("directory instead of file", func(t *testing.T) {
		dirPath := t.TempDir()
		response := launcher.handleUpdateProxyBinary(context.Background(), &IPCRequest{
			Action:     "update-proxy-binary",
			BinaryPath: dirPath,
		})
		if response.OK {
			t.Error("expected error for directory path")
		}
		if !strings.Contains(response.Error, "not a regular file") {
			t.Errorf("error = %q, want substring 'not a regular file'", response.Error)
		}
	})

	t.Run("non-executable file", func(t *testing.T) {
		nonExecPath := filepath.Join(t.TempDir(), "not-executable")
		if err := os.WriteFile(nonExecPath, []byte("#!/bin/sh\n"), 0644); err != nil {
			t.Fatalf("writing test file: %v", err)
		}
		response := launcher.handleUpdateProxyBinary(context.Background(), &IPCRequest{
			Action:     "update-proxy-binary",
			BinaryPath: nonExecPath,
		})
		if response.OK {
			t.Error("expected error for non-executable file")
		}
		if !strings.Contains(response.Error, "not executable") {
			t.Errorf("error = %q, want substring 'not executable'", response.Error)
		}
	})

	t.Run("valid executable", func(t *testing.T) {
		execPath := filepath.Join(t.TempDir(), "proxy-binary")
		if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatalf("writing test file: %v", err)
		}
		response := launcher.handleUpdateProxyBinary(context.Background(), &IPCRequest{
			Action:     "update-proxy-binary",
			BinaryPath: execPath,
		})
		if !response.OK {
			t.Fatalf("handleUpdateProxyBinary failed: %s", response.Error)
		}
		if launcher.proxyBinaryPath != execPath {
			t.Errorf("proxyBinaryPath = %q, want %q", launcher.proxyBinaryPath, execPath)
		}
	})
}

func TestHandleUpdateProxyBinary_ViaIPC(t *testing.T) {
	// Test that the "update-proxy-binary" action is correctly routed through
	// the IPC connection handler, not just the direct method call.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")

	execPath := filepath.Join(t.TempDir(), "new-proxy")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("writing test binary: %v", err)
	}

	launcher := &Launcher{
		proxyBinaryPath: "/old/proxy",
		sandboxes:       make(map[string]*managedSandbox),
		logger:          slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		launcher.handleConnection(context.Background(), conn)
	}()

	// Send the IPC request.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	request := IPCRequest{
		Action:     "update-proxy-binary",
		BinaryPath: execPath,
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("encoding request: %v", err)
	}

	var response IPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if !response.OK {
		t.Fatalf("IPC update-proxy-binary failed: %s", response.Error)
	}
	if launcher.proxyBinaryPath != execPath {
		t.Errorf("proxyBinaryPath = %q, want %q", launcher.proxyBinaryPath, execPath)
	}
}

// sendIPCRequest is a test helper that opens a connection to the launcher
// socket, sends a request, reads the response, and returns it. For
// short-lived IPC actions (not wait-sandbox).
func sendIPCRequest(t *testing.T, socketPath string, request IPCRequest) IPCResponse {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial launcher: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var response IPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func TestWaitSandbox_Validation(t *testing.T) {
	t.Parallel()

	launcher := &Launcher{
		sandboxes: make(map[string]*managedSandbox),
		logger:    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	tests := []struct {
		name      string
		request   IPCRequest
		expectErr string
	}{
		{
			name:      "missing principal",
			request:   IPCRequest{Action: "wait-sandbox"},
			expectErr: "principal is required",
		},
		{
			name:      "invalid principal",
			request:   IPCRequest{Action: "wait-sandbox", Principal: "../escape"},
			expectErr: "invalid principal",
		},
		{
			name:      "unknown principal",
			request:   IPCRequest{Action: "wait-sandbox", Principal: "test/nonexistent"},
			expectErr: "no sandbox running",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseChan := make(chan IPCResponse, 1)
			go func() {
				conn, dialErr := net.Dial("unix", socketPath)
				if dialErr != nil {
					t.Errorf("Dial: %v", dialErr)
					return
				}
				defer conn.Close()

				if encodeErr := json.NewEncoder(conn).Encode(test.request); encodeErr != nil {
					t.Errorf("Encode: %v", encodeErr)
					return
				}
				var response IPCResponse
				if decodeErr := json.NewDecoder(conn).Decode(&response); decodeErr != nil {
					t.Errorf("Decode: %v", decodeErr)
					return
				}
				responseChan <- response
			}()

			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				t.Fatalf("Accept: %v", acceptErr)
			}
			launcher.handleConnection(context.Background(), conn)

			response := <-responseChan
			if response.OK {
				t.Error("expected error response")
			}
			if !strings.Contains(response.Error, test.expectErr) {
				t.Errorf("error = %q, want substring %q", response.Error, test.expectErr)
			}
		})
	}
}

func TestWaitSandbox_ProcessExit(t *testing.T) {
	t.Parallel()
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create a sandbox.
	createResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/waitnormal",
		EncryptedCredentials: ciphertext,
	})
	if !createResponse.OK {
		t.Fatalf("create-sandbox: %s", createResponse.Error)
	}

	// Set up the IPC socket for wait-sandbox.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	// Start wait-sandbox in a goroutine (it blocks until the process exits).
	responseChan := make(chan IPCResponse, 1)
	go func() {
		conn, dialErr := net.Dial("unix", socketPath)
		if dialErr != nil {
			t.Errorf("Dial: %v", dialErr)
			return
		}
		defer conn.Close()

		if encodeErr := json.NewEncoder(conn).Encode(IPCRequest{
			Action:    "wait-sandbox",
			Principal: "test/waitnormal",
		}); encodeErr != nil {
			t.Errorf("Encode: %v", encodeErr)
			return
		}

		var response IPCResponse
		if decodeErr := json.NewDecoder(conn).Decode(&response); decodeErr != nil {
			t.Errorf("Decode: %v", decodeErr)
			return
		}
		responseChan <- response
	}()

	// Accept and handle the wait-sandbox request.
	conn, acceptErr := listener.Accept()
	if acceptErr != nil {
		t.Fatalf("Accept: %v", acceptErr)
	}
	handleDone := make(chan struct{})
	go func() {
		launcher.handleConnection(context.Background(), conn)
		close(handleDone)
	}()

	// Verify wait-sandbox is actually blocking: no response yet.
	select {
	case <-responseChan:
		t.Fatal("wait-sandbox returned before process exit")
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocking.
	}

	// Kill the tmux session to end the sandbox. The session watcher
	// detects the session is gone and closes the done channel, which
	// unblocks wait-sandbox.
	launcher.destroyTmuxSession("test/waitnormal")

	// wait-sandbox should now return.
	select {
	case response := <-responseChan:
		if !response.OK {
			t.Fatalf("wait-sandbox failed: %s", response.Error)
		}
		if response.ExitCode == nil {
			t.Fatal("ExitCode should be set")
		}
		// No exit-code file was written (bare shell, no sandbox script),
		// so the session watcher reports -1.
		if *response.ExitCode != -1 {
			t.Errorf("expected exit code -1 (no exit-code file), got %d", *response.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait-sandbox did not return after sandbox exit")
	}

	<-handleDone
}

func TestWaitSandbox_AlreadyExited(t *testing.T) {
	t.Parallel()
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create a sandbox and immediately destroy the tmux session.
	createResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/waitalready",
		EncryptedCredentials: ciphertext,
	})
	if !createResponse.OK {
		t.Fatalf("create-sandbox: %s", createResponse.Error)
	}

	sandbox := launcher.sandboxes["test/waitalready"]
	launcher.destroyTmuxSession("test/waitalready")

	select {
	case <-sandbox.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session watcher to close done channel")
	}

	// Now call wait-sandbox — should return immediately since the
	// sandbox has already exited.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	responseChan := make(chan IPCResponse, 1)
	go func() {
		conn, dialErr := net.Dial("unix", socketPath)
		if dialErr != nil {
			t.Errorf("Dial: %v", dialErr)
			return
		}
		defer conn.Close()

		if encodeErr := json.NewEncoder(conn).Encode(IPCRequest{
			Action:    "wait-sandbox",
			Principal: "test/waitalready",
		}); encodeErr != nil {
			t.Errorf("Encode: %v", encodeErr)
			return
		}

		var response IPCResponse
		if decodeErr := json.NewDecoder(conn).Decode(&response); decodeErr != nil {
			t.Errorf("Decode: %v", decodeErr)
			return
		}
		responseChan <- response
	}()

	conn, acceptErr := listener.Accept()
	if acceptErr != nil {
		t.Fatalf("Accept: %v", acceptErr)
	}
	launcher.handleConnection(context.Background(), conn)

	select {
	case response := <-responseChan:
		if !response.OK {
			t.Fatalf("wait-sandbox failed: %s", response.Error)
		}
		if response.ExitCode == nil {
			t.Fatal("ExitCode should be set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait-sandbox did not return for already-exited process")
	}
}

func TestWaitSandbox_DoesNotBlockOtherRequests(t *testing.T) {
	t.Parallel()
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN": "syt_test",
	})

	// Create a sandbox that stays alive.
	createResponse := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/waitblock",
		EncryptedCredentials: ciphertext,
	})
	if !createResponse.OK {
		t.Fatalf("create-sandbox: %s", createResponse.Error)
	}

	// Start a real IPC server so we can test concurrent requests.
	socketDir := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDir, "launcher.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Accept connections in a loop (like the real launcher).
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go launcher.handleConnection(ctx, conn)
		}
	}()

	// Send a wait-sandbox request that will block (process is still alive).
	waitConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial wait: %v", err)
	}
	defer waitConn.Close()

	if encodeErr := json.NewEncoder(waitConn).Encode(IPCRequest{
		Action:    "wait-sandbox",
		Principal: "test/waitblock",
	}); encodeErr != nil {
		t.Fatalf("Encode wait: %v", encodeErr)
	}

	// Give the handler a moment to start blocking.
	time.Sleep(50 * time.Millisecond)

	// While wait-sandbox is blocking, send a status request — it should
	// succeed without being blocked by the wait.
	statusResponse := sendIPCRequest(t, socketPath, IPCRequest{Action: "status"})
	if !statusResponse.OK {
		t.Fatalf("status request blocked by wait-sandbox: %s", statusResponse.Error)
	}
}

func TestListSandboxes(t *testing.T) {
	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)

	// Empty launcher should return an empty list.
	emptyResponse := launcher.handleListSandboxes()
	if !emptyResponse.OK {
		t.Fatalf("list-sandboxes on empty launcher failed: %s", emptyResponse.Error)
	}
	if len(emptyResponse.Sandboxes) != 0 {
		t.Errorf("expected 0 sandboxes, got %d", len(emptyResponse.Sandboxes))
	}

	// Create two sandboxes.
	ciphertext := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN":   "syt_fake_test_token",
		"MATRIX_USER_ID": "@test/list-a:bureau.local",
	})

	responseA := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/list-a",
		EncryptedCredentials: ciphertext,
	})
	if !responseA.OK {
		t.Fatalf("create sandbox A: %s", responseA.Error)
	}

	ciphertextB := encryptCredentials(t, launcher.keypair, map[string]string{
		"MATRIX_TOKEN":   "syt_fake_test_token_b",
		"MATRIX_USER_ID": "@test/list-b:bureau.local",
	})
	responseB := launcher.handleCreateSandbox(context.Background(), &IPCRequest{
		Action:               "create-sandbox",
		Principal:            "test/list-b",
		EncryptedCredentials: ciphertextB,
	})
	if !responseB.OK {
		t.Fatalf("create sandbox B: %s", responseB.Error)
	}

	// List should return both sandboxes.
	listResponse := launcher.handleListSandboxes()
	if !listResponse.OK {
		t.Fatalf("list-sandboxes failed: %s", listResponse.Error)
	}
	if len(listResponse.Sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(listResponse.Sandboxes))
	}

	// Verify both are present with valid PIDs.
	found := make(map[string]int)
	for _, entry := range listResponse.Sandboxes {
		found[entry.Localpart] = entry.ProxyPID
	}
	if pid, ok := found["test/list-a"]; !ok {
		t.Error("test/list-a not found in list-sandboxes response")
	} else if pid == 0 {
		t.Error("test/list-a has zero proxy PID")
	}
	if pid, ok := found["test/list-b"]; !ok {
		t.Error("test/list-b not found in list-sandboxes response")
	} else if pid == 0 {
		t.Error("test/list-b has zero proxy PID")
	}

	// Destroy one sandbox.
	destroyResponse := launcher.handleDestroySandbox(context.Background(), &IPCRequest{
		Action:    "destroy-sandbox",
		Principal: "test/list-a",
	})
	if !destroyResponse.OK {
		t.Fatalf("destroy sandbox A: %s", destroyResponse.Error)
	}

	// List should return only the surviving sandbox.
	afterDestroyResponse := launcher.handleListSandboxes()
	if !afterDestroyResponse.OK {
		t.Fatalf("list-sandboxes after destroy failed: %s", afterDestroyResponse.Error)
	}
	if len(afterDestroyResponse.Sandboxes) != 1 {
		t.Fatalf("expected 1 sandbox after destroy, got %d", len(afterDestroyResponse.Sandboxes))
	}
	if afterDestroyResponse.Sandboxes[0].Localpart != "test/list-b" {
		t.Errorf("surviving sandbox = %q, want test/list-b", afterDestroyResponse.Sandboxes[0].Localpart)
	}
}
