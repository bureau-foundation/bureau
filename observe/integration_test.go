// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/tmux"
)

// safeBuffer is a thread-safe bytes.Buffer. Session.Run writes terminal
// output from a goroutine while the test goroutine polls for expected
// content, so the buffer must synchronize concurrent access.
type safeBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (sb *safeBuffer) Write(data []byte) (int, error) {
	sb.mutex.Lock()
	defer sb.mutex.Unlock()
	return sb.buffer.Write(data)
}

func (sb *safeBuffer) String() string {
	sb.mutex.Lock()
	defer sb.mutex.Unlock()
	return sb.buffer.String()
}

// integrationFixture holds all the handles needed to drive an
// end-to-end observation test: client session, I/O pipes, result
// channels for both the client and relay goroutines.
type integrationFixture struct {
	server      *tmux.Server
	sessionName string
	session     *Session
	inputWriter *io.PipeWriter
	output      *safeBuffer
	runResult   chan error
	relayResult chan error
}

// setupIntegration creates a full observation stack for integration testing:
//   - An isolated tmux server with a session running cat
//   - A Relay goroutine attached to that session via a net.Pipe
//   - A bridge goroutine that handles the JSON handshake and forwards
//     binary protocol bytes between the client and relay
//   - A connected client Session with Run started in a goroutine
//
// The fixture's inputWriter feeds the client's stdin; output captures
// the client's stdout. Close inputWriter to trigger clean shutdown.
//
// Cleanup is registered via t.Cleanup as a safety net for error paths.
func setupIntegration(t *testing.T, sessionName string, readOnly bool) *integrationFixture {
	t.Helper()

	server := TmuxServer(t)
	TmuxSession(t, server, sessionName, "")

	// Pipe: relay reads/writes relayEnd, bridge reads/writes bridgeEnd.
	relayEnd, bridgeEnd := net.Pipe()

	relayResult := make(chan error, 1)
	go func() {
		relayResult <- Relay(relayEnd, server, sessionName, readOnly)
	}()

	daemonSocket := startObservationBridge(t, bridgeEnd, sessionName)

	mode := "readwrite"
	if readOnly {
		mode = "readonly"
	}
	session, err := Connect(daemonSocket, ObserveRequest{
		Principal: sessionName,
		Mode:      mode,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	inputReader, inputWriter := io.Pipe()
	var output safeBuffer

	runResult := make(chan error, 1)
	go func() {
		runResult <- session.Run(inputReader, &output)
	}()

	t.Cleanup(func() {
		inputWriter.Close()
		session.Close()
	})

	fixture := &integrationFixture{
		server:      server,
		sessionName: sessionName,
		session:     session,
		inputWriter: inputWriter,
		output:      &output,
		runResult:   runResult,
		relayResult: relayResult,
	}

	// Wait for the session to fully establish before returning.
	pollForContent(t, &output, "Connected to")

	return fixture
}

// awaitShutdown waits for both Session.Run and Relay to return, failing
// the test if either returns an error or does not return within 10 seconds.
func (fixture *integrationFixture) awaitShutdown(t *testing.T) {
	t.Helper()
	runError := testutil.RequireReceive(t, fixture.runResult, 10*time.Second, "session.Run did not return")
	if runError != nil {
		t.Errorf("session.Run returned error: %v", runError)
	}
	relayError := testutil.RequireReceive(t, fixture.relayResult, 10*time.Second, "relay did not shut down")
	if relayError != nil {
		t.Errorf("relay returned error: %v", relayError)
	}
}

// startObservationBridge creates a unix socket listener that simulates
// the daemon's observation routing. When a client connects, the bridge:
//   - Reads the JSON ObserveRequest and sends a JSON ObserveResponse
//   - Copies all subsequent bytes bidirectionally between the client
//     connection and the relay's pipe end
//
// This allows integration tests to connect a client to a relay without
// a real daemon, testing the full observation protocol stack.
//
// Returns the daemon socket path for the client to connect to.
func startObservationBridge(t *testing.T, relayPipe net.Conn, sessionName string) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "observe.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("bridge: listen on %s: %v", socketPath, err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			return
		}

		clientReader := bufio.NewReader(accepted)

		// Read the client's JSON observation request (newline-delimited).
		requestLine, err := clientReader.ReadBytes('\n')
		if err != nil {
			accepted.Close()
			return
		}
		var request ObserveRequest
		if err := json.Unmarshal(requestLine, &request); err != nil {
			accepted.Close()
			return
		}

		// Send success response.
		response := ObserveResponse{
			OK:      true,
			Session: sessionName,
			Machine: "machine/integration-test",
		}
		responseJSON, err := json.Marshal(response)
		if err != nil {
			accepted.Close()
			return
		}
		if _, err := accepted.Write(append(responseJSON, '\n')); err != nil {
			accepted.Close()
			return
		}

		// Bridge bytes bidirectionally between client and relay.
		// When either copy finishes (one side disconnected), close the
		// other side to propagate shutdown through the pipeline.
		var bridgeWait sync.WaitGroup
		bridgeWait.Add(2)
		go func() {
			defer bridgeWait.Done()
			io.Copy(accepted, relayPipe)
			accepted.Close()
		}()
		go func() {
			defer bridgeWait.Done()
			io.Copy(relayPipe, clientReader)
			relayPipe.Close()
		}()
		bridgeWait.Wait()
	}()

	return socketPath
}

// pollForContent polls the safeBuffer until the expected substring appears
// or the test deadline expires. Yields to the scheduler between checks.
func pollForContent(t *testing.T, buffer *safeBuffer, expected string) {
	t.Helper()
	for {
		if strings.Contains(buffer.String(), expected) {
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("timed out waiting for %q in output (collected so far: %q)", expected, buffer.String())
		}
		runtime.Gosched()
	}
}

// pollForTmuxContent polls a tmux pane until the expected substring appears
// in the captured content or the test deadline expires.
func pollForTmuxContent(t *testing.T, server *tmux.Server, target, expected string) {
	t.Helper()
	pollTmuxContent(t, server, target, expected)
}

// TestIntegrationRoundTrip verifies the full observation data path:
// client → bridge → relay → tmux PTY → tmux session → and back.
//
// This exercises both client.go and relay.go together against a real
// tmux session, with the bridge simulating the daemon's routing layer.
func TestIntegrationRoundTrip(t *testing.T) {
	t.Parallel()
	fixture := setupIntegration(t, "integration/roundtrip", false)

	// Verify metadata came from the real relay (which queries tmux).
	if fixture.session.Metadata.Session != fixture.sessionName {
		t.Errorf("metadata session = %q, want %q",
			fixture.session.Metadata.Session, fixture.sessionName)
	}
	if len(fixture.session.Metadata.Panes) == 0 {
		t.Fatal("metadata should have panes from real tmux session")
	}
	pane := fixture.session.Metadata.Panes[0]
	if pane.Width != 80 {
		t.Errorf("pane width = %d, want 80", pane.Width)
	}
	if pane.Height < 23 || pane.Height > 24 {
		t.Errorf("pane height = %d, want 23 or 24", pane.Height)
	}

	// Send input through the full stack:
	// inputWriter → Session.Run → bridge → relay → PTY → tmux (cat echoes).
	if _, err := fixture.inputWriter.Write([]byte("e2e-input-test\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Verify the input appeared in the tmux pane.
	pollForTmuxContent(t, fixture.server, fixture.sessionName, "e2e-input-test")

	// Verify the echoed output came back through the full return path:
	// tmux → PTY → relay → bridge → Session.Run → output buffer.
	pollForContent(t, fixture.output, "e2e-input-test")

	// Type directly in tmux and verify it reaches the client.
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "tmux-direct-output")
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "Enter")
	pollForContent(t, fixture.output, "tmux-direct-output")

	// Clean shutdown: close the input pipe so Session.Run's input
	// goroutine reads EOF, which triggers the shutdown cascade through
	// the bridge to the relay.
	fixture.inputWriter.Close()
	fixture.awaitShutdown(t)
}

// TestIntegrationReadOnly verifies that read-only mode blocks input at
// the relay level while still allowing output to flow from tmux back
// through the full stack to the client.
func TestIntegrationReadOnly(t *testing.T) {
	t.Parallel()
	fixture := setupIntegration(t, "integration/readonly", true)

	// Send input through the client. In read-only mode, the relay
	// receives the data message but does not write it to the PTY.
	if _, err := fixture.inputWriter.Write([]byte("SHOULD-NOT-APPEAR\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Type something visible in tmux so we know the relay is alive
	// and forwarding output, then verify the blocked input did not
	// appear in the pane.
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "visible-marker")
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "Enter")
	pollForContent(t, fixture.output, "visible-marker")

	content := TmuxCapturePane(t, fixture.server, fixture.sessionName)
	if strings.Contains(content, "SHOULD-NOT-APPEAR") {
		t.Errorf("read-only mode forwarded input to tmux pane:\n%s", content)
	}

	// Verify output still flows: type directly in tmux → relay → client.
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "readonly-output-test")
	TmuxSendKeys(t, fixture.server, fixture.sessionName, "Enter")
	pollForContent(t, fixture.output, "readonly-output-test")

	fixture.inputWriter.Close()
	fixture.awaitShutdown(t)
}

// TestIntegrationResize verifies that terminal resize messages propagate
// through the full stack: client → bridge → relay → PTY TIOCSWINSZ → tmux.
func TestIntegrationResize(t *testing.T) {
	t.Parallel()
	fixture := setupIntegration(t, "integration/resize", false)

	// Send a resize message through the full pipeline. writeMessage is
	// package-private but accessible from tests in the same package.
	if err := fixture.session.writeMessage(NewResizeMessage(120, 40)); err != nil {
		t.Fatalf("send resize: %v", err)
	}

	// Poll tmux for the resize to propagate.
	pollTmuxClientDimensions(t, fixture.server, fixture.sessionName, "120 40")

	fixture.inputWriter.Close()
	fixture.awaitShutdown(t)
}

// pollTmuxClientDimensions polls tmux until the attached client's
// dimensions contain the expected value, or the test deadline expires.
func pollTmuxClientDimensions(t *testing.T, server *tmux.Server, sessionName, expected string) {
	t.Helper()
	for {
		clientDimensions := mustTmuxTrimmed(t, server, "list-clients",
			"-t", sessionName,
			"-F", "#{client_width} #{client_height}")
		if strings.Contains(clientDimensions, expected) {
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("timed out waiting for client dimensions to contain %q (last: %q)",
				expected, clientDimensions)
		}
		runtime.Gosched()
	}
}

// TestIntegrationSessionEnd verifies that killing the tmux session
// propagates cleanly through the full stack: tmux exit → relay detects
// PTY close → relay closes connection → bridge propagates → client's
// Session.Run returns.
func TestIntegrationSessionEnd(t *testing.T) {
	t.Parallel()
	fixture := setupIntegration(t, "integration/end", false)

	// Kill the tmux session. The relay should detect the exit and
	// propagate the shutdown through the bridge to the client.
	if _, err := fixture.server.Run("kill-session", "-t", fixture.sessionName); err != nil {
		t.Fatalf("kill-session: %v", err)
	}

	// Close the input pipe to unblock Session.Run's input goroutine.
	// Without this, the input goroutine blocks on Read indefinitely
	// even after the connection is closed, preventing Run from returning.
	fixture.inputWriter.Close()

	fixture.awaitShutdown(t)
}
