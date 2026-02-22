// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	pipelineschema "github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// pipelineTestState provides a mock Matrix server for pipeline CLI tests.
// It supports alias resolution, room state listing, individual state event
// fetches, state event writes, room message sends, and /sync responses
// that deliver command results.
type pipelineTestState struct {
	mu sync.Mutex

	// roomAliases maps full aliases ("#bureau/pipeline:test.local") to room IDs.
	roomAliases map[string]string

	// roomEvents maps roomID to the list of state events returned by GetRoomState.
	roomEvents map[string][]messaging.Event

	// stateEvents maps "roomID\x00eventType\x00stateKey" to raw JSON content
	// for individual state event fetches.
	stateEvents map[string]json.RawMessage

	// sentStateEvents captures SendStateEvent calls for verification.
	sentStateEvents []sentStateEvent

	// sentEvents captures SendEvent calls (m.room.message) for verification.
	sentEvents []sentEvent

	// pendingSyncEvents accumulates events to deliver in the next /sync
	// response (keyed by room ID). handleSendEvent populates this when
	// autoCommandResult is set; handleSync drains it.
	pendingSyncEvents map[string][]map[string]any

	// autoCommandResult, when non-nil, is called after a command message
	// is sent to generate a command_result that will be delivered via the
	// next /sync. It receives the request ID and room ID and returns the
	// result payload (the "result" field of the command_result content).
	autoCommandResult func(requestID, roomID string) map[string]any

	// syncBatchCounter generates unique batch tokens.
	syncBatchCounter int
}

type sentStateEvent struct {
	RoomID   string
	Type     string
	StateKey string
	Body     json.RawMessage
}

type sentEvent struct {
	RoomID string
	Type   string
	Body   json.RawMessage
}

func newPipelineTestState() *pipelineTestState {
	return &pipelineTestState{
		roomAliases:       make(map[string]string),
		roomEvents:        make(map[string][]messaging.Event),
		stateEvents:       make(map[string]json.RawMessage),
		pendingSyncEvents: make(map[string][]map[string]any),
	}
}

// addPipelineRoom sets up a room with pipeline state events for list tests.
func (s *pipelineTestState) addPipelineRoom(roomAlias, roomID string, pipelines map[string]pipelineschema.PipelineContent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.roomAliases[roomAlias] = roomID

	var events []messaging.Event
	for name, content := range pipelines {
		contentJSON, err := json.Marshal(content)
		if err != nil {
			panic(fmt.Sprintf("marshaling pipeline %q: %v", name, err))
		}
		var contentMap map[string]any
		if err := json.Unmarshal(contentJSON, &contentMap); err != nil {
			panic(fmt.Sprintf("unmarshaling pipeline %q to map: %v", name, err))
		}

		stateKey := name
		events = append(events, messaging.Event{
			Type:     schema.EventTypePipeline,
			StateKey: &stateKey,
			Content:  contentMap,
		})

		// Also register for individual state event fetches.
		key := roomID + "\x00" + string(schema.EventTypePipeline) + "\x00" + name
		s.stateEvents[key] = contentJSON
	}

	s.roomEvents[roomID] = events
}

func (s *pipelineTestState) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.RawPath
		if path == "" {
			path = request.URL.Path
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasPrefix(path, "/_matrix/client/v3/directory/room/"):
			s.handleResolveAlias(writer, path)

		case request.Method == http.MethodGet &&
			strings.HasSuffix(path, "/state") &&
			strings.HasPrefix(path, "/_matrix/client/v3/rooms/"):
			s.handleGetRoomState(writer, path)

		case request.Method == http.MethodGet &&
			strings.Contains(path, "/state/"):
			s.handleGetStateEvent(writer, path)

		case request.Method == http.MethodPut &&
			strings.Contains(path, "/state/"):
			s.handleSendStateEvent(writer, request, path)

		case request.Method == http.MethodPut &&
			strings.Contains(path, "/send/"):
			s.handleSendEvent(writer, request, path)

		case strings.HasPrefix(path, "/_matrix/client/v3/sync"):
			s.handleSync(writer, request)

		default:
			http.Error(writer, fmt.Sprintf(`{"errcode":"M_UNRECOGNIZED","error":"unknown: %s %s"}`, request.Method, path), http.StatusNotFound)
		}
	})
}

func (s *pipelineTestState) handleResolveAlias(writer http.ResponseWriter, path string) {
	encoded := strings.TrimPrefix(path, "/_matrix/client/v3/directory/room/")
	alias, err := url.PathUnescape(encoded)
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad alias encoding"}`, http.StatusBadRequest)
		return
	}
	roomID, exists := s.roomAliases[alias]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"room alias %q not found"}`, alias)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"room_id":"%s"}`, roomID)
}

func (s *pipelineTestState) handleGetRoomState(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	roomIDEncoded := strings.TrimSuffix(trimmed, "/state")
	roomID, err := url.PathUnescape(roomIDEncoded)
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	events, exists := s.roomEvents[roomID]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"room %q not found"}`, roomID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(events)
}

func (s *pipelineTestState) handleGetStateEvent(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/state/", 2)
	if len(parts) != 2 {
		http.Error(writer, `{"errcode":"M_UNRECOGNIZED","error":"bad state path"}`, http.StatusBadRequest)
		return
	}
	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	eventAndKey := parts[1]
	slashIndex := strings.Index(eventAndKey, "/")
	if slashIndex < 0 {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"missing state key"}`, http.StatusBadRequest)
		return
	}
	eventType, err := url.PathUnescape(eventAndKey[:slashIndex])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad eventType encoding"}`, http.StatusBadRequest)
		return
	}
	stateKey, err := url.PathUnescape(eventAndKey[slashIndex+1:])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad stateKey encoding"}`, http.StatusBadRequest)
		return
	}

	key := roomID + "\x00" + eventType + "\x00" + stateKey
	content, exists := s.stateEvents[key]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"state event not found: %s/%s in %s"}`, eventType, stateKey, roomID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Write(content)
}

func (s *pipelineTestState) handleSendStateEvent(writer http.ResponseWriter, request *http.Request, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/state/", 2)
	if len(parts) != 2 {
		http.Error(writer, `{"errcode":"M_UNRECOGNIZED","error":"bad state path"}`, http.StatusBadRequest)
		return
	}
	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	eventAndKey := parts[1]
	slashIndex := strings.Index(eventAndKey, "/")
	if slashIndex < 0 {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"missing state key"}`, http.StatusBadRequest)
		return
	}
	eventType, err := url.PathUnescape(eventAndKey[:slashIndex])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad eventType encoding"}`, http.StatusBadRequest)
		return
	}
	stateKey, err := url.PathUnescape(eventAndKey[slashIndex+1:])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad stateKey encoding"}`, http.StatusBadRequest)
		return
	}

	var body json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, `{"errcode":"M_BAD_JSON","error":"invalid body"}`, http.StatusBadRequest)
		return
	}

	s.sentStateEvents = append(s.sentStateEvents, sentStateEvent{
		RoomID:   roomID,
		Type:     eventType,
		StateKey: stateKey,
		Body:     body,
	})

	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"event_id":"$test-event-id"}`)
}

func (s *pipelineTestState) handleSendEvent(writer http.ResponseWriter, request *http.Request, path string) {
	// Path: /_matrix/client/v3/rooms/{roomID}/send/{eventType}/{txnId}
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/send/", 2)
	if len(parts) != 2 {
		http.Error(writer, `{"errcode":"M_UNRECOGNIZED","error":"bad send path"}`, http.StatusBadRequest)
		return
	}
	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}
	typeAndTxn := parts[1]
	slashIndex := strings.Index(typeAndTxn, "/")
	eventType := typeAndTxn
	if slashIndex >= 0 {
		eventType = typeAndTxn[:slashIndex]
	}
	eventType, _ = url.PathUnescape(eventType)

	var body json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, `{"errcode":"M_BAD_JSON","error":"invalid body"}`, http.StatusBadRequest)
		return
	}

	s.sentEvents = append(s.sentEvents, sentEvent{
		RoomID: roomID,
		Type:   eventType,
		Body:   body,
	})

	// If autoCommandResult is set and this is a command message, queue
	// a matching command_result for delivery via the next /sync.
	if s.autoCommandResult != nil {
		var command schema.CommandMessage
		if json.Unmarshal(body, &command) == nil && command.MsgType == schema.MsgTypeCommand {
			resultPayload := s.autoCommandResult(command.RequestID, roomID)
			resultJSON, _ := json.Marshal(resultPayload)
			commandResult := map[string]any{
				"msgtype":    schema.MsgTypeCommandResult,
				"body":       "command result",
				"status":     "success",
				"request_id": command.RequestID,
				"result":     json.RawMessage(resultJSON),
			}
			s.pendingSyncEvents[roomID] = append(s.pendingSyncEvents[roomID], commandResult)
		}
	}

	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"event_id":"$test-event-id"}`)
}

func (s *pipelineTestState) handleSync(writer http.ResponseWriter, request *http.Request) {
	s.syncBatchCounter++
	batchToken := fmt.Sprintf("mock_batch_%d", s.syncBatchCounter)

	since := request.URL.Query().Get("since")
	if since == "" || len(s.pendingSyncEvents) == 0 {
		// Initial sync or no pending events — return empty rooms.
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"next_batch":%q,"rooms":{"join":{}}}`, batchToken)
		return
	}

	// Build /sync response with pending events in each room's timeline.
	joinedRooms := make(map[string]any)
	for roomID, events := range s.pendingSyncEvents {
		timelineEvents := make([]map[string]any, len(events))
		for i, event := range events {
			timelineEvents[i] = map[string]any{
				"type":    "m.room.message",
				"content": event,
			}
		}
		joinedRooms[roomID] = map[string]any{
			"timeline": map[string]any{"events": timelineEvents},
			"state":    map[string]any{"events": []any{}},
		}
	}
	s.pendingSyncEvents = make(map[string][]map[string]any)

	response := map[string]any{
		"next_batch": batchToken,
		"rooms":      map[string]any{"join": joinedRooms},
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(response)
}

// newPipelineTestSession creates a mock Matrix session connected to the
// test server. Returns the session and sets BUREAU_SESSION_FILE so that
// cli.ConnectOperator() finds it.
func newPipelineTestSession(t *testing.T, state *pipelineTestState) *messaging.DirectSession {
	t.Helper()
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	operatorUserID, err := ref.ParseUserID("@operator:test.local")
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	session, err := client.SessionFromToken(operatorUserID, "operator-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// setupTestOperatorSession writes a temporary operator session file
// pointing at the mock server and sets BUREAU_SESSION_FILE so that
// cli.ConnectOperator() finds it. Returns a cleanup function.
func setupTestOperatorSession(t *testing.T, serverURL string) {
	t.Helper()

	directory := t.TempDir()
	sessionPath := filepath.Join(directory, "session.json")
	session := &cli.OperatorSession{
		UserID:      "@operator:test.local",
		AccessToken: "test-token",
		Homeserver:  serverURL,
	}
	if err := cli.SaveSessionTo(session, sessionPath); err != nil {
		t.Fatalf("SaveSessionTo: %v", err)
	}

	t.Setenv("BUREAU_SESSION_FILE", sessionPath)
}

// startTestServer starts the mock Matrix server and configures the
// operator session to point at it. Returns the state for assertions.
func startTestServer(t *testing.T, state *pipelineTestState) {
	t.Helper()

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	setupTestOperatorSession(t, server.URL)
}

// writePipelineFile creates a temporary JSONC pipeline file and returns
// its path. The caller does not need to clean up — t.TempDir() handles it.
func writePipelineFile(t *testing.T, content string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "pipeline.jsonc")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
