// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/netutil"
)

// Bridge forwards TCP connections to a Unix socket.
type Bridge struct {
	// ListenAddr is the TCP address to listen on (e.g. "127.0.0.1:8642").
	ListenAddr string

	// SocketPath is the path to the Unix socket to forward connections to.
	SocketPath string

	// Logger receives structured log output. If nil, slog.Default() is
	// used. Per-connection events are logged at Debug level; errors and
	// lifecycle events at Info/Error.
	Logger *slog.Logger

	listener    net.Listener
	cancel      context.CancelFunc
	done        chan struct{}
	connections sync.WaitGroup
}

// logger returns the configured logger or the default.
func (b *Bridge) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
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
	probeConnection, err := net.DialTimeout("unix", b.SocketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("bridge: socket %s not reachable: %w", b.SocketPath, err)
	}
	probeConnection.Close()

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

	b.logger().Info("bridge started",
		"listen_addr", b.ListenAddr,
		"socket_path", b.SocketPath,
	)
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
// It waits for all in-flight connection goroutines to finish before
// returning, so that closing the done channel signals full quiescence.
func (b *Bridge) acceptLoop(ctx context.Context) {
	var connectionCount int64

	for {
		connection, err := b.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				b.connections.Wait()
				return
			default:
				b.logger().Error("accept failed", "error", err)
				continue
			}
		}

		connectionCount++
		connectionID := connectionCount
		b.connections.Add(1)
		go func() {
			defer b.connections.Done()
			b.handleConnection(connection, connectionID)
		}()
	}
}

func (b *Bridge) handleConnection(tcpConnection net.Conn, connectionID int64) {
	defer tcpConnection.Close()

	logger := b.logger().With("connection_id", connectionID)
	logger.Debug("connection accepted",
		"remote_addr", tcpConnection.RemoteAddr(),
	)

	// Connect to Unix socket.
	unixConnection, err := net.DialTimeout("unix", b.SocketPath, 5*time.Second)
	if err != nil {
		logger.Error("failed to connect to socket", "error", err)
		return
	}
	defer unixConnection.Close()

	// Bridge the connections bidirectionally.
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)

	// TCP -> Unix
	go func() {
		defer waitGroup.Done()
		bytesCopied, copyError := io.Copy(unixConnection, tcpConnection)
		if copyError != nil && !netutil.IsExpectedCloseError(copyError) {
			logger.Debug("tcp->unix copy error",
				"bytes_copied", bytesCopied,
				"error", copyError,
			)
		}
		if unixConn, ok := unixConnection.(*net.UnixConn); ok {
			unixConn.CloseWrite()
		}
	}()

	// Unix -> TCP
	go func() {
		defer waitGroup.Done()
		bytesCopied, copyError := io.Copy(tcpConnection, unixConnection)
		if copyError != nil && !netutil.IsExpectedCloseError(copyError) {
			logger.Debug("unix->tcp copy error",
				"bytes_copied", bytesCopied,
				"error", copyError,
			)
		}
		if tcpConn, ok := tcpConnection.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	waitGroup.Wait()

	logger.Debug("connection closed")
}
