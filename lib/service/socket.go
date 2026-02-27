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

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
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

// AuthStreamFunc processes a long-lived authenticated stream. After
// the initial CBOR handshake (request decoding and token verification),
// the handler takes full ownership of the connection: it may read and
// write freely and blocks until the stream should end.
//
// The raw parameter is the initial CBOR request (including action and
// token fields). The token has been verified. The context cancels when
// the server shuts down. The connection is closed by the server after
// the handler returns — the handler must not close it.
//
// Unlike ActionFunc and AuthActionFunc, the server does not write a
// response for stream handlers. The handler is responsible for all
// writes to the connection (e.g., snapshot frames, live events,
// heartbeats).
type AuthStreamFunc func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn)

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

	// Clock provides the current time for token expiry checks.
	// Production callers pass clock.Real(); tests pass a fake
	// clock for deterministic token verification.
	Clock clock.Clock
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
// socket. Most connections handle exactly one request-response cycle:
// the client writes a CBOR value, the server processes it and writes
// a CBOR response, then the connection closes.
//
// Stream handlers (registered via HandleAuthStream) are an exception:
// the handler takes ownership of the connection after the initial CBOR
// handshake and may read/write freely for the connection's lifetime.
// This supports protocols like subscribe where the server streams
// events until the client disconnects.
//
// Actions are registered with Handle (unauthenticated), HandleAuth
// (authenticated request-response), or HandleAuthStream (authenticated
// long-lived stream) before calling Serve. Unknown actions receive an
// error response.
type SocketServer struct {
	socketPath     string
	handlers       map[string]ActionFunc
	authHandlers   map[string]AuthActionFunc
	streamHandlers map[string]AuthStreamFunc
	authConfig     *AuthConfig
	logger         *slog.Logger
	telemetry      *TelemetryEmitter

	// activeConnections tracks in-flight request handlers for graceful
	// shutdown. Serve waits for all active connections to complete
	// before returning.
	activeConnections sync.WaitGroup

	// ready is closed after the socket is bound and accepting
	// connections. Callers can block on Ready() to wait for the
	// server to be reachable.
	ready chan struct{}
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
		if authConfig.Clock == nil {
			panic("service.SocketServer: AuthConfig.Clock must not be nil")
		}
	}
	return &SocketServer{
		socketPath:     socketPath,
		handlers:       make(map[string]ActionFunc),
		authHandlers:   make(map[string]AuthActionFunc),
		streamHandlers: make(map[string]AuthStreamFunc),
		authConfig:     authConfig,
		logger:         logger,
		ready:          make(chan struct{}),
	}
}

// Ready returns a channel that is closed once the server socket is
// bound and accepting connections.
func (s *SocketServer) Ready() <-chan struct{} {
	return s.ready
}

// SetTelemetry attaches a telemetry emitter to the server. When set,
// every handled request produces a telemetry span covering the full
// request lifecycle (decode, auth, handler execution, response write).
// Must be called before Serve.
func (s *SocketServer) SetTelemetry(emitter *TelemetryEmitter) {
	s.telemetry = emitter
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

// HandleAuthStream registers an authenticated stream handler for the
// given action name. After verifying the service token, the server
// passes the connection to the handler, which takes ownership and
// blocks until the stream ends. The server clears read/write deadlines
// before calling the handler (stream connections are long-lived).
//
// Panics if no AuthConfig was provided to NewSocketServer, or if the
// action is already registered.
func (s *SocketServer) HandleAuthStream(action string, handler AuthStreamFunc) {
	if s.authConfig == nil {
		panic("service.SocketServer: HandleAuthStream requires AuthConfig")
	}
	s.checkActionAvailable(action)
	s.streamHandlers[action] = handler
}

// checkActionAvailable panics if the action name is already
// registered in any handler map.
func (s *SocketServer) checkActionAvailable(action string) {
	if _, exists := s.handlers[action]; exists {
		panic(fmt.Sprintf("service.SocketServer: duplicate handler for action %q", action))
	}
	if _, exists := s.authHandlers[action]; exists {
		panic(fmt.Sprintf("service.SocketServer: duplicate handler for action %q", action))
	}
	if _, exists := s.streamHandlers[action]; exists {
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

	close(s.ready)

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
			if netutil.IsExpectedCloseError(err) {
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

// requestOutcome collects the span-relevant fields from a single
// socket request. Each exit path in handleConnection populates these
// fields; a deferred call to recordRequestSpan builds the span.
type requestOutcome struct {
	action       string
	handlerType  string // "unauthenticated", "authenticated", "stream"
	subject      string // token subject for authenticated requests
	status       telemetry.SpanStatus
	errorMessage string
	// spanEnd, when non-zero, overrides time.Now() as the span end
	// time. Used by stream handlers to capture dispatch time (auth +
	// routing) without including the unbounded stream lifetime.
	spanEnd time.Time
}

// recordRequestSpan emits a telemetry span for a completed socket
// request. No-op when the server has no telemetry emitter attached.
func (s *SocketServer) recordRequestSpan(startTime time.Time, outcome *requestOutcome) {
	if s.telemetry == nil {
		return
	}

	endTime := outcome.spanEnd
	if endTime.IsZero() {
		endTime = time.Now()
	}

	attributes := map[string]any{
		"action":       outcome.action,
		"handler_type": outcome.handlerType,
	}
	if outcome.subject != "" {
		attributes["subject"] = outcome.subject
	}

	s.telemetry.RecordSpan(telemetry.Span{
		TraceID:       telemetry.NewTraceID(),
		SpanID:        telemetry.NewSpanID(),
		Operation:     "socket.handle",
		StartTime:     startTime.UnixNano(),
		Duration:      endTime.Sub(startTime).Nanoseconds(),
		Status:        outcome.status,
		StatusMessage: outcome.errorMessage,
		Attributes:    attributes,
	})
}

// handleConnection processes one request-response cycle.
func (s *SocketServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	startTime := time.Now()
	var outcome requestOutcome
	recordSpan := true
	defer func() {
		if recordSpan {
			s.recordRequestSpan(startTime, &outcome)
		}
	}()

	conn.SetReadDeadline(startTime.Add(readTimeout))

	// Decode one CBOR value from the connection. CBOR is self-
	// delimiting so no framing protocol is needed. LimitReader
	// prevents a malicious client from exhausting memory.
	var raw codec.RawMessage
	if err := codec.NewDecoder(io.LimitReader(conn, maxRequestSize)).Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// Client connected but sent nothing — not a
			// meaningful request, skip telemetry.
			recordSpan = false
			return
		}
		outcome.status = telemetry.SpanStatusError
		outcome.errorMessage = fmt.Sprintf("invalid request: %v", err)
		s.writeError(conn, outcome.errorMessage)
		return
	}

	// Extract the action field for routing.
	var header struct {
		Action string `cbor:"action"`
	}
	if err := codec.Unmarshal(raw, &header); err != nil {
		outcome.status = telemetry.SpanStatusError
		outcome.errorMessage = fmt.Sprintf("invalid request: %v", err)
		s.writeError(conn, outcome.errorMessage)
		return
	}
	if header.Action == "" {
		outcome.status = telemetry.SpanStatusError
		outcome.errorMessage = "missing required field: action"
		s.writeError(conn, outcome.errorMessage)
		return
	}

	outcome.action = header.Action

	// Try unauthenticated handler first.
	if handler, exists := s.handlers[header.Action]; exists {
		outcome.handlerType = "unauthenticated"
		result, err := handler(ctx, []byte(raw))
		if err != nil {
			s.logger.Debug("action failed",
				"action", header.Action,
				"error", err,
			)
			outcome.status = telemetry.SpanStatusError
			outcome.errorMessage = err.Error()
			s.writeError(conn, outcome.errorMessage)
			return
		}
		outcome.status = telemetry.SpanStatusOK
		s.writeSuccess(conn, result)
		return
	}

	// Try authenticated request-response handler.
	if authHandler, exists := s.authHandlers[header.Action]; exists {
		outcome.handlerType = "authenticated"
		token, err := s.verifyRequestToken(raw, header.Action)
		if err != nil {
			outcome.status = telemetry.SpanStatusError
			outcome.errorMessage = err.Error()
			s.writeError(conn, outcome.errorMessage)
			return
		}
		outcome.subject = token.Subject.String()

		result, err := authHandler(ctx, token, []byte(raw))
		if err != nil {
			s.logger.Debug("action failed",
				"action", header.Action,
				"subject", token.Subject,
				"error", err,
			)
			outcome.status = telemetry.SpanStatusError
			outcome.errorMessage = err.Error()
			s.writeError(conn, outcome.errorMessage)
			return
		}

		outcome.status = telemetry.SpanStatusOK
		s.writeSuccess(conn, result)
		return
	}

	// Try authenticated stream handler. Stream handlers take
	// ownership of the connection: the server clears deadlines
	// and does not write a response. The handler blocks until
	// the stream ends.
	if streamHandler, exists := s.streamHandlers[header.Action]; exists {
		outcome.handlerType = "stream"
		token, err := s.verifyRequestToken(raw, header.Action)
		if err != nil {
			outcome.status = telemetry.SpanStatusError
			outcome.errorMessage = err.Error()
			s.writeError(conn, outcome.errorMessage)
			return
		}
		outcome.subject = token.Subject.String()
		outcome.status = telemetry.SpanStatusOK
		// Capture dispatch time before the stream handler runs.
		// The span reflects auth + routing latency, not the
		// unbounded stream lifetime.
		outcome.spanEnd = time.Now()

		// Clear deadlines — stream connections are long-lived.
		conn.SetReadDeadline(time.Time{})
		conn.SetWriteDeadline(time.Time{})

		s.logger.Info("stream handler started",
			"action", header.Action,
			"subject", token.Subject,
		)

		streamHandler(ctx, token, []byte(raw), conn)

		s.logger.Info("stream handler ended",
			"action", header.Action,
			"subject", token.Subject,
		)
		return
	}

	outcome.status = telemetry.SpanStatusError
	outcome.errorMessage = fmt.Sprintf("unknown action %q", header.Action)
	s.writeError(conn, outcome.errorMessage)
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
	token, err := servicetoken.VerifyForServiceAt(
		config.PublicKey,
		tokenField.Token,
		config.Audience,
		config.Clock.Now(),
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

// RegisterRevocationHandler registers the standard "revoke-tokens"
// action. This handler accepts signed revocation requests from the
// daemon and adds the specified token IDs to the blacklist. The request
// is verified using the daemon's public key (from AuthConfig), so only
// the daemon can issue revocations.
//
// Requires an AuthConfig — panics if called without one (the public key
// and blacklist are required for verification and revocation).
func (s *SocketServer) RegisterRevocationHandler() {
	if s.authConfig == nil {
		panic("service.SocketServer: RegisterRevocationHandler requires AuthConfig")
	}

	// Registered as an unauthenticated handler because the request
	// carries its own Ed25519 signature from the daemon's signing key.
	// Service tokens authenticate principals to services; revocation
	// requests authenticate the daemon to services. Different trust
	// relationship, same cryptographic key.
	s.Handle("revoke-tokens", func(ctx context.Context, raw []byte) (any, error) {
		var envelope struct {
			Revocation []byte `cbor:"revocation"`
		}
		if err := codec.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("decoding revocation envelope: %w", err)
		}
		if envelope.Revocation == nil {
			return nil, errors.New("missing required field: revocation")
		}

		request, err := servicetoken.VerifyRevocation(s.authConfig.PublicKey, envelope.Revocation)
		if err != nil {
			s.logger.Warn("revocation request verification failed", "error", err)
			return nil, errors.New("revocation verification failed")
		}

		for _, entry := range request.Entries {
			s.authConfig.Blacklist.Revoke(entry.TokenID, time.Unix(entry.ExpiresAt, 0))
		}

		s.logger.Info("tokens revoked via daemon push",
			"count", len(request.Entries),
		)

		return nil, nil
	})
}
