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
					EventType: "m.bureau.workspace",
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
	if putBody.EventType != "m.bureau.workspace" {
		t.Errorf("expected event_type m.bureau.workspace, got %q", putBody.EventType)
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

func TestTriggerVariableConversion(t *testing.T) {
	t.Parallel()

	triggerPath := writeTrigger(t, map[string]any{
		"status":        "teardown",
		"teardown_mode": "archive",
		"project":       "iree",
		"machine":       "machine/workstation",
		"updated_at":    "2026-02-10T03:00:00Z",
		"nested":        map[string]any{"key": "value"},
		"count":         3.0,
		"enabled":       true,
		"empty":         nil,
	})

	variables, err := loadTriggerVariables(triggerPath)
	if err != nil {
		t.Fatalf("loadTriggerVariables: %v", err)
	}

	tests := []struct {
		key      string
		expected string
		present  bool
	}{
		{"EVENT_status", "teardown", true},
		{"EVENT_teardown_mode", "archive", true},
		{"EVENT_project", "iree", true},
		{"EVENT_machine", "machine/workstation", true},
		{"EVENT_updated_at", "2026-02-10T03:00:00Z", true},
		{"EVENT_nested", `{"key":"value"}`, true},
		{"EVENT_count", "3", true},
		{"EVENT_enabled", "true", true},
		{"EVENT_empty", "", false},
		// Original keys without EVENT_ prefix should NOT be present.
		{"status", "", false},
		{"teardown_mode", "", false},
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

func TestTriggerVariablesMissingFile(t *testing.T) {
	t.Parallel()

	// Non-existent trigger file should return empty map, not error.
	variables, err := loadTriggerVariables(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("expected no error for missing trigger file, got: %v", err)
	}
	if len(variables) != 0 {
		t.Errorf("expected empty variables for missing trigger file, got %d entries", len(variables))
	}
}

func TestTriggerVariablesPrecedence(t *testing.T) {
	t.Parallel()

	// Verify that payload variables take precedence over trigger variables
	// when merged. If both trigger and payload define a variable with the
	// same name, payload wins.
	triggerPath := writeTrigger(t, map[string]any{
		"status": "teardown",
		"mode":   "from-trigger",
	})
	payloadPath := writePayload(t, map[string]any{
		"EVENT_mode": "from-payload",
		"OTHER":      "payload-only",
	})

	triggerVariables, err := loadTriggerVariables(triggerPath)
	if err != nil {
		t.Fatalf("loadTriggerVariables: %v", err)
	}
	payloadVariables, err := loadPayloadVariables(payloadPath)
	if err != nil {
		t.Fatalf("loadPayloadVariables: %v", err)
	}

	// Merge: trigger first (lower priority), payload on top.
	merged := make(map[string]string, len(triggerVariables)+len(payloadVariables))
	for key, value := range triggerVariables {
		merged[key] = value
	}
	for key, value := range payloadVariables {
		merged[key] = value
	}

	// EVENT_mode should be "from-payload" (payload wins over trigger's EVENT_mode).
	if merged["EVENT_mode"] != "from-payload" {
		t.Errorf("EVENT_mode = %q, want %q (payload should win)", merged["EVENT_mode"], "from-payload")
	}
	// EVENT_status should come from trigger (no collision).
	if merged["EVENT_status"] != "teardown" {
		t.Errorf("EVENT_status = %q, want %q", merged["EVENT_status"], "teardown")
	}
	// OTHER should come from payload (no collision).
	if merged["OTHER"] != "payload-only" {
		t.Errorf("OTHER = %q, want %q", merged["OTHER"], "payload-only")
	}
}

// writeTrigger writes a trigger JSON file and returns its path.
func writeTrigger(t *testing.T, content map[string]any) string {
	t.Helper()

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal trigger: %v", err)
	}

	path := filepath.Join(t.TempDir(), "trigger.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write trigger: %v", err)
	}
	return path
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

func TestResultLogSuccess(t *testing.T) {
	socketPath, _ := startMockProxy(t)
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")

	pipelinePath := writePipeline(t, "result-ok", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "step-two", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)

	// Expected: start, step (ok), step (ok), complete.
	if len(entries) != 4 {
		t.Fatalf("expected 4 JSONL entries, got %d", len(entries))
	}

	assertResultField(t, entries[0], "type", "start")
	assertResultField(t, entries[0], "pipeline", "result-ok")
	assertResultFieldFloat(t, entries[0], "step_count", 2)

	assertResultField(t, entries[1], "type", "step")
	assertResultField(t, entries[1], "name", "step-one")
	assertResultField(t, entries[1], "status", "ok")

	assertResultField(t, entries[2], "type", "step")
	assertResultField(t, entries[2], "name", "step-two")
	assertResultField(t, entries[2], "status", "ok")

	assertResultField(t, entries[3], "type", "complete")
	assertResultField(t, entries[3], "status", "ok")
}

func TestResultLogFailure(t *testing.T) {
	socketPath, _ := startMockProxy(t)
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")

	pipelinePath := writePipeline(t, "result-fail", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "passes", Run: "true"},
			{Name: "fails", Run: "false"},
			{Name: "never-reached", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	err := run()
	if err == nil {
		t.Fatal("expected error from step failure")
	}

	entries := readResultLog(t, resultPath)

	// Expected: start, step (ok), step (failed), failed.
	if len(entries) != 4 {
		t.Fatalf("expected 4 JSONL entries, got %d", len(entries))
	}

	assertResultField(t, entries[0], "type", "start")
	assertResultField(t, entries[1], "type", "step")
	assertResultField(t, entries[1], "name", "passes")
	assertResultField(t, entries[1], "status", "ok")

	assertResultField(t, entries[2], "type", "step")
	assertResultField(t, entries[2], "name", "fails")
	assertResultField(t, entries[2], "status", "failed")
	if _, exists := entries[2]["error"]; !exists {
		t.Error("failed step entry should have an error field")
	}

	assertResultField(t, entries[3], "type", "failed")
	assertResultField(t, entries[3], "status", "failed")
	assertResultField(t, entries[3], "failed_step", "fails")
}

func TestResultLogSkippedAndOptional(t *testing.T) {
	socketPath, _ := startMockProxy(t)
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")

	pipelinePath := writePipeline(t, "result-mixed", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "skipped-step", Run: "true", When: "false"},
			{Name: "optional-fail", Run: "false", Optional: true},
			{Name: "passes", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)

	// Expected: start, step (skipped), step (failed optional), step (ok), complete.
	if len(entries) != 5 {
		t.Fatalf("expected 5 JSONL entries, got %d", len(entries))
	}

	assertResultField(t, entries[1], "name", "skipped-step")
	assertResultField(t, entries[1], "status", "skipped")

	assertResultField(t, entries[2], "name", "optional-fail")
	assertResultField(t, entries[2], "status", "failed (optional)")

	assertResultField(t, entries[3], "name", "passes")
	assertResultField(t, entries[3], "status", "ok")

	assertResultField(t, entries[4], "type", "complete")
}

func TestResultLogNotCreatedWithoutEnvVar(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	pipelinePath := writePipeline(t, "no-result", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))
	// BUREAU_RESULT_PATH intentionally not set.
	t.Setenv("BUREAU_RESULT_PATH", "")

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	// No file should be created.
	// (We can't easily verify this without a known path, but the point is
	// run() succeeds without BUREAU_RESULT_PATH.)
}

func TestResultLogWithThreadLogging(t *testing.T) {
	socketPath, _ := startMockProxy(t)
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")

	pipelinePath := writePipeline(t, "result-with-log", &schema.PipelineContent{
		Log: &schema.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []schema.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 entries, got %d", len(entries))
	}

	// The complete entry should include the log event ID from the thread.
	lastEntry := entries[len(entries)-1]
	assertResultField(t, lastEntry, "type", "complete")
	logEventID, ok := lastEntry["log_event_id"].(string)
	if !ok {
		t.Fatal("complete entry missing log_event_id")
	}
	// The mock proxy returns "$msg_1" as the event ID for all messages.
	if logEventID != "$msg_1" {
		t.Errorf("log_event_id = %q, want %q", logEventID, "$msg_1")
	}
}

func TestGracefulTerminationSendsSIGTERM(t *testing.T) {
	socketPath, _ := startMockProxy(t)
	markerDir := t.TempDir()
	markerFile := filepath.Join(markerDir, "got_sigterm")

	// This command traps SIGTERM and writes a marker file before exiting.
	// Without grace_period, the process would receive SIGKILL (no chance
	// to run the trap handler). With grace_period, it receives SIGTERM
	// first and the trap fires.
	trapCommand := fmt.Sprintf(
		"trap 'touch %s; exit 0' TERM; sleep 30",
		markerFile,
	)

	pipelinePath := writePipeline(t, "graceful-test", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{
				Name:        "graceful-step",
				Run:         trapCommand,
				Timeout:     "500ms",
				GracePeriod: "5s",
			},
		},
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", socketPath)
	t.Setenv("BUREAU_PAYLOAD_PATH", filepath.Join(t.TempDir(), "nonexistent.json"))

	os.Args = []string{"bureau-pipeline-executor", pipelinePath}
	startTime := time.Now()
	err := run()
	elapsed := time.Since(startTime)

	// The step should fail (timeout exceeded), but the process got SIGTERM.
	if err == nil {
		t.Fatal("expected error from timeout")
	}

	// Should complete relatively quickly (the trap handler exits immediately).
	if elapsed > 5*time.Second {
		t.Errorf("took too long: %v (grace period should not have been exhausted)", elapsed)
	}

	// The marker file should exist, proving SIGTERM was delivered (not SIGKILL).
	if _, statError := os.Stat(markerFile); os.IsNotExist(statError) {
		t.Error("marker file not created: process received SIGKILL instead of SIGTERM")
	}
}

func TestGracefulTerminationEscalatesToSIGKILL(t *testing.T) {
	socketPath, _ := startMockProxy(t)

	// This command traps SIGTERM but does NOT exit — it ignores the signal
	// and continues sleeping. The grace period should expire and SIGKILL
	// should terminate it.
	pipelinePath := writePipeline(t, "escalation-test", &schema.PipelineContent{
		Steps: []schema.PipelineStep{
			{
				Name:        "stubborn-step",
				Run:         "trap '' TERM; sleep 30",
				Timeout:     "200ms",
				GracePeriod: "500ms",
			},
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

	// Should take roughly timeout + grace_period (200ms + 500ms = 700ms),
	// not the full 30s sleep. Allow generous upper bound for CI.
	if elapsed > 5*time.Second {
		t.Errorf("took too long: %v (SIGKILL escalation should have fired)", elapsed)
	}

	// The step should have taken at least the grace period (the process
	// ignores SIGTERM, so it survives until SIGKILL).
	if elapsed < 500*time.Millisecond {
		t.Errorf("completed too quickly: %v (grace period should have been waited out)", elapsed)
	}
}

// readResultLog reads a JSONL file and returns each line as a parsed map.
func readResultLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading result log: %v", err)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parsing JSONL line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// assertResultField checks that a JSONL entry has a string field with the
// expected value.
func assertResultField(t *testing.T, entry map[string]any, key, expected string) {
	t.Helper()
	value, ok := entry[key].(string)
	if !ok {
		t.Errorf("entry missing string field %q (entry: %v)", key, entry)
		return
	}
	if value != expected {
		t.Errorf("entry[%q] = %q, want %q", key, value, expected)
	}
}

// assertResultFieldFloat checks that a JSONL entry has a numeric field with
// the expected value (JSON numbers decode as float64).
func assertResultFieldFloat(t *testing.T, entry map[string]any, key string, expected float64) {
	t.Helper()
	value, ok := entry[key].(float64)
	if !ok {
		t.Errorf("entry missing numeric field %q (entry: %v)", key, entry)
		return
	}
	if value != expected {
		t.Errorf("entry[%q] = %v, want %v", key, value, expected)
	}
}
