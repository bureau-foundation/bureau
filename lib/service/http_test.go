// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- VerifyWebhookHMAC ---

func TestVerifyWebhookHMAC(t *testing.T) {
	secret := []byte("webhook-secret-for-testing")
	body := []byte(`{"action":"opened","number":42}`)

	// Compute valid signature.
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	validHex := hex.EncodeToString(mac.Sum(nil))
	validPrefixed := "sha256=" + validHex

	t.Run("valid_with_prefix", func(t *testing.T) {
		if err := VerifyWebhookHMAC(secret, body, validPrefixed); err != nil {
			t.Errorf("VerifyWebhookHMAC() = %v, want nil", err)
		}
	})

	t.Run("valid_without_prefix", func(t *testing.T) {
		if err := VerifyWebhookHMAC(secret, body, validHex); err != nil {
			t.Errorf("VerifyWebhookHMAC() = %v, want nil", err)
		}
	})

	t.Run("wrong_signature", func(t *testing.T) {
		wrong := "sha256=" + strings.Repeat("ab", 32)
		err := VerifyWebhookHMAC(secret, body, wrong)
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "signature mismatch") {
			t.Errorf("error = %q, want 'signature mismatch'", err)
		}
	})

	t.Run("wrong_secret", func(t *testing.T) {
		err := VerifyWebhookHMAC([]byte("wrong-secret"), body, validPrefixed)
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "signature mismatch") {
			t.Errorf("error = %q, want 'signature mismatch'", err)
		}
	})

	t.Run("different_body", func(t *testing.T) {
		err := VerifyWebhookHMAC(secret, []byte("different body"), validPrefixed)
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
	})

	t.Run("empty_secret", func(t *testing.T) {
		err := VerifyWebhookHMAC(nil, body, validPrefixed)
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "secret is empty") {
			t.Errorf("error = %q, want 'secret is empty'", err)
		}
	})

	t.Run("empty_body", func(t *testing.T) {
		err := VerifyWebhookHMAC(secret, nil, validPrefixed)
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "body is empty") {
			t.Errorf("error = %q, want 'body is empty'", err)
		}
	})

	t.Run("empty_signature", func(t *testing.T) {
		err := VerifyWebhookHMAC(secret, body, "")
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "signature is empty") {
			t.Errorf("error = %q, want 'signature is empty'", err)
		}
	})

	t.Run("invalid_hex", func(t *testing.T) {
		err := VerifyWebhookHMAC(secret, body, "sha256=not-valid-hex")
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "invalid hex") {
			t.Errorf("error = %q, want 'invalid hex'", err)
		}
	})

	t.Run("truncated_signature", func(t *testing.T) {
		// Only first 16 bytes of the signature — wrong length.
		err := VerifyWebhookHMAC(secret, body, "sha256="+validHex[:32])
		if err == nil {
			t.Fatal("VerifyWebhookHMAC() = nil, want error")
		}
		if !strings.Contains(err.Error(), "signature mismatch") {
			t.Errorf("error = %q, want 'signature mismatch'", err)
		}
	})
}

// --- HTTPServer lifecycle ---

func TestHTTPServerLifecycle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		fmt.Fprintf(writer, "ok")
	})

	server := NewHTTPServer(HTTPServerConfig{
		Address:         "127.0.0.1:0", // OS-assigned port
		Handler:         handler,
		ShutdownTimeout: 2 * time.Second,
		Logger:          logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx)
	}()

	// Wait for the server to be ready. t.Context() is cancelled
	// when the test deadline passes, so no wall-clock timeout needed.
	select {
	case <-server.Ready():
	case <-t.Context().Done():
		t.Fatal("server did not become ready before test deadline")
	}

	// Verify we can reach the server.
	address := server.Addr().String()
	response, err := http.Get("http://" + address + "/test")
	if err != nil {
		t.Fatalf("GET /test: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("GET /test status = %d, want 200", response.StatusCode)
	}
	responseBody, _ := io.ReadAll(response.Body)
	if string(responseBody) != "ok" {
		t.Errorf("GET /test body = %q, want %q", responseBody, "ok")
	}

	// Cancel the context to trigger shutdown.
	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("Serve() = %v, want nil", err)
		}
	case <-t.Context().Done():
		t.Fatal("server did not shut down before test deadline")
	}
}

func TestHTTPServerPanicsOnMissingConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	tests := []struct {
		name   string
		config HTTPServerConfig
	}{
		{
			name:   "missing_address",
			config: HTTPServerConfig{Handler: handler, Logger: logger},
		},
		{
			name:   "missing_handler",
			config: HTTPServerConfig{Address: ":0", Logger: logger},
		},
		{
			name:   "missing_logger",
			config: HTTPServerConfig{Address: ":0", Handler: handler},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("NewHTTPServer did not panic")
				}
			}()
			NewHTTPServer(tt.config)
		})
	}
}
