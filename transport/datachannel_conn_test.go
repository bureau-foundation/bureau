// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestDataChannelConn_ReadWrite(t *testing.T) {
	// Use io.Pipe as a stand-in for a detached data channel — both provide
	// a synchronous stream-oriented ReadWriteCloser.
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	clientStream := &pipeReadWriteCloser{Reader: clientReader, Writer: clientWriter}
	serverStream := &pipeReadWriteCloser{Reader: serverReader, Writer: serverWriter}

	clientConn := NewDataChannelConn(clientStream, "client/dc-1", "server/dc-1")
	serverConn := NewDataChannelConn(serverStream, "server/dc-1", "client/dc-1")
	defer clientConn.Close()
	defer serverConn.Close()

	// Write from client, read from server.
	message := []byte("hello from client")
	go func() {
		if _, err := clientConn.Write(message); err != nil {
			t.Errorf("Write error: %v", err)
		}
	}()

	buffer := make([]byte, 256)
	bytesRead, err := serverConn.Read(buffer)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if string(buffer[:bytesRead]) != "hello from client" {
		t.Errorf("read = %q, want %q", string(buffer[:bytesRead]), "hello from client")
	}
}

func TestDataChannelConn_Addresses(t *testing.T) {
	stream := &pipeReadWriteCloser{Reader: io.NopCloser(nil).(io.Reader), Writer: io.Discard}
	conn := NewDataChannelConn(stream, "local/dc-1", "remote/dc-1")

	if conn.LocalAddr().Network() != "webrtc" {
		t.Errorf("LocalAddr().Network() = %q, want %q", conn.LocalAddr().Network(), "webrtc")
	}
	if conn.LocalAddr().String() != "local/dc-1" {
		t.Errorf("LocalAddr().String() = %q, want %q", conn.LocalAddr().String(), "local/dc-1")
	}
	if conn.RemoteAddr().Network() != "webrtc" {
		t.Errorf("RemoteAddr().Network() = %q, want %q", conn.RemoteAddr().Network(), "webrtc")
	}
	if conn.RemoteAddr().String() != "remote/dc-1" {
		t.Errorf("RemoteAddr().String() = %q, want %q", conn.RemoteAddr().String(), "remote/dc-1")
	}
}

func TestDataChannelConn_ImplementsNetConn(t *testing.T) {
	// Compile-time check already exists, but verify at runtime too.
	stream := &pipeReadWriteCloser{Reader: io.NopCloser(nil).(io.Reader), Writer: io.Discard}
	conn := NewDataChannelConn(stream, "a", "b")
	var _ net.Conn = conn
	conn.Close()
}

func TestDataChannelConn_DeadlineClosesStream(t *testing.T) {
	reader, writer := io.Pipe()
	stream := &pipeReadWriteCloser{Reader: reader, Writer: writer}
	conn := NewDataChannelConn(stream, "local", "remote")

	// Set a deadline that fires immediately.
	conn.SetReadDeadline(time.Now().Add(-1 * time.Second))

	// The underlying pipe should be closed, causing reads to fail.
	buffer := make([]byte, 10)
	_, err := conn.Read(buffer)
	if err == nil {
		t.Fatal("expected error from Read after expired deadline, got nil")
	}
}

func TestDataChannelConn_ClearDeadline(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	clientStream := &pipeReadWriteCloser{Reader: clientReader, Writer: clientWriter}
	serverStream := &pipeReadWriteCloser{Reader: serverReader, Writer: serverWriter}

	clientConn := NewDataChannelConn(clientStream, "client", "server")
	serverConn := NewDataChannelConn(serverStream, "server", "client")
	defer clientConn.Close()
	defer serverConn.Close()

	// Set and then clear a deadline. The clear (zero time) should prevent
	// the deadline from firing.
	clientConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	clientConn.SetReadDeadline(time.Time{})

	// Wait past the original deadline.
	time.Sleep(100 * time.Millisecond)

	// The connection should still be alive.
	message := []byte("still alive")
	go func() {
		serverConn.Write(message)
	}()

	buffer := make([]byte, 256)
	bytesRead, err := clientConn.Read(buffer)
	if err != nil {
		t.Fatalf("Read error after clearing deadline: %v", err)
	}
	if string(buffer[:bytesRead]) != "still alive" {
		t.Errorf("read = %q, want %q", string(buffer[:bytesRead]), "still alive")
	}
}

func TestDataChannelConn_CloseStopsTimers(t *testing.T) {
	reader, writer := io.Pipe()
	stream := &pipeReadWriteCloser{Reader: reader, Writer: writer}
	conn := NewDataChannelConn(stream, "local", "remote")

	// Set a future deadline, then close. The timer should be cleaned up.
	conn.SetDeadline(time.Now().Add(1 * time.Hour))
	conn.Close()

	// After close, the underlying pipe should be closed.
	_, err := reader.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}

// pipeReadWriteCloser combines separate io.Reader and io.Writer into an
// io.ReadWriteCloser. Closing closes the reader (if closable) and writer
// (if closable).
type pipeReadWriteCloser struct {
	io.Reader
	io.Writer
	closed bool
}

func (p *pipeReadWriteCloser) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	var firstError error
	if closer, ok := p.Reader.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			firstError = err
		}
	}
	if closer, ok := p.Writer.(io.Closer); ok {
		if err := closer.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}
