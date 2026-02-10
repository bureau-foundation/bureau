// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// readMessageTimeout reads a single message from conn with a deadline.
func readMessageTimeout(t *testing.T, conn net.Conn, timeout time.Duration) Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	message, err := ReadMessage(conn)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	return message
}

// readDataUntil reads data messages from conn until the accumulated output
// contains the expected substring, or the timeout expires.
func readDataUntil(t *testing.T, conn net.Conn, expected string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var collected strings.Builder
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		message, err := ReadMessage(conn)
		if err != nil {
			t.Fatalf("read data message: %v (collected so far: %q)", err, collected.String())
		}
		if message.Type == MessageTypeData {
			collected.Write(message.Payload)
			if strings.Contains(collected.String(), expected) {
				conn.SetReadDeadline(time.Time{})
				return collected.String()
			}
		}
	}
	t.Fatalf("timed out waiting for %q in output (collected: %q)", expected, collected.String())
	return ""
}

// startRelay launches Relay in a goroutine and returns the error channel.
// The relayConn end is passed to Relay; the caller uses clientConn.
func startRelay(t *testing.T, clientConn, relayConn net.Conn, serverSocket, sessionName string, readOnly bool) chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- Relay(relayConn, serverSocket, sessionName, readOnly)
	}()
	return result
}

// consumeHandshake reads and validates the metadata and history messages
// that the relay sends on connect. Returns the parsed metadata.
func consumeHandshake(t *testing.T, conn net.Conn) MetadataPayload {
	t.Helper()

	metadataMessage := readMessageTimeout(t, conn, 5*time.Second)
	if metadataMessage.Type != MessageTypeMetadata {
		t.Fatalf("expected metadata message (type 0x%02x), got type 0x%02x",
			MessageTypeMetadata, metadataMessage.Type)
	}
	var metadata MetadataPayload
	if err := json.Unmarshal(metadataMessage.Payload, &metadata); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}

	historyMessage := readMessageTimeout(t, conn, 5*time.Second)
	if historyMessage.Type != MessageTypeHistory {
		t.Fatalf("expected history message (type 0x%02x), got type 0x%02x",
			MessageTypeHistory, historyMessage.Type)
	}

	return metadata
}

func TestRelayMetadataAndHistory(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-meta", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	metadata := consumeHandshake(t, clientConn)

	if metadata.Session != sessionName {
		t.Errorf("metadata session = %q, want %q", metadata.Session, sessionName)
	}
	if len(metadata.Panes) == 0 {
		t.Error("metadata has no panes")
	}
	if len(metadata.Panes) > 0 {
		pane := metadata.Panes[0]
		// tmux may or may not show a status bar (1 row), so the pane
		// height is either 23 or 24 depending on configuration.
		if pane.Width != 80 {
			t.Errorf("pane width = %d, want 80", pane.Width)
		}
		if pane.Height < 23 || pane.Height > 24 {
			t.Errorf("pane height = %d, want 23 or 24", pane.Height)
		}
		if !pane.Active {
			t.Error("first pane should be active")
		}
	}

	clientConn.Close()
	err := <-relayResult
	if err != nil {
		t.Errorf("relay returned error: %v", err)
	}
}

func TestRelayReadOutput(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-read", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	consumeHandshake(t, clientConn)

	// Type into the tmux session directly. The relay should capture and
	// forward the terminal output as data messages.
	TmuxSendKeys(t, serverSocket, sessionName, "hello-from-tmux")
	TmuxSendKeys(t, serverSocket, sessionName, "Enter")

	readDataUntil(t, clientConn, "hello-from-tmux", 5*time.Second)

	clientConn.Close()
	err := <-relayResult
	if err != nil {
		t.Errorf("relay returned error: %v", err)
	}
}

func TestRelaySendInput(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-input", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	consumeHandshake(t, clientConn)

	// Send input through the relay. The tmux session runs cat, so the
	// input should appear in the pane.
	err := WriteMessage(clientConn, NewDataMessage([]byte("relay-input-test\r")))
	if err != nil {
		t.Fatalf("send input: %v", err)
	}

	// Wait for it to render, then check the pane content.
	readDataUntil(t, clientConn, "relay-input-test", 5*time.Second)

	content := TmuxCapturePane(t, serverSocket, sessionName)
	if !strings.Contains(content, "relay-input-test") {
		t.Errorf("tmux pane does not contain relay input, got:\n%s", content)
	}

	clientConn.Close()
	err = <-relayResult
	if err != nil {
		t.Errorf("relay returned error: %v", err)
	}
}

func TestRelayReadOnly(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-ro", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, true)

	consumeHandshake(t, clientConn)

	// Send input through the relay — it should be ignored in read-only mode.
	err := WriteMessage(clientConn, NewDataMessage([]byte("SHOULD-NOT-APPEAR\r")))
	if err != nil {
		t.Fatalf("send input: %v", err)
	}

	// Give time for the input to potentially propagate (it shouldn't).
	time.Sleep(500 * time.Millisecond)

	content := TmuxCapturePane(t, serverSocket, sessionName)
	if strings.Contains(content, "SHOULD-NOT-APPEAR") {
		t.Errorf("read-only relay forwarded input to tmux pane:\n%s", content)
	}

	// Verify that output still flows: type via tmux directly.
	TmuxSendKeys(t, serverSocket, sessionName, "readonly-output")
	TmuxSendKeys(t, serverSocket, sessionName, "Enter")

	readDataUntil(t, clientConn, "readonly-output", 5*time.Second)

	clientConn.Close()
	err = <-relayResult
	if err != nil {
		t.Errorf("relay returned error: %v", err)
	}
}

func TestRelayResize(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-resize", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	consumeHandshake(t, clientConn)

	// Send a resize message. The relay should apply TIOCSWINSZ on the PTY
	// master, which propagates to the tmux client. tmux adjusts the session
	// to match the smallest attached client.
	err := WriteMessage(clientConn, NewResizeMessage(100, 50))
	if err != nil {
		t.Fatalf("send resize: %v", err)
	}

	// Wait for tmux to process the resize and then check the client dimensions.
	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command("tmux", "-S", serverSocket, "list-clients",
		"-t", sessionName,
		"-F", "#{client_width} #{client_height}")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("list-clients: %v", err)
	}
	clientDimensions := strings.TrimSpace(string(output))
	if !strings.Contains(clientDimensions, "100 50") {
		t.Errorf("client dimensions = %q, want to contain %q", clientDimensions, "100 50")
	}

	clientConn.Close()
	err = <-relayResult
	if err != nil {
		t.Errorf("relay returned error: %v", err)
	}
}

func TestRelaySessionEnd(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-end", "")

	clientConn, relayConn := net.Pipe()
	defer clientConn.Close()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	consumeHandshake(t, clientConn)

	// Kill the tmux session. The relay should detect the exit and shut down.
	killCmd := exec.Command("tmux", "-S", serverSocket, "kill-session", "-t", sessionName)
	if output, err := killCmd.CombinedOutput(); err != nil {
		t.Fatalf("kill session: %v\n%s", err, output)
	}

	select {
	case err := <-relayResult:
		if err != nil {
			t.Errorf("relay returned error on session end: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not shut down within 10 seconds after session was killed")
	}
}

func TestRelayConnectionClose(t *testing.T) {
	t.Parallel()
	serverSocket := TmuxServer(t)
	sessionName := TmuxSession(t, serverSocket, "test-close", "")

	clientConn, relayConn := net.Pipe()
	relayResult := startRelay(t, clientConn, relayConn, serverSocket, sessionName, false)

	consumeHandshake(t, clientConn)

	// Close the client connection. The relay should detect the close and
	// shut down, killing the tmux attach process.
	clientConn.Close()

	select {
	case err := <-relayResult:
		if err != nil {
			t.Errorf("relay returned error on connection close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not shut down within 10 seconds after connection closed")
	}
}
