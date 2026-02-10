// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Compile-time interface checks.
var (
	_ Listener = (*TCPListener)(nil)
	_ Dialer   = (*TCPDialer)(nil)
)

// TCPListener accepts inbound TCP connections from peer daemons. This is
// the development and same-LAN transport — it requires direct TCP
// reachability between daemons. For NAT traversal, use a WebRTC-based
// transport (not yet implemented).
type TCPListener struct {
	listener net.Listener
	server   *http.Server
}

// NewTCPListener creates a TCP transport listener on the specified address
// (e.g., ":7891" or "192.168.1.10:7891"). Use ":0" for a random available
// port.
func NewTCPListener(address string) (*TCPListener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return &TCPListener{listener: listener}, nil
}

// Serve starts accepting TCP connections and dispatches to handler.
// Blocks until ctx is cancelled or Close is called.
func (l *TCPListener) Serve(ctx context.Context, handler http.Handler) error {
	l.server = &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming responses.
	}

	go func() {
		<-ctx.Done()
		l.server.Close()
	}()

	err := l.server.Serve(l.listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Address returns the TCP address in "host:port" format.
func (l *TCPListener) Address() string {
	return l.listener.Addr().String()
}

// Close shuts down the TCP listener.
func (l *TCPListener) Close() error {
	if l.server != nil {
		return l.server.Close()
	}
	return l.listener.Close()
}

// TCPDialer opens TCP connections to peer daemons. Used by the daemon's
// relay handler to forward service requests to remote machines.
type TCPDialer struct {
	// Timeout is the maximum time to wait for a TCP connection to be
	// established. Zero means no standalone timeout — only the context
	// deadline applies.
	Timeout time.Duration
}

// DialContext opens a TCP connection to the given address (host:port).
func (d *TCPDialer) DialContext(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: d.Timeout}).DialContext(ctx, "tcp", address)
}
