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
	"time"

	"github.com/bureau-foundation/bureau/lib/netutil"
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

	// mu protects connections. Held briefly during register/unregister
	// and during stop's close sweep.
	mu sync.Mutex

	// connections tracks agent-side connections with active bridges so
	// stop() can force-close them to unblock bridgeReaders. Without this,
	// stop() would block indefinitely waiting for observation sessions to
	// end naturally.
	connections map[net.Conn]struct{}

	// activeConnections tracks in-flight handleConnection goroutines so
	// stop() can wait for them to drain before returning. The caller
	// (Server.Shutdown) closes the credential source after stop() returns;
	// without the wait, goroutines holding borrowed *secret.Buffer pointers
	// would panic on String() after the buffers are closed.
	activeConnections sync.WaitGroup

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
		connections:  make(map[net.Conn]struct{}),
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
		proxy.activeConnections.Add(1)
		go proxy.handleConnection(connection)
	}
}

// trackConnection registers a connection for force-close during shutdown.
// Returns false if the proxy is already stopping (connections map is nil),
// in which case the caller should abort.
func (proxy *observeProxy) trackConnection(connection net.Conn) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.connections == nil {
		return false
	}
	proxy.connections[connection] = struct{}{}
	return true
}

// untrackConnection removes a connection from the active set.
func (proxy *observeProxy) untrackConnection(connection net.Conn) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.connections != nil {
		delete(proxy.connections, connection)
	}
}

// handleConnection processes a single observation connection from an agent.
// It reads the agent's request, injects credentials, forwards to the daemon,
// and bridges bytes bidirectionally on success.
func (proxy *observeProxy) handleConnection(agentConnection net.Conn) {
	defer proxy.activeConnections.Done()
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

	// Log if the agent tried to supply its own credentials. This is a
	// security signal — the agent may be attempting to impersonate another
	// identity. The proxy always overwrites with its own credentials
	// regardless, but the attempt is worth recording.
	if agentObserver, ok := request["observer"].(string); ok && agentObserver != "" {
		proxy.logger.Warn("observe proxy: agent supplied credentials (overwritten)",
			"agent_observer", agentObserver,
			"proxy_observer", userIDBuffer.String(),
		)
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
	startTime := time.Now()

	proxy.logger.Info("observe proxy: bridging session",
		"principal", principal,
		"observer", userIDBuffer.String(),
	)

	// Register both connections so stop() can force-close them to unblock
	// the bridge during shutdown. If tracking fails, the proxy is already
	// stopping — abort rather than enter a bridge that can't be interrupted.
	if !proxy.trackConnection(agentConnection) {
		return
	}
	if !proxy.trackConnection(daemonConnection) {
		proxy.untrackConnection(agentConnection)
		return
	}
	defer proxy.untrackConnection(agentConnection)
	defer proxy.untrackConnection(daemonConnection)

	// Bridge all subsequent bytes bidirectionally. The daemon switches
	// to the binary observation protocol after the JSON handshake; we
	// pass everything through transparently.
	//
	// The buffered readers may hold bytes read ahead during the JSON
	// handshake. Use them as the read sources (not the raw connections)
	// so those buffered bytes aren't lost.
	if err := bridgeReaders(agentConnection, agentReader, daemonConnection, daemonReader); err != nil {
		proxy.logger.Warn("observe proxy: bridge error",
			"principal", principal,
			"observer", userIDBuffer.String(),
			"error", err,
		)
	}

	proxy.logger.Info("observe proxy: session ended",
		"principal", principal,
		"observer", userIDBuffer.String(),
		"duration", time.Since(startTime).String(),
	)
}

// sendError sends a JSON error response to the agent and logs the error.
func (proxy *observeProxy) sendError(connection net.Conn, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	proxy.logger.Warn("observe proxy error", "error", message)
	response := map[string]any{
		"ok":    false,
		"error": message,
	}
	data, err := json.Marshal(response)
	if err != nil {
		proxy.logger.Warn("marshaling observe error response", "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := connection.Write(data); err != nil {
		proxy.logger.Warn("writing observe error response", "error", err)
	}
}

// stop shuts down the observation proxy. It closes the listener to stop
// accepting new connections, force-closes all active bridged connections
// to unblock in-flight goroutines, then waits for all goroutines to finish.
// After stop returns, no goroutines hold references to borrowed credentials.
func (proxy *observeProxy) stop() {
	if proxy.listener != nil {
		proxy.listener.Close()
	}

	// Force-close all tracked connections. This unblocks any io.Copy calls
	// inside bridgeReaders, allowing handleConnection goroutines to run
	// their deferred cleanup and exit. Set connections to nil so concurrent
	// trackConnection calls from late-arriving goroutines return false.
	proxy.mu.Lock()
	for connection := range proxy.connections {
		connection.Close()
	}
	proxy.connections = nil
	proxy.mu.Unlock()

	// Wait for all handleConnection goroutines to finish. After this returns,
	// no goroutine holds borrowed *secret.Buffer pointers from the credential
	// source, so the caller can safely close the credential source.
	proxy.activeConnections.Wait()
}

// bridgeReaders copies data bidirectionally between two connections using
// the provided readers (which may contain buffered data from the JSON
// handshake). Returns when either direction finishes. Both connections are
// closed before returning. Returns the error from the direction that
// terminated first, or nil if termination was due to normal connection closure.
func bridgeReaders(connectionA net.Conn, readerA io.Reader, connectionB net.Conn, readerB io.Reader) error {
	type copyResult struct {
		bytesCopied int64
		err         error
	}
	done := make(chan copyResult, 2)

	go func() {
		bytesCopied, err := io.Copy(connectionB, readerA)
		done <- copyResult{bytesCopied, err}
	}()

	go func() {
		bytesCopied, err := io.Copy(connectionA, readerB)
		done <- copyResult{bytesCopied, err}
	}()

	// Wait for one direction to finish, then close both to unblock the other.
	first := <-done
	connectionA.Close()
	connectionB.Close()
	<-done

	if first.err != nil && !netutil.IsExpectedCloseError(first.err) {
		return first.err
	}
	return nil
}
