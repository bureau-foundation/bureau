// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/transport"
)

// startTransport initializes the WebRTC transport (for peer daemon
// communication) and the relay Unix socket (for consumer proxy outbound
// routing). The WebRTC transport handles both inbound and outbound
// connections via the same pool of PeerConnections; signaling is done
// through Matrix state events in the machine room.
func (d *Daemon) startTransport(ctx context.Context, relaySocketPath string) error {
	// Fetch initial TURN credentials from the homeserver.
	turn, err := d.session.TURNCredentials(ctx)
	if err != nil {
		d.logger.Warn("failed to fetch TURN credentials, using host candidates only", "error", err)
		// Proceed without TURN — host/srflx candidates may suffice on LAN.
	}
	iceConfig := transport.ICEConfigFromTURN(turn, transport.ExcludeVirtualInterfaceFilter)

	// Create the Matrix signaler for SDP exchange.
	signaler := transport.NewMatrixSignaler(d.session, d.machineRoomID, d.logger)

	// Create the peer authenticator for mutual Ed25519 authentication
	// on each new PeerConnection. Uses the daemon's token signing
	// keypair (already published to #bureau/system) so no new key
	// management infrastructure is needed.
	authenticator := &peerAuthenticator{
		privateKey:   d.tokenSigningPrivateKey,
		session:      d.session,
		systemRoomID: d.systemRoomID,
		serverName:   d.machine.Server().String(),
	}

	// Create the WebRTC transport (implements both Listener and Dialer).
	webrtcTransport := transport.NewWebRTCTransport(signaler, d.machine.Localpart(), iceConfig, authenticator, d.logger)
	d.transportListener = webrtcTransport
	d.transportDialer = webrtcTransport
	d.relaySocketPath = relaySocketPath

	d.logger.Info("WebRTC transport started",
		"machine", d.machine.Localpart(),
		"relay_socket", relaySocketPath,
		"turn_configured", turn != nil,
	)

	// Start the inbound handler on the transport listener. This serves
	// requests from peer daemons and routes them to local provider proxies.
	// Also handles observation requests from peer daemons.
	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", d.handleTransportInbound)
	inboundMux.HandleFunc("/observe/", d.handleTransportObserve)
	inboundMux.HandleFunc("/tunnel/", d.handleTransportTunnel)

	go func() {
		if err := webrtcTransport.Serve(ctx, inboundMux); err != nil {
			d.logger.Error("transport serve error", "error", err)
		}
	}()

	// Start TURN credential refresh goroutine.
	if turn != nil {
		go d.refreshTURNCredentials(ctx, webrtcTransport, turn.TTL)
	}

	// Create the relay Unix socket. Consumer proxies register remote
	// services pointing here; the relay handler forwards requests to
	// the correct peer daemon via the transport.
	if err := os.MkdirAll(filepath.Dir(relaySocketPath), 0755); err != nil {
		webrtcTransport.Close()
		return fmt.Errorf("creating relay socket directory: %w", err)
	}
	if err := os.Remove(relaySocketPath); err != nil && !os.IsNotExist(err) {
		webrtcTransport.Close()
		return fmt.Errorf("removing existing relay socket: %w", err)
	}

	relayListener, err := net.Listen("unix", relaySocketPath)
	if err != nil {
		webrtcTransport.Close()
		return fmt.Errorf("creating relay socket at %s: %w", relaySocketPath, err)
	}
	d.relayListener = relayListener

	if err := os.Chmod(relaySocketPath, 0660); err != nil {
		relayListener.Close()
		webrtcTransport.Close()
		return fmt.Errorf("setting relay socket permissions: %w", err)
	}

	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/http/", d.handleRelay)
	d.relayServer = &http.Server{
		Handler:      relayMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming.
	}

	go func() {
		if err := d.relayServer.Serve(relayListener); err != nil && err != http.ErrServerClosed {
			d.logger.Error("relay server error", "error", err)
		}
	}()

	d.logger.Info("relay socket started", "socket", relaySocketPath)
	return nil
}

// stopTransport shuts down the transport listener, relay socket, and
// any active tunnel socket.
func (d *Daemon) stopTransport() {
	d.stopAllTunnels()
	if d.transportListener != nil {
		d.transportListener.Close()
	}
	if d.relayServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.relayServer.Shutdown(ctx)
	}
	if d.relayListener != nil {
		os.Remove(d.relaySocketPath)
	}
}

// refreshTURNCredentials periodically fetches fresh TURN credentials from
// the Matrix homeserver and updates the WebRTC transport's ICE configuration.
// TURN credentials are time-limited HMAC tokens; the TTL comes from the
// homeserver's /voip/turnServer response.
func (d *Daemon) refreshTURNCredentials(ctx context.Context, webrtcTransport *transport.WebRTCTransport, ttlSeconds int) {
	// Refresh at half the TTL to avoid using expired credentials, with a
	// floor of 5 minutes to prevent hammering the homeserver.
	interval := time.Duration(ttlSeconds/2) * time.Second
	if interval < 5*time.Minute {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			turn, err := d.session.TURNCredentials(ctx)
			if err != nil {
				d.logger.Warn("TURN credential refresh failed", "error", err)
				continue
			}
			webrtcTransport.UpdateICEConfig(transport.ICEConfigFromTURN(turn, transport.ExcludeVirtualInterfaceFilter))
			d.logger.Debug("TURN credentials refreshed")
		}
	}
}

// handleRelay is the HTTP handler for the relay Unix socket. Consumer
// proxies send requests here for remote services. The handler parses the
// service name from the path, looks up which peer machine hosts it, and
// reverse-proxies the request to that peer's transport listener.
//
// Request path format: /http/<service-name>/...
// The entire path is forwarded unchanged to the peer daemon's inbound handler.
func (d *Daemon) handleRelay(w http.ResponseWriter, r *http.Request) {
	serviceName, err := parseServiceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Find which machine provides this service.
	_, service, ok := d.serviceByProxyName(serviceName)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown service: %s", serviceName), http.StatusNotFound)
		return
	}

	// Look up the machine's transport address.
	peerAddress, ok := d.peerAddresses[service.Machine.UserID().String()]
	if !ok || peerAddress == "" {
		d.logger.Warn("no transport address for remote service's machine",
			"service", serviceName,
			"machine", service.Machine,
		)
		http.Error(w, fmt.Sprintf("no transport address for machine %s", service.Machine), http.StatusBadGateway)
		return
	}

	// Get or create a cached HTTP transport for this peer.
	roundTripper := d.peerHTTPTransport(peerAddress)

	// Reverse proxy the request to the peer daemon. The path stays the
	// same: /http/<service>/... → peer daemon's inbound handler picks
	// it up and routes to the local provider proxy.
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = peerAddress
			// Path, query, and headers are preserved from the original request.
		},
		Transport: roundTripper,
	}

	d.logger.Info("relay forwarding to peer",
		"service", serviceName,
		"peer_address", peerAddress,
		"method", r.Method,
		"path", r.URL.Path,
	)

	proxy.ServeHTTP(w, r)
}

// handleTransportInbound is the HTTP handler for the transport listener.
// Peer daemons send requests here for services running on this machine.
// The handler parses the service name from the path, finds the local
// provider proxy, and reverse-proxies the request to it.
//
// Request path format: /http/<service-name>/...
// The entire path is forwarded unchanged to the provider proxy.
func (d *Daemon) handleTransportInbound(w http.ResponseWriter, r *http.Request) {
	serviceName, err := parseServiceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	providerSocket, ok := d.localProviderSocket(serviceName)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown local service: %s", serviceName), http.StatusNotFound)
		return
	}

	// Reverse proxy to the provider proxy via Unix socket. The path
	// is forwarded unchanged — the provider proxy's HandleHTTPProxy
	// will strip /http/<service>/ and route to the backend.
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = "localhost"
			// Path, query, and headers are preserved from the original request.
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", providerSocket)
			},
		},
	}

	d.logger.Info("inbound routing to local provider",
		"service", serviceName,
		"provider_socket", providerSocket,
		"method", r.Method,
		"path", r.URL.Path,
	)

	proxy.ServeHTTP(w, r)
}

// syncPeerAddresses reads MachineStatus state events from the machine room
// to discover transport addresses of peer daemons. Called on initial sync and
// whenever the machine room has state changes, so the relay handler has
// up-to-date routing information.
//
// Builds the complete set of current peers from state events, then reconciles
// against the in-memory map: adding new peers, updating changed addresses,
// and removing peers that are no longer present. Cached HTTP transports for
// stale addresses are closed to release connections.
func (d *Daemon) syncPeerAddresses(ctx context.Context) error {
	events, err := d.session.GetRoomState(ctx, d.machineRoomID)
	if err != nil {
		return fmt.Errorf("fetching machine room state: %w", err)
	}

	// Build the authoritative set of peer addresses from state events.
	currentPeers := make(map[string]string) // machine principal → transport address
	for _, event := range events {
		if event.Type != schema.EventTypeMachineStatus {
			continue
		}

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}

		var status schema.MachineStatus
		if err := json.Unmarshal(contentJSON, &status); err != nil {
			continue
		}

		// Skip our own machine and entries without transport addresses.
		if status.Principal == d.machine.UserID().String() || status.TransportAddress == "" {
			continue
		}

		currentPeers[status.Principal] = status.TransportAddress
	}

	// Collect addresses that may become unreferenced after mutations.
	staleAddresses := make(map[string]bool)

	// Add new peers and update changed addresses.
	updated := 0
	for machineID, address := range currentPeers {
		oldAddress, exists := d.peerAddresses[machineID]
		if exists && oldAddress == address {
			continue
		}
		if exists {
			d.logger.Info("peer transport address changed",
				"machine", machineID,
				"old_address", oldAddress,
				"new_address", address,
			)
			staleAddresses[oldAddress] = true
		} else {
			d.logger.Info("peer transport address discovered",
				"machine", machineID,
				"address", address,
			)
		}
		d.peerAddresses[machineID] = address
		updated++
	}

	// Remove peers no longer present in state events.
	removed := 0
	for machineID, address := range d.peerAddresses {
		if _, exists := currentPeers[machineID]; !exists {
			d.logger.Info("peer transport address removed",
				"machine", machineID,
				"address", address,
			)
			staleAddresses[address] = true
			delete(d.peerAddresses, machineID)
			removed++
		}
	}

	// Clean up cached transports for addresses no longer referenced by
	// any peer. An address might appear in staleAddresses but still be
	// used by a different peer (e.g., two machines shared an address and
	// only one was removed), so we check against the post-mutation map.
	if len(staleAddresses) > 0 {
		activeAddresses := make(map[string]bool, len(d.peerAddresses))
		for _, address := range d.peerAddresses {
			activeAddresses[address] = true
		}

		d.peerTransportsMu.Lock()
		for address := range staleAddresses {
			if activeAddresses[address] {
				continue
			}
			if roundTripper, exists := d.peerTransports[address]; exists {
				delete(d.peerTransports, address)
				if httpTransport, ok := roundTripper.(*http.Transport); ok {
					httpTransport.CloseIdleConnections()
				}
			}
		}
		d.peerTransportsMu.Unlock()
	}

	if updated > 0 || removed > 0 {
		d.logger.Info("peer addresses synced",
			"peers", len(d.peerAddresses),
			"updated", updated,
			"removed", removed,
		)
	}
	return nil
}

// serviceByProxyName looks up a service entry by its flat proxy name
// (e.g., "service-stt-whisper"). Returns the service localpart, the
// service entry, and whether it was found.
func (d *Daemon) serviceByProxyName(proxyName string) (string, *schema.Service, bool) {
	for localpart, service := range d.services {
		if principal.ProxyServiceName(localpart) == proxyName {
			return localpart, service, true
		}
	}
	return "", nil, false
}

// localProviderSocket returns the proxy Unix socket path for a local
// service identified by its flat proxy name (e.g., "service-stt-whisper").
// Returns the socket path and whether the service was found as a local
// provider on this machine.
func (d *Daemon) localProviderSocket(proxyName string) (string, bool) {
	for localpart, service := range d.services {
		if principal.ProxyServiceName(localpart) != proxyName {
			continue
		}
		if service.Machine != d.machine {
			continue // Remote, not local.
		}
		return service.Principal.ServiceSocketPath(d.fleetRunDir), true
	}
	return "", false
}

// peerHTTPTransport returns a cached http.RoundTripper for the given peer
// transport address. Each peer gets its own transport with connection
// pooling. Thread-safe.
func (d *Daemon) peerHTTPTransport(address string) http.RoundTripper {
	d.peerTransportsMu.RLock()
	roundTripper, ok := d.peerTransports[address]
	d.peerTransportsMu.RUnlock()
	if ok {
		return roundTripper
	}

	d.peerTransportsMu.Lock()
	defer d.peerTransportsMu.Unlock()

	// Double-check after acquiring write lock.
	if roundTripper, ok := d.peerTransports[address]; ok {
		return roundTripper
	}

	roundTripper = transport.HTTPTransport(d.transportDialer, address)
	d.peerTransports[address] = roundTripper
	return roundTripper
}

// parseServiceFromPath extracts the flat service name from an HTTP path
// like /http/<service-name>/... Used by both the relay and inbound handlers.
func parseServiceFromPath(path string) (string, error) {
	if !strings.HasPrefix(path, "/http/") {
		return "", fmt.Errorf("path must start with /http/")
	}
	remaining := strings.TrimPrefix(path, "/http/")
	parts := strings.SplitN(remaining, "/", 2)
	if parts[0] == "" {
		return "", fmt.Errorf("empty service name in path")
	}
	return parts[0], nil
}

// handleTransportTunnel handles tunnel requests arriving from peer daemons
// over the transport listener. A peer daemon sends an HTTP POST to
// /tunnel/<localpart> to establish a raw byte bridge to a service's Unix
// socket on this machine. This is used for non-HTTP protocols (e.g., the
// artifact service's length-prefixed CBOR wire protocol) that cannot be
// reverse-proxied through the standard /http/ path.
//
// The handler:
//   - Extracts the service localpart from the URL path
//   - Connects to the service's Unix socket via fleet-scoped socket path
//   - Hijacks the HTTP connection and clears deadlines
//   - Writes a manual HTTP 200 response on the hijacked connection
//   - Bridges bytes between the hijacked transport connection and the
//     local service socket using netutil.BridgeConnections
//
// Authentication relies on the transport layer's mutual Ed25519 handshake
// — only authenticated peer daemons can reach the inbound mux. The handler
// does not perform additional authorization because it provides raw socket
// access to a service already running on this machine; the service itself
// handles authorization for its wire protocol.
func (d *Daemon) handleTransportTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract localpart from path: /tunnel/<localpart>
	serviceLocalpart := strings.TrimPrefix(r.URL.Path, "/tunnel/")
	if serviceLocalpart == "" {
		http.Error(w, "empty service localpart in path", http.StatusBadRequest)
		return
	}

	// Connect to the local service's Unix socket.
	serviceRef, parseErr := ref.ParseEntityLocalpart(serviceLocalpart, d.machine.Server())
	if parseErr != nil {
		http.Error(w, fmt.Sprintf("invalid service localpart: %v", parseErr), http.StatusBadRequest)
		return
	}
	socketPath := serviceRef.ServiceSocketPath(d.fleetRunDir)
	serviceConnection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		d.logger.Error("tunnel: cannot connect to local service socket",
			"localpart", serviceLocalpart,
			"socket", socketPath,
			"error", err,
		)
		http.Error(w, fmt.Sprintf("cannot connect to service %s: %v", serviceLocalpart, err), http.StatusBadGateway)
		return
	}

	// Hijack the HTTP connection to switch to raw byte bridging.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		serviceConnection.Close()
		d.logger.Error("tunnel: response writer does not support hijacking")
		http.Error(w, "server does not support connection hijacking", http.StatusInternalServerError)
		return
	}

	transportConnection, transportBuffer, err := hijacker.Hijack()
	if err != nil {
		serviceConnection.Close()
		d.logger.Error("tunnel: hijack failed", "error", err)
		return
	}

	// Clear any read/write deadlines set by http.Server during Hijack.
	// The server sets SetReadDeadline(aLongTimeAgo) to abort its internal
	// background reader before returning the raw connection. Without
	// clearing this, subsequent reads return os.ErrDeadlineExceeded
	// immediately and the bridge dies in microseconds.
	transportConnection.SetDeadline(time.Time{})

	// Write the HTTP response manually on the hijacked connection.
	fmt.Fprintf(transportBuffer, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(transportBuffer, "\r\n")
	transportBuffer.Flush()

	d.logger.Info("tunnel: bridging to local service",
		"localpart", serviceLocalpart,
		"socket", socketPath,
	)

	// For artifact services, inject a daemon-minted token into the
	// first message before forwarding. The remote client connects
	// without a token (tunnel connections are daemon-to-daemon
	// authenticated), so we mint one with the local daemon's signing
	// key that the local artifact service can verify.
	// Wrap the transport connection with the bufio.Reader to
	// preserve any bytes the HTTP server read ahead during hijack.
	transportReader := &bufferedConn{reader: transportBuffer.Reader, Conn: transportConnection}

	if d.serviceHasCapability(serviceLocalpart, "content-addressed-store") {
		if err := d.bridgeWithTokenInjection(transportReader, serviceConnection); err != nil {
			d.logger.Warn("tunnel: bridge with token injection error",
				"localpart", serviceLocalpart,
				"error", err,
			)
		}
	} else {
		if err := netutil.BridgeConnections(transportReader, serviceConnection); err != nil {
			d.logger.Warn("tunnel: bridge error",
				"localpart", serviceLocalpart,
				"error", err,
			)
		}
	}

	d.logger.Debug("tunnel: connection closed",
		"localpart", serviceLocalpart,
	)
}

// serviceHasCapability returns true if the named service (by localpart)
// is in the service directory and has the specified capability string.
func (d *Daemon) serviceHasCapability(localpart, capability string) bool {
	service, exists := d.services[localpart]
	if !exists {
		return false
	}
	for _, cap := range service.Capabilities {
		if cap == capability {
			return true
		}
	}
	return false
}

// bridgeWithTokenInjection reads the first length-prefixed CBOR
// message from the transport side (the remote client's request),
// injects a daemon-minted service token, writes the modified message
// to the service socket, then bridges remaining bytes bidirectionally.
//
// The artifact wire protocol uses 4-byte uint32 big-endian length
// prefixes followed by CBOR message bytes. The first message is
// always a request (store, fetch, exists, etc.) that includes a
// "token" field for authentication. For tunnel connections, the
// remote client sends no token — this function mints one with the
// local daemon's signing key so the local artifact service can
// verify the request.
func (d *Daemon) bridgeWithTokenInjection(transportConn, serviceConn net.Conn) error {
	// Read the first length-prefixed CBOR message from the remote client.
	raw, err := artifactstore.ReadRawMessage(transportConn)
	if err != nil {
		serviceConn.Close()
		return fmt.Errorf("reading first message from transport: %w", err)
	}

	// Decode as a generic map to inject the token field.
	var message map[string]any
	if err := codec.Unmarshal(raw, &message); err != nil {
		serviceConn.Close()
		return fmt.Errorf("decoding first message: %w", err)
	}

	// Mint a service token for the artifact audience.
	tokenID, err := generateTokenID()
	if err != nil {
		serviceConn.Close()
		return fmt.Errorf("generating token ID for tunnel injection: %w", err)
	}

	now := d.clock.Now()
	token := &servicetoken.Token{
		Subject:   d.machine.UserID(),
		Machine:   d.machine,
		Audience:  "artifact",
		Grants:    []servicetoken.Grant{{Actions: []string{"artifact/*"}}},
		ID:        tokenID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}

	tokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, token)
	if err != nil {
		serviceConn.Close()
		return fmt.Errorf("minting tunnel token: %w", err)
	}

	// Inject the token into the decoded message and re-encode.
	message["token"] = tokenBytes
	modifiedBytes, err := codec.Marshal(message)
	if err != nil {
		serviceConn.Close()
		return fmt.Errorf("re-encoding message with injected token: %w", err)
	}

	// Write the modified message to the local service socket.
	if err := artifactstore.WriteRawMessage(serviceConn, modifiedBytes); err != nil {
		serviceConn.Close()
		return fmt.Errorf("writing modified message to service: %w", err)
	}

	// Bridge remaining bytes between transport and service.
	return netutil.BridgeConnections(transportConn, serviceConn)
}

// tunnelInstance holds the state for a single outbound tunnel socket.
// Each tunnel creates a local Unix socket that bridges accepted
// connections to a remote service via the daemon-to-daemon transport.
type tunnelInstance struct {
	socketPath string
	listener   net.Listener
	cancel     context.CancelFunc
}

// startTunnel creates a named outbound tunnel socket that bridges each
// accepted connection to a remote service via the daemon-to-daemon
// transport. The tunnel is stored in d.tunnels[name], replacing any
// existing tunnel with the same name.
//
// Each client connection (one per action: exists, fetch, etc.) becomes
// one transport data channel. This maps naturally to the transport's
// one-data-channel-per-dial model. No persistent tunnel, no
// multiplexing.
func (d *Daemon) startTunnel(name, serviceLocalpart, peerAddress, tunnelSocketPath string) error {
	// Stop any existing tunnel with this name (idempotent).
	d.stopTunnel(name)

	if err := os.MkdirAll(filepath.Dir(tunnelSocketPath), 0755); err != nil {
		return fmt.Errorf("creating tunnel socket directory: %w", err)
	}
	if err := os.Remove(tunnelSocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing tunnel socket: %w", err)
	}

	listener, err := net.Listen("unix", tunnelSocketPath)
	if err != nil {
		return fmt.Errorf("creating tunnel socket at %s: %w", tunnelSocketPath, err)
	}

	if err := os.Chmod(tunnelSocketPath, 0660); err != nil {
		listener.Close()
		os.Remove(tunnelSocketPath)
		return fmt.Errorf("setting tunnel socket permissions: %w", err)
	}

	tunnelContext, tunnelCancel := context.WithCancel(context.Background())
	d.tunnels[name] = &tunnelInstance{
		socketPath: tunnelSocketPath,
		listener:   listener,
		cancel:     tunnelCancel,
	}

	go d.tunnelAcceptLoop(tunnelContext, listener, serviceLocalpart, peerAddress)

	d.logger.Info("tunnel socket started",
		"name", name,
		"socket", tunnelSocketPath,
		"remote_localpart", serviceLocalpart,
		"peer_address", peerAddress,
	)
	return nil
}

// tunnelAcceptLoop accepts connections on the tunnel socket and bridges each
// to the remote service. Each connection is handled in its own goroutine:
// dial the peer via transport, send HTTP POST to /tunnel/<localpart>, read
// the 200 OK response, then bridge bytes.
func (d *Daemon) tunnelAcceptLoop(ctx context.Context, listener net.Listener, serviceLocalpart, peerAddress string) {
	for {
		localConnection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // Shutdown requested.
			}
			d.logger.Error("tunnel: accept error", "error", err)
			return
		}
		go d.handleTunnelConnection(ctx, localConnection, serviceLocalpart, peerAddress)
	}
}

// handleTunnelConnection bridges a single connection from the local artifact
// service to the remote shared cache service. It dials the peer daemon via
// the transport, performs an HTTP POST handshake on /tunnel/<localpart>,
// reads the 200 OK, and bridges bytes until either side closes.
func (d *Daemon) handleTunnelConnection(ctx context.Context, localConnection net.Conn, serviceLocalpart, peerAddress string) {
	// Dial the remote daemon via the transport layer.
	dialContext, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()

	if d.transportDialer == nil {
		d.logger.Error("tunnel: no transport dialer available")
		localConnection.Close()
		return
	}

	transportConnection, err := d.transportDialer.DialContext(dialContext, peerAddress)
	if err != nil {
		d.logger.Error("tunnel: failed to dial peer",
			"peer_address", peerAddress,
			"error", err,
		)
		localConnection.Close()
		return
	}

	// Send HTTP POST to /tunnel/<localpart> on the remote daemon.
	httpRequest, err := http.NewRequest("POST",
		"http://transport/tunnel/"+serviceLocalpart, nil)
	if err != nil {
		transportConnection.Close()
		localConnection.Close()
		d.logger.Error("tunnel: failed to build request", "error", err)
		return
	}
	if err := httpRequest.Write(transportConnection); err != nil {
		transportConnection.Close()
		localConnection.Close()
		d.logger.Error("tunnel: failed to send request to peer",
			"peer_address", peerAddress,
			"error", err,
		)
		return
	}

	// Read the HTTP response. The bufio.Reader may read ahead past the
	// HTTP headers — we preserve those bytes via bufferedConn for the
	// bridge.
	bufferedReader := bufio.NewReader(transportConnection)
	httpResponse, err := http.ReadResponse(bufferedReader, httpRequest)
	if err != nil {
		transportConnection.Close()
		localConnection.Close()
		d.logger.Error("tunnel: failed to read peer response",
			"peer_address", peerAddress,
			"error", err,
		)
		return
	}
	httpResponse.Body.Close()

	if httpResponse.StatusCode != http.StatusOK {
		transportConnection.Close()
		localConnection.Close()
		d.logger.Error("tunnel: peer returned error",
			"peer_address", peerAddress,
			"status", httpResponse.StatusCode,
		)
		return
	}

	// Bridge bytes between the local artifact service and the remote
	// shared cache. Use bufferedConn to include any bytes the
	// bufio.Reader read ahead beyond the HTTP response headers.
	peerConnection := &bufferedConn{reader: bufferedReader, Conn: transportConnection}
	if err := netutil.BridgeConnections(localConnection, peerConnection); err != nil {
		d.logger.Warn("tunnel: bridge error",
			"remote_localpart", serviceLocalpart,
			"peer_address", peerAddress,
			"error", err,
		)
	}
}

// stopTunnel shuts down a named tunnel's accept loop and removes its
// Unix socket. Safe to call when the named tunnel does not exist.
func (d *Daemon) stopTunnel(name string) {
	tunnel, exists := d.tunnels[name]
	if !exists {
		return
	}

	tunnel.cancel()
	tunnel.listener.Close()
	os.Remove(tunnel.socketPath)
	delete(d.tunnels, name)
}

// stopAllTunnels shuts down all active tunnels. Called during
// transport shutdown.
func (d *Daemon) stopAllTunnels() {
	for name := range d.tunnels {
		d.stopTunnel(name)
	}
}

// peerAuthenticator implements transport.PeerAuthenticator using the
// daemon's Ed25519 token signing keypair. Peer public keys are fetched
// from #bureau/system via m.bureau.token_signing_key state events.
type peerAuthenticator struct {
	privateKey   ed25519.PrivateKey
	session      peerAuthSession
	systemRoomID ref.RoomID
	serverName   string
}

// peerAuthSession is the subset of messaging.Session needed by
// peerAuthenticator, extracted for testability.
type peerAuthSession interface {
	GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error)
}

func (a *peerAuthenticator) Sign(message []byte) []byte {
	return ed25519.Sign(a.privateKey, message)
}

func (a *peerAuthenticator) VerifyPeer(peerLocalpart string, message, signature []byte) error {
	// Fetch the peer's token signing public key from Matrix room state.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Token signing keys are published with the machine's full user ID
	// (e.g., "@bureau/fleet/prod/machine/box:server") as the state key.
	// The transport layer identifies peers by localpart, so we construct
	// the full user ID here.
	stateKey := "@" + peerLocalpart + ":" + a.serverName
	rawContent, err := a.session.GetStateEvent(ctx, a.systemRoomID,
		schema.EventTypeTokenSigningKey, stateKey)
	if err != nil {
		return fmt.Errorf("fetching token signing key for %s: %w", peerLocalpart, err)
	}

	var content schema.TokenSigningKeyContent
	if err := json.Unmarshal(rawContent, &content); err != nil {
		return fmt.Errorf("parsing token signing key for %s: %w", peerLocalpart, err)
	}

	publicKeyBytes, err := hex.DecodeString(content.PublicKey)
	if err != nil {
		return fmt.Errorf("decoding public key hex for %s: %w", peerLocalpart, err)
	}

	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key length for %s: got %d bytes, want %d",
			peerLocalpart, len(publicKeyBytes), ed25519.PublicKeySize)
	}

	publicKey := ed25519.PublicKey(publicKeyBytes)
	if !ed25519.Verify(publicKey, message, signature) {
		return fmt.Errorf("Ed25519 signature verification failed for %s", peerLocalpart)
	}

	return nil
}
