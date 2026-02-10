// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockDaemon simulates the daemon side of an observation connection.
// It reads the ObserveRequest, sends an ObserveResponse, then sends the
// metadata and history messages per the protocol.
type mockDaemon struct {
	conn     net.Conn
	metadata MetadataPayload
	history  []byte
}

func (daemon *mockDaemon) handshake(t *testing.T) ObserveRequest {
	t.Helper()

	// Read the client's observation request.
	var request ObserveRequest
	decoder := json.NewDecoder(daemon.conn)
	if err := decoder.Decode(&request); err != nil {
		t.Fatalf("mock daemon: read request: %v", err)
	}

	// Send success response.
	response := ObserveResponse{
		OK:      true,
		Session: daemon.metadata.Session,
		Machine: daemon.metadata.Machine,
	}
	encoder := json.NewEncoder(daemon.conn)
	if err := encoder.Encode(response); err != nil {
		t.Fatalf("mock daemon: send response: %v", err)
	}

	// Send metadata message.
	metadataJSON, err := json.Marshal(daemon.metadata)
	if err != nil {
		t.Fatalf("mock daemon: marshal metadata: %v", err)
	}
	if err := WriteMessage(daemon.conn, NewMetadataMessage(metadataJSON)); err != nil {
		t.Fatalf("mock daemon: send metadata: %v", err)
	}

	// Send history message.
	historyPayload := daemon.history
	if historyPayload == nil {
		historyPayload = []byte{}
	}
	if err := WriteMessage(daemon.conn, NewHistoryMessage(historyPayload)); err != nil {
		t.Fatalf("mock daemon: send history: %v", err)
	}

	return request
}

func (daemon *mockDaemon) sendData(t *testing.T, data []byte) {
	t.Helper()
	if err := WriteMessage(daemon.conn, NewDataMessage(data)); err != nil {
		t.Fatalf("mock daemon: send data: %v", err)
	}
}

func (daemon *mockDaemon) readMessage(t *testing.T, timeout time.Duration) Message {
	t.Helper()
	daemon.conn.SetReadDeadline(time.Now().Add(timeout))
	defer daemon.conn.SetReadDeadline(time.Time{})
	message, err := ReadMessage(daemon.conn)
	if err != nil {
		t.Fatalf("mock daemon: read message: %v", err)
	}
	return message
}

// newTestDaemon creates a mock daemon and a unix socket for the client to
// connect to. Returns the daemon, socket path, and a cleanup function.
func newTestDaemon(t *testing.T, metadata MetadataPayload, history []byte) (*mockDaemon, string) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", socketPath, err)
	}
	t.Cleanup(func() { listener.Close() })

	daemon := &mockDaemon{
		metadata: metadata,
		history:  history,
	}

	// Accept one connection in a goroutine.
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		daemon.conn = conn
		close(accepted)
	}()

	t.Cleanup(func() {
		<-accepted
		if daemon.conn != nil {
			daemon.conn.Close()
		}
	})

	return daemon, socketPath
}

var testMetadata = MetadataPayload{
	Session:   "bureau/test/agent",
	Principal: "@test/agent:bureau.local",
	Machine:   "@machine/testbox:bureau.local",
	Panes: []PaneInfo{
		{Index: 0, Command: "cat", Width: 80, Height: 24, Active: true},
	},
}

func TestConnectSuccess(t *testing.T) {
	t.Parallel()
	daemon, socketPath := newTestDaemon(t, testMetadata, nil)

	// Run the handshake in a goroutine since Connect blocks until it
	// finishes reading the metadata.
	handshakeDone := make(chan ObserveRequest, 1)
	go func() {
		// Wait for the connection to be accepted before running handshake.
		time.Sleep(100 * time.Millisecond)
		handshakeDone <- daemon.handshake(t)
	}()

	session, err := Connect(socketPath, ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	request := <-handshakeDone
	if request.Principal != "test/agent" {
		t.Errorf("request principal = %q, want %q", request.Principal, "test/agent")
	}
	if request.Mode != "readwrite" {
		t.Errorf("request mode = %q, want %q", request.Mode, "readwrite")
	}

	if session.Metadata.Session != "bureau/test/agent" {
		t.Errorf("metadata session = %q, want %q", session.Metadata.Session, "bureau/test/agent")
	}
	if session.Metadata.Principal != "@test/agent:bureau.local" {
		t.Errorf("metadata principal = %q, want %q", session.Metadata.Principal, "@test/agent:bureau.local")
	}
}

func TestConnectDenied(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(t.TempDir(), "observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Daemon that denies the request.
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request ObserveRequest
		json.NewDecoder(conn).Decode(&request)
		response := ObserveResponse{
			OK:    false,
			Error: "principal not found",
		}
		json.NewEncoder(conn).Encode(response)
	}()

	_, err = Connect(socketPath, ObserveRequest{
		Principal: "nonexistent/agent",
		Mode:      "readwrite",
	})
	if err == nil {
		t.Fatal("Connect should have returned an error for denied request")
	}
	if !strings.Contains(err.Error(), "principal not found") {
		t.Errorf("error = %q, should contain %q", err.Error(), "principal not found")
	}
}

func TestConnectNoSocket(t *testing.T) {
	t.Parallel()
	_, err := Connect("/nonexistent/path/observe.sock", ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err == nil {
		t.Fatal("Connect should have returned an error for nonexistent socket")
	}
}

func TestSessionRunReceivesHistory(t *testing.T) {
	t.Parallel()
	daemon, socketPath := newTestDaemon(t, testMetadata, []byte("previous output\r\n"))

	go func() {
		time.Sleep(100 * time.Millisecond)
		daemon.handshake(t)
		// Close after handshake to end the session.
		time.Sleep(200 * time.Millisecond)
		daemon.conn.Close()
	}()

	session, err := Connect(socketPath, ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	var output bytes.Buffer
	err = session.Run(strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "previous output") {
		t.Errorf("output should contain history, got: %q", result)
	}
	if !strings.Contains(result, "Connected to @test/agent:bureau.local on @machine/testbox:bureau.local") {
		t.Errorf("output should contain status line, got: %q", result)
	}
}

func TestSessionRunRelaysData(t *testing.T) {
	t.Parallel()
	daemon, socketPath := newTestDaemon(t, testMetadata, nil)

	go func() {
		time.Sleep(100 * time.Millisecond)
		daemon.handshake(t)
		// Send some terminal output.
		daemon.sendData(t, []byte("remote terminal output"))
		time.Sleep(200 * time.Millisecond)
		daemon.conn.Close()
	}()

	session, err := Connect(socketPath, ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	var output bytes.Buffer
	err = session.Run(strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "remote terminal output") {
		t.Errorf("output should contain remote data, got: %q", result)
	}
}

func TestSessionRunSendsInput(t *testing.T) {
	t.Parallel()
	daemon, socketPath := newTestDaemon(t, testMetadata, nil)

	go func() {
		time.Sleep(100 * time.Millisecond)
		daemon.handshake(t)

		// Read input from the client.
		message := daemon.readMessage(t, 5*time.Second)
		if message.Type != MessageTypeData {
			t.Errorf("expected data message, got type 0x%02x", message.Type)
		}
		if !strings.Contains(string(message.Payload), "keyboard input") {
			t.Errorf("payload = %q, should contain %q", message.Payload, "keyboard input")
		}

		// Close to end the session.
		daemon.conn.Close()
	}()

	session, err := Connect(socketPath, ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	inputReader, inputWriter := io.Pipe()
	go func() {
		inputWriter.Write([]byte("keyboard input"))
		// Keep the pipe open until the session ends; closing it would
		// trigger the input goroutine to exit.
		time.Sleep(1 * time.Second)
		inputWriter.Close()
	}()

	var output bytes.Buffer
	err = session.Run(inputReader, &output)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestSessionRunWithPipe(t *testing.T) {
	t.Parallel()
	// Test using net.Pipe directly (no real unix socket) to verify the
	// protocol exchange works end-to-end.
	clientConn, daemonConn := net.Pipe()
	defer clientConn.Close()
	defer daemonConn.Close()

	metadata := testMetadata
	historyContent := []byte("scrollback history\r\n")

	// Simulate daemon side.
	go func() {
		// Read request.
		var request ObserveRequest
		json.NewDecoder(daemonConn).Decode(&request)

		// Send response.
		json.NewEncoder(daemonConn).Encode(ObserveResponse{
			OK:      true,
			Session: metadata.Session,
			Machine: metadata.Machine,
		})

		// Send metadata.
		metadataJSON, _ := json.Marshal(metadata)
		WriteMessage(daemonConn, NewMetadataMessage(metadataJSON))

		// Send history.
		WriteMessage(daemonConn, NewHistoryMessage(historyContent))

		// Send some live data.
		WriteMessage(daemonConn, NewDataMessage([]byte("live output")))

		// Wait a bit and close.
		time.Sleep(200 * time.Millisecond)
		daemonConn.Close()
	}()

	// Create a temporary unix socket for Connect to dial.
	socketPath := filepath.Join(t.TempDir(), "observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Bridge: accept the Connect call and pipe it to daemonConn.
	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			return
		}
		// Copy bidirectionally between accepted and clientConn.
		go io.Copy(accepted, clientConn)
		io.Copy(clientConn, accepted)
		accepted.Close()
	}()

	session, err := Connect(socketPath, ObserveRequest{
		Principal: "test/agent",
		Mode:      "readwrite",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	var output bytes.Buffer
	// Use a pre-closed reader so the input goroutine exits quickly.
	emptyReader, emptyWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("os.Pipe: %v", pipeErr)
	}
	emptyWriter.Close()

	err = session.Run(emptyReader, &output)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "scrollback history") {
		t.Errorf("output should contain history, got: %q", result)
	}
	if !strings.Contains(result, "live output") {
		t.Errorf("output should contain live data, got: %q", result)
	}
	if !strings.Contains(result, "Connected to") {
		t.Errorf("output should contain status line, got: %q", result)
	}
}
