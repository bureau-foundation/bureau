// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"net"
	"net/http"
)

// Listener accepts inbound connections from peer daemons. The daemon
// creates a Listener and calls Serve with an HTTP handler that routes
// incoming requests to local provider proxies via their Unix sockets.
type Listener interface {
	// Serve starts accepting connections and dispatches to handler.
	// Blocks until ctx is cancelled or Close is called. Returns nil
	// on clean shutdown.
	Serve(ctx context.Context, handler http.Handler) error

	// Address returns the transport address to publish in MachineStatus
	// so peer daemons can connect. The format is transport-specific
	// (e.g., "192.168.1.10:7891" for TCP).
	Address() string

	// Close shuts down the listener. Subsequent calls to Serve return
	// immediately.
	Close() error
}

// Dialer opens connections to peer daemons. The daemon uses a Dialer to
// construct HTTP round-trippers for reverse proxying service requests to
// remote machines.
type Dialer interface {
	// DialContext opens a network connection to a peer daemon at the
	// given transport address. The address format matches what the
	// peer's Listener.Address() returns.
	DialContext(ctx context.Context, address string) (net.Conn, error)
}

// HTTPTransport creates an http.RoundTripper that routes all requests
// through the given Dialer to the specified transport address. The URL
// host in requests is ignored — all connections go through the dialer
// to the specified address. This is used by the daemon's relay handler
// to forward service requests to peer daemons.
func HTTPTransport(dialer Dialer, address string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, address)
		},
	}
}
