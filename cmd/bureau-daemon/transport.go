// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/transport"
)

// startTransport initializes the WebRTC transport (for peer daemon
// communication) and the relay Unix socket (for consumer proxy outbound
// routing). The WebRTC transport handles both inbound and outbound
// connections via the same pool of PeerConnections; signaling is done
// through Matrix state events in the machines room.
func (d *Daemon) startTransport(ctx context.Context, relaySocketPath string) error {
	// Fetch initial TURN credentials from the homeserver.
	turn, err := d.session.TURNCredentials(ctx)
	if err != nil {
		d.logger.Warn("failed to fetch TURN credentials, using host candidates only", "error", err)
		// Proceed without TURN — host/srflx candidates may suffice on LAN.
	}
	iceConfig := transport.ICEConfigFromTURN(turn)

	// Create the Matrix signaler for SDP exchange.
	signaler := transport.NewMatrixSignaler(d.session, d.machinesRoomID, d.logger)

	// Create the WebRTC transport (implements both Listener and Dialer).
	webrtcTransport := transport.NewWebRTCTransport(signaler, d.machineName, iceConfig, d.logger)
	d.transportListener = webrtcTransport
	d.transportDialer = webrtcTransport
	d.relaySocketPath = relaySocketPath

	d.logger.Info("WebRTC transport started",
		"machine", d.machineName,
		"relay_socket", relaySocketPath,
		"turn_configured", turn != nil,
	)

	// Start the inbound handler on the transport listener. This serves
	// requests from peer daemons and routes them to local provider proxies.
	// Also handles observation requests from peer daemons.
	inboundMux := http.NewServeMux()
	inboundMux.HandleFunc("/http/", d.handleTransportInbound)
	inboundMux.HandleFunc("/observe/", d.handleTransportObserve)

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

// stopTransport shuts down the transport listener and relay socket.
func (d *Daemon) stopTransport() {
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
			webrtcTransport.UpdateICEConfig(transport.ICEConfigFromTURN(turn))
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
	peerAddress, ok := d.peerAddresses[service.Machine]
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

// syncPeerAddresses reads MachineStatus state events from the machines room
// to discover transport addresses of peer daemons. Called on each poll cycle
// so the relay handler has up-to-date routing information.
func (d *Daemon) syncPeerAddresses(ctx context.Context) error {
	events, err := d.session.GetRoomState(ctx, d.machinesRoomID)
	if err != nil {
		return fmt.Errorf("fetching machines room state: %w", err)
	}

	updated := 0
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
		if status.Principal == d.machineUserID || status.TransportAddress == "" {
			continue
		}

		if d.peerAddresses[status.Principal] != status.TransportAddress {
			d.logger.Info("peer transport address discovered",
				"machine", status.Principal,
				"address", status.TransportAddress,
			)
			d.peerAddresses[status.Principal] = status.TransportAddress
			updated++
		}
	}

	if updated > 0 {
		d.logger.Info("peer addresses synced", "peers", len(d.peerAddresses), "updated", updated)
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
		if service.Machine != d.machineUserID {
			continue // Remote, not local.
		}
		providerLocalpart, err := principal.LocalpartFromMatrixID(service.Principal)
		if err != nil {
			d.logger.Error("cannot derive socket from service principal",
				"service", localpart,
				"principal", service.Principal,
				"error", err,
			)
			return "", false
		}
		return principal.SocketPath(providerLocalpart), true
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
