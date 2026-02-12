// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// capturedMessage records a message sent via the Matrix send event API.
type capturedMessage struct {
	RoomID  string
	Content map[string]any
}

// commandTestHarness wraps a daemon, mock Matrix server, and captured
// messages for command handler tests.
type commandTestHarness struct {
	daemon       *Daemon
	matrixState  *mockMatrixState
	matrixServer *httptest.Server

	sentMessagesMu sync.Mutex
	sentMessages   []capturedMessage
}

// newCommandTestHarness creates a test daemon connected to a mock Matrix
// server. The mock captures messages sent via SendEvent so tests can
// verify command results.
func newCommandTestHarness(t *testing.T) *commandTestHarness {
	t.Helper()

	workspaceRoot := t.TempDir()

	matrixState := newMockMatrixState()
	harness := &commandTestHarness{
		matrixState: matrixState,
	}

	// Wrap the mock handler to intercept send event requests.
	baseHandler := matrixState.handler()
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intercept PUT requests to /rooms/{roomId}/send/m.room.message/{txnId}
		if r.Method == "PUT" && isSendMessagePath(r.URL.Path) {
			roomID := extractRoomIDFromSendPath(r.URL.Path)
			var content map[string]any
			if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}

			harness.sentMessagesMu.Lock()
			harness.sentMessages = append(harness.sentMessages, capturedMessage{
				RoomID:  roomID,
				Content: content,
			})
			harness.sentMessagesMu.Unlock()

			// Return a mock event ID.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"event_id": fmt.Sprintf("$result_%d", len(harness.sentMessages)),
			})
			return
		}
		baseHandler.ServeHTTP(w, r)
	})

	matrixServer := httptest.NewServer(wrappedHandler)
	t.Cleanup(matrixServer.Close)
	harness.matrixServer = matrixServer

	// Create a real messaging session pointing at the mock server.
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@machine/test:bureau.local", "syt_daemon_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		clock:             clock.Real(),
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      "!config:test",
		machineRoomID:     "!machine:test",
		serviceRoomID:     "!service:test",
		workspaceRoot:     workspaceRoot,
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastMatrixPolicy:  make(map[string]*schema.MatrixPolicy),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		lastSpecs:         make(map[string]*schema.SandboxSpec),
		previousSpecs:     make(map[string]*schema.SandboxSpec),
		lastTemplates:     make(map[string]*schema.TemplateContent),
		healthMonitors:    make(map[string]*healthMonitor),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	harness.daemon = daemon
	return harness
}

// getSentMessages returns a copy of the captured messages.
func (h *commandTestHarness) getSentMessages() []capturedMessage {
	h.sentMessagesMu.Lock()
	defer h.sentMessagesMu.Unlock()
	result := make([]capturedMessage, len(h.sentMessages))
	copy(result, h.sentMessages)
	return result
}

// buildCommandEvent creates a messaging.Event that simulates a command
// message arriving via /sync.
func buildCommandEvent(eventID, sender, command, workspace, requestID string) messaging.Event {
	content := map[string]any{
		"msgtype": schema.MsgTypeCommand,
		"body":    command + " " + workspace,
		"command": command,
	}
	if workspace != "" {
		content["workspace"] = workspace
	}
	if requestID != "" {
		content["request_id"] = requestID
	}

	return messaging.Event{
		EventID: eventID,
		Type:    "m.room.message",
		Sender:  sender,
		Content: content,
	}
}

// isSendMessagePath checks if a URL path matches the Matrix send event
// pattern for m.room.message.
func isSendMessagePath(path string) bool {
	// Pattern: /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}
	return strings.Contains(path, "/send/m.room.message/")
}

// extractRoomIDFromSendPath extracts the room ID from a send event path.
func extractRoomIDFromSendPath(path string) string {
	// Path: /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}
	const prefix = "/_matrix/client/v3/rooms/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	index := strings.Index(rest, "/send/")
	if index < 0 {
		return ""
	}
	roomID, _ := url.PathUnescape(rest[:index])
	return roomID
}

func TestCommandDispatch(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create some workspace directories for workspace.list to find.
	for _, name := range []string{"project-alpha", "project-beta"} {
		if err := os.Mkdir(filepath.Join(harness.daemon.workspaceRoot, name), 0755); err != nil {
			t.Fatalf("creating workspace dir: %v", err)
		}
	}

	// Set up power levels so the sender is authorized (PL 0 suffices for
	// workspace.list).
	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	// Simulate a command arriving via sync.
	event := buildCommandEvent("$cmd1", "@operator:bureau.local", "workspace.list", "", "req-001")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	// Verify a result was posted.
	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.RoomID != roomID {
		t.Errorf("result room = %q, want %q", message.RoomID, roomID)
	}

	if message.Content["msgtype"] != schema.MsgTypeCommandResult {
		t.Errorf("msgtype = %v, want %q", message.Content["msgtype"], schema.MsgTypeCommandResult)
	}
	if message.Content["status"] != "success" {
		t.Errorf("status = %v, want 'success'", message.Content["status"])
	}
	if message.Content["request_id"] != "req-001" {
		t.Errorf("request_id = %v, want 'req-001'", message.Content["request_id"])
	}

	// Verify the result contains workspace names.
	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", message.Content["result"])
	}
	workspaces, ok := result["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspaces is not a slice: %T", result["workspaces"])
	}
	if len(workspaces) != 2 {
		t.Errorf("expected 2 workspaces, got %d: %v", len(workspaces), workspaces)
	}
}

func TestCommandAuthorizationDenied(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create the workspace directory.
	workspace := "test-project"
	if err := os.MkdirAll(filepath.Join(harness.daemon.workspaceRoot, workspace, ".bare"), 0755); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}

	const roomID = "!workspace:test"
	// Set power levels: sender has PL 0, workspace.fetch requires PL 50.
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$cmd2", "@lowperm:bureau.local", "workspace.fetch", workspace, "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorMessage, _ := message.Content["error"].(string)
	if !strings.Contains(errorMessage, "power level") {
		t.Errorf("error should mention power level, got: %q", errorMessage)
	}
}

func TestCommandAuthorizationAllowed(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	// Set power levels: sender has PL 50 (sufficient for workspace.fetch).
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users": map[string]any{
			"@operator:bureau.local": float64(50),
		},
		"users_default": 0,
	})

	// workspace.fetch needs a .bare directory, but will fail because git
	// isn't initialized — that's fine, we're testing authorization not git.
	workspace := "test-project"
	if err := os.MkdirAll(filepath.Join(harness.daemon.workspaceRoot, workspace, ".bare"), 0755); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}

	event := buildCommandEvent("$cmd3", "@operator:bureau.local", "workspace.fetch", workspace, "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	// The command was authorized (we get a result, not an auth error).
	// It may fail because flock/git aren't configured, but the error
	// won't be about authorization.
	message := messages[0]
	errorMessage, _ := message.Content["error"].(string)
	if strings.Contains(errorMessage, "authorization denied") {
		t.Errorf("should not get authorization error with PL 50, got: %q", errorMessage)
	}
}

func TestCommandUnknownCommand(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$cmd4", "@user:bureau.local", "workspace.nonexistent", "", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorMessage, _ := message.Content["error"].(string)
	if !strings.Contains(errorMessage, "unknown command") {
		t.Errorf("error should mention unknown command, got: %q", errorMessage)
	}
}

func TestCommandSkipsSelfMessages(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	// Event sender matches daemon's own user ID.
	event := buildCommandEvent("$cmd5", "@machine/test:bureau.local", "workspace.list", "", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 0 {
		t.Errorf("expected 0 sent messages (self-message should be skipped), got %d", len(messages))
	}
}

func TestCommandMissingWorkspace(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	// workspace.status requires a workspace, but we send an empty one.
	event := buildCommandEvent("$cmd6", "@user:bureau.local", "workspace.status", "", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorMessage, _ := message.Content["error"].(string)
	if !strings.Contains(errorMessage, "requires a workspace") {
		t.Errorf("error should mention missing workspace, got: %q", errorMessage)
	}
}

func TestCommandPathTraversal(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	traversalAttempts := []string{
		"../etc/passwd",
		"foo/../../etc",
		"/absolute/path",
		".hidden-dir",
	}

	for _, workspace := range traversalAttempts {
		event := buildCommandEvent("$trav", "@user:bureau.local", "workspace.status", workspace, "")

		ctx := context.Background()
		harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})
	}

	messages := harness.getSentMessages()
	if len(messages) != len(traversalAttempts) {
		t.Fatalf("expected %d error messages, got %d", len(traversalAttempts), len(messages))
	}

	for index, message := range messages {
		if message.Content["status"] != "error" {
			t.Errorf("traversal attempt %d: status = %v, want 'error'", index, message.Content["status"])
		}
	}
}

func TestCommandWorkspaceList(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create workspace directories.
	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(harness.daemon.workspaceRoot, name), 0755); err != nil {
			t.Fatalf("creating dir: %v", err)
		}
	}

	// Also create a file (should be excluded — only directories).
	if err := os.WriteFile(filepath.Join(harness.daemon.workspaceRoot, "not-a-dir.txt"), []byte("test"), 0644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$list", "@user:bureau.local", "workspace.list", "", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	result, ok := messages[0].Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", messages[0].Content["result"])
	}

	workspaces, ok := result["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspaces is not a slice: %T", result["workspaces"])
	}

	if len(workspaces) != 3 {
		t.Errorf("expected 3 workspaces, got %d: %v", len(workspaces), workspaces)
	}

	// Verify each expected name is present.
	workspaceSet := make(map[string]bool)
	for _, workspace := range workspaces {
		workspaceSet[workspace.(string)] = true
	}
	for _, name := range names {
		if !workspaceSet[name] {
			t.Errorf("workspace %q not found in result", name)
		}
	}
}

func TestCommandWorkspaceDu(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create a workspace with some content.
	workspace := "test-project"
	workspaceDir := filepath.Join(harness.daemon.workspaceRoot, workspace)
	if err := os.Mkdir(workspaceDir, 0755); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "data.txt"), []byte("hello world"), 0644); err != nil {
		t.Fatalf("creating data file: %v", err)
	}

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$du", "@user:bureau.local", "workspace.du", workspace, "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "success" {
		t.Errorf("status = %v, want 'success' (error: %v)", message.Content["status"], message.Content["error"])
	}

	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", message.Content["result"])
	}

	size, ok := result["size"].(string)
	if !ok || size == "" {
		t.Errorf("size should be a non-empty string, got: %v", result["size"])
	}
}

func TestCommandThreadedReply(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	const commandEventID = "$cmd-thread-test"
	event := buildCommandEvent(commandEventID, "@user:bureau.local", "workspace.list", "", "req-thread")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	content := messages[0].Content

	// Verify the thread relation structure.
	relatesTo, ok := content["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatal("m.relates_to missing or not a map")
	}

	if relatesTo["rel_type"] != "m.thread" {
		t.Errorf("rel_type = %v, want 'm.thread'", relatesTo["rel_type"])
	}
	if relatesTo["event_id"] != commandEventID {
		t.Errorf("event_id = %v, want %q", relatesTo["event_id"], commandEventID)
	}
	if relatesTo["is_falling_back"] != true {
		t.Errorf("is_falling_back = %v, want true", relatesTo["is_falling_back"])
	}

	inReplyTo, ok := relatesTo["m.in_reply_to"].(map[string]any)
	if !ok {
		t.Fatal("m.in_reply_to missing or not a map")
	}
	if inReplyTo["event_id"] != commandEventID {
		t.Errorf("in_reply_to event_id = %v, want %q", inReplyTo["event_id"], commandEventID)
	}

	// Verify duration_ms is present and non-negative.
	durationMilliseconds, ok := content["duration_ms"].(float64)
	if !ok {
		t.Fatal("duration_ms missing or not a number")
	}
	if durationMilliseconds < 0 {
		t.Errorf("duration_ms = %v, should be non-negative", durationMilliseconds)
	}
}

func TestCommandWorkspaceStatus(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create a workspace with a .bare directory.
	workspace := "my-project"
	workspaceDir := filepath.Join(harness.daemon.workspaceRoot, workspace)
	if err := os.MkdirAll(filepath.Join(workspaceDir, ".bare"), 0755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$status", "@user:bureau.local", "workspace.status", workspace, "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "success" {
		t.Fatalf("status = %v, want 'success' (error: %v)", message.Content["status"], message.Content["error"])
	}

	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", message.Content["result"])
	}

	if result["exists"] != true {
		t.Errorf("exists = %v, want true", result["exists"])
	}
	if result["is_dir"] != true {
		t.Errorf("is_dir = %v, want true", result["is_dir"])
	}
	if result["has_bare_repo"] != true {
		t.Errorf("has_bare_repo = %v, want true", result["has_bare_repo"])
	}
}

func TestCommandWorkspaceStatusNonExistent(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$status2", "@user:bureau.local", "workspace.status", "nonexistent", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "success" {
		t.Fatalf("status = %v, want 'success'", message.Content["status"])
	}

	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", message.Content["result"])
	}

	if result["exists"] != false {
		t.Errorf("exists = %v, want false", result["exists"])
	}
}

func TestCommandNonMessageEventsIgnored(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"

	// A state event (not m.room.message) should be ignored.
	stateEvent := messaging.Event{
		EventID: "$state1",
		Type:    "m.bureau.machine_config",
		Sender:  "@admin:bureau.local",
		Content: map[string]any{
			"msgtype": schema.MsgTypeCommand,
			"command": "workspace.list",
		},
	}

	// A regular text message (not m.bureau.command) should be ignored.
	textEvent := messaging.Event{
		EventID: "$text1",
		Type:    "m.room.message",
		Sender:  "@user:bureau.local",
		Content: map[string]any{
			"msgtype": "m.text",
			"body":    "hello world",
		},
	}

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{stateEvent, textEvent})

	messages := harness.getSentMessages()
	if len(messages) != 0 {
		t.Errorf("expected 0 messages for non-command events, got %d", len(messages))
	}
}

func TestCommandWorkspaceListEmpty(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, "m.room.power_levels", "", map[string]any{
		"users":         map[string]any{},
		"users_default": 0,
	})

	event := buildCommandEvent("$empty", "@user:bureau.local", "workspace.list", "", "")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, roomID, []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	result, ok := messages[0].Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", messages[0].Content["result"])
	}

	workspaces, ok := result["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspaces is not a slice: %T", result["workspaces"])
	}
	if len(workspaces) != 0 {
		t.Errorf("expected 0 workspaces, got %d", len(workspaces))
	}
}
