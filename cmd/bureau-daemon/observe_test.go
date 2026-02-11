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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/observe"
)

// testObserverToken is the Matrix access token used by test observers.
// The mock Matrix server in newTestDaemonWithObserve accepts this token
// and returns testObserverUserID from whoami.
const testObserverToken = "syt_test_observer_token"

// testObserverUserID is the Matrix user ID returned by the mock whoami
// handler when testObserverToken is presented.
const testObserverUserID = "@ops/test-observer:bureau.local"

// buildMockRelay returns the path to the pre-built mock observation relay
// binary. The binary is provided as a Bazel data dependency via
// BUREAU_MOCK_RELAY_BINARY.
func buildMockRelay(t *testing.T) string {
	t.Helper()
	return testutil.DataBinary(t, "BUREAU_MOCK_RELAY_BINARY")
}

// connectObserve connects to the daemon's observe socket and sends an
// observation request. The test observer token is automatically included
// if the request doesn't already have one. Returns the connection and
// decoded response.
func connectObserve(t *testing.T, socketPath string, request observeRequest) (net.Conn, observeResponse) {
	if request.Token == "" {
		request.Token = testObserverToken
	}
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
// testing. It starts a mock Matrix server for token verification, configures
// a permissive ObservePolicy (AllowedObservers: ["**"], ReadWriteObservers:
// ["**"]), starts the observe listener, and returns the daemon.
func newTestDaemonWithObserve(t *testing.T, relayBinary string, runningPrincipals []string) *Daemon {
	t.Helper()

	socketDir := testutil.SocketDir(t)
	observeSocketPath := filepath.Join(socketDir, "observe.sock")
	tmuxSocket := filepath.Join(socketDir, "tmux.sock")

	running := make(map[string]bool)
	for _, localpart := range runningPrincipals {
		running[localpart] = true
	}

	// Mock Matrix server for token verification. Accepts testObserverToken
	// and returns testObserverUserID from /account/whoami.
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			authorization := r.Header.Get("Authorization")
			if authorization != "Bearer "+testObserverToken {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{
					"errcode": "M_UNKNOWN_TOKEN",
					"error":   "Invalid token",
				})
				return
			}
			json.NewEncoder(w).Encode(messaging.WhoAmIResponse{
				UserID: testObserverUserID,
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(matrixServer.Close)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Build PrincipalAssignment list for the test config. Each running
	// principal gets a permissive ObservePolicy.
	var principals []schema.PrincipalAssignment
	for _, localpart := range runningPrincipals {
		principals = append(principals, schema.PrincipalAssignment{
			Localpart: localpart,
			AutoStart: true,
		})
	}

	daemon := &Daemon{
		runDir:        principal.DefaultRunDir,
		client:        client,
		tokenVerifier: newTokenVerifier(client, 5*time.Minute, logger),
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers:   []string{"**"},
				ReadWriteObservers: []string{"**"},
			},
			Principals: principals,
		},
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
		logger:             logger,
	}

	ctx := context.Background()
	if err := daemon.startObserveListener(ctx); err != nil {
		t.Fatalf("startObserveListener: %v", err)
	}
	t.Cleanup(daemon.stopObserveListener)

	return daemon
}

// connectList connects to the daemon's observe socket and sends a list
// request. Returns the decoded ListResponse.
func connectList(t *testing.T, socketPath string, observable bool) observe.ListResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second))

	request := observeRequest{Action: "list", Observable: observable, Token: testObserverToken}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send list request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read list response: %v", err)
	}

	var response observe.ListResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	return response
}

func TestListLocalPrincipals(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay",
		[]string{"iree/amdgpu/pm", "service/stt/whisper"})

	response := connectList(t, daemon.observeSocketPath, false)
	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	if len(response.Principals) != 2 {
		t.Fatalf("got %d principals, want 2", len(response.Principals))
	}

	// Verify both are local, observable, on the test machine.
	foundPrincipals := make(map[string]observe.ListPrincipal)
	for _, principal := range response.Principals {
		foundPrincipals[principal.Localpart] = principal
	}

	for _, localpart := range []string{"iree/amdgpu/pm", "service/stt/whisper"} {
		principal, ok := foundPrincipals[localpart]
		if !ok {
			t.Errorf("missing principal %s", localpart)
			continue
		}
		if !principal.Observable {
			t.Errorf("%s should be observable", localpart)
		}
		if !principal.Local {
			t.Errorf("%s should be local", localpart)
		}
		if principal.Machine != "machine/test" {
			t.Errorf("%s machine = %q, want machine/test", localpart, principal.Machine)
		}
	}

	// Self machine should always appear.
	if len(response.Machines) != 1 {
		t.Fatalf("got %d machines, want 1", len(response.Machines))
	}
	if !response.Machines[0].Self {
		t.Error("expected self machine")
	}
	if response.Machines[0].Name != "machine/test" {
		t.Errorf("machine name = %q, want machine/test", response.Machines[0].Name)
	}
}

func TestListWithRemoteServices(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay",
		[]string{"agent/alice"})

	// Add a remote service to the daemon's service directory.
	daemon.services["service/tts/piper"] = &schema.Service{
		Principal: "@service/tts/piper:bureau.local",
		Machine:   "@machine/cloud-gpu:bureau.local",
		Protocol:  "http",
	}
	// Add the peer address so the remote service is reachable.
	daemon.peerAddresses["@machine/cloud-gpu:bureau.local"] = "192.168.1.100:9090"
	daemon.transportDialer = &testTCPDialer{}

	response := connectList(t, daemon.observeSocketPath, false)
	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	if len(response.Principals) != 2 {
		t.Fatalf("got %d principals, want 2", len(response.Principals))
	}

	foundPrincipals := make(map[string]observe.ListPrincipal)
	for _, principal := range response.Principals {
		foundPrincipals[principal.Localpart] = principal
	}

	// Local agent should be observable and local.
	alice := foundPrincipals["agent/alice"]
	if !alice.Observable || !alice.Local {
		t.Errorf("agent/alice: observable=%v local=%v, want true/true", alice.Observable, alice.Local)
	}

	// Remote service should be observable (peer is reachable) but not local.
	piper := foundPrincipals["service/tts/piper"]
	if !piper.Observable {
		t.Error("service/tts/piper should be observable (peer reachable)")
	}
	if piper.Local {
		t.Error("service/tts/piper should not be local")
	}
	if piper.Machine != "machine/cloud-gpu" {
		t.Errorf("service/tts/piper machine = %q, want machine/cloud-gpu", piper.Machine)
	}

	// Should see two machines: self + peer.
	if len(response.Machines) != 2 {
		t.Fatalf("got %d machines, want 2", len(response.Machines))
	}
}

func TestListObservableFilter(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay",
		[]string{"agent/alice"})

	// Add a remote service with NO reachable peer (unreachable).
	daemon.services["service/tts/piper"] = &schema.Service{
		Principal: "@service/tts/piper:bureau.local",
		Machine:   "@machine/cloud-gpu:bureau.local",
		Protocol:  "http",
	}
	// No peer address and no transport dialer → not observable.

	// Without filter: both principals appear.
	allResponse := connectList(t, daemon.observeSocketPath, false)
	if !allResponse.OK {
		t.Fatalf("expected OK, got error: %s", allResponse.Error)
	}
	if len(allResponse.Principals) != 2 {
		t.Fatalf("unfiltered: got %d principals, want 2", len(allResponse.Principals))
	}

	// With observable filter: only the local running principal appears.
	filteredResponse := connectList(t, daemon.observeSocketPath, true)
	if !filteredResponse.OK {
		t.Fatalf("expected OK, got error: %s", filteredResponse.Error)
	}
	if len(filteredResponse.Principals) != 1 {
		t.Fatalf("filtered: got %d principals, want 1", len(filteredResponse.Principals))
	}
	if filteredResponse.Principals[0].Localpart != "agent/alice" {
		t.Errorf("filtered principal = %q, want agent/alice", filteredResponse.Principals[0].Localpart)
	}
}

func TestListNoPrincipals(t *testing.T) {
	daemon := newTestDaemonWithObserve(t, "/nonexistent/relay", nil)

	response := connectList(t, daemon.observeSocketPath, false)
	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if len(response.Principals) != 0 {
		t.Errorf("got %d principals, want 0", len(response.Principals))
	}
	// Self machine should still appear.
	if len(response.Machines) != 1 {
		t.Errorf("got %d machines, want 1", len(response.Machines))
	}
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
