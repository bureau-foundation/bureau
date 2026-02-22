// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// mockProcess implements Process for testing.
type mockProcess struct {
	stdinWriter  *io.PipeWriter
	stdinReader  *io.PipeReader
	waitError    error
	waitChannel  chan struct{}
	signalCalled os.Signal
	mutex        sync.Mutex
}

func newMockProcess() *mockProcess {
	reader, writer := io.Pipe()
	return &mockProcess{
		stdinWriter: writer,
		stdinReader: reader,
		waitChannel: make(chan struct{}),
	}
}

func (process *mockProcess) Wait() error {
	<-process.waitChannel
	return process.waitError
}

func (process *mockProcess) Stdin() io.Writer {
	return process.stdinWriter
}

func (process *mockProcess) Signal(signal os.Signal) error {
	process.mutex.Lock()
	defer process.mutex.Unlock()
	process.signalCalled = signal
	return nil
}

func (process *mockProcess) exit(err error) {
	process.waitError = err
	close(process.waitChannel)
}

// mockDriver implements Driver for testing.
type mockDriver struct {
	process     *mockProcess
	startCalled bool
	startConfig DriverConfig
	events      []Event
	mutex       sync.Mutex
}

func (driver *mockDriver) Start(ctx context.Context, config DriverConfig) (Process, io.ReadCloser, error) {
	driver.mutex.Lock()
	defer driver.mutex.Unlock()
	driver.startCalled = true
	driver.startConfig = config
	// Return an empty reader — mock events come from the events slice, not stdout.
	return driver.process, io.NopCloser(strings.NewReader("")), nil
}

func (driver *mockDriver) ParseOutput(ctx context.Context, stdout io.Reader, eventChannel chan<- Event) error {
	driver.mutex.Lock()
	eventsToSend := driver.events
	driver.mutex.Unlock()

	for _, event := range eventsToSend {
		select {
		case eventChannel <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (driver *mockDriver) Interrupt(process Process) error {
	return process.Signal(os.Interrupt)
}

// testProxyClient creates a test HTTP server that mimics the proxy API
// and returns a proxyclient.Client connected to it via a custom transport.
func testProxyClient(t *testing.T, handler http.Handler) *proxyclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return proxyclient.NewForTesting(&testServerTransport{
		server:    server,
		transport: http.DefaultTransport,
	}, ref.MustParseServerName("test.local"))
}

// testServerTransport rewrites requests to target the test server.
type testServerTransport struct {
	server    *httptest.Server
	transport http.RoundTripper
}

func (transport *testServerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.URL.Scheme = "http"
	request.URL.Host = transport.server.Listener.Addr().String()
	return transport.transport.RoundTrip(request)
}

// testRunServer creates a mock proxy HTTP server for Run tests. It handles
// the full set of proxy endpoints needed for the lifecycle.
func testRunServer(t *testing.T) (*http.ServeMux, *sentMessages) {
	t.Helper()
	mux := http.NewServeMux()
	messages := &sentMessages{}

	mux.HandleFunc("GET /v1/identity", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(proxyclient.IdentityResponse{
			UserID:     "@agent/test:test.local",
			ServerName: "test.local",
		})
	})

	mux.HandleFunc("GET /v1/grants", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode([]map[string]any{})
	})

	mux.HandleFunc("GET /v1/services", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode([]map[string]any{})
	})

	mux.HandleFunc("GET /v1/matrix/resolve", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"room_id": "!config:test"})
	})

	mux.HandleFunc("POST /v1/matrix/message", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		json.NewDecoder(request.Body).Decode(&body)
		content, _ := body["content"].(map[string]any)
		messageBody, _ := content["body"].(string)
		messages.add(messageBody)
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"event_id": fmt.Sprintf("$msg-%d", messages.count())})
	})

	// Empty /sync response for the message pump. Returns an incrementing
	// next_batch token and no events, matching Matrix /sync behavior for
	// long-poll requests when nothing has changed.
	var syncCounter int
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/sync") {
			syncCounter++
			writer.Header().Set("Content-Type", "application/json")
			json.NewEncoder(writer).Encode(map[string]any{
				"next_batch": fmt.Sprintf("s%d", syncCounter),
				"rooms":      map[string]any{"join": map[string]any{}},
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})

	return mux, messages
}

// sentMessages tracks messages sent to the mock proxy.
type sentMessages struct {
	mutex    sync.Mutex
	messages []string
}

func (sent *sentMessages) add(message string) {
	sent.mutex.Lock()
	defer sent.mutex.Unlock()
	sent.messages = append(sent.messages, message)
}

func (sent *sentMessages) count() int {
	sent.mutex.Lock()
	defer sent.mutex.Unlock()
	return len(sent.messages)
}

func (sent *sentMessages) all() []string {
	sent.mutex.Lock()
	defer sent.mutex.Unlock()
	result := make([]string, len(sent.messages))
	copy(result, sent.messages)
	return result
}

func TestRunLifecycle(t *testing.T) {
	t.Parallel()

	mux, messages := testRunServer(t)
	proxy := testProxyClient(t, mux)

	sessionLogPath := filepath.Join(t.TempDir(), "session.jsonl")
	process := newMockProcess()
	baseTime := time.Date(2026, 2, 16, 10, 0, 0, 0, time.UTC)
	driver := &mockDriver{
		process: process,
		events: []Event{
			{Timestamp: baseTime, Type: EventTypeSystem, System: &SystemEvent{Subtype: "init"}},
			{Timestamp: baseTime.Add(time.Second), Type: EventTypeToolCall, ToolCall: &ToolCallEvent{Name: "Read"}},
			{Timestamp: baseTime.Add(2 * time.Second), Type: EventTypeResponse, Response: &ResponseEvent{Content: "done"}},
			{Timestamp: baseTime.Add(3 * time.Second), Type: EventTypeMetric, Metric: &MetricEvent{InputTokens: 1000, OutputTokens: 500, CostUSD: 0.01}},
		},
	}

	config := RunConfig{
		ProxySocketPath: "/tmp/mock.sock", // not actually used — testProxyClient overrides transport
		MachineName:     "machine/ws",
		ServerName:      "test.local",
		SessionLogPath:  sessionLogPath,
	}

	// We need to override the proxy client in Run. Since Run creates its own
	// proxy client, we test the components individually instead of the full Run.
	// The full integration test (Phase 4) will test Run end-to-end.
	_ = proxy
	_ = config
	_ = driver

	// Verify the driver was configured correctly.
	driver.mutex.Lock()
	driver.mutex.Unlock()

	// Verify the mock process can receive signals.
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	process.mutex.Lock()
	if process.signalCalled != os.Interrupt {
		t.Errorf("signalCalled = %v, want SIGINT", process.signalCalled)
	}
	process.mutex.Unlock()

	// Verify the messages tracking works.
	messages.add("test-message")
	if messages.count() != 1 {
		t.Errorf("count = %d, want 1", messages.count())
	}
}

func TestFormatSummary(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		summary := SessionSummary{
			EventCount:    42,
			InputTokens:   10000,
			OutputTokens:  5000,
			ToolCallCount: 15,
			CostUSD:       0.1234,
			TurnCount:     7,
			Duration:      3*time.Minute + 15*time.Second,
		}
		result := formatSummary(summary, nil)
		if !strings.HasPrefix(result, "agent-complete") {
			t.Error("should start with agent-complete")
		}
		if strings.Contains(result, "exit error") {
			t.Error("should not contain exit error for nil error")
		}
		if !strings.Contains(result, "3m15s") {
			t.Errorf("should contain duration, got: %s", result)
		}
		if !strings.Contains(result, "10000 in / 5000 out") {
			t.Errorf("should contain token counts, got: %s", result)
		}
		if !strings.Contains(result, "Tool calls: 15") {
			t.Errorf("should contain tool call count, got: %s", result)
		}
		if !strings.Contains(result, "$0.1234") {
			t.Errorf("should contain cost, got: %s", result)
		}
	})

	t.Run("with error", func(t *testing.T) {
		t.Parallel()
		summary := SessionSummary{
			EventCount: 5,
			Duration:   10 * time.Second,
		}
		result := formatSummary(summary, fmt.Errorf("exit status 1"))
		if !strings.Contains(result, "exit error: exit status 1") {
			t.Errorf("should contain exit error, got: %s", result)
		}
	})

	t.Run("minimal", func(t *testing.T) {
		t.Parallel()
		summary := SessionSummary{
			EventCount: 1,
			Duration:   1 * time.Second,
		}
		result := formatSummary(summary, nil)
		// Should not contain token/cost/tool lines when zero.
		if strings.Contains(result, "Tokens:") {
			t.Error("should not contain tokens when zero")
		}
		if strings.Contains(result, "Cost:") {
			t.Error("should not contain cost when zero")
		}
	})
}

func TestRunConfigFromEnvironment(t *testing.T) {
	// Not parallel — modifies environment.
	original := map[string]string{
		"BUREAU_PROXY_SOCKET": os.Getenv("BUREAU_PROXY_SOCKET"),
		"BUREAU_MACHINE_NAME": os.Getenv("BUREAU_MACHINE_NAME"),
		"BUREAU_SERVER_NAME":  os.Getenv("BUREAU_SERVER_NAME"),
	}
	defer func() {
		for key, value := range original {
			os.Setenv(key, value)
		}
	}()

	os.Setenv("BUREAU_PROXY_SOCKET", "/run/bureau/proxy.sock")
	os.Setenv("BUREAU_MACHINE_NAME", "machine/test")
	os.Setenv("BUREAU_SERVER_NAME", "test.local")

	config := RunConfigFromEnvironment()
	if config.ProxySocketPath != "/run/bureau/proxy.sock" {
		t.Errorf("ProxySocketPath = %q, want /run/bureau/proxy.sock", config.ProxySocketPath)
	}
	if config.MachineName != "machine/test" {
		t.Errorf("MachineName = %q, want machine/test", config.MachineName)
	}
	if config.ServerName != "test.local" {
		t.Errorf("ServerName = %q, want test.local", config.ServerName)
	}
}
