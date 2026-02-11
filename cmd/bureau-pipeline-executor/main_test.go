// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// mockProxy serves the /v1/matrix/* API for executor tests. Records all
// requests for assertion.
type mockProxy struct {
	mu sync.Mutex

	whoamiUserID     string
	resolveResponses map[string]string // alias → room ID
	stateResponses   map[string]string // "roomID/type/key" → JSON content
	putStateEventID  string
	messageEventID   string

	recordedPuts     []recordedRequest
	recordedMessages []recordedRequest
}

type recordedRequest struct {
	Path string
	Body string
}

func newMockProxy() *mockProxy {
	return &mockProxy{
		whoamiUserID:     "@pipeline/test:bureau.local",
		resolveResponses: map[string]string{},
		stateResponses:   map[string]string{},
		putStateEventID:  "$state_1",
		messageEventID:   "$msg_1",
	}
}

func (m *mockProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/matrix/whoami":
		json.NewEncoder(w).Encode(map[string]string{
			"user_id": m.whoamiUserID,
		})

	case r.Method == "GET" && r.URL.Path == "/v1/matrix/resolve":
		alias := r.URL.Query().Get("alias")
		m.mu.Lock()
		roomID, ok := m.resolveResponses[alias]
		m.mu.Unlock()
		if ok {
			json.NewEncoder(w).Encode(map[string]string{"room_id": roomID})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"errcode": "M_NOT_FOUND", "error": "not found"})
		}

	case r.Method == "GET" && r.URL.Path == "/v1/matrix/state":
		room := r.URL.Query().Get("room")
		eventType := r.URL.Query().Get("type")
		stateKey := r.URL.Query().Get("key")
		lookupKey := room + "/" + eventType + "/" + stateKey
		m.mu.Lock()
		content, ok := m.stateResponses[lookupKey]
		m.mu.Unlock()
		if ok {
			w.Write([]byte(content))
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"errcode": "M_NOT_FOUND", "error": "not found"})
		}

	case r.Method == "POST" && r.URL.Path == "/v1/matrix/state":
		m.mu.Lock()
		m.recordedPuts = append(m.recordedPuts, recordedRequest{
			Path: r.URL.Path,
			Body: string(body),
		})
		eventID := m.putStateEventID
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"event_id": eventID})

	case r.Method == "POST" && r.URL.Path == "/v1/matrix/message":
		m.mu.Lock()
		m.recordedMessages = append(m.recordedMessages, recordedRequest{
			Path: r.URL.Path,
			Body: string(body),
		})
		eventID := m.messageEventID
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"event_id": eventID})

	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":"unrecognized: %s %s"}`, r.Method, r.URL.Path)
	}
}

// startMockProxy starts a mock proxy on a Unix socket and returns the
// socket path and mock handler.
func startMockProxy(t *testing.T) (string, *mockProxy) {
	t.Helper()

	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "proxy.sock")

	mock := newMockProxy()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on mock proxy socket: %v", err)
	}
	server := &http.Server{Handler: mock}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	return socketPath, mock
}

// writePipeline writes a JSONC pipeline file and returns its path.
func writePipeline(t *testing.T, name string, content *schema.PipelineContent) string {
	t.Helper()

	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		t.Fatalf("marshal pipeline: %v", err)
	}

	path := filepath.Join(t.TempDir(), name+".jsonc")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write pipeline: %v", err)
	}
	return path
}

// writePayload writes a payload JSON file and returns its path.
func writePayload(t *testing.T, payload map[string]any) string {
	t.Helper()

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return path
}

func TestRunSteps(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "test-pipeline", &schema.PipelineContent{
		Description: "Test pipeline",
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "echo step one"},
			{Name: "step-two", Run: "echo step two"},
			{Name: "step-three", Run: "echo step three"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent-payload.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestWhenGuardSkip(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "when-skip", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "always-runs", Run: "true"},
			{Name: "skipped", Run: "echo should not run", When: "false"},
			{Name: "also-runs", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestWhenGuardPass(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "when-pass", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "guarded-pass", Run: "true", When: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestCheckFailure(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "check-fail", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "passes-then-fails-check", Run: "true", Check: "false"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err == nil {
		t.Fatal("expected error from check failure")
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("expected check-related error, got: %v", err)
	}
}

func TestOptionalStepContinues(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "optional-fail", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "optional-fail", Run: "false", Optional: true},
			{Name: "step-three", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("pipeline should continue past optional failure, got: %v", err)
	}
}

func TestRequiredStepAborts(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "required-fail", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "required-fail", Run: "false"},
			{Name: "never-reached", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err == nil {
		t.Fatal("expected error from required step failure")
	}
	if !strings.Contains(err.Error(), "required-fail") {
		t.Errorf("expected error mentioning step name, got: %v", err)
	}
}

func TestPublishStep(t *testing.T) {
	socketPath, mock := startMockProxy(t)

	pipelinePath := writePipeline(t, "publish-test", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{
				Name: "publish-ready",
				Publish: &schema.PipelinePublish{
					EventType: "m.bureau.workspace.ready",
					Room:      "!room1:bureau.local",
					StateKey:  "",
					Content:   map[string]any{"version": "1.0"},
				},
			},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.recordedPuts) != 1 {
		t.Fatalf("expected 1 PUT state request, got %d", len(mock.recordedPuts))
	}

	var putBody struct {
		Room      string         `json:"room"`
		EventType string         `json:"event_type"`
		StateKey  string         `json:"state_key"`
		Content   map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(mock.recordedPuts[0].Body), &putBody); err != nil {
		t.Fatalf("unmarshal PUT body: %v", err)
	}
	if putBody.EventType != "m.bureau.workspace.ready" {
		t.Errorf("expected event_type m.bureau.workspace.ready, got %q", putBody.EventType)
	}
	if putBody.Room != "!room1:bureau.local" {
		t.Errorf("expected room !room1:bureau.local, got %q", putBody.Room)
	}
}

func TestTimeout(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "timeout-test", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "slow-step", Run: "sleep 30", Timeout: "200ms"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}

	startTime := time.Now()
	err := run()
	elapsed := time.Since(startTime)

	if err == nil {
		t.Fatal("expected error from timeout")
	}
	// Should complete well under 5 seconds (the 200ms timeout should kick in).
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestVariableExpansion(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	payloadPath := writePayload(t, map[string]any{
		"REPOSITORY": "https://github.com/iree-org/iree.git",
		"PROJECT":    "iree",
	})

	pipelinePath := writePipeline(t, "var-test", &schema.PipelineContent{
		Variables: map[string]schema.PipelineVariable{
			"REPOSITORY": {Required: true},
			"PROJECT":    {Required: true},
		},
		Steps: []schema.PipelineStep{
			{Name: "use-vars", Run: `echo "repo=${REPOSITORY} project=${PROJECT}"`},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", payloadPath)

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestPipelineRefResolution(t *testing.T) {
	socketPath, mock := startMockProxy(t)

	// Set up ref resolution: bureau/pipeline:dev-workspace-init
	// → alias #bureau/pipeline:bureau.local → room ID → state event
	mock.resolveResponses["#bureau/pipeline:bureau.local"] = "!pipeline_room:bureau.local"
	mock.stateResponses["!pipeline_room:bureau.local/m.bureau.pipeline/dev-workspace-init"] = `{
		"description": "Test pipeline from ref",
		"steps": [{"name": "hello", "run": "echo hello from ref"}]
	}`

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", "bureau/pipeline:dev-workspace-init"}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestFileResolution(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "file-resolve", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "from-file", Run: "echo from file"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestThreadLogging(t *testing.T) {
	socketPath, mock := startMockProxy(t)

	pipelinePath := writePipeline(t, "log-test", &schema.PipelineContent{
		Log: &schema.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "step-two", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Should have: 1 root message + 2 step replies + 1 complete reply = 4 messages.
	if len(mock.recordedMessages) != 4 {
		t.Fatalf("expected 4 messages (root + 2 steps + complete), got %d", len(mock.recordedMessages))
	}

	// Root message should mention "started".
	if !strings.Contains(mock.recordedMessages[0].Body, "started") {
		t.Errorf("root message should mention 'started', got: %s", mock.recordedMessages[0].Body)
	}

	// Step replies should contain m.relates_to.
	for index := 1; index < len(mock.recordedMessages); index++ {
		if !strings.Contains(mock.recordedMessages[index].Body, "m.relates_to") {
			t.Errorf("message %d should be a thread reply (contain m.relates_to), got: %s",
				index, mock.recordedMessages[index].Body)
		}
	}
}

func TestThreadLoggingWithAliasResolution(t *testing.T) {
	socketPath, mock := startMockProxy(t)
	mock.resolveResponses["#iree/workspace:bureau.local"] = "!workspace_room:bureau.local"

	pipelinePath := writePipeline(t, "log-alias-test", &schema.PipelineContent{
		Variables: map[string]schema.PipelineVariable{
			"WORKSPACE_ROOM": {Default: "#iree/workspace:bureau.local"},
		},
		Log: &schema.PipelineLog{Room: "${WORKSPACE_ROOM}"},
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	messageCount := len(mock.recordedMessages)
	mock.mu.Unlock()

	// Should have: root + 1 step + complete = 3 messages.
	if messageCount != 3 {
		t.Errorf("expected 3 messages, got %d", messageCount)
	}
}

func TestThreadLoggingFailureIsFatal(t *testing.T) {
	// Use a dedicated mock that returns errors on message send, rather
	// than the standard mock which always succeeds.
	failMock := &failingMessageProxy{
		whoamiUserID: "@pipeline/test:bureau.local",
	}
	tempDir := t.TempDir()
	failSocket := filepath.Join(tempDir, "fail-proxy.sock")
	listener, err := net.Listen("unix", failSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	failServer := &http.Server{Handler: failMock}
	go failServer.Serve(listener)
	t.Cleanup(func() { failServer.Close() })

	pipelinePath := writePipeline(t, "log-fatal-test", &schema.PipelineContent{
		Log: &schema.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", failSocket)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err = run()
	if err == nil {
		t.Fatal("expected fatal error when thread creation fails")
	}
	if !strings.Contains(err.Error(), "creating pipeline log thread") {
		t.Errorf("expected thread creation error, got: %v", err)
	}
}

// failingMessageProxy returns errors on POST /v1/matrix/message.
type failingMessageProxy struct {
	whoamiUserID string
}

func (m *failingMessageProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" && r.URL.Path == "/v1/matrix/whoami" {
		json.NewEncoder(w).Encode(map[string]string{"user_id": m.whoamiUserID})
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/matrix/message" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "no permission"})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func TestNoLogConfigRunsWithoutThread(t *testing.T) {
	socketPath, mock := startMockProxy(t)

	pipelinePath := writePipeline(t, "no-log", &schema.PipelineContent{
		// No Log field — thread logging disabled.
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	messageCount := len(mock.recordedMessages)
	mock.mu.Unlock()

	// No thread logging → no messages sent.
	if messageCount != 0 {
		t.Errorf("expected 0 messages with no log config, got %d", messageCount)
	}
}

func TestPayloadVariableConversion(t *testing.T) {
	payloadPath := writePayload(t, map[string]any{
		"string_var":      "hello",
		"number_var":      42.0,
		"float_var":       3.14,
		"bool_true":       true,
		"bool_false":      false,
		"null_var":        nil,
		"object_var":      map[string]any{"nested": "value"},
		"pipeline_ref":    "bureau/pipeline:should-be-excluded",
		"pipeline_inline": map[string]any{"steps": []any{}},
	})

	variables, err := loadPayloadVariables(payloadPath)
	if err != nil {
		t.Fatalf("loadPayloadVariables: %v", err)
	}

	tests := []struct {
		key      string
		expected string
		present  bool
	}{
		{"string_var", "hello", true},
		{"number_var", "42", true},
		{"float_var", "3.14", true},
		{"bool_true", "true", true},
		{"bool_false", "false", true},
		{"null_var", "", false},
		{"object_var", `{"nested":"value"}`, true},
		{"pipeline_ref", "", false},
		{"pipeline_inline", "", false},
	}

	for _, test := range tests {
		value, present := variables[test.key]
		if present != test.present {
			t.Errorf("key %q: present=%v, want %v", test.key, present, test.present)
			continue
		}
		if present && value != test.expected {
			t.Errorf("key %q: value=%q, want %q", test.key, value, test.expected)
		}
	}
}

func TestPayloadInlineResolution(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	payloadPath := writePayload(t, map[string]any{
		"pipeline_inline": map[string]any{
			"steps": []any{
				map[string]any{"name": "from-inline", "run": "echo inline"},
			},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", payloadPath)

	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestPayloadRefResolution(t *testing.T) {
	socketPath, mock := startMockProxy(t)

	mock.resolveResponses["#bureau/pipeline:bureau.local"] = "!pipeline_room:bureau.local"
	mock.stateResponses["!pipeline_room:bureau.local/m.bureau.pipeline/test-pipeline"] = `{
		"steps": [{"name": "from-ref", "run": "echo from payload ref"}]
	}`

	payloadPath := writePayload(t, map[string]any{
		"pipeline_ref": "bureau/pipeline:test-pipeline",
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", payloadPath)

	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestIsFilePath(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"/path/to/pipeline.jsonc", true},
		{"./relative/pipeline.json", true},
		{"../parent/pipeline.json", true},
		{"pipeline.jsonc", true},
		{"pipeline.json", true},
		{"subdir/pipeline", true},
		{"/path/with:colon", true},                      // absolute path takes precedence over colon
		{"bureau/pipeline:dev-workspace-init", false},   // valid template ref
		{"iree/template@other.example:8448:foo", false}, // federated ref with port
		{"just-a-ref:name", false},                      // valid template ref (no slashes)
		{"no-colon-no-slash", false},                    // not recognizable as either
	}

	for _, test := range tests {
		result := isFilePath(test.input)
		if result != test.expected {
			t.Errorf("isFilePath(%q) = %v, want %v", test.input, result, test.expected)
		}
	}
}

func TestNoPipelineSpecified(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err == nil {
		t.Fatal("expected error when no pipeline is specified")
	}
	if !strings.Contains(err.Error(), "no pipeline specified") {
		t.Errorf("expected 'no pipeline specified' error, got: %v", err)
	}
}

func TestSandboxGuardRejectsOutsideSandbox(t *testing.T) {
	t.Setenv("BUREAU_SANDBOX", "")
	os.Args = []string{"bureau-pipeline-executor", "anything"}
	err := run()
	if err == nil {
		t.Fatal("expected error when BUREAU_SANDBOX is not set")
	}
	if !strings.Contains(err.Error(), "BUREAU_SANDBOX") {
		t.Errorf("expected sandbox guard error, got: %v", err)
	}
}
