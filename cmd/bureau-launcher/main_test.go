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

	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

func TestDerivePassword(t *testing.T) {
	// Deterministic: same inputs produce same output.
	password1, err := derivePassword("token123", "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password1.Close()
	password2, err := derivePassword("token123", "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password2.Close()
	if !password1.Equal(password2) {
		t.Errorf("derivePassword not deterministic: %q != %q", password1.String(), password2.String())
	}

	// Different tokens produce different passwords.
	password3, err := derivePassword("different-token", "machine/workstation")
	if err != nil {
		t.Fatalf("derivePassword: %v", err)
	}
	defer password3.Close()
	if password1.Equal(password3) {
		t.Errorf("different tokens should produce different passwords")
	}

	// Different machine names produce different passwords.
	password4, err := derivePassword("token123", "machine/other")
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
				"GITHUB_PAT":       "ghp_test",
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
			"MATRIX_TOKEN":         "syt_test_token",
			"MATRIX_USER_ID":       "@test/echo:bureau.local",
			"OPENAI_API_KEY":       "sk-test",
			"GITHUB_PAT":           "ghp_test",
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
		stateDir:        filepath.Join(tempDir, "state"),
		proxyBinaryPath: proxyBinaryPath,
		socketBasePath:  filepath.Join(tempDir, "principal") + "/",
		adminBasePath:   filepath.Join(tempDir, "admin") + "/",
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
	socketPath := launcher.socketBasePath + "test/echo.sock"
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
	adminSocketPath := launcher.adminBasePath + "test/echo.sock"
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
	socketPath := launcher.socketBasePath + "test/nocreds.sock"
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

	// Kill the proxy process to simulate a crash.
	sandbox := launcher.sandboxes["test/crash"]
	sandbox.process.Kill()
	<-sandbox.done // wait for the process to exit

	// Attempting to create the same sandbox should succeed because the
	// process has exited (the launcher detects this and cleans up).
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

// newTestTmuxSocket returns a path for a test-isolated tmux server socket
// in a temporary directory, and registers cleanup to kill the tmux server
// when the test finishes. All tmux commands in tests MUST use this socket
// via -S to avoid interfering with the user's or agent's tmux sessions.
func newTestTmuxSocket(t *testing.T) string {
	t.Helper()
	socketPath := filepath.Join(testutil.SocketDir(t), "tmux.sock")
	t.Cleanup(func() {
		exec.Command("tmux", "-S", socketPath, "kill-server").Run()
	})
	return socketPath
}

// tmuxSessionExists checks whether a tmux session with the given name exists
// on the specified tmux server socket.
func tmuxSessionExists(socketPath, sessionName string) bool {
	cmd := exec.Command("tmux", "-S", socketPath, "has-session", "-t", sessionName)
	return cmd.Run() == nil
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

	tmuxSocket := newTestTmuxSocket(t)

	launcher := &Launcher{
		tmuxServerSocket: tmuxSocket,
		sandboxes:        make(map[string]*managedSandbox),
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	localpart := "test/tmux"
	sessionName := tmuxSessionName(localpart)

	// Create the session.
	if err := launcher.createTmuxSession(localpart); err != nil {
		t.Fatalf("createTmuxSession: %v", err)
	}

	// Verify the session exists on the Bureau tmux server.
	if !tmuxSessionExists(tmuxSocket, sessionName) {
		t.Fatalf("tmux session %q should exist after creation", sessionName)
	}

	// Destroy the session.
	launcher.destroyTmuxSession(localpart)

	// Verify the session is gone.
	if tmuxSessionExists(tmuxSocket, sessionName) {
		t.Errorf("tmux session %q should not exist after destruction", sessionName)
	}
}

func TestTmuxSessionConfiguration(t *testing.T) {
	requireTmux(t)

	tmuxSocket := newTestTmuxSocket(t)

	launcher := &Launcher{
		tmuxServerSocket: tmuxSocket,
		sandboxes:        make(map[string]*managedSandbox),
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
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
		cmd := exec.Command("tmux", "-S", tmuxSocket,
			"show-option", "-gv", test.option)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("show-option -gv %s: %v (output: %s)", test.option, err, output)
			continue
		}
		got := strings.TrimSpace(string(output))
		if got != test.want {
			t.Errorf("global tmux option %s = %q, want %q", test.option, got, test.want)
		}
	}

	// Verify per-session status-left contains the principal name.
	cmd := exec.Command("tmux", "-S", tmuxSocket,
		"show-option", "-t", sessionName, "-v", "status-left")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("show-option status-left: %v (output: %s)", err, output)
	} else {
		got := strings.TrimSpace(string(output))
		if !strings.Contains(got, localpart) {
			t.Errorf("status-left = %q, should contain principal %q", got, localpart)
		}
	}
}

func TestTmuxSessionSkippedWhenSocketEmpty(t *testing.T) {
	// When tmuxServerSocket is empty, tmux operations are no-ops.
	launcher := &Launcher{
		tmuxServerSocket: "",
		sandboxes:        make(map[string]*managedSandbox),
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// createTmuxSession should return nil (no error, no action).
	if err := launcher.createTmuxSession("test/noop"); err != nil {
		t.Errorf("createTmuxSession with empty socket should succeed, got: %v", err)
	}

	// destroyTmuxSession should not panic or error.
	launcher.destroyTmuxSession("test/noop")
}

func TestTmuxSessionMultipleSessions(t *testing.T) {
	requireTmux(t)

	tmuxSocket := newTestTmuxSocket(t)

	launcher := &Launcher{
		tmuxServerSocket: tmuxSocket,
		sandboxes:        make(map[string]*managedSandbox),
		logger:           slog.New(slog.NewJSONHandler(os.Stderr, nil)),
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
		if !tmuxSessionExists(tmuxSocket, sessionName) {
			t.Errorf("session %q should exist", sessionName)
		}
	}

	// Destroy one session and verify the others survive.
	launcher.destroyTmuxSession("test/b")
	if tmuxSessionExists(tmuxSocket, tmuxSessionName("test/b")) {
		t.Error("session bureau/test/b should be gone after destroy")
	}
	if !tmuxSessionExists(tmuxSocket, tmuxSessionName("test/a")) {
		t.Error("session bureau/test/a should still exist")
	}
	if !tmuxSessionExists(tmuxSocket, tmuxSessionName("test/c")) {
		t.Error("session bureau/test/c should still exist")
	}
}

func TestTmuxSessionWithProxyLifecycle(t *testing.T) {
	requireTmux(t)

	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)
	launcher.tmuxServerSocket = newTestTmuxSocket(t)

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
	if !tmuxSessionExists(launcher.tmuxServerSocket, sessionName) {
		t.Fatalf("tmux session %q should exist after sandbox creation", sessionName)
	}

	// Verify the proxy is also running (agent socket responds).
	socketPath := launcher.socketBasePath + "test/tmuxproxy.sock"
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

	if tmuxSessionExists(launcher.tmuxServerSocket, sessionName) {
		t.Errorf("tmux session %q should not exist after sandbox destruction", sessionName)
	}
	if _, exists := launcher.sandboxes["test/tmuxproxy"]; exists {
		t.Error("sandbox should be removed from map after destroy")
	}
}

func TestTmuxSessionCleanedUpOnCrashedProxy(t *testing.T) {
	requireTmux(t)

	proxyBinary := buildProxyBinary(t)
	launcher := newTestLauncher(t, proxyBinary)
	launcher.tmuxServerSocket = newTestTmuxSocket(t)

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
	if !tmuxSessionExists(launcher.tmuxServerSocket, sessionName) {
		t.Fatal("tmux session should exist after creation")
	}

	// Kill the proxy to simulate a crash.
	sandbox := launcher.sandboxes["test/crash"]
	sandbox.process.Kill()
	<-sandbox.done

	// Re-creating the sandbox should clean up the old tmux session
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
	if !tmuxSessionExists(launcher.tmuxServerSocket, sessionName) {
		t.Error("tmux session should exist after recreate")
	}
}
