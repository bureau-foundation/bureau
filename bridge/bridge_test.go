// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

// echoServer listens on a Unix socket and echoes back everything it reads.
// Each connection is handled independently. The listener is closed when the
// test completes.
func echoServer(t *testing.T) string {
	t.Helper()
	socketDirectory := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDirectory, "echo.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("echoServer: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}
			go func() {
				defer connection.Close()
				io.Copy(connection, connection)
			}()
		}
	}()

	return socketPath
}

// prefixServer listens on a Unix socket, reads all data from the client,
// prepends a prefix, and writes the result back. It performs a proper
// half-close sequence: after reading EOF from the client, it writes the
// response and then closes its write side.
func prefixServer(t *testing.T, prefix string) string {
	t.Helper()
	socketDirectory := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDirectory, "prefix.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("prefixServer: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			connection, acceptError := listener.Accept()
			if acceptError != nil {
				return
			}
			go func() {
				defer connection.Close()
				data, readError := io.ReadAll(connection)
				if readError != nil {
					return
				}
				response := append([]byte(prefix), data...)
				connection.Write(response)
				if unixConnection, ok := connection.(*net.UnixConn); ok {
					unixConnection.CloseWrite()
				}
			}()
		}
	}()

	return socketPath
}

func TestStart_MissingListenAddr(t *testing.T) {
	bridge := &Bridge{
		SocketPath: "/tmp/irrelevant.sock",
	}
	err := bridge.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for missing ListenAddr")
	}
	if got := err.Error(); got != "bridge: ListenAddr is required" {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestStart_MissingSocketPath(t *testing.T) {
	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
	}
	err := bridge.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for missing SocketPath")
	}
	if got := err.Error(); got != "bridge: SocketPath is required" {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestStart_UnreachableSocket(t *testing.T) {
	socketDirectory := testutil.SocketDir(t)
	socketPath := filepath.Join(socketDirectory, "nonexistent.sock")

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	err := bridge.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable socket")
	}
}

func TestAddr_BeforeStart(t *testing.T) {
	bridge := &Bridge{}
	if bridge.Addr() != nil {
		t.Fatal("expected nil Addr before Start")
	}
}

func TestAddr_AfterStart(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bridge.Stop()

	address := bridge.Addr()
	if address == nil {
		t.Fatal("expected non-nil Addr after Start")
	}
	tcpAddress, ok := address.(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected *net.TCPAddr, got %T", address)
	}
	if tcpAddress.Port == 0 {
		t.Fatal("expected non-zero port after binding to port 0")
	}
}

func TestRoundTrip(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bridge.Stop()

	connection, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close()

	payload := []byte("hello, bridge")
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	response := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(payload, response) {
		t.Fatalf("expected %q, got %q", payload, response)
	}
}

// TestHalfClose verifies that the bridge correctly propagates TCP FIN to
// the Unix socket (via CloseWrite) and then delivers the backend's
// response back through the bridge before closing.
//
// The prefixServer reads until EOF from the client side, then writes a
// prefixed response. If CloseWrite doesn't propagate through the bridge,
// the server never sees EOF and the test hangs.
func TestHalfClose(t *testing.T) {
	socketPath := prefixServer(t, "REPLY:")

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bridge.Stop()

	connection, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close()

	payload := []byte("request-data")
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Close the write side to signal EOF to the backend through the bridge.
	tcpConnection := connection.(*net.TCPConn)
	if err := tcpConnection.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// Read the full response. The backend prepends "REPLY:" and then
	// closes its write side, so we should get EOF after reading.
	response, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	expected := []byte("REPLY:request-data")
	if !bytes.Equal(expected, response) {
		t.Fatalf("expected %q, got %q", expected, response)
	}
}

func TestConcurrentConnections(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bridge.Stop()

	const connectionCount = 10
	var waitGroup sync.WaitGroup
	waitGroup.Add(connectionCount)

	errors := make(chan error, connectionCount)

	for i := range connectionCount {
		go func() {
			defer waitGroup.Done()
			connection, dialError := net.Dial("tcp", bridge.Addr().String())
			if dialError != nil {
				errors <- dialError
				return
			}
			defer connection.Close()

			payload := []byte("connection-" + string(rune('A'+i)))
			if _, writeError := connection.Write(payload); writeError != nil {
				errors <- writeError
				return
			}

			response := make([]byte, len(payload))
			if _, readError := io.ReadFull(connection, response); readError != nil {
				errors <- readError
				return
			}
			if !bytes.Equal(payload, response) {
				errors <- &mismatchError{expected: payload, got: response}
				return
			}
		}()
	}

	waitGroup.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("connection error: %v", err)
	}
}

type mismatchError struct {
	expected, got []byte
}

func (e *mismatchError) Error() string {
	return "expected " + string(e.expected) + ", got " + string(e.got)
}

func TestLargePayload(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bridge.Stop()

	// 1 MB payload exercises buffered io.Copy across multiple reads.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 251) // Prime mod avoids repeating at power-of-two boundaries.
	}

	connection, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close()

	// Write and read concurrently to avoid deadlock: the echo server
	// writes back as it reads, so the TCP receive buffer can fill if
	// we block on write before reading.
	var readResult []byte
	var readError error
	var readDone sync.WaitGroup
	readDone.Add(1)
	go func() {
		defer readDone.Done()
		readResult, readError = io.ReadAll(connection)
	}()

	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	connection.(*net.TCPConn).CloseWrite()

	readDone.Wait()
	if readError != nil {
		t.Fatalf("ReadAll: %v", readError)
	}
	if !bytes.Equal(payload, readResult) {
		t.Fatalf("payload mismatch: sent %d bytes, got %d bytes", len(payload), len(readResult))
	}
}

func TestStopDrainsConnections(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Open a connection that stays alive.
	connection, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Stop in a goroutine — it should return once the connection drains.
	stopDone := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopDone)
	}()

	// Give Stop a moment to close the listener and cancel the context.
	// The bridge's Stop closes real OS sockets, so there is no injectable
	// clock — we are waiting for kernel-level socket teardown.
	time.Sleep(50 * time.Millisecond) //nolint:realclock OS socket teardown

	// Close our connection, which will cause the handleConnection goroutine
	// to finish and allow the acceptLoop to exit.
	connection.Close()

	select {
	case <-stopDone:
		// Bridge stopped.
	case <-time.After(5 * time.Second): //nolint:realclock test hang prevention
		t.Fatal("Stop did not return after connection closed")
	}
}

func TestStopIdempotent(t *testing.T) {
	socketPath := echoServer(t)

	bridge := &Bridge{
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
	}
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Calling Stop twice should not panic.
	bridge.Stop()
	bridge.Stop()
}
