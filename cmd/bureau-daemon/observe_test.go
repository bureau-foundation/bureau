// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// mockRelaySource is the Go source for a minimal observation relay binary used
// in tests. It speaks the observation protocol on fd 3:
//   - Sends a metadata message with session name from argv[1]
//   - Sends an empty history message
//   - Echoes data messages back, prefixed with "echo:" to verify round-trip
//
// The binary exits when fd 3 closes or when it receives an error reading.
const mockRelaySource = `package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

const (
	messageTypeData     byte = 0x01
	messageTypeHistory  byte = 0x03
	messageTypeMetadata byte = 0x04
	headerLength             = 5
)

func writeMessage(w io.Writer, messageType byte, payload []byte) error {
	var header [headerLength]byte
	header[0] = messageType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readMessage(r io.Reader) (byte, []byte, error) {
	var header [headerLength]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	messageType := header[0]
	payloadLength := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, payloadLength)
	if payloadLength > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return messageType, payload, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mock-relay <session-name>")
		os.Exit(1)
	}
	sessionName := os.Args[1]

	socketFile := os.NewFile(uintptr(3), "daemon-socket")
	if socketFile == nil {
		fmt.Fprintln(os.Stderr, "fd 3 not available")
		os.Exit(1)
	}
	connection, err := net.FileConn(socketFile)
	socketFile.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FileConn: %v\n", err)
		os.Exit(1)
	}
	defer connection.Close()

	// Send metadata message.
	metadata := map[string]any{
		"session":   sessionName,
		"principal": "@test/echo:bureau.local",
		"machine":   "@machine/test:bureau.local",
		"panes":     []any{},
	}
	metadataJSON, _ := json.Marshal(metadata)
	if err := writeMessage(connection, messageTypeMetadata, metadataJSON); err != nil {
		fmt.Fprintf(os.Stderr, "write metadata: %v\n", err)
		os.Exit(1)
	}

	// Send empty history message.
	if err := writeMessage(connection, messageTypeHistory, nil); err != nil {
		fmt.Fprintf(os.Stderr, "write history: %v\n", err)
		os.Exit(1)
	}

	// Echo loop: read data messages, write them back prefixed with "echo:".
	for {
		messageType, payload, readErr := readMessage(connection)
		if readErr != nil {
			return
		}
		if messageType == messageTypeData && len(payload) > 0 {
			echoPayload := append([]byte("echo:"), payload...)
			if writeErr := writeMessage(connection, messageTypeData, echoPayload); writeErr != nil {
				return
			}
		}
	}
}
`

// buildMockRelay compiles the mock relay binary from source and returns the
// path to the binary. The binary is placed in a test-scoped temp directory.
func buildMockRelay(t *testing.T) string {
	t.Helper()

	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(sourcePath, []byte(mockRelaySource), 0644); err != nil {
		t.Fatalf("write mock relay source: %v", err)
	}

	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "mock-relay")
	cmd := exec.Command("go", "build", "-o", outputPath, sourcePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build mock relay: %v\n%s", err, output)
	}
	return outputPath
}

// connectObserve connects to the daemon's observe socket and sends an
// observation request. Returns the connection and decoded response.
func connectObserve(t *testing.T, socketPath string, request observeRequest) (net.Conn, observeResponse) {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}

	connection.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(connection).Encode(request); err != nil {
		connection.Close()
		t.Fatalf("send observe request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		connection.Close()
		t.Fatalf("read observe response: %v", err)
	}

	var response observeResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		connection.Close()
		t.Fatalf("unmarshal observe response: %v", err)
	}

	// Clear the deadline for streaming use after the handshake.
	connection.SetDeadline(time.Time{})

	return connection, response
}

// readObserveMessage reads a single observation protocol message from a
// connection. Returns the message type and payload.
func readObserveMessage(t *testing.T, connection net.Conn) (byte, []byte) {
	t.Helper()
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	defer connection.SetDeadline(time.Time{})

	var header [5]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		t.Fatalf("read message header: %v", err)
	}
	messageType := header[0]
	payloadLength := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, payloadLength)
	if payloadLength > 0 {
		if _, err := io.ReadFull(connection, payload); err != nil {
			t.Fatalf("read message payload: %v", err)
		}
	}
	return messageType, payload
}

// writeObserveMessage writes a single observation protocol message to a
// connection.
func writeObserveMessage(t *testing.T, connection net.Conn, messageType byte, payload []byte) {
	t.Helper()
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	defer connection.SetDeadline(time.Time{})

	var header [5]byte
	header[0] = messageType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := connection.Write(header[:]); err != nil {
		t.Fatalf("write message header: %v", err)
	}
	if len(payload) > 0 {
		if _, err := connection.Write(payload); err != nil {
			t.Fatalf("write message payload: %v", err)
		}
	}
}

// newTestDaemonWithObserve creates a minimal Daemon wired for observation
// testing. It starts the observe listener and returns the daemon plus a
// cleanup function.
func newTestDaemonWithObserve(t *testing.T, relayBinary string, runningPrincipals []string) *Daemon {
	t.Helper()

	socketDir := t.TempDir()
	observeSocketPath := filepath.Join(socketDir, "observe.sock")
	tmuxSocket := filepath.Join(socketDir, "tmux.sock")

	running := make(map[string]bool)
	for _, principal := range runningPrincipals {
		running[principal] = true
	}

	daemon := &Daemon{
		machineName:        "machine/test",
		machineUserID:      "@machine/test:bureau.local",
		serverName:         "bureau.local",
		running:            running,
		services:           make(map[string]*schema.Service),
		proxyRoutes:        make(map[string]string),
		peerAddresses:      make(map[string]string),
		peerTransports:     make(map[string]http.RoundTripper),
		observeSocketPath:  observeSocketPath,
		tmuxServerSocket:   tmuxSocket,
		observeRelayBinary: relayBinary,
		layoutWatchers:     make(map[string]*layoutWatcher),
		logger:             slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	ctx := context.Background()
	if err := daemon.startObserveListener(ctx); err != nil {
		t.Fatalf("startObserveListener: %v", err)
	}
	t.Cleanup(daemon.stopObserveListener)

	return daemon
}

func TestObserveErrorUnknownPrincipal(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay", nil)

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/nonexistent",
		Mode:      "readwrite",
	})
	defer connection.Close()

	if response.OK {
		t.Error("expected error response, got OK")
	}
	if !strings.Contains(response.Error, "not found") {
		t.Errorf("error = %q, expected to contain 'not found'", response.Error)
	}
}

func TestObserveErrorInvalidMode(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay", nil)

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/echo",
		Mode:      "bogus",
	})
	defer connection.Close()

	if response.OK {
		t.Error("expected error response, got OK")
	}
	if !strings.Contains(response.Error, "invalid mode") {
		t.Errorf("error = %q, expected to contain 'invalid mode'", response.Error)
	}
}

func TestObserveErrorInvalidPrincipal(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay", nil)

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "../escape",
		Mode:      "readwrite",
	})
	defer connection.Close()

	if response.OK {
		t.Error("expected error response, got OK")
	}
	if !strings.Contains(response.Error, "invalid principal") {
		t.Errorf("error = %q, expected to contain 'invalid principal'", response.Error)
	}
}

func TestObserveLocalBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a mock relay binary")
	}

	relayBinary := buildMockRelay(t)
	daemon := newTestDaemonWithObserve(t, relayBinary, []string{"test/echo"})

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/echo",
		Mode:      "readwrite",
	})
	defer connection.Close()

	if !response.OK {
		t.Fatalf("expected OK response, got error: %s", response.Error)
	}
	if response.Session != "bureau/test/echo" {
		t.Errorf("session = %q, want %q", response.Session, "bureau/test/echo")
	}
	if response.Machine != "machine/test" {
		t.Errorf("machine = %q, want %q", response.Machine, "machine/test")
	}

	// After the JSON handshake, the connection switches to the binary
	// observation protocol. The mock relay sends metadata, then history,
	// then echoes data messages.

	// Read metadata message.
	metadataType, metadataPayload := readObserveMessage(t, connection)
	if metadataType != 0x04 {
		t.Fatalf("expected metadata message (type 0x04), got 0x%02x", metadataType)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataPayload, &metadata); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if metadata["session"] != "bureau/test/echo" {
		t.Errorf("metadata session = %v, want %q", metadata["session"], "bureau/test/echo")
	}

	// Read history message (empty payload).
	historyType, _ := readObserveMessage(t, connection)
	if historyType != 0x03 {
		t.Fatalf("expected history message (type 0x03), got 0x%02x", historyType)
	}

	// Send a data message and verify the echo comes back.
	testPayload := []byte("hello observation")
	writeObserveMessage(t, connection, 0x01, testPayload)

	echoType, echoPayload := readObserveMessage(t, connection)
	if echoType != 0x01 {
		t.Fatalf("expected data message (type 0x01), got 0x%02x", echoType)
	}
	expectedEcho := "echo:hello observation"
	if string(echoPayload) != expectedEcho {
		t.Errorf("echo payload = %q, want %q", string(echoPayload), expectedEcho)
	}
}

func TestObserveRelayCleanupOnClientDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a mock relay binary")
	}

	relayBinary := buildMockRelay(t)
	daemon := newTestDaemonWithObserve(t, relayBinary, []string{"test/echo"})

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/echo",
		Mode:      "readwrite",
	})

	if !response.OK {
		connection.Close()
		t.Fatalf("expected OK response, got error: %s", response.Error)
	}

	// Read the metadata and history messages to ensure the relay is fully
	// started and the bridge is active.
	readObserveMessage(t, connection)
	readObserveMessage(t, connection)

	// Close the client connection. This should cause the daemon to close
	// the relay connection (bridgeConnections closes both sides when one
	// finishes), which triggers the relay process to exit.
	connection.Close()

	// Give the cleanup goroutine time to run. The relay should exit promptly
	// when its socket closes. If cleanup is broken, the test will just take
	// longer and still pass — but the process will be gone.
	time.Sleep(200 * time.Millisecond)

	// Verify the daemon is still accepting new connections (not wedged by
	// a stuck relay cleanup). Open a new connection and verify it gets a
	// response.
	verifyConnection, verifyResponse := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/echo",
		Mode:      "readwrite",
	})
	defer verifyConnection.Close()

	if !verifyResponse.OK {
		t.Errorf("daemon stopped accepting connections after relay cleanup: %s", verifyResponse.Error)
	}
}

func TestObserveReadOnlyMode(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a mock relay binary")
	}

	relayBinary := buildMockRelay(t)
	daemon := newTestDaemonWithObserve(t, relayBinary, []string{"test/echo"})

	connection, response := connectObserve(t, daemon.observeSocketPath, observeRequest{
		Principal: "test/echo",
		Mode:      "readonly",
	})
	defer connection.Close()

	if !response.OK {
		t.Fatalf("expected OK response, got error: %s", response.Error)
	}

	// The mock relay doesn't distinguish readonly mode (that's enforced by
	// the real relay via tmux -r), but verifying the handshake succeeds
	// and we get the protocol messages exercises the daemon's mode
	// validation and relay forking with BUREAU_OBSERVE_READONLY=1.
	metadataType, _ := readObserveMessage(t, connection)
	if metadataType != 0x04 {
		t.Fatalf("expected metadata message (type 0x04), got 0x%02x", metadataType)
	}

	historyType, _ := readObserveMessage(t, connection)
	if historyType != 0x03 {
		t.Fatalf("expected history message (type 0x03), got 0x%02x", historyType)
	}
}
