// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTPServer serves HTTP on a TCP listener. Used by forge connectors
// for webhook ingestion — webhooks arrive as HTTP requests from
// external forges (GitHub, Forgejo, GitLab). The server manages
// listener lifecycle and graceful shutdown; the caller provides the
// http.Handler (routing, signature verification, payload processing).
//
// Follows the same lifecycle pattern as SocketServer: Serve(ctx)
// blocks until the context is cancelled and active requests drain.
type HTTPServer struct {
	address string
	handler http.Handler
	logger  *slog.Logger

	// shutdownTimeout is the maximum time to wait for active
	// requests to complete after the context is cancelled.
	shutdownTimeout time.Duration

	// ready is closed after the listener is bound and the server
	// is accepting connections.
	ready chan struct{}

	// addr is the resolved listen address, available after the
	// server starts accepting connections (after ready is closed).
	addr net.Addr
}

// HTTPServerConfig configures an HTTPServer.
type HTTPServerConfig struct {
	// Address is the TCP listen address (e.g., ":8080",
	// "127.0.0.1:9000"). Required.
	Address string

	// Handler is the HTTP handler for incoming requests. Required.
	Handler http.Handler

	// ShutdownTimeout is the maximum time to wait for in-flight
	// requests to complete during graceful shutdown. Defaults to
	// 10 seconds if zero.
	ShutdownTimeout time.Duration

	// Logger is the structured logger. Required.
	Logger *slog.Logger
}

// NewHTTPServer creates a server that will listen on the configured
// TCP address. Call Serve to start accepting connections.
func NewHTTPServer(config HTTPServerConfig) *HTTPServer {
	if config.Address == "" {
		panic("service.HTTPServer: Address is required")
	}
	if config.Handler == nil {
		panic("service.HTTPServer: Handler is required")
	}
	if config.Logger == nil {
		panic("service.HTTPServer: Logger is required")
	}

	timeout := config.ShutdownTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	return &HTTPServer{
		address:         config.Address,
		handler:         config.Handler,
		logger:          config.Logger,
		shutdownTimeout: timeout,
		ready:           make(chan struct{}),
	}
}

// Ready returns a channel that is closed once the server is bound
// and accepting connections.
func (s *HTTPServer) Ready() <-chan struct{} {
	return s.ready
}

// Addr returns the resolved listen address. Only valid after Ready()
// is closed. Useful when the configured address uses port 0 (OS-
// assigned port) — the resolved address contains the actual port.
func (s *HTTPServer) Addr() net.Addr {
	return s.addr
}

// Serve starts accepting HTTP connections. Blocks until ctx is
// cancelled, then performs graceful shutdown: stops accepting new
// connections and waits up to ShutdownTimeout for active requests
// to complete.
func (s *HTTPServer) Serve(ctx context.Context) error {
	// Bind the listener early so we can extract the resolved
	// address and signal readiness before entering the serve loop.
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.address, err)
	}
	s.addr = listener.Addr()
	close(s.ready)

	server := &http.Server{
		Handler: s.handler,

		// Timeouts protect against slow clients holding
		// connections open. Webhook payloads are small (typically
		// < 100KB) so generous timeouts are fine.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	s.logger.Info("http server listening", "address", s.addr.String())

	// Serve in a goroutine so we can wait for the context.
	serveDone := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveDone <- err
		}
		close(serveDone)
	}()

	// Wait for context cancellation or serve error.
	select {
	case <-ctx.Done():
		s.logger.Info("http server shutting down")
	case err := <-serveDone:
		if err != nil {
			return err
		}
		// Server closed without error and without context cancel
		// — shouldn't happen, but handle gracefully.
		return nil
	}

	// Graceful shutdown: stop accepting new connections, wait for
	// in-flight requests to complete.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("http server shutdown error", "error", err)
		return fmt.Errorf("http server shutdown: %w", err)
	}

	s.logger.Info("http server stopped")
	return nil
}

// --- Webhook signature verification ---

// VerifyWebhookHMAC verifies an HMAC-SHA256 signature on a webhook
// payload. The signature parameter is the hex-encoded HMAC digest
// (with or without a "sha256=" prefix, as used by GitHub and Forgejo).
//
// Returns nil if the signature is valid, or an error describing the
// verification failure. The error message is safe to log but does not
// include the expected signature (to avoid leaking the secret via
// error messages).
func VerifyWebhookHMAC(secret, body []byte, signature string) error {
	if len(secret) == 0 {
		return errors.New("webhook HMAC: secret is empty")
	}
	if len(body) == 0 {
		return errors.New("webhook HMAC: body is empty")
	}
	if signature == "" {
		return errors.New("webhook HMAC: signature is empty")
	}

	// Strip the "sha256=" prefix if present (GitHub convention).
	hexSignature := strings.TrimPrefix(signature, "sha256=")

	signatureBytes, err := hex.DecodeString(hexSignature)
	if err != nil {
		return fmt.Errorf("webhook HMAC: invalid hex signature: %w", err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)

	if subtle.ConstantTimeCompare(expected, signatureBytes) != 1 {
		return errors.New("webhook HMAC: signature mismatch")
	}
	return nil
}
