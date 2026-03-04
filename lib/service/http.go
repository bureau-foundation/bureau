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
	"os"
	"strings"
	"time"
)

// HTTPServer serves HTTP on either a TCP listener or a Unix domain
// socket. Used by forge connectors for webhook ingestion and by the
// model service for OpenAI-compatible HTTP endpoints. The server
// manages listener lifecycle and graceful shutdown; the caller
// provides the http.Handler (routing, signature verification,
// payload processing).
//
// Follows the same lifecycle pattern as SocketServer: Serve(ctx)
// blocks until the context is cancelled and active requests drain.
type HTTPServer struct {
	address    string
	socketPath string
	handler    http.Handler
	logger     *slog.Logger

	// shutdownTimeout is the maximum time to wait for active
	// requests to complete after the context is cancelled.
	shutdownTimeout time.Duration

	// readTimeout overrides the default ReadTimeout on the
	// underlying http.Server. Zero means no timeout. Negative
	// values are treated as zero (no timeout).
	readTimeout time.Duration

	// writeTimeout overrides the default WriteTimeout on the
	// underlying http.Server. Zero means no timeout.
	writeTimeout time.Duration

	// idleTimeout overrides the default IdleTimeout on the
	// underlying http.Server. Zero means no timeout.
	idleTimeout time.Duration

	// hasReadTimeout distinguishes "caller set 0" (no timeout)
	// from "caller didn't set anything" (use default).
	hasReadTimeout  bool
	hasWriteTimeout bool
	hasIdleTimeout  bool

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
	// "127.0.0.1:9000"). Mutually exclusive with SocketPath —
	// exactly one must be set.
	Address string

	// SocketPath is the Unix domain socket path. The server
	// removes any stale socket at this path before binding, sets
	// permissions to 0770 (group-writable for setgid directories),
	// and removes the socket on shutdown. Mutually exclusive with
	// Address — exactly one must be set.
	SocketPath string

	// Handler is the HTTP handler for incoming requests. Required.
	Handler http.Handler

	// ShutdownTimeout is the maximum time to wait for in-flight
	// requests to complete during graceful shutdown. Defaults to
	// 10 seconds if zero.
	ShutdownTimeout time.Duration

	// ReadTimeout overrides the default 30s ReadTimeout on the
	// http.Server. Set to a negative value to disable (0 uses the
	// default). Services with long-lived streaming responses (SSE)
	// should set this to -1.
	ReadTimeout time.Duration

	// WriteTimeout overrides the default 30s WriteTimeout. Set to
	// a negative value to disable. Same SSE caveat as ReadTimeout.
	WriteTimeout time.Duration

	// IdleTimeout overrides the default 60s IdleTimeout. Set to a
	// negative value to disable.
	IdleTimeout time.Duration

	// Logger is the structured logger. Required.
	Logger *slog.Logger
}

// NewHTTPServer creates a server that will listen on the configured
// address or Unix socket. Call Serve to start accepting connections.
func NewHTTPServer(config HTTPServerConfig) *HTTPServer {
	if config.Address == "" && config.SocketPath == "" {
		panic("service.HTTPServer: one of Address or SocketPath is required")
	}
	if config.Address != "" && config.SocketPath != "" {
		panic("service.HTTPServer: Address and SocketPath are mutually exclusive")
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
		socketPath:      config.SocketPath,
		handler:         config.Handler,
		logger:          config.Logger,
		shutdownTimeout: timeout,
		readTimeout:     config.ReadTimeout,
		writeTimeout:    config.WriteTimeout,
		idleTimeout:     config.IdleTimeout,
		hasReadTimeout:  config.ReadTimeout != 0,
		hasWriteTimeout: config.WriteTimeout != 0,
		hasIdleTimeout:  config.IdleTimeout != 0,
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
	listener, err := s.listen()
	if err != nil {
		return err
	}
	s.addr = listener.Addr()
	close(s.ready)

	// Apply timeout defaults. Negative config values disable the
	// timeout (set to 0). When the caller didn't set a value at
	// all (hasXxxTimeout is false), use sensible defaults.
	readTimeout := 30 * time.Second
	if s.hasReadTimeout {
		readTimeout = s.readTimeout
		if readTimeout < 0 {
			readTimeout = 0
		}
	}
	writeTimeout := 30 * time.Second
	if s.hasWriteTimeout {
		writeTimeout = s.writeTimeout
		if writeTimeout < 0 {
			writeTimeout = 0
		}
	}
	idleTimeout := 60 * time.Second
	if s.hasIdleTimeout {
		idleTimeout = s.idleTimeout
		if idleTimeout < 0 {
			idleTimeout = 0
		}
	}

	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	if s.socketPath != "" {
		s.logger.Info("http server listening", "socket", s.socketPath)
	} else {
		s.logger.Info("http server listening", "address", s.addr.String())
	}

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

	// Remove the socket file after shutdown so stale sockets don't
	// confuse future startups or other processes.
	if s.socketPath != "" {
		os.Remove(s.socketPath)
	}

	s.logger.Info("http server stopped")
	return nil
}

// listen creates and returns the net.Listener for this server. For
// Unix sockets: removes stale socket files, binds, and sets 0770
// permissions (group-writable for setgid directories — connect()
// on Unix domain sockets requires write permission).
func (s *HTTPServer) listen() (net.Listener, error) {
	if s.socketPath != "" {
		// Remove stale socket file from a previous run.
		if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing stale HTTP socket %s: %w", s.socketPath, err)
		}

		listener, err := net.Listen("unix", s.socketPath)
		if err != nil {
			return nil, fmt.Errorf("listening on HTTP socket %s: %w", s.socketPath, err)
		}

		if err := os.Chmod(s.socketPath, 0770); err != nil {
			listener.Close()
			return nil, fmt.Errorf("setting HTTP socket permissions on %s: %w", s.socketPath, err)
		}

		return listener, nil
	}

	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", s.address, err)
	}
	return listener, nil
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
