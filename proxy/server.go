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

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Server is a credential proxy server that listens on a Unix socket and optionally TCP.
type Server struct {
	socketPath      string
	adminSocketPath string
	listenAddress   string
	handler         *Handler
	httpServer      *http.Server
	adminServer     *http.Server
	unixListener    net.Listener
	adminListener   net.Listener
	tcpListener     net.Listener
	observeProxy    *observeProxy
	observeConfig   *observeProxyConfig
	logger          *slog.Logger
}

// ServerConfig holds configuration for creating a new Server.
type ServerConfig struct {
	SocketPath    string
	ListenAddress string // Optional TCP address (e.g., "127.0.0.1:8080")
	Logger        *slog.Logger

	// AdminSocketPath is an optional separate Unix socket for admin operations.
	// When set, admin endpoints (service registration, etc.) are only available
	// on this socket. The daemon connects here; agents never see it.
	// When empty, admin endpoints are not exposed.
	AdminSocketPath string
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

	// Agent-facing mux: only endpoints safe for sandboxed agents.
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("POST /v1/proxy", handler.HandleProxy)
	agentMux.HandleFunc("GET /v1/identity", handler.HandleIdentity)
	agentMux.HandleFunc("GET /v1/services", handler.HandleServiceDirectory)
	agentMux.HandleFunc("GET /health", handler.HandleHealth)
	agentMux.HandleFunc("/http/", handler.HandleHTTPProxy)

	server := &Server{
		socketPath:      config.SocketPath,
		adminSocketPath: config.AdminSocketPath,
		listenAddress:   config.ListenAddress,
		handler:         handler,
		httpServer: &http.Server{
			Handler:      agentMux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 5 * time.Minute, // Long timeout for streaming
		},
		logger: logger,
	}

	// Admin mux: all agent endpoints plus service management. The admin
	// socket is only accessible to the daemon (not bind-mounted into
	// sandboxes), so there is no agent-accessible attack surface.
	if config.AdminSocketPath != "" {
		adminMux := http.NewServeMux()
		adminMux.HandleFunc("GET /v1/admin/services", handler.HandleAdminListServices)
		adminMux.HandleFunc("PUT /v1/admin/services/{name}", handler.HandleAdminRegisterService)
		adminMux.HandleFunc("DELETE /v1/admin/services/{name}", handler.HandleAdminUnregisterService)
		adminMux.HandleFunc("PUT /v1/admin/directory", handler.HandleAdminSetDirectory)
		adminMux.HandleFunc("PUT /v1/admin/visibility", handler.HandleAdminSetVisibility)
		// Fall through to agent endpoints for all other paths.
		adminMux.Handle("/", agentMux)

		server.adminServer = &http.Server{
			Handler:      adminMux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
	}

	return server, nil
}

// RegisterService registers a CLI service that can be proxied.
func (s *Server) RegisterService(name string, service Service) {
	s.handler.RegisterService(name, service)
}

// RegisterHTTPService registers an HTTP proxy service.
func (s *Server) RegisterHTTPService(name string, service *HTTPService) {
	s.handler.RegisterHTTPService(name, service)
}

// UnregisterHTTPService removes an HTTP proxy service by name.
// Returns true if the service existed and was removed.
func (s *Server) UnregisterHTTPService(name string) bool {
	return s.handler.UnregisterHTTPService(name)
}

// ListHTTPServices returns the names of all registered HTTP proxy services.
func (s *Server) ListHTTPServices() []string {
	return s.handler.ListHTTPServices()
}

// SetIdentity configures the agent's identity, making it available via the
// GET /v1/identity endpoint.
func (s *Server) SetIdentity(identity IdentityInfo) {
	s.handler.SetIdentity(identity)
}

// SetMatrixPolicy configures the Matrix access policy. See
// Handler.SetMatrixPolicy for details.
func (s *Server) SetMatrixPolicy(policy *schema.MatrixPolicy) {
	s.handler.SetMatrixPolicy(policy)
}

// SetServiceDirectory replaces the cached service directory. See
// Handler.SetServiceDirectory for details.
func (s *Server) SetServiceDirectory(entries []ServiceDirectoryEntry) {
	s.handler.SetServiceDirectory(entries)
}

// SetServiceVisibility configures which services this agent can discover.
// See Handler.SetServiceVisibility for details.
func (s *Server) SetServiceVisibility(patterns []string) {
	s.handler.SetServiceVisibility(patterns)
}

// SetObserveConfig configures the observation proxy. When called before
// Start(), the server will listen on the observation socket and forward
// observation requests to the daemon with injected credentials. The
// observation socket speaks the daemon's native observation protocol
// (JSON handshake + binary framing), not HTTP.
func (s *Server) SetObserveConfig(socketPath, daemonSocket string, credential CredentialSource) {
	s.observeConfig = &observeProxyConfig{
		SocketPath:   socketPath,
		DaemonSocket: daemonSocket,
		Credential:   credential,
		Logger:       s.logger,
	}
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

	// Optionally start admin socket (daemon-only, not visible to sandboxes)
	if s.adminSocketPath != "" && s.adminServer != nil {
		if err := os.Remove(s.adminSocketPath); err != nil && !os.IsNotExist(err) {
			unixListener.Close()
			return fmt.Errorf("failed to remove existing admin socket: %w", err)
		}

		adminListener, err := net.Listen("unix", s.adminSocketPath)
		if err != nil {
			unixListener.Close()
			return fmt.Errorf("failed to listen on admin socket: %w", err)
		}
		s.adminListener = adminListener

		if err := os.Chmod(s.adminSocketPath, 0660); err != nil {
			adminListener.Close()
			unixListener.Close()
			return fmt.Errorf("failed to chmod admin socket: %w", err)
		}

		s.logger.Info("proxy admin server started", "socket", s.adminSocketPath)

		go func() {
			if err := s.adminServer.Serve(adminListener); err != nil && err != http.ErrServerClosed {
				s.logger.Error("admin server error", "error", err)
			}
		}()
	}

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

	// Start the observation proxy if configured.
	if s.observeConfig != nil {
		observeProxy, err := startObserveProxy(*s.observeConfig)
		if err != nil {
			return fmt.Errorf("start observe proxy: %w", err)
		}
		s.observeProxy = observeProxy
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
	if s.adminServer != nil {
		if adminErr := s.adminServer.Shutdown(ctx); adminErr != nil && err == nil {
			err = adminErr
		}
	}
	if s.observeProxy != nil {
		s.observeProxy.stop()
	}
	if s.unixListener != nil {
		os.Remove(s.socketPath)
	}
	if s.adminListener != nil {
		os.Remove(s.adminSocketPath)
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
