// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
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
// Actions are registered with Handle before calling Serve. Unknown
// actions receive an error response.
type SocketServer struct {
	socketPath string
	handlers   map[string]ActionFunc
	logger     *slog.Logger

	// activeConnections tracks in-flight request handlers for graceful
	// shutdown. Serve waits for all active connections to complete
	// before returning.
	activeConnections sync.WaitGroup
}

// NewSocketServer creates a server that will listen on socketPath.
// Register actions with Handle before calling Serve.
func NewSocketServer(socketPath string, logger *slog.Logger) *SocketServer {
	return &SocketServer{
		socketPath: socketPath,
		handlers:   make(map[string]ActionFunc),
		logger:     logger,
	}
}

// Handle registers a handler for the given action name. Panics if
// called after Serve has started or if the action is already
// registered.
func (s *SocketServer) Handle(action string, handler ActionFunc) {
	if _, exists := s.handlers[action]; exists {
		panic(fmt.Sprintf("service.SocketServer: duplicate handler for action %q", action))
	}
	s.handlers[action] = handler
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

	handler, exists := s.handlers[header.Action]
	if !exists {
		s.writeError(conn, fmt.Sprintf("unknown action %q", header.Action))
		return
	}

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
