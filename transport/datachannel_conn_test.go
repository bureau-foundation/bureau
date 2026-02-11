// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"io"
	"net"
	"os"
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

func TestDataChannelConn_ExpiredDeadlineFailsRead(t *testing.T) {
	reader, writer := io.Pipe()
	stream := &pipeReadWriteCloser{Reader: reader, Writer: writer}
	conn := NewDataChannelConn(stream, "local", "remote")
	defer conn.Close()
	defer writer.Close()

	// Set a deadline that is already in the past.
	conn.SetReadDeadline(time.Now().Add(-1 * time.Second))

	// Read should fail with a deadline error.
	buffer := make([]byte, 10)
	_, err := conn.Read(buffer)
	if err == nil {
		t.Fatal("expected error from Read after expired deadline, got nil")
	}

	// The error must be os.ErrDeadlineExceeded (which implements net.Error
	// with Timeout()=true). http.Server relies on this to distinguish
	// timeouts from connection failures.
	if err != os.ErrDeadlineExceeded {
		t.Errorf("expected os.ErrDeadlineExceeded, got %T: %v", err, err)
	}
	timeoutError, ok := err.(net.Error)
	if !ok || !timeoutError.Timeout() {
		t.Errorf("expected net.Error with Timeout()=true, got %T: %v", err, err)
	}
}

// TestDataChannelConn_HijackPattern validates the exact sequence performed by
// http.Server during Hijack():
//
//  1. SetReadDeadline(time.Time{})     — server clears deadline before starting background reader
//  2. Read blocks in background goroutine
//  3. SetReadDeadline(aLongTimeAgo)    — server aborts the background reader
//  4. Read returns os.ErrDeadlineExceeded
//  5. SetDeadline(time.Time{})         — hijack caller clears all deadlines
//  6. Connection is still alive        — Read/Write work normally
//
// This test catches the race condition where Read captures a nil readCancel
// (from step 1), and then SetReadDeadline (step 3) creates a new cancel
// channel that the blocked Read never sees.
func TestDataChannelConn_HijackPattern(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	clientStream := &pipeReadWriteCloser{Reader: clientReader, Writer: clientWriter}
	serverStream := &pipeReadWriteCloser{Reader: serverReader, Writer: serverWriter}

	conn := NewDataChannelConn(clientStream, "local", "remote")
	peer := NewDataChannelConn(serverStream, "remote", "local")
	defer conn.Close()
	defer peer.Close()

	// Step 1: Clear deadline (server does this before starting background read).
	conn.SetReadDeadline(time.Time{})

	// Step 2: Start a background Read that blocks (simulating http.Server's
	// startBackgroundRead goroutine).
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 256)
		_, err := conn.Read(buffer)
		readDone <- err
	}()

	// Give the goroutine time to enter Read and block in the select.
	time.Sleep(20 * time.Millisecond)

	// Step 3: Set deadline in the past to abort the read. This is exactly
	// what http.Server's abortPendingRead does during Hijack.
	conn.SetReadDeadline(time.Now().Add(-1 * time.Second))

	// Step 4: The read should return quickly with a deadline error.
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("expected error from Read after SetReadDeadline(past), got nil")
		}
		timeoutError, ok := err.(net.Error)
		if !ok || !timeoutError.Timeout() {
			t.Fatalf("expected net.Error with Timeout()=true, got %T: %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return within 2 seconds after SetReadDeadline(past)")
	}

	// Step 5: Clear all deadlines (caller does this after Hijack).
	conn.SetDeadline(time.Time{})

	// Step 6: The connection must still be alive. Both Read and Write
	// should work normally. This verifies the non-destructive property:
	// the read deadline did not close the underlying stream.
	message := []byte("post-hijack data")
	go func() {
		peer.Write(message)
	}()

	buffer := make([]byte, 256)
	bytesRead, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("Read after clearing deadline: %v", err)
	}
	if string(buffer[:bytesRead]) != "post-hijack data" {
		t.Errorf("read = %q, want %q", string(buffer[:bytesRead]), "post-hijack data")
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

func TestDataChannelConn_ReadSmallBuffer(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	clientStream := &pipeReadWriteCloser{Reader: clientReader, Writer: clientWriter}
	serverStream := &pipeReadWriteCloser{Reader: serverReader, Writer: serverWriter}

	clientConn := NewDataChannelConn(clientStream, "client", "server")
	serverConn := NewDataChannelConn(serverStream, "server", "client")
	defer clientConn.Close()
	defer serverConn.Close()

	// Send a message larger than the read buffer to exercise the
	// remainder handling in deliverPumpResult.
	message := []byte("this message is longer than the tiny buffer")
	go func() {
		clientConn.Write(message)
	}()

	// Read with a small buffer — should take multiple reads.
	var received []byte
	buffer := make([]byte, 10)
	for len(received) < len(message) {
		bytesRead, err := serverConn.Read(buffer)
		if err != nil {
			t.Fatalf("Read error after %d bytes: %v", len(received), err)
		}
		received = append(received, buffer[:bytesRead]...)
	}

	if string(received) != string(message) {
		t.Errorf("received = %q, want %q", string(received), string(message))
	}
}

func TestDataChannelConn_WriteDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	stream := &pipeReadWriteCloser{Reader: reader, Writer: writer}
	conn := NewDataChannelConn(stream, "local", "remote")
	defer conn.Close()

	// Set a write deadline in the past.
	conn.SetWriteDeadline(time.Now().Add(-1 * time.Second))

	_, err := conn.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected error from Write after expired deadline, got nil")
	}
	if err != os.ErrDeadlineExceeded {
		t.Errorf("expected os.ErrDeadlineExceeded, got %T: %v", err, err)
	}

	// Clear the write deadline. The connection should be usable again
	// (the expired write deadline does not close the stream unless the
	// timer fires on a blocked write).
	conn.SetWriteDeadline(time.Time{})

	// Write should succeed now.
	go func() {
		reader.Read(make([]byte, 256))
	}()
	_, err = conn.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write after clearing deadline: %v", err)
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
