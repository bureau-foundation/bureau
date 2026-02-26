// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
)

// --- Mock proxy (HTTP over Unix socket, for Matrix operations) ---

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
	case r.Method == "GET" && r.URL.Path == "/v1/identity":
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":     m.whoamiUserID,
			"server_name": "bureau.local",
		})

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

// --- Mock ticket service (CBOR over Unix socket) ---

// mockTicketService is a minimal CBOR service that handles the ticket
// actions the executor uses: update (claim/progress), close, add-note,
// and show. Uses service.SocketServer with unauthenticated handlers
// for simplicity — the ServiceClient's token is ignored.
type mockTicketService struct {
	mu sync.Mutex

	// ticketStatus is returned by the "show" action. Tests can change
	// this to simulate external cancellation.
	ticketStatus string

	// claimCalls records each "update" call that sets status.
	claimCalls []mockTicketCall

	// progressCalls records "update" calls that set pipeline content.
	progressCalls []mockTicketCall

	// closeCalls records each "close" call.
	closeCalls []mockTicketCall

	// noteCalls records each "add-note" call.
	noteCalls []mockTicketCall

	// attachCalls records each "add-attachment" call.
	attachCalls []mockTicketCall

	// showCalls counts show invocations for assertions.
	showCalls int

	// failActions is a set of actions that should return errors.
	// Used to test retry logic and error handling.
	failActions map[string]bool
}

type mockTicketCall struct {
	Fields map[string]any
}

func newMockTicketService() *mockTicketService {
	return &mockTicketService{
		ticketStatus: string(ticket.StatusInProgress),
		failActions:  map[string]bool{},
	}
}

// startMockTicketService creates a CBOR socket server with mock
// ticket handlers and returns the socket path and token path. The
// token file contains dummy bytes — the mock server ignores auth.
func startMockTicketService(t *testing.T) (string, string, *mockTicketService) {
	t.Helper()

	tempDir := t.TempDir()
	socketPath := filepath.Join(tempDir, "ticket.sock")
	tokenPath := filepath.Join(tempDir, "ticket.token")

	// Write a dummy token file — the mock server ignores it.
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0644); err != nil {
		t.Fatalf("write dummy token: %v", err)
	}

	mock := newMockTicketService()

	// Use service.SocketServer with unauthenticated handlers.
	// The ServiceClient sends a token field, but Handle (unauthenticated)
	// ignores it — the raw CBOR bytes include the token but the handler
	// decodes only the fields it cares about.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, nil)

	server.Handle("update", func(ctx context.Context, raw []byte) (any, error) {
		if mock.shouldFail("update") {
			return nil, fmt.Errorf("mock: update failure")
		}
		var fields map[string]any
		if err := codec.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decoding update request: %w", err)
		}
		mock.mu.Lock()
		defer mock.mu.Unlock()
		// Distinguish claim (has "status") from progress (has "pipeline").
		if _, hasStatus := fields["status"]; hasStatus {
			mock.claimCalls = append(mock.claimCalls, mockTicketCall{Fields: fields})
		} else {
			mock.progressCalls = append(mock.progressCalls, mockTicketCall{Fields: fields})
		}
		return map[string]any{
			"id":      fields["ticket"],
			"content": map[string]any{"status": string(ticket.StatusInProgress)},
		}, nil
	})

	server.Handle("close", func(ctx context.Context, raw []byte) (any, error) {
		if mock.shouldFail("close") {
			return nil, fmt.Errorf("mock: close failure")
		}
		var fields map[string]any
		if err := codec.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decoding close request: %w", err)
		}
		mock.mu.Lock()
		mock.closeCalls = append(mock.closeCalls, mockTicketCall{Fields: fields})
		mock.mu.Unlock()
		return map[string]any{
			"id":      fields["ticket"],
			"content": map[string]any{"status": string(ticket.StatusClosed)},
		}, nil
	})

	server.Handle("add-note", func(ctx context.Context, raw []byte) (any, error) {
		if mock.shouldFail("add-note") {
			return nil, fmt.Errorf("mock: add-note failure")
		}
		var fields map[string]any
		if err := codec.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decoding add-note request: %w", err)
		}
		mock.mu.Lock()
		mock.noteCalls = append(mock.noteCalls, mockTicketCall{Fields: fields})
		mock.mu.Unlock()
		return map[string]any{
			"id":      fields["ticket"],
			"content": map[string]any{"status": string(ticket.StatusInProgress)},
		}, nil
	})

	server.Handle("add-attachment", func(ctx context.Context, raw []byte) (any, error) {
		if mock.shouldFail("add-attachment") {
			return nil, fmt.Errorf("mock: add-attachment failure")
		}
		var fields map[string]any
		if err := codec.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decoding add-attachment request: %w", err)
		}
		mock.mu.Lock()
		mock.attachCalls = append(mock.attachCalls, mockTicketCall{Fields: fields})
		mock.mu.Unlock()
		return map[string]any{
			"id":      fields["ticket"],
			"content": map[string]any{"status": string(ticket.StatusInProgress)},
		}, nil
	})

	server.Handle("show", func(ctx context.Context, raw []byte) (any, error) {
		if mock.shouldFail("show") {
			return nil, fmt.Errorf("mock: show failure")
		}
		mock.mu.Lock()
		mock.showCalls++
		status := mock.ticketStatus
		mock.mu.Unlock()
		return map[string]any{
			"Content": map[string]any{
				"Status": status,
			},
		}, nil
	})

	// Start serving in background, shut down on test cleanup.
	serverCtx, serverCancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Serve(serverCtx)
	}()
	t.Cleanup(func() {
		serverCancel()
		<-serverDone
	})

	// Wait for the socket to be ready.
	select {
	case <-server.Ready():
	case <-serverCtx.Done():
		t.Fatal("socket server did not start")
	}

	return socketPath, tokenPath, mock
}

func (m *mockTicketService) shouldFail(action string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failActions[action]
}

func (m *mockTicketService) setTicketStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ticketStatus = status
}

// --- Test helpers ---

// writePipeline writes a JSONC pipeline file and returns its path.
func writePipeline(t *testing.T, name string, content *pipeline.PipelineContent) string {
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

// writeTicketTrigger writes a trigger.json containing a TicketContent
// for a pipeline ticket with the given pipeline ref and variables.
func writeTicketTrigger(t *testing.T, pipelineRef string, variables map[string]string) string {
	t.Helper()

	content := map[string]any{
		"version":  1,
		"title":    "Pipeline: " + pipelineRef,
		"status":   string(ticket.StatusOpen),
		"priority": 2,
		"type":     "pipeline",
		"pipeline": map[string]any{
			"pipeline_ref": pipelineRef,
			"total_steps":  0,
		},
	}
	if variables != nil {
		content["pipeline"].(map[string]any)["variables"] = variables
	}
	return writeTrigger(t, content)
}

// setupTestEnv configures environment variables for a test that calls
// run(). Sets up the proxy, ticket service, and trigger.json. Returns
// the mock proxy and mock ticket service for assertions.
//
// The pipelineRef must be resolvable by the mock proxy — callers should
// set up resolveResponses and stateResponses on the returned mock proxy
// before calling run().
func setupTestEnv(t *testing.T, pipelineRef string, variables map[string]string) (*mockProxy, *mockTicketService) {
	t.Helper()

	proxySocket, mock := startMockProxy(t)
	ticketSocket, ticketToken, ticketMock := startMockTicketService(t)
	triggerPath := writeTicketTrigger(t, pipelineRef, variables)

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", proxySocket)
	t.Setenv("BUREAU_TICKET_ID", "pip-test")
	t.Setenv("BUREAU_TICKET_ROOM", "!pipeline_room:bureau.local")
	t.Setenv("BUREAU_TICKET_SOCKET", ticketSocket)
	t.Setenv("BUREAU_TICKET_TOKEN", ticketToken)
	t.Setenv("BUREAU_TRIGGER_PATH", triggerPath)

	os.Args = []string{"bureau-pipeline-executor"}

	return mock, ticketMock
}

// setupTestEnvWithPipeline is a convenience that sets up the
// environment AND registers a pipeline in the mock proxy so that the
// pipeline ref resolves correctly.
func setupTestEnvWithPipeline(t *testing.T, pipelineName string, pipelineContent *pipeline.PipelineContent) (*mockProxy, *mockTicketService) {
	t.Helper()

	pipelineRef := "bureau/pipeline:" + pipelineName
	mock, ticketMock := setupTestEnv(t, pipelineRef, nil)

	// Register the pipeline in the mock proxy.
	pipelineJSON, err := json.Marshal(pipelineContent)
	if err != nil {
		t.Fatalf("marshal pipeline content: %v", err)
	}
	mock.resolveResponses["#bureau/pipeline:bureau.local"] = "!pipeline_room:bureau.local"
	mock.stateResponses["!pipeline_room:bureau.local/m.bureau.pipeline/"+pipelineName] = string(pipelineJSON)

	return mock, ticketMock
}

// --- Tests ---

func TestRunSteps(t *testing.T) {
	_, ticketMock := setupTestEnvWithPipeline(t, "test-pipeline", &pipeline.PipelineContent{
		Description: "Test pipeline",
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "echo step one"},
			{Name: "step-two", Run: "echo step two"},
			{Name: "step-three", Run: "echo step three"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	ticketMock.mu.Lock()
	defer ticketMock.mu.Unlock()

	// Should have claimed the ticket.
	if len(ticketMock.claimCalls) != 1 {
		t.Fatalf("expected 1 claim call, got %d", len(ticketMock.claimCalls))
	}

	// Should have closed the ticket.
	if len(ticketMock.closeCalls) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(ticketMock.closeCalls))
	}

	// Should have posted 3 step notes.
	if len(ticketMock.noteCalls) != 3 {
		t.Fatalf("expected 3 note calls, got %d", len(ticketMock.noteCalls))
	}
}

func TestWhenGuardSkip(t *testing.T) {
	setupTestEnvWithPipeline(t, "when-skip", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "always-runs", Run: "true"},
			{Name: "skipped", Run: "echo should not run", When: "false"},
			{Name: "also-runs", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestWhenGuardPass(t *testing.T) {
	setupTestEnvWithPipeline(t, "when-pass", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "guarded-pass", Run: "true", When: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestCheckFailure(t *testing.T) {
	setupTestEnvWithPipeline(t, "check-fail", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "passes-then-fails-check", Run: "true", Check: "false"},
		},
	})

	err := run()
	if err == nil {
		t.Fatal("expected error from check failure")
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("expected check-related error, got: %v", err)
	}
}

func TestOptionalStepContinues(t *testing.T) {
	setupTestEnvWithPipeline(t, "optional-fail", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "optional-fail", Run: "false", Optional: true},
			{Name: "step-three", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("pipeline should continue past optional failure, got: %v", err)
	}
}

func TestRequiredStepAborts(t *testing.T) {
	setupTestEnvWithPipeline(t, "required-fail", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "required-fail", Run: "false"},
			{Name: "never-reached", Run: "true"},
		},
	})

	err := run()
	if err == nil {
		t.Fatal("expected error from required step failure")
	}
	if !strings.Contains(err.Error(), "required-fail") {
		t.Errorf("expected error mentioning step name, got: %v", err)
	}
}

func TestPublishStep(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "publish-test", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name: "publish-ready",
				Publish: &pipeline.PipelinePublish{
					EventType: "m.bureau.workspace",
					Room:      "!room1:bureau.local",
					StateKey:  "",
					Content:   map[string]any{"version": "1.0"},
				},
			},
		},
	})

	if err := run(); err != nil {
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
	setupTestEnvWithPipeline(t, "timeout-test", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "slow-step", Run: "sleep 30", Timeout: "200ms"},
		},
	})

	wallClock := clock.Real()
	startTime := wallClock.Now()
	err := run()
	elapsed := wallClock.Now().Sub(startTime)

	if err == nil {
		t.Fatal("expected error from timeout")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestVariableExpansion(t *testing.T) {
	pipelineRef := "bureau/pipeline:var-test"
	mock, _ := setupTestEnv(t, pipelineRef, map[string]string{
		"REPOSITORY": "https://github.com/iree-org/iree.git",
		"PROJECT":    "iree",
	})

	pipelineJSON, _ := json.Marshal(&pipeline.PipelineContent{
		Variables: map[string]pipeline.PipelineVariable{
			"REPOSITORY": {Required: true},
			"PROJECT":    {Required: true},
		},
		Steps: []pipeline.PipelineStep{
			{Name: "use-vars", Run: `echo "repo=${REPOSITORY} project=${PROJECT}"`},
		},
	})

	mock.resolveResponses["#bureau/pipeline:bureau.local"] = "!pipeline_room:bureau.local"
	mock.stateResponses["!pipeline_room:bureau.local/m.bureau.pipeline/var-test"] = string(pipelineJSON)

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestPipelineRefResolution(t *testing.T) {
	setupTestEnvWithPipeline(t, "dev-workspace-init", &pipeline.PipelineContent{
		Description: "Test pipeline from ref",
		Steps:       []pipeline.PipelineStep{{Name: "hello", Run: "echo hello from ref"}},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestThreadLogging(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "log-test", &pipeline.PipelineContent{
		Log: &pipeline.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "step-two", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// Should have: 1 root message + 2 step replies + 1 complete reply = 4 messages.
	if len(mock.recordedMessages) != 4 {
		t.Fatalf("expected 4 messages (root + 2 steps + complete), got %d", len(mock.recordedMessages))
	}

	if !strings.Contains(mock.recordedMessages[0].Body, "started") {
		t.Errorf("root message should mention 'started', got: %s", mock.recordedMessages[0].Body)
	}

	for index := 1; index < len(mock.recordedMessages); index++ {
		if !strings.Contains(mock.recordedMessages[index].Body, "m.relates_to") {
			t.Errorf("message %d should be a thread reply (contain m.relates_to), got: %s",
				index, mock.recordedMessages[index].Body)
		}
	}
}

func TestThreadLoggingWithAliasResolution(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "log-alias-test", &pipeline.PipelineContent{
		Variables: map[string]pipeline.PipelineVariable{
			"WORKSPACE_ROOM": {Default: "#iree/workspace:bureau.local"},
		},
		Log: &pipeline.PipelineLog{Room: "${WORKSPACE_ROOM}"},
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	mock.resolveResponses["#iree/workspace:bureau.local"] = "!workspace_room:bureau.local"

	if err := run(); err != nil {
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
	// Use a dedicated mock that returns errors on message send.
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

	pipelineRef := "bureau/pipeline:log-fatal-test"
	ticketSocket, ticketToken, _ := startMockTicketService(t)
	triggerPath := writeTicketTrigger(t, pipelineRef, nil)

	// The pipeline content with log room — thread creation will fail.
	pipelineJSON, _ := json.Marshal(&pipeline.PipelineContent{
		Log:   &pipeline.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []pipeline.PipelineStep{{Name: "step-one", Run: "true"}},
	})
	failMock.mu.Lock()
	failMock.resolveResponses = map[string]string{
		"#bureau/pipeline:bureau.local": "!pipeline_room:bureau.local",
	}
	failMock.stateResponses = map[string]string{
		"!pipeline_room:bureau.local/m.bureau.pipeline/log-fatal-test": string(pipelineJSON),
	}
	failMock.mu.Unlock()

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", failSocket)
	t.Setenv("BUREAU_TICKET_ID", "pip-test")
	t.Setenv("BUREAU_TICKET_ROOM", "!pipeline_room:bureau.local")
	t.Setenv("BUREAU_TICKET_SOCKET", ticketSocket)
	t.Setenv("BUREAU_TICKET_TOKEN", ticketToken)
	t.Setenv("BUREAU_TRIGGER_PATH", triggerPath)

	os.Args = []string{"bureau-pipeline-executor"}
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
	mu               sync.Mutex
	whoamiUserID     string
	resolveResponses map[string]string
	stateResponses   map[string]string
}

func (m *failingMessageProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/identity":
		json.NewEncoder(w).Encode(map[string]string{"user_id": m.whoamiUserID, "server_name": "bureau.local"})
	case r.Method == "GET" && r.URL.Path == "/v1/matrix/whoami":
		json.NewEncoder(w).Encode(map[string]string{"user_id": m.whoamiUserID})
	case r.Method == "GET" && r.URL.Path == "/v1/matrix/resolve":
		alias := r.URL.Query().Get("alias")
		m.mu.Lock()
		roomID, ok := m.resolveResponses[alias]
		m.mu.Unlock()
		if ok {
			json.NewEncoder(w).Encode(map[string]string{"room_id": roomID})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"errcode": "M_NOT_FOUND"})
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
		}
	case r.Method == "POST" && r.URL.Path == "/v1/matrix/message":
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "no permission"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestNoLogConfigRunsWithoutThread(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "no-log", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	messageCount := len(mock.recordedMessages)
	mock.mu.Unlock()

	if messageCount != 0 {
		t.Errorf("expected 0 messages with no log config, got %d", messageCount)
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

	variables, err := loadTriggerVariables(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("expected no error for missing trigger file, got: %v", err)
	}
	if len(variables) != 0 {
		t.Errorf("expected empty variables for missing trigger file, got %d entries", len(variables))
	}
}

func TestSandboxGuardRejectsOutsideSandbox(t *testing.T) {
	t.Setenv("BUREAU_SANDBOX", "")
	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err == nil {
		t.Fatal("expected error when BUREAU_SANDBOX is not set")
	}
	if !strings.Contains(err.Error(), "BUREAU_SANDBOX") {
		t.Errorf("expected sandbox guard error, got: %v", err)
	}
}

func TestMissingTicketIDRejectsRun(t *testing.T) {
	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_TICKET_ID", "")
	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err == nil {
		t.Fatal("expected error when BUREAU_TICKET_ID is not set")
	}
	if !strings.Contains(err.Error(), "BUREAU_TICKET_ID") {
		t.Errorf("expected ticket ID error, got: %v", err)
	}
}

func TestMissingTicketRoomRejectsRun(t *testing.T) {
	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_TICKET_ID", "pip-test")
	t.Setenv("BUREAU_TICKET_ROOM", "")
	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err == nil {
		t.Fatal("expected error when BUREAU_TICKET_ROOM is not set")
	}
	if !strings.Contains(err.Error(), "BUREAU_TICKET_ROOM") {
		t.Errorf("expected ticket room error, got: %v", err)
	}
}

func TestResultLogSuccess(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")
	setupTestEnvWithPipeline(t, "result-ok", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "step-two", Run: "true"},
		},
	})
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)

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
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")
	setupTestEnvWithPipeline(t, "result-fail", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "passes", Run: "true"},
			{Name: "fails", Run: "false"},
			{Name: "never-reached", Run: "true"},
		},
	})
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	err := run()
	if err == nil {
		t.Fatal("expected error from step failure")
	}

	entries := readResultLog(t, resultPath)

	if len(entries) != 4 {
		t.Fatalf("expected 4 JSONL entries, got %d", len(entries))
	}

	assertResultField(t, entries[0], "type", "start")
	assertResultField(t, entries[1], "name", "passes")
	assertResultField(t, entries[1], "status", "ok")
	assertResultField(t, entries[2], "name", "fails")
	assertResultField(t, entries[2], "status", "failed")
	assertResultField(t, entries[3], "type", "failed")
	assertResultField(t, entries[3], "failed_step", "fails")
}

func TestResultLogSkippedAndOptional(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")
	setupTestEnvWithPipeline(t, "result-mixed", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "skipped-step", Run: "true", When: "false"},
			{Name: "optional-fail", Run: "false", Optional: true},
			{Name: "passes", Run: "true"},
		},
	})
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)

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
	setupTestEnvWithPipeline(t, "no-result", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})
	t.Setenv("BUREAU_RESULT_PATH", "")

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestResultLogWithThreadLogging(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")
	setupTestEnvWithPipeline(t, "result-with-log", &pipeline.PipelineContent{
		Log: &pipeline.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	entries := readResultLog(t, resultPath)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 entries, got %d", len(entries))
	}

	lastEntry := entries[len(entries)-1]
	assertResultField(t, lastEntry, "type", "complete")
	logEventID, ok := lastEntry["log_event_id"].(string)
	if !ok {
		t.Fatal("complete entry missing log_event_id")
	}
	if logEventID != "$msg_1" {
		t.Errorf("log_event_id = %q, want %q", logEventID, "$msg_1")
	}
}

func TestGracefulTerminationSendsSIGTERM(t *testing.T) {
	markerDir := t.TempDir()
	markerFile := filepath.Join(markerDir, "got_sigterm")

	trapCommand := fmt.Sprintf(
		"trap 'touch %s; exit 0' TERM; sleep 30",
		markerFile,
	)

	setupTestEnvWithPipeline(t, "graceful-test", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name:        "graceful-step",
				Run:         trapCommand,
				Timeout:     "500ms",
				GracePeriod: "5s",
			},
		},
	})

	wallClock := clock.Real()
	startTime := wallClock.Now()
	err := run()
	elapsed := wallClock.Now().Sub(startTime)

	if err == nil {
		t.Fatal("expected error from timeout")
	}

	if elapsed > 5*time.Second {
		t.Errorf("took too long: %v (grace period should not have been exhausted)", elapsed)
	}

	if _, statError := os.Stat(markerFile); os.IsNotExist(statError) {
		t.Error("marker file not created: process received SIGKILL instead of SIGTERM")
	}
}

func TestGracefulTerminationEscalatesToSIGKILL(t *testing.T) {
	setupTestEnvWithPipeline(t, "escalation-test", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name:        "stubborn-step",
				Run:         "trap '' TERM; sleep 30",
				Timeout:     "200ms",
				GracePeriod: "500ms",
			},
		},
	})

	wallClock := clock.Real()
	startTime := wallClock.Now()
	err := run()
	elapsed := wallClock.Now().Sub(startTime)

	if err == nil {
		t.Fatal("expected error from timeout")
	}

	if elapsed > 5*time.Second {
		t.Errorf("took too long: %v (SIGKILL escalation should have fired)", elapsed)
	}

	if elapsed < 500*time.Millisecond {
		t.Errorf("completed too quickly: %v (grace period should have been waited out)", elapsed)
	}
}

func TestAssertStateEquals(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "assert-eq", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name: "check-active",
				AssertState: &pipeline.PipelineAssertState{
					Room:      "!room:bureau.local",
					EventType: "m.bureau.worktree",
					StateKey:  "feature/test",
					Field:     "status",
					Equals:    "active",
				},
			},
			{Name: "after-assert", Run: "true"},
		},
	})
	mock.stateResponses["!room:bureau.local/m.bureau.worktree/feature/test"] = `{"status":"active","project":"iree"}`

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
}

func TestAssertStateAbort(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.jsonl")
	mock, _ := setupTestEnvWithPipeline(t, "assert-abort", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name: "assert-still-removing",
				AssertState: &pipeline.PipelineAssertState{
					Room:       "!room:bureau.local",
					EventType:  "m.bureau.worktree",
					StateKey:   "feature/test",
					Field:      "status",
					Equals:     "removing",
					OnMismatch: "abort",
					Message:    "worktree state changed, aborting",
				},
			},
			{Name: "never-reached", Run: "true"},
		},
	})
	mock.stateResponses["!room:bureau.local/m.bureau.worktree/feature/test"] = `{"status":"active"}`
	t.Setenv("BUREAU_RESULT_PATH", resultPath)

	err := run()
	if err != nil {
		t.Fatalf("abort should return nil error, got: %v", err)
	}

	entries := readResultLog(t, resultPath)
	lastEntry := entries[len(entries)-1]
	assertResultField(t, lastEntry, "type", "aborted")
	assertResultField(t, lastEntry, "status", "aborted")
	assertResultField(t, lastEntry, "aborted_step", "assert-still-removing")
}

func TestOnFailurePublishesState(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "on-failure-pub", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "will-fail", Run: "false"},
		},
		OnFailure: []pipeline.PipelineStep{
			{
				Name: "publish-failed",
				Publish: &pipeline.PipelinePublish{
					EventType: "m.bureau.worktree",
					Room:      "!room:bureau.local",
					StateKey:  "feature/test",
					Content:   map[string]any{"status": "failed"},
				},
			},
		},
	})

	err := run()
	if err == nil {
		t.Fatal("expected error from step failure")
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.recordedPuts) != 1 {
		t.Fatalf("expected 1 PUT state request from on_failure, got %d", len(mock.recordedPuts))
	}
}

func TestOnFailureNotRunOnAbort(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "on-failure-abort", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{
				Name: "assert-removing",
				AssertState: &pipeline.PipelineAssertState{
					Room:       "!room:bureau.local",
					EventType:  "m.bureau.worktree",
					StateKey:   "feature/test",
					Field:      "status",
					Equals:     "removing",
					OnMismatch: "abort",
				},
			},
		},
		OnFailure: []pipeline.PipelineStep{
			{
				Name: "should-not-run",
				Publish: &pipeline.PipelinePublish{
					EventType: "m.bureau.worktree",
					Room:      "!room:bureau.local",
					StateKey:  "feature/test",
					Content:   map[string]any{"status": "failed"},
				},
			},
		},
	})
	mock.stateResponses["!room:bureau.local/m.bureau.worktree/feature/test"] = `{"status":"active"}`

	if err := run(); err != nil {
		t.Fatalf("abort should not return error, got: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.recordedPuts) != 0 {
		t.Errorf("on_failure should not run on abort, but %d PUT requests were recorded", len(mock.recordedPuts))
	}
}

func TestOnFailureVariables(t *testing.T) {
	markerDir := t.TempDir()
	markerFile := filepath.Join(markerDir, "failure-context")

	setupTestEnvWithPipeline(t, "on-failure-vars", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "doomed-step", Run: "false"},
		},
		OnFailure: []pipeline.PipelineStep{
			{
				Name: "record-failure",
				Run:  fmt.Sprintf("echo \"step=${FAILED_STEP} error=${FAILED_ERROR}\" > %s", markerFile),
			},
		},
	})

	err := run()
	if err == nil {
		t.Fatal("expected error from step failure")
	}

	data, readError := os.ReadFile(markerFile)
	if readError != nil {
		t.Fatalf("marker file not written: %v", readError)
	}
	content := string(data)
	if !strings.Contains(content, "step=doomed-step") {
		t.Errorf("FAILED_STEP not set correctly, marker file: %q", content)
	}
}

// --- Ticket interaction tests ---

func TestTicketClaimOnContention(t *testing.T) {
	// Test that isContention correctly identifies contention errors.
	contentionError := &service.ServiceError{
		Action:  "update",
		Message: "ticket pip-test is already in_progress (assigned to @other:bureau.local)",
	}
	if !isContention(contentionError) {
		t.Error("expected isContention to return true for contention error")
	}

	// Non-contention errors should not match.
	otherError := &service.ServiceError{
		Action:  "update",
		Message: "some other error",
	}
	if isContention(otherError) {
		t.Error("expected isContention to return false for non-contention error")
	}

	// Plain errors should not match.
	if isContention(fmt.Errorf("connection refused")) {
		t.Error("expected isContention to return false for plain error")
	}
}

func TestTicketProgressAndCloseOnSuccess(t *testing.T) {
	_, ticketMock := setupTestEnvWithPipeline(t, "progress-test", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
			{Name: "step-two", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	ticketMock.mu.Lock()
	defer ticketMock.mu.Unlock()

	// Verify claim call.
	if len(ticketMock.claimCalls) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(ticketMock.claimCalls))
	}
	claimFields := ticketMock.claimCalls[0].Fields
	if claimFields["status"] != string(ticket.StatusInProgress) {
		t.Errorf("claim status = %v, want %s", claimFields["status"], ticket.StatusInProgress)
	}

	// Verify close call includes success message.
	if len(ticketMock.closeCalls) != 1 {
		t.Fatalf("expected 1 close, got %d", len(ticketMock.closeCalls))
	}
	closeReason, _ := ticketMock.closeCalls[0].Fields["reason"].(string)
	if !strings.Contains(closeReason, "completed successfully") {
		t.Errorf("close reason should mention success, got: %q", closeReason)
	}

	// Verify step notes.
	if len(ticketMock.noteCalls) != 2 {
		t.Fatalf("expected 2 notes (one per step), got %d", len(ticketMock.noteCalls))
	}
}

func TestTicketClosedOnFailure(t *testing.T) {
	_, ticketMock := setupTestEnvWithPipeline(t, "close-on-fail", &pipeline.PipelineContent{
		Steps: []pipeline.PipelineStep{
			{Name: "will-fail", Run: "false"},
		},
	})

	err := run()
	if err == nil {
		t.Fatal("expected error from step failure")
	}

	ticketMock.mu.Lock()
	defer ticketMock.mu.Unlock()

	if len(ticketMock.closeCalls) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(ticketMock.closeCalls))
	}
	closeReason, _ := ticketMock.closeCalls[0].Fields["reason"].(string)
	if !strings.Contains(closeReason, "will-fail") {
		t.Errorf("close reason should mention failed step, got: %q", closeReason)
	}
}

func TestFormatStepNote(t *testing.T) {
	tests := []struct {
		name     string
		result   stepResult
		expected string
	}{
		{
			name:     "ok",
			result:   stepResult{status: "ok", duration: 1234 * time.Millisecond},
			expected: "step 1/3: build... ok (1.2s)",
		},
		{
			name:     "skipped",
			result:   stepResult{status: "skipped"},
			expected: "step 1/3: build... skipped",
		},
		{
			name:     "failed",
			result:   stepResult{status: "failed", duration: 567 * time.Millisecond, err: fmt.Errorf("exit code 1")},
			expected: "step 1/3: build... failed (0.6s): exit code 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			note := formatStepNote(0, 3, "build", test.result)
			if note != test.expected {
				t.Errorf("got %q, want %q", note, test.expected)
			}
		})
	}
}

func TestPipelineResultIncludesTicketID(t *testing.T) {
	mock, _ := setupTestEnvWithPipeline(t, "result-ticket-id", &pipeline.PipelineContent{
		Log: &pipeline.PipelineLog{Room: "!log_room:bureau.local"},
		Steps: []pipeline.PipelineStep{
			{Name: "step-one", Run: "true"},
		},
	})

	if err := run(); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	// The pipeline result state event should have been published.
	if len(mock.recordedPuts) != 1 {
		t.Fatalf("expected 1 PUT (pipeline result), got %d", len(mock.recordedPuts))
	}

	var putBody struct {
		Content struct {
			TicketID string `json:"ticket_id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(mock.recordedPuts[0].Body), &putBody); err != nil {
		t.Fatalf("unmarshal PUT body: %v", err)
	}
	if putBody.Content.TicketID != "pip-test" {
		t.Errorf("ticket_id = %q, want %q", putBody.Content.TicketID, "pip-test")
	}
}

// --- Trigger parsing tests ---

func TestNonPipelineTicketTypeRejected(t *testing.T) {
	proxySocket, _ := startMockProxy(t)
	ticketSocket, ticketToken, _ := startMockTicketService(t)

	// Write a trigger with type "task" instead of "pipeline".
	triggerPath := writeTrigger(t, map[string]any{
		"version":  1,
		"title":    "Not a pipeline",
		"status":   string(ticket.StatusOpen),
		"priority": 2,
		"type":     "task",
	})

	t.Setenv("BUREAU_SANDBOX", "1")
	t.Setenv("BUREAU_PROXY_SOCKET", proxySocket)
	t.Setenv("BUREAU_TICKET_ID", "tkt-test")
	t.Setenv("BUREAU_TICKET_ROOM", "!room:bureau.local")
	t.Setenv("BUREAU_TICKET_SOCKET", ticketSocket)
	t.Setenv("BUREAU_TICKET_TOKEN", ticketToken)
	t.Setenv("BUREAU_TRIGGER_PATH", triggerPath)

	os.Args = []string{"bureau-pipeline-executor"}
	err := run()
	if err == nil {
		t.Fatal("expected error for non-pipeline ticket type")
	}
	if !strings.Contains(err.Error(), "expected \"pipeline\"") {
		t.Errorf("expected type mismatch error, got: %v", err)
	}
}

// --- JSONL helpers ---

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
