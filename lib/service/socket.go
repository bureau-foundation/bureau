// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// ActionFunc processes a socket request for a specific action. The raw
// parameter is the full JSON request (including the "action" field).
// The handler decodes action-specific fields from this raw message.
//
// Return a value to include in the success response, or an error for
// a failure response. If the returned value is nil, the response is
// {"ok": true}. If non-nil, the value is marshaled as a JSON object
// and merged with {"ok": true}. Non-object values are wrapped in a
// "data" field.
type ActionFunc func(ctx context.Context, raw json.RawMessage) (any, error)

// SocketServer serves the NDJSON request-response protocol on a Unix
// socket. Each connection handles exactly one request-response cycle:
// the client writes a JSON line, the server processes it and writes a
// JSON response line, then the connection closes.
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

// maxRequestSize is the maximum size of a single JSON request line.
// 1 MB is generous for any ticket operation (the largest ticket state
// events are ~35KB per the design doc's size analysis).
const maxRequestSize = 1024 * 1024

// handleConnection processes one request-response cycle.
func (s *SocketServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(readTimeout))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRequestSize)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			s.logger.Debug("reading request failed", "error", err)
		}
		return
	}

	// Copy the scanned bytes — scanner reuses its buffer.
	raw := make([]byte, len(scanner.Bytes()))
	copy(raw, scanner.Bytes())

	// Extract the action field for routing.
	var header struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		s.writeError(conn, fmt.Sprintf("invalid JSON: %v", err))
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

	result, err := handler(ctx, json.RawMessage(raw))
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

// writeError sends a failure response: {"ok": false, "error": "..."}.
// Write failures are logged at debug level — the connection is closing
// regardless, and the caller has already received the error.
func (s *SocketServer) writeError(conn net.Conn, message string) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"ok":    false,
		"error": message,
	}); err != nil {
		s.logger.Debug("failed to write error response", "error", err)
	}
}

// writeSuccess sends a success response. If result is nil, sends
// {"ok": true}. If result marshals to a JSON object, merges with
// {"ok": true} for a flat response. Otherwise wraps in
// {"ok": true, "data": ...}.
//
// Handlers must not include an "ok" field in their return value —
// doing so will be overwritten by this function. The "ok" field is
// reserved for the protocol envelope.
func (s *SocketServer) writeSuccess(conn net.Conn, result any) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	encoder := json.NewEncoder(conn)

	if result == nil {
		if err := encoder.Encode(map[string]any{"ok": true}); err != nil {
			s.logger.Debug("failed to write success response", "error", err)
		}
		return
	}

	// Marshal the handler's result to JSON bytes.
	data, err := json.Marshal(result)
	if err != nil {
		s.writeError(conn, fmt.Sprintf("internal: marshaling response: %v", err))
		return
	}

	// If the result is a JSON object, merge ok:true into it so
	// the response is flat (matching the daemon IPC convention).
	var asMap map[string]any
	if err := json.Unmarshal(data, &asMap); err == nil {
		asMap["ok"] = true
		if err := encoder.Encode(asMap); err != nil {
			s.logger.Debug("failed to write success response", "error", err)
		}
		return
	}

	// Non-object result (array, scalar): wrap in a data field.
	if err := encoder.Encode(map[string]any{
		"ok":   true,
		"data": json.RawMessage(data),
	}); err != nil {
		s.logger.Debug("failed to write success response", "error", err)
	}
}
