// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// ActionFunc processes a socket request for a specific action. The raw
// parameter is the full CBOR request (including the "action" field).
// The handler decodes action-specific fields from this raw message.
//
// Return a value to include in the success response, or an error for
// a failure response. If the returned value is nil, the response
// contains only {ok: true}. If non-nil, the value is marshaled as
// CBOR and placed in the response's "data" field.
type ActionFunc func(ctx context.Context, raw []byte) (any, error)

// AuthActionFunc processes a socket request that requires a valid
// service token. The token has already been verified (signature,
// expiry, audience, blacklist) by the server before the handler is
// called. The handler uses the token's Subject, Machine, and Grants
// fields to identify the caller and make authorization decisions.
type AuthActionFunc func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error)

// AuthConfig holds the cryptographic material needed to verify
// service tokens on incoming requests. All fields are required.
type AuthConfig struct {
	// PublicKey is the daemon's Ed25519 public key for verifying
	// token signatures.
	PublicKey ed25519.PublicKey

	// Audience is the expected service role (e.g., "ticket",
	// "artifact"). Tokens scoped to a different audience are
	// rejected.
	Audience string

	// Blacklist is the token revocation list. Consulted after
	// cryptographic verification succeeds. The service receives
	// revocation notices from the daemon and adds token IDs here.
	Blacklist *servicetoken.Blacklist
}

// Response is the wire-format envelope for all socket protocol
// responses. Handlers return a result value (or nil) and an error;
// the server wraps these into a Response before encoding.
type Response struct {
	OK    bool             `cbor:"ok"`
	Error string           `cbor:"error,omitempty"`
	Data  codec.RawMessage `cbor:"data,omitempty"`
}

// SocketServer serves a CBOR request-response protocol on a Unix
// socket. Each connection handles exactly one request-response cycle:
// the client writes a CBOR value, the server processes it and writes
// a CBOR response, then the connection closes.
//
// Actions are registered with Handle (unauthenticated) or HandleAuth
// (authenticated, requires a valid service token) before calling
// Serve. Unknown actions receive an error response.
type SocketServer struct {
	socketPath   string
	handlers     map[string]ActionFunc
	authHandlers map[string]AuthActionFunc
	authConfig   *AuthConfig
	logger       *slog.Logger

	// activeConnections tracks in-flight request handlers for graceful
	// shutdown. Serve waits for all active connections to complete
	// before returning.
	activeConnections sync.WaitGroup
}

// NewSocketServer creates a server that will listen on socketPath.
// If authConfig is non-nil, authenticated handlers may be registered
// via HandleAuth. Register actions with Handle/HandleAuth before
// calling Serve.
func NewSocketServer(socketPath string, logger *slog.Logger, authConfig *AuthConfig) *SocketServer {
	if authConfig != nil {
		if authConfig.PublicKey == nil {
			panic("service.SocketServer: AuthConfig.PublicKey must not be nil")
		}
		if authConfig.Audience == "" {
			panic("service.SocketServer: AuthConfig.Audience must not be empty")
		}
		if authConfig.Blacklist == nil {
			panic("service.SocketServer: AuthConfig.Blacklist must not be nil")
		}
	}
	return &SocketServer{
		socketPath:   socketPath,
		handlers:     make(map[string]ActionFunc),
		authHandlers: make(map[string]AuthActionFunc),
		authConfig:   authConfig,
		logger:       logger,
	}
}

// Handle registers an unauthenticated handler for the given action
// name. Use for health checks and public endpoints. Panics if the
// action is already registered (authenticated or unauthenticated).
func (s *SocketServer) Handle(action string, handler ActionFunc) {
	s.checkActionAvailable(action)
	s.handlers[action] = handler
}

// HandleAuth registers an authenticated handler for the given action
// name. The server extracts the "token" field from the request,
// verifies it against the configured AuthConfig, and passes the
// decoded token to the handler. Requests without a valid token
// receive an error response without invoking the handler.
//
// Panics if no AuthConfig was provided to NewSocketServer, or if the
// action is already registered.
func (s *SocketServer) HandleAuth(action string, handler AuthActionFunc) {
	if s.authConfig == nil {
		panic("service.SocketServer: HandleAuth requires AuthConfig")
	}
	s.checkActionAvailable(action)
	s.authHandlers[action] = handler
}

// checkActionAvailable panics if the action name is already
// registered in either handler map.
func (s *SocketServer) checkActionAvailable(action string) {
	if _, exists := s.handlers[action]; exists {
		panic(fmt.Sprintf("service.SocketServer: duplicate handler for action %q", action))
	}
	if _, exists := s.authHandlers[action]; exists {
		panic(fmt.Sprintf("service.SocketServer: duplicate handler for action %q", action))
	}
}

// Serve starts accepting connections on the Unix socket and dispatches
// requests to registered action handlers. Blocks until ctx is
// cancelled, then stops accepting new connections and waits for active
// handlers to complete.
//
// Any existing socket file at the configured path is removed before
// listening. The socket file is removed on return.
func (s *SocketServer) Serve(ctx context.Context) error {
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", s.socketPath, err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.socketPath, err)
	}
	defer func() {
		listener.Close()
		os.Remove(s.socketPath)
	}()

	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	s.logger.Info("socket server listening", "path", s.socketPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			s.logger.Error("accept failed", "error", err)
			continue
		}

		s.activeConnections.Add(1)
		go func() {
			defer s.activeConnections.Done()
			s.handleConnection(ctx, conn)
		}()
	}

	s.activeConnections.Wait()
	return nil
}

// readTimeout is how long we wait for the client to send its request.
// A well-behaved client sends the request immediately after connecting.
const readTimeout = 30 * time.Second

// writeTimeout is how long we wait for the response to be written.
const writeTimeout = 10 * time.Second

// maxRequestSize is the maximum size of a single CBOR request.
// 1 MB is generous for any ticket operation (the largest ticket state
// events are ~35KB per the design doc's size analysis).
const maxRequestSize = 1024 * 1024

// handleConnection processes one request-response cycle.
func (s *SocketServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(readTimeout))

	// Decode one CBOR value from the connection. CBOR is self-
	// delimiting so no framing protocol is needed. LimitReader
	// prevents a malicious client from exhausting memory.
	var raw codec.RawMessage
	if err := codec.NewDecoder(io.LimitReader(conn, maxRequestSize)).Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// Client connected but sent nothing.
			return
		}
		s.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Extract the action field for routing.
	var header struct {
		Action string `cbor:"action"`
	}
	if err := codec.Unmarshal(raw, &header); err != nil {
		s.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if header.Action == "" {
		s.writeError(conn, "missing required field: action")
		return
	}

	// Try unauthenticated handler first.
	if handler, exists := s.handlers[header.Action]; exists {
		result, err := handler(ctx, []byte(raw))
		if err != nil {
			s.logger.Debug("action failed",
				"action", header.Action,
				"error", err,
			)
			s.writeError(conn, err.Error())
			return
		}
		s.writeSuccess(conn, result)
		return
	}

	// Try authenticated handler.
	authHandler, exists := s.authHandlers[header.Action]
	if !exists {
		s.writeError(conn, fmt.Sprintf("unknown action %q", header.Action))
		return
	}

	// Verify the service token before dispatching.
	token, err := s.verifyRequestToken(raw, header.Action)
	if err != nil {
		s.writeError(conn, err.Error())
		return
	}

	result, err := authHandler(ctx, token, []byte(raw))
	if err != nil {
		s.logger.Debug("action failed",
			"action", header.Action,
			"subject", token.Subject,
			"error", err,
		)
		s.writeError(conn, err.Error())
		return
	}

	s.writeSuccess(conn, result)
}

// VerifyRequestToken extracts the "token" field from a CBOR request,
// verifies signature, expiry, audience, and blacklist status. Returns
// the decoded token on success. On failure, logs the detailed reason
// server-side and returns an error with a client-safe message.
//
// This is the shared implementation used by both SocketServer (via its
// verifyRequestToken method) and services that manage their own
// connection handling (e.g., the artifact service's binary streaming).
func VerifyRequestToken(config *AuthConfig, raw []byte, action string, logger *slog.Logger) (*servicetoken.Token, error) {
	var tokenField struct {
		Token []byte `cbor:"token"`
	}
	if err := codec.Unmarshal(raw, &tokenField); err != nil {
		logger.Warn("auth: failed to decode token field",
			"action", action,
			"error", err,
		)
		return nil, errors.New("authentication failed")
	}
	if tokenField.Token == nil {
		logger.Debug("auth: missing token field", "action", action)
		return nil, errors.New("authentication required: missing token field")
	}

	// Verify signature, decode payload, check expiry and audience.
	token, err := servicetoken.VerifyForService(
		config.PublicKey,
		tokenField.Token,
		config.Audience,
	)
	if err != nil {
		if errors.Is(err, servicetoken.ErrTokenExpired) {
			logger.Info("auth: token expired",
				"action", action,
				"error", err,
			)
			return nil, errors.New("authentication failed: token expired")
		}
		logger.Warn("auth: token verification failed",
			"action", action,
			"error", err,
		)
		return nil, errors.New("authentication failed")
	}

	// Check blacklist (emergency revocation).
	if config.Blacklist.IsRevoked(token.ID) {
		logger.Info("auth: token revoked",
			"action", action,
			"subject", token.Subject,
			"token_id", token.ID,
		)
		return nil, errors.New("authentication failed: token revoked")
	}

	return token, nil
}

// verifyRequestToken delegates to the standalone VerifyRequestToken
// function using the server's configured auth material and logger.
func (s *SocketServer) verifyRequestToken(raw codec.RawMessage, action string) (*servicetoken.Token, error) {
	return VerifyRequestToken(s.authConfig, []byte(raw), action, s.logger)
}

// writeError sends a failure response: {ok: false, error: "..."}.
// Write failures are logged at debug level — the connection is closing
// regardless, and the caller has already received the error.
func (s *SocketServer) writeError(conn net.Conn, message string) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := codec.NewEncoder(conn).Encode(Response{
		OK:    false,
		Error: message,
	}); err != nil {
		s.logger.Debug("failed to write error response", "error", err)
	}
}

// writeSuccess sends a success response. If result is nil, the
// response is {ok: true}. If non-nil, the value is marshaled as CBOR
// and placed in the "data" field: {ok: true, data: <cbor>}.
func (s *SocketServer) writeSuccess(conn net.Conn, result any) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))

	response := Response{OK: true}

	if result != nil {
		data, err := codec.Marshal(result)
		if err != nil {
			s.writeError(conn, fmt.Sprintf("internal: marshaling response: %v", err))
			return
		}
		response.Data = data
	}

	if err := codec.NewEncoder(conn).Encode(response); err != nil {
		s.logger.Debug("failed to write success response", "error", err)
	}
}
