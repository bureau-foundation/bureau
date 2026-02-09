// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// Server is a credential proxy server that listens on a Unix socket and optionally TCP.
type Server struct {
	socketPath    string
	listenAddress string
	handler       *Handler
	httpServer    *http.Server
	unixListener  net.Listener
	tcpListener   net.Listener
	logger        *slog.Logger
}

// ServerConfig holds configuration for creating a new Server.
type ServerConfig struct {
	SocketPath    string
	ListenAddress string // Optional TCP address (e.g., "127.0.0.1:8080")
	Logger        *slog.Logger
}

// NewServer creates a new proxy server.
func NewServer(config ServerConfig) (*Server, error) {
	if config.SocketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	handler := NewHandler(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/proxy", handler.HandleProxy)
	mux.HandleFunc("GET /health", handler.HandleHealth)
	// HTTP proxy routes - catch all methods and paths under /http/
	mux.HandleFunc("/http/", handler.HandleHTTPProxy)

	return &Server{
		socketPath:    config.SocketPath,
		listenAddress: config.ListenAddress,
		handler:       handler,
		httpServer: &http.Server{
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 5 * time.Minute, // Long timeout for streaming
		},
		logger: logger,
	}, nil
}

// RegisterService registers a CLI service that can be proxied.
func (s *Server) RegisterService(name string, service Service) {
	s.handler.RegisterService(name, service)
}

// RegisterHTTPService registers an HTTP proxy service.
func (s *Server) RegisterHTTPService(name string, service *HTTPService) {
	s.handler.RegisterHTTPService(name, service)
}

// Start begins listening on the Unix socket and optionally TCP.
func (s *Server) Start() error {
	// Remove existing socket file if present
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	unixListener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	s.unixListener = unixListener

	// Set socket permissions so clients can connect
	if err := os.Chmod(s.socketPath, 0660); err != nil {
		unixListener.Close()
		return fmt.Errorf("failed to chmod socket: %w", err)
	}

	s.logger.Info("proxy server started", "socket", s.socketPath)

	// Start serving on Unix socket
	go func() {
		if err := s.httpServer.Serve(unixListener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("unix server error", "error", err)
		}
	}()

	// Optionally start TCP listener
	if s.listenAddress != "" {
		tcpListener, err := net.Listen("tcp", s.listenAddress)
		if err != nil {
			unixListener.Close()
			return fmt.Errorf("failed to listen on TCP %s: %w", s.listenAddress, err)
		}
		s.tcpListener = tcpListener
		s.logger.Info("proxy server TCP started", "address", s.listenAddress)

		// Need a separate http.Server instance for TCP since Serve takes ownership
		tcpServer := &http.Server{
			Handler:      s.httpServer.Handler,
			ReadTimeout:  s.httpServer.ReadTimeout,
			WriteTimeout: s.httpServer.WriteTimeout,
		}
		go func() {
			if err := tcpServer.Serve(tcpListener); err != nil && err != http.ErrServerClosed {
				s.logger.Error("tcp server error", "error", err)
			}
		}()
	}

	// Notify systemd that we're ready (no-op if not running under systemd)
	notifySystemd("READY=1")

	return nil
}

// notifySystemd sends a notification to systemd's sd_notify socket.
// This is used to signal readiness when running as a systemd service.
// Does nothing if NOTIFY_SOCKET is not set.
func notifySystemd(state string) {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return
	}

	conn, err := net.Dial("unixgram", socketPath)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.Write([]byte(state))
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down proxy server")
	err := s.httpServer.Shutdown(ctx)
	if s.unixListener != nil {
		os.Remove(s.socketPath)
	}
	if s.tcpListener != nil {
		s.tcpListener.Close()
	}
	return err
}

// SocketPath returns the path to the Unix socket.
func (s *Server) SocketPath() string {
	return s.socketPath
}
