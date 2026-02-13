// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

// mockDaemonObserve creates a mock daemon observation socket that verifies
// credentials were injected, responds with success, and echoes back any
// data it receives (simulating the binary observation protocol phase).
// Returns the socket path.
func mockDaemonObserve(t *testing.T, expectedObserver, expectedToken string) string {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "daemon-observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("mockDaemonObserve: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()

				reader := bufio.NewReader(connection)
				requestLine, readErr := reader.ReadBytes('\n')
				if readErr != nil {
					return
				}

				var request map[string]any
				if jsonErr := json.Unmarshal(requestLine, &request); jsonErr != nil {
					response := map[string]any{"ok": false, "error": "invalid JSON"}
					data, _ := json.Marshal(response)
					connection.Write(append(data, '\n'))
					return
				}

				// Verify credentials were injected by the proxy.
				observer, _ := request["observer"].(string)
				token, _ := request["token"].(string)
				if observer != expectedObserver || token != expectedToken {
					response := map[string]any{
						"ok":    false,
						"error": "unexpected credentials",
					}
					data, _ := json.Marshal(response)
					connection.Write(append(data, '\n'))
					return
				}

				// Send success response.
				response := map[string]any{
					"ok":           true,
					"session":      "bureau/test/agent",
					"machine":      "machine/test",
					"granted_mode": "readwrite",
				}
				data, _ := json.Marshal(response)
				connection.Write(append(data, '\n'))

				// Echo back any data received (simulates binary
				// protocol bridge).
				buffer := make([]byte, 4096)
				for {
					bytesRead, echoErr := reader.Read(buffer)
					if bytesRead > 0 {
						connection.Write(buffer[:bytesRead])
					}
					if echoErr != nil {
						return
					}
				}
			}()
		}
	}()

	return socketPath
}

// mockDaemonObserveError creates a mock daemon that always returns an error.
func mockDaemonObserveError(t *testing.T, errorMessage string) string {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "daemon-observe.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("mockDaemonObserveError: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				reader := bufio.NewReader(connection)
				reader.ReadBytes('\n') // consume request
				response := map[string]any{"ok": false, "error": errorMessage}
				data, _ := json.Marshal(response)
				connection.Write(append(data, '\n'))
			}()
		}
	}()

	return socketPath
}

func TestObserveProxyInjectsCredentials(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "syt_test_proxy_token",
	})

	daemonSocket := mockDaemonObserve(t, "@test/agent:bureau.local", "syt_test_proxy_token")
	observeSocket := filepath.Join(t.TempDir(), "observe.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	// Agent connects without credentials — proxy injects them.
	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe proxy: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	// Send a request without observer/token (the proxy adds them).
	request := map[string]string{
		"principal": "other/agent",
		"mode":      "readwrite",
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send request: %v", err)
	}

	// Read the response (forwarded from mock daemon).
	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if response["ok"] != true {
		t.Fatalf("expected ok=true, got %v (error: %v)", response["ok"], response["error"])
	}
	if response["granted_mode"] != "readwrite" {
		t.Errorf("granted_mode = %v, want readwrite", response["granted_mode"])
	}
}

func TestObserveProxyBridgesData(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	daemonSocket := mockDaemonObserve(t, "@test/agent:bureau.local", "test-token")
	observeSocket := filepath.Join(t.TempDir(), "observe.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	// Handshake.
	json.NewEncoder(connection).Encode(map[string]string{
		"principal": "other/agent",
		"mode":      "readwrite",
	})
	reader := bufio.NewReader(connection)
	reader.ReadBytes('\n') // consume response

	// Send binary data after handshake and verify it echoes back.
	testData := []byte("hello from agent")
	if _, err := connection.Write(testData); err != nil {
		t.Fatalf("write test data: %v", err)
	}

	received := make([]byte, len(testData))
	if _, err := reader.Read(received); err != nil {
		t.Fatalf("read echoed data: %v", err)
	}

	if string(received) != string(testData) {
		t.Errorf("echoed data = %q, want %q", received, testData)
	}
}

func TestObserveProxyForwardsDaemonError(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	daemonSocket := mockDaemonObserveError(t, "principal not found")
	observeSocket := filepath.Join(t.TempDir(), "observe.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	json.NewEncoder(connection).Encode(map[string]string{
		"principal": "nonexistent",
		"mode":      "readonly",
	})

	responseLine, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	json.Unmarshal(responseLine, &response)

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if errorMessage != "principal not found" {
		t.Errorf("error = %q, want 'principal not found'", errorMessage)
	}
}

func TestObserveProxyNoCredentials(t *testing.T) {
	t.Parallel()

	// Credential source with no Matrix credentials.
	credentials := testCredentials(t, map[string]string{
		"SOME_OTHER_KEY": "value",
	})

	observeSocket := filepath.Join(t.TempDir(), "observe.sock")
	// Daemon socket doesn't matter — we should fail before connecting.
	daemonSocket := filepath.Join(t.TempDir(), "nonexistent.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	json.NewEncoder(connection).Encode(map[string]string{
		"principal": "test",
		"mode":      "readonly",
	})

	responseLine, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	json.Unmarshal(responseLine, &response)

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if errorMessage != "proxy has no Matrix credentials configured" {
		t.Errorf("error = %q, want credentials error", errorMessage)
	}
}

func TestObserveProxyDaemonUnreachable(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	observeSocket := filepath.Join(t.TempDir(), "observe.sock")
	daemonSocket := filepath.Join(t.TempDir(), "nonexistent.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	json.NewEncoder(connection).Encode(map[string]string{
		"principal": "test",
		"mode":      "readonly",
	})

	responseLine, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	json.Unmarshal(responseLine, &response)

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
	errorMessage, _ := response["error"].(string)
	if errorMessage == "" {
		t.Error("expected non-empty error message")
	}
}

func TestObserveProxyStripsAgentCredentials(t *testing.T) {
	t.Parallel()

	// Verify that even if the agent sends observer/token, the proxy
	// replaces them with its own.
	var receivedObserver, receivedToken string
	var receivedMutex sync.Mutex

	socketDir := t.TempDir()
	daemonSocket := filepath.Join(socketDir, "daemon.sock")
	listener, err := net.Listen("unix", daemonSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()

		reader := bufio.NewReader(connection)
		requestLine, _ := reader.ReadBytes('\n')
		var request map[string]any
		json.Unmarshal(requestLine, &request)

		receivedMutex.Lock()
		receivedObserver, _ = request["observer"].(string)
		receivedToken, _ = request["token"].(string)
		receivedMutex.Unlock()

		response := map[string]any{"ok": false, "error": "test done"}
		data, _ := json.Marshal(response)
		connection.Write(append(data, '\n'))
	}()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@proxy/identity:bureau.local",
		"MATRIX_TOKEN":   "proxy-token-secret",
	})

	observeSocket := filepath.Join(t.TempDir(), "observe.sock")
	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	// Agent tries to send its own credentials (which should be replaced).
	agentRequest := map[string]string{
		"principal": "target/agent",
		"mode":      "readwrite",
		"observer":  "@evil/attacker:bureau.local",
		"token":     "stolen-token",
	}
	json.NewEncoder(connection).Encode(agentRequest)
	bufio.NewReader(connection).ReadBytes('\n') // consume response

	receivedMutex.Lock()
	defer receivedMutex.Unlock()

	if receivedObserver != "@proxy/identity:bureau.local" {
		t.Errorf("observer = %q, want @proxy/identity:bureau.local (proxy should replace agent's value)",
			receivedObserver)
	}
	if receivedToken != "proxy-token-secret" {
		t.Errorf("token = %q, want proxy-token-secret (proxy should replace agent's value)",
			receivedToken)
	}
}

func TestObserveProxyConcurrentConnections(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	daemonSocket := mockDaemonObserve(t, "@test/agent:bureau.local", "test-token")
	observeSocket := filepath.Join(t.TempDir(), "observe.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	// Open multiple concurrent connections.
	const connectionCount = 5
	var waitGroup sync.WaitGroup

	for index := 0; index < connectionCount; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			connection, dialErr := net.DialTimeout("unix", observeSocket, 5*time.Second)
			if dialErr != nil {
				t.Errorf("dial: %v", dialErr)
				return
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

			json.NewEncoder(connection).Encode(map[string]string{
				"principal": "test/agent",
				"mode":      "readonly",
			})

			responseLine, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr != nil {
				t.Errorf("read: %v", readErr)
				return
			}

			var response map[string]any
			json.Unmarshal(responseLine, &response)
			if response["ok"] != true {
				t.Errorf("expected ok=true, got %v", response["ok"])
			}
		}()
	}

	waitGroup.Wait()
}

func TestObserveProxyInvalidJSON(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	observeSocket := filepath.Join(t.TempDir(), "observe.sock")
	daemonSocket := filepath.Join(t.TempDir(), "daemon.sock")
	// Create the daemon socket so the proxy can be created.
	daemonListener, _ := net.Listen("unix", daemonSocket)
	defer daemonListener.Close()

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}
	defer proxy.stop()

	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	// Send invalid JSON.
	connection.Write([]byte("not json\n"))

	responseLine, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	json.Unmarshal(responseLine, &response)
	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

// TestObserveProxyStopDrainsActiveBridges verifies that stop() waits for
// in-flight bridged connections to finish before returning. This is the
// exact scenario that caused a CI panic: stop() returned, the credential
// source was closed, and a handleConnection goroutine still in flight
// called userIDBuffer.String() on a closed *secret.Buffer.
func TestObserveProxyStopDrainsActiveBridges(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test/agent:bureau.local",
		"MATRIX_TOKEN":   "test-token",
	})

	daemonSocket := mockDaemonObserve(t, "@test/agent:bureau.local", "test-token")
	observeSocket := filepath.Join(t.TempDir(), "observe.sock")

	proxy, err := startObserveProxy(observeProxyConfig{
		SocketPath:   observeSocket,
		DaemonSocket: daemonSocket,
		Credential:   credentials,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("startObserveProxy: %v", err)
	}

	// Establish a bridged session (handshake completes, now in binary bridge).
	connection, err := net.DialTimeout("unix", observeSocket, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:realclock // kernel I/O deadline

	json.NewEncoder(connection).Encode(map[string]string{
		"principal": "test/agent",
		"mode":      "readwrite",
	})
	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var response map[string]any
	json.Unmarshal(responseLine, &response)
	if response["ok"] != true {
		t.Fatalf("handshake failed: %v", response["error"])
	}

	// Bridge is now active. Verify data flows.
	testData := []byte("pre-stop data")
	if _, err := connection.Write(testData); err != nil {
		t.Fatalf("write pre-stop data: %v", err)
	}
	received := make([]byte, len(testData))
	if _, err := reader.Read(received); err != nil {
		t.Fatalf("read pre-stop echo: %v", err)
	}

	// Stop the proxy while the bridge is active. This must force-close
	// the connections and wait for the goroutine to drain. If it doesn't
	// wait, closing the credential source afterward would panic.
	stopDone := make(chan struct{})
	go func() {
		proxy.stop()
		close(stopDone)
	}()

	testutil.RequireClosed(t, stopDone, 5*time.Second, "stop() draining goroutines")

	// Close the credential source AFTER stop(). Before the fix, this
	// would cause a panic in a handleConnection goroutine that was still
	// running. With the fix, all goroutines have exited by now.
	credentials.Close()

	// Verify the connection was force-closed by stop().
	_, err = connection.Read(make([]byte, 1))
	if err == nil {
		t.Error("expected read error after stop(), connection should be closed")
	}
}

func TestStartObserveProxyValidation(t *testing.T) {
	t.Parallel()

	credentials := testCredentials(t, map[string]string{
		"MATRIX_USER_ID": "@test:bureau.local",
		"MATRIX_TOKEN":   "token",
	})

	t.Run("missing socket path", func(t *testing.T) {
		_, err := startObserveProxy(observeProxyConfig{
			DaemonSocket: "/tmp/daemon.sock",
			Credential:   credentials,
		})
		if err == nil {
			t.Error("expected error for missing socket path")
		}
	})

	t.Run("missing daemon socket", func(t *testing.T) {
		_, err := startObserveProxy(observeProxyConfig{
			SocketPath: filepath.Join(t.TempDir(), "observe.sock"),
			Credential: credentials,
		})
		if err == nil {
			t.Error("expected error for missing daemon socket")
		}
	})

	t.Run("missing credentials", func(t *testing.T) {
		_, err := startObserveProxy(observeProxyConfig{
			SocketPath:   filepath.Join(t.TempDir(), "observe.sock"),
			DaemonSocket: "/tmp/daemon.sock",
		})
		if err == nil {
			t.Error("expected error for missing credentials")
		}
	})
}
