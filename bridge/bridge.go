// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package bridge provides a TCP-to-Unix socket forwarder.
//
// This enables HTTP clients inside a network-isolated sandbox to reach
// the credential proxy via localhost. The bridge listens on a TCP port
// and forwards all connections to a Unix socket.
//
// Inside a sandbox with --unshare-net, this allows:
//
//	ANTHROPIC_BASE_URL=http://127.0.0.1:8642/http/anthropic
//
// The agent can reach the proxy via localhost, but cannot reach the internet.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Bridge forwards TCP connections to a Unix socket.
type Bridge struct {
	// ListenAddr is the TCP address to listen on (e.g. "127.0.0.1:8642").
	ListenAddr string

	// SocketPath is the path to the Unix socket to forward connections to.
	SocketPath string

	// Verbose enables per-connection logging.
	Verbose bool

	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
}

// Start begins listening for TCP connections and forwarding them to the
// Unix socket. It returns once the listener is bound and accepting, or
// returns an error if binding fails. The bridge runs in the background
// until Stop is called or the context is cancelled.
func (b *Bridge) Start(ctx context.Context) error {
	if b.ListenAddr == "" {
		return fmt.Errorf("bridge: ListenAddr is required")
	}
	if b.SocketPath == "" {
		return fmt.Errorf("bridge: SocketPath is required")
	}

	// Validate socket exists before starting.
	if _, err := net.DialTimeout("unix", b.SocketPath, 5*time.Second); err != nil {
		return fmt.Errorf("bridge: socket %s not reachable: %w", b.SocketPath, err)
	}

	listener, err := net.Listen("tcp", b.ListenAddr)
	if err != nil {
		return fmt.Errorf("bridge: failed to listen on %s: %w", b.ListenAddr, err)
	}

	b.listener = listener

	ctx, b.cancel = context.WithCancel(ctx)
	b.done = make(chan struct{})

	go func() {
		defer close(b.done)
		b.acceptLoop(ctx)
	}()

	log.Printf("bridge: %s -> %s", b.ListenAddr, b.SocketPath)
	return nil
}

// Addr returns the listener's address, useful when binding to port 0.
// Returns nil if the bridge has not been started.
func (b *Bridge) Addr() net.Addr {
	if b.listener == nil {
		return nil
	}
	return b.listener.Addr()
}

// Stop shuts down the bridge, closing the listener and waiting for all
// in-flight connections to drain.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.listener != nil {
		b.listener.Close()
	}
	if b.done != nil {
		<-b.done
	}
}

// Wait blocks until the bridge has stopped.
func (b *Bridge) Wait() {
	if b.done != nil {
		<-b.done
	}
}

// acceptLoop accepts connections and bridges them to the Unix socket.
func (b *Bridge) acceptLoop(ctx context.Context) {
	var connectionCount int64

	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("bridge: accept error: %v", err)
				continue
			}
		}

		connectionCount++
		connectionID := connectionCount
		go b.handleConnection(ctx, conn, connectionID)
	}
}

func (b *Bridge) handleConnection(_ context.Context, tcpConn net.Conn, connectionID int64) {
	defer tcpConn.Close()

	if b.Verbose {
		log.Printf("bridge: [%d] new connection from %s", connectionID, tcpConn.RemoteAddr())
	}

	// Connect to Unix socket.
	unixConn, err := net.DialTimeout("unix", b.SocketPath, 5*time.Second)
	if err != nil {
		log.Printf("bridge: [%d] failed to connect to socket: %v", connectionID, err)
		return
	}
	defer unixConn.Close()

	// Bridge the connections bidirectionally.
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)

	// TCP -> Unix
	go func() {
		defer waitGroup.Done()
		bytesCopied, err := io.Copy(unixConn, tcpConn)
		if b.Verbose && err != nil && !isClosedError(err) {
			log.Printf("bridge: [%d] tcp->unix error after %d bytes: %v", connectionID, bytesCopied, err)
		}
		if uc, ok := unixConn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
	}()

	// Unix -> TCP
	go func() {
		defer waitGroup.Done()
		bytesCopied, err := io.Copy(tcpConn, unixConn)
		if b.Verbose && err != nil && !isClosedError(err) {
			log.Printf("bridge: [%d] unix->tcp error after %d bytes: %v", connectionID, bytesCopied, err)
		}
		if tc, ok := tcpConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	waitGroup.Wait()

	if b.Verbose {
		log.Printf("bridge: [%d] connection closed", connectionID)
	}
}

// isClosedError checks if an error is due to a closed connection or EOF.
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return false
}
