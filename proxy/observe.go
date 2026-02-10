// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
)

// observeProxy transparently forwards observation requests from sandboxed
// agents to the daemon's observation relay. The proxy injects its own
// Matrix credentials (observer identity and token) so the agent never
// touches authentication material.
//
// The proxy speaks the same line-protocol as the daemon's observe socket:
// the agent sends a JSON request, the proxy rewrites it with credentials,
// forwards to the daemon, sends the response back, and then bridges all
// subsequent bytes (binary observation protocol) bidirectionally.
type observeProxy struct {
	// daemonSocket is the path to the daemon's observation unix socket.
	daemonSocket string

	// credential provides MATRIX_USER_ID and MATRIX_TOKEN for injection.
	credential CredentialSource

	// listener is the proxy's observation unix socket.
	listener net.Listener

	logger *slog.Logger
}

// observeProxyConfig holds configuration for the observation proxy.
type observeProxyConfig struct {
	// SocketPath is the path for the proxy's observation unix socket.
	SocketPath string

	// DaemonSocket is the path to the daemon's observation unix socket.
	DaemonSocket string

	// Credential provides MATRIX_USER_ID and MATRIX_TOKEN.
	Credential CredentialSource

	Logger *slog.Logger
}

// startObserveProxy creates and starts an observation proxy listener.
// Returns the proxy (for shutdown) or an error if the socket cannot
// be created.
func startObserveProxy(config observeProxyConfig) (*observeProxy, error) {
	if config.SocketPath == "" {
		return nil, fmt.Errorf("observe socket path is required")
	}
	if config.DaemonSocket == "" {
		return nil, fmt.Errorf("daemon observe socket path is required")
	}
	if config.Credential == nil {
		return nil, fmt.Errorf("credential source is required")
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Remove existing socket file if present.
	if err := os.Remove(config.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove existing observe socket: %w", err)
	}

	listener, err := net.Listen("unix", config.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on observe socket: %w", err)
	}

	// Socket is bind-mounted into the sandbox, so the agent process
	// needs access. Mode 0660 matches the agent socket permissions.
	if err := os.Chmod(config.SocketPath, 0660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod observe socket: %w", err)
	}

	proxy := &observeProxy{
		daemonSocket: config.DaemonSocket,
		credential:   config.Credential,
		listener:     listener,
		logger:       logger,
	}

	go proxy.acceptLoop()

	logger.Info("observe proxy started", "socket", config.SocketPath)
	return proxy, nil
}

// acceptLoop accepts connections on the observation socket and handles
// each one in a goroutine. Runs until the listener is closed.
func (proxy *observeProxy) acceptLoop() {
	for {
		connection, err := proxy.listener.Accept()
		if err != nil {
			// Listener closed during shutdown — normal exit.
			return
		}
		go proxy.handleConnection(connection)
	}
}

// handleConnection processes a single observation connection from an agent.
// It reads the agent's request, injects credentials, forwards to the daemon,
// and bridges bytes bidirectionally on success.
func (proxy *observeProxy) handleConnection(agentConnection net.Conn) {
	defer agentConnection.Close()

	// Read the agent's JSON request line.
	agentReader := bufio.NewReader(agentConnection)
	requestLine, err := agentReader.ReadBytes('\n')
	if err != nil {
		proxy.logger.Warn("observe proxy: failed to read agent request", "error", err)
		return
	}

	// Parse the request to inject credentials. We use a generic map so we
	// don't need to know every possible request type (observe, query_layout,
	// list). The proxy injects observer/token on all of them.
	var request map[string]any
	if err := json.Unmarshal(requestLine, &request); err != nil {
		proxy.sendError(agentConnection, "invalid JSON request: %v", err)
		return
	}

	// Inject the proxy's own Matrix credentials. The agent doesn't know
	// (and shouldn't know) the Matrix token — the proxy adds it.
	userIDBuffer := proxy.credential.Get("MATRIX_USER_ID")
	tokenBuffer := proxy.credential.Get("MATRIX_TOKEN")
	if userIDBuffer == nil || tokenBuffer == nil {
		proxy.sendError(agentConnection, "proxy has no Matrix credentials configured")
		return
	}
	request["observer"] = userIDBuffer.String()
	request["token"] = tokenBuffer.String()

	augmentedRequest, err := json.Marshal(request)
	if err != nil {
		proxy.sendError(agentConnection, "failed to marshal augmented request: %v", err)
		return
	}
	augmentedRequest = append(augmentedRequest, '\n')

	// Connect to the daemon's observation socket.
	daemonConnection, err := net.Dial("unix", proxy.daemonSocket)
	if err != nil {
		proxy.sendError(agentConnection, "failed to connect to daemon: %v", err)
		return
	}
	defer daemonConnection.Close()

	// Forward the augmented request to the daemon.
	if _, err := daemonConnection.Write(augmentedRequest); err != nil {
		proxy.sendError(agentConnection, "failed to forward request to daemon: %v", err)
		return
	}

	// Read the daemon's JSON response line.
	daemonReader := bufio.NewReader(daemonConnection)
	responseLine, err := daemonReader.ReadBytes('\n')
	if err != nil {
		proxy.sendError(agentConnection, "failed to read daemon response: %v", err)
		return
	}

	// Forward the daemon's response to the agent verbatim.
	if _, err := agentConnection.Write(responseLine); err != nil {
		return
	}

	// Check if the daemon indicated success. If not, the connection ends
	// here — no binary protocol follows.
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(responseLine, &response); err != nil || !response.OK {
		return
	}

	principal, _ := request["principal"].(string)
	proxy.logger.Info("observe proxy: bridging session",
		"principal", principal,
		"observer", userIDBuffer.String(),
	)

	// Bridge all subsequent bytes bidirectionally. The daemon switches
	// to the binary observation protocol after the JSON handshake; we
	// pass everything through transparently.
	//
	// The buffered readers may hold bytes read ahead during the JSON
	// handshake. Use them as the read sources (not the raw connections)
	// so those buffered bytes aren't lost.
	bridgeReaders(agentConnection, agentReader, daemonConnection, daemonReader)
}

// sendError sends a JSON error response to the agent and logs the error.
func (proxy *observeProxy) sendError(connection net.Conn, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	proxy.logger.Warn("observe proxy error", "error", message)
	response := map[string]any{
		"ok":    false,
		"error": message,
	}
	data, _ := json.Marshal(response)
	data = append(data, '\n')
	connection.Write(data)
}

// stop shuts down the observation proxy listener and cleans up the socket.
func (proxy *observeProxy) stop() {
	if proxy.listener != nil {
		proxy.listener.Close()
	}
}

// bridgeReaders copies data bidirectionally between two connections using
// the provided readers (which may contain buffered data from the JSON
// handshake). Blocks until either direction returns an error or EOF.
func bridgeReaders(connectionA net.Conn, readerA io.Reader, connectionB net.Conn, readerB io.Reader) {
	var once sync.Once
	done := make(chan struct{})

	go func() {
		io.Copy(connectionB, readerA)
		once.Do(func() { close(done) })
	}()

	go func() {
		io.Copy(connectionA, readerB)
		once.Do(func() { close(done) })
	}()

	<-done
	// Close both connections to unblock the other goroutine.
	connectionA.Close()
	connectionB.Close()
}
