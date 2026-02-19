// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// mockMatrixHandler serves canned Matrix API responses for testing the
// proxy's structured /v1/matrix/* endpoints. Each endpoint records the
// request it received for assertion.
type mockMatrixHandler struct {
	// Canned responses, keyed by request path or pattern.
	whoamiResponse    string
	resolveResponses  map[string]string // alias → {"room_id": "!..."}
	stateResponses    map[string]string // "roomID/eventType/stateKey" → JSON content
	putStateEventID   string
	sendMessageEvents []recordedMatrixEvent

	// Canned responses for new endpoints.
	joinedRoomsResponse string            // JSON for GET /joined_rooms
	roomStateResponses  map[string]string // roomID → JSON array of state events
	membersResponses    map[string]string // roomID → JSON members response
	messagesResponses   map[string]string // roomID → JSON messages response
	threadResponses     map[string]string // "roomID/eventID" → JSON thread response
	displayNameResponse map[string]string // userID → JSON display name
	createRoomResponse  string            // JSON for POST /createRoom
	joinResponse        map[string]string // roomIDOrAlias → JSON join response
	syncResponse        string            // JSON for GET /sync

	// Recorded requests for assertion.
	recorded []recordedMatrixRequest
}

type recordedMatrixRequest struct {
	Method   string
	Path     string
	RawQuery string // query string portion of the URL
	Body     string
}

type recordedMatrixEvent struct {
	EventID string
}

func newMockMatrixHandler() *mockMatrixHandler {
	return &mockMatrixHandler{
		whoamiResponse:      `{"user_id":"@pipeline/test:bureau.local"}`,
		resolveResponses:    map[string]string{},
		stateResponses:      map[string]string{},
		putStateEventID:     "$state_evt_1",
		joinedRoomsResponse: `{"joined_rooms":[]}`,
		roomStateResponses:  map[string]string{},
		membersResponses:    map[string]string{},
		messagesResponses:   map[string]string{},
		threadResponses:     map[string]string{},
		displayNameResponse: map[string]string{},
		createRoomResponse:  `{"room_id":"!new:bureau.local"}`,
		joinResponse:        map[string]string{},
		syncResponse:        `{"next_batch":"s_initial","rooms":{"join":{}}}`,
	}
}

func (m *mockMatrixHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.recorded = append(m.recorded, recordedMatrixRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Body:     string(body),
	})

	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Path

	// GET /_matrix/client/v3/account/whoami
	if r.Method == "GET" && path == "/_matrix/client/v3/account/whoami" {
		w.Write([]byte(m.whoamiResponse))
		return
	}

	// GET /_matrix/client/v3/directory/room/{alias}
	if r.Method == "GET" && strings.HasPrefix(path, "/_matrix/client/v3/directory/room/") {
		encodedAlias := strings.TrimPrefix(path, "/_matrix/client/v3/directory/room/")
		alias, _ := url.PathUnescape(encodedAlias)
		if response, ok := m.resolveResponses[alias]; ok {
			w.Write([]byte(response))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Room alias not found"}`))
		return
	}

	// GET /_matrix/client/v3/rooms/{roomId}/state/{type}/{key}
	if r.Method == "GET" && strings.Contains(path, "/state/") {
		// Parse: /_matrix/client/v3/rooms/{roomId}/state/{type}/{key}
		trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
		parts := strings.SplitN(trimmed, "/state/", 2)
		if len(parts) == 2 {
			roomID, _ := url.PathUnescape(parts[0])
			stateSegments := strings.SplitN(parts[1], "/", 2)
			eventType, _ := url.PathUnescape(stateSegments[0])
			stateKey := ""
			if len(stateSegments) > 1 {
				stateKey, _ = url.PathUnescape(stateSegments[1])
			}
			lookupKey := roomID + "/" + eventType + "/" + stateKey
			if response, ok := m.stateResponses[lookupKey]; ok {
				w.Write([]byte(response))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"State event not found"}`))
		return
	}

	// PUT /_matrix/client/v3/rooms/{roomId}/state/{type}/{key}
	if r.Method == "PUT" && strings.Contains(path, "/state/") {
		w.Write([]byte(`{"event_id":"` + m.putStateEventID + `"}`))
		return
	}

	// PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId}
	if r.Method == "PUT" && strings.Contains(path, "/send/") {
		eventID := "$msg_evt_" + strings.TrimRight(path[strings.LastIndex(path, "/")+1:], "/")
		w.Write([]byte(`{"event_id":"` + eventID + `"}`))
		return
	}

	// GET /_matrix/client/v3/joined_rooms
	if r.Method == "GET" && path == "/_matrix/client/v3/joined_rooms" {
		w.Write([]byte(m.joinedRoomsResponse))
		return
	}

	// GET /_matrix/client/v3/rooms/{roomId}/state (no trailing type/key)
	if r.Method == "GET" && strings.HasSuffix(path, "/state") && strings.HasPrefix(path, "/_matrix/client/v3/rooms/") {
		roomID, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimSuffix(path, "/state"), "/_matrix/client/v3/rooms/"))
		if response, ok := m.roomStateResponses[roomID]; ok {
			w.Write([]byte(response))
			return
		}
		w.Write([]byte(`[]`))
		return
	}

	// GET /_matrix/client/v3/rooms/{roomId}/members
	if r.Method == "GET" && strings.HasSuffix(path, "/members") && strings.HasPrefix(path, "/_matrix/client/v3/rooms/") {
		roomID, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimSuffix(path, "/members"), "/_matrix/client/v3/rooms/"))
		if response, ok := m.membersResponses[roomID]; ok {
			w.Write([]byte(response))
			return
		}
		w.Write([]byte(`{"chunk":[]}`))
		return
	}

	// GET /_matrix/client/v3/rooms/{roomId}/messages
	if r.Method == "GET" && strings.Contains(path, "/messages") && strings.HasPrefix(path, "/_matrix/client/v3/rooms/") {
		roomID, _ := url.PathUnescape(strings.TrimPrefix(path, "/_matrix/client/v3/rooms/"))
		roomID = strings.TrimSuffix(roomID, "/messages")
		if response, ok := m.messagesResponses[roomID]; ok {
			w.Write([]byte(response))
			return
		}
		w.Write([]byte(`{"start":"","end":"","chunk":[]}`))
		return
	}

	// GET /_matrix/client/v3/rooms/{roomId}/relations/{eventId}/m.thread
	if r.Method == "GET" && strings.Contains(path, "/relations/") {
		trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
		parts := strings.SplitN(trimmed, "/relations/", 2)
		if len(parts) == 2 {
			roomID, _ := url.PathUnescape(parts[0])
			eventPart := strings.TrimSuffix(parts[1], "/m.thread")
			eventID, _ := url.PathUnescape(eventPart)
			lookupKey := roomID + "/" + eventID
			if response, ok := m.threadResponses[lookupKey]; ok {
				w.Write([]byte(response))
				return
			}
		}
		w.Write([]byte(`{"chunk":[]}`))
		return
	}

	// GET /_matrix/client/v3/profile/{userId}/displayname
	if r.Method == "GET" && strings.Contains(path, "/profile/") && strings.HasSuffix(path, "/displayname") {
		trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/profile/")
		userID, _ := url.PathUnescape(strings.TrimSuffix(trimmed, "/displayname"))
		if response, ok := m.displayNameResponse[userID]; ok {
			w.Write([]byte(response))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"User not found"}`))
		return
	}

	// POST /_matrix/client/v3/createRoom
	if r.Method == "POST" && path == "/_matrix/client/v3/createRoom" {
		w.Write([]byte(m.createRoomResponse))
		return
	}

	// POST /_matrix/client/v3/join/{roomIdOrAlias}
	if r.Method == "POST" && strings.HasPrefix(path, "/_matrix/client/v3/join/") {
		roomIDOrAlias, _ := url.PathUnescape(strings.TrimPrefix(path, "/_matrix/client/v3/join/"))
		if response, ok := m.joinResponse[roomIDOrAlias]; ok {
			w.Write([]byte(response))
			return
		}
		w.Write([]byte(`{"room_id":"` + roomIDOrAlias + `"}`))
		return
	}

	// POST /_matrix/client/v3/rooms/{roomId}/invite
	if r.Method == "POST" && strings.HasSuffix(path, "/invite") && strings.HasPrefix(path, "/_matrix/client/v3/rooms/") {
		w.Write([]byte(`{}`))
		return
	}

	// GET /_matrix/client/v3/sync
	if r.Method == "GET" && path == "/_matrix/client/v3/sync" {
		w.Write([]byte(m.syncResponse))
		return
	}

	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"unrecognized request"}`))
}

// setupMatrixProxyTest creates a proxy server with a mock Matrix backend
// and returns the agent HTTP client and mock handler.
func setupMatrixProxyTest(t *testing.T) (*http.Client, *mockMatrixHandler) {
	t.Helper()

	mock := newMockMatrixHandler()

	// Start mock Matrix homeserver on a Unix socket (so we can configure
	// the proxy to forward to it).
	tempDir := t.TempDir()
	matrixSocket := filepath.Join(tempDir, "matrix.sock")
	matrixListener, err := net.Listen("unix", matrixSocket)
	if err != nil {
		t.Fatalf("listen on matrix socket: %v", err)
	}
	matrixServer := &http.Server{Handler: mock}
	go matrixServer.Serve(matrixListener)
	t.Cleanup(func() { matrixServer.Close() })

	// Create proxy server with agent socket.
	agentSocket := filepath.Join(tempDir, "proxy.sock")
	server, err := NewServer(ServerConfig{
		SocketPath: agentSocket,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Register a "matrix" HTTP service pointing at the mock homeserver.
	matrixService, err := NewHTTPService(HTTPServiceConfig{
		Name:         matrixServiceName,
		UpstreamUnix: matrixSocket,
		InjectHeaders: map[string]string{
			"Authorization": "matrix-bearer",
		},
		Credential: testCredentials(t, map[string]string{
			"matrix-bearer": "Bearer syt_test_token",
		}),
	})
	if err != nil {
		t.Fatalf("create matrix service: %v", err)
	}
	server.RegisterHTTPService(matrixServiceName, matrixService)

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	return client, mock
}

func TestMatrixWhoami(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.whoamiResponse = `{"user_id":"@pipeline/executor:bureau.local"}`

	resp, err := client.Get("http://localhost/v1/matrix/whoami")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["user_id"] != "@pipeline/executor:bureau.local" {
		t.Errorf("expected user_id @pipeline/executor:bureau.local, got %q", result["user_id"])
	}

	// Verify the mock received the request with Authorization header.
	if len(mock.recorded) != 1 {
		t.Fatalf("expected 1 recorded request, got %d", len(mock.recorded))
	}
	if mock.recorded[0].Path != "/_matrix/client/v3/account/whoami" {
		t.Errorf("expected whoami path, got %q", mock.recorded[0].Path)
	}
}

func TestMatrixResolve(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.resolveResponses["#bureau/pipeline:bureau.local"] = `{"room_id":"!pipeline_room:bureau.local","servers":["bureau.local"]}`

	t.Run("resolve existing alias", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/resolve?alias=" + url.QueryEscape("#bureau/pipeline:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		if result["room_id"] != "!pipeline_room:bureau.local" {
			t.Errorf("expected room_id !pipeline_room:bureau.local, got %v", result["room_id"])
		}
	})

	t.Run("resolve missing alias", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/resolve?alias=" + url.QueryEscape("#nonexistent:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for missing alias, got %d", resp.StatusCode)
		}
	})

	t.Run("missing alias parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/resolve")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixGetState(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.stateResponses["!room1:bureau.local/m.bureau.pipeline/dev-workspace-init"] = `{"description":"Initialize a dev workspace","steps":[{"name":"clone"}]}`

	t.Run("get existing state event", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/state?" +
			"room=" + url.QueryEscape("!room1:bureau.local") +
			"&type=m.bureau.pipeline" +
			"&key=dev-workspace-init")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		if result["description"] != "Initialize a dev workspace" {
			t.Errorf("expected description field, got %v", result)
		}
	})

	t.Run("get missing state event", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/state?" +
			"room=" + url.QueryEscape("!room1:bureau.local") +
			"&type=m.bureau.pipeline" +
			"&key=nonexistent")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("resolve alias for get state", func(t *testing.T) {
		// Register the alias resolution and state event.
		mock.resolveResponses["#bureau/pipeline:bureau.local"] = `{"room_id":"!room1:bureau.local"}`

		resp, err := client.Get("http://localhost/v1/matrix/state?" +
			"room=" + url.QueryEscape("#bureau/pipeline:bureau.local") +
			"&type=m.bureau.pipeline" +
			"&key=dev-workspace-init")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("empty state key", func(t *testing.T) {
		mock.stateResponses["!room1:bureau.local/m.room.topic/"] = `{"topic":"test"}`

		resp, err := client.Get("http://localhost/v1/matrix/state?" +
			"room=" + url.QueryEscape("!room1:bureau.local") +
			"&type=m.room.topic" +
			"&key=")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("missing required parameters", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/state?room=!room1:bureau.local")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixPutState(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	t.Run("put state event with room ID", func(t *testing.T) {
		request := MatrixStateRequest{
			Room:      "!room1:bureau.local",
			EventType: "m.bureau.workspace",
			StateKey:  "",
			Content: map[string]any{
				"version": "1.0",
			},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/state", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result MatrixEventResponse
		json.NewDecoder(resp.Body).Decode(&result)
		if result.EventID != "$state_evt_1" {
			t.Errorf("expected event_id $state_evt_1, got %q", result.EventID)
		}

		// Verify the mock received a PUT (not POST) to the correct path.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/state/") {
				putRequest = &mock.recorded[i]
				break
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a state path")
		}
		if !strings.Contains(putRequest.Body, `"version"`) {
			t.Errorf("expected content in body, got %q", putRequest.Body)
		}
	})

	t.Run("put state with alias resolution", func(t *testing.T) {
		mock.resolveResponses["#iree/amdgpu/inference:bureau.local"] = `{"room_id":"!workspace_room:bureau.local"}`

		request := MatrixStateRequest{
			Room:      "#iree/amdgpu/inference:bureau.local",
			EventType: "m.bureau.workspace",
			StateKey:  "",
			Content:   map[string]any{"ready": true},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/state", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify the forwarded request used the resolved room ID, not the alias.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/state/") {
				putRequest = &mock.recorded[i]
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a state path")
		}
		if !strings.Contains(putRequest.Path, "!workspace_room:bureau.local") {
			t.Errorf("expected resolved room ID in path, got %q", putRequest.Path)
		}
	})

	t.Run("missing required fields", func(t *testing.T) {
		request := MatrixStateRequest{Room: "!room:bureau.local"}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/state", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid JSON body", func(t *testing.T) {
		resp, err := client.Post("http://localhost/v1/matrix/state", "application/json", bytes.NewReader([]byte("not json")))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixSendMessage(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	t.Run("send text message", func(t *testing.T) {
		request := MatrixMessageRequest{
			Room: "!room1:bureau.local",
			Content: map[string]any{
				"msgtype": "m.text",
				"body":    "Pipeline dev-workspace-init started (4 steps)",
			},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result MatrixEventResponse
		json.NewDecoder(resp.Body).Decode(&result)
		if result.EventID == "" {
			t.Error("expected non-empty event_id")
		}

		// Verify the mock received a PUT to a send path.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/send/") {
				putRequest = &mock.recorded[i]
				break
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a send path")
		}
		if !strings.Contains(putRequest.Path, "m.room.message") {
			t.Errorf("expected m.room.message in path, got %q", putRequest.Path)
		}
		if !strings.Contains(putRequest.Body, "Pipeline dev-workspace-init") {
			t.Errorf("expected message body in request, got %q", putRequest.Body)
		}
	})

	t.Run("send thread reply", func(t *testing.T) {
		request := MatrixMessageRequest{
			Room: "!room1:bureau.local",
			Content: map[string]any{
				"msgtype": "m.text",
				"body":    "step 1/4: create-project-directory... ok (0.1s)",
				"m.relates_to": map[string]any{
					"rel_type": "m.thread",
					"event_id": "$root_event",
				},
			},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify thread content was forwarded intact.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/send/") {
				putRequest = &mock.recorded[i]
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a send path")
		}
		if !strings.Contains(putRequest.Body, `"m.relates_to"`) {
			t.Errorf("expected m.relates_to in body, got %q", putRequest.Body)
		}
		if !strings.Contains(putRequest.Body, `"m.thread"`) {
			t.Errorf("expected m.thread in body, got %q", putRequest.Body)
		}
	})

	t.Run("send with alias resolution", func(t *testing.T) {
		mock.resolveResponses["#iree/amdgpu/inference:bureau.local"] = `{"room_id":"!workspace_room:bureau.local"}`

		request := MatrixMessageRequest{
			Room: "#iree/amdgpu/inference:bureau.local",
			Content: map[string]any{
				"msgtype": "m.text",
				"body":    "hello from alias",
			},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify the forwarded request used the resolved room ID.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/send/") {
				putRequest = &mock.recorded[i]
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a send path")
		}
		if !strings.Contains(putRequest.Path, "!workspace_room:bureau.local") {
			t.Errorf("expected resolved room ID in path, got %q", putRequest.Path)
		}
	})

	t.Run("missing room", func(t *testing.T) {
		request := MatrixMessageRequest{
			Content: map[string]any{"msgtype": "m.text", "body": "test"},
		}
		body, _ := json.Marshal(request)

		resp, err := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("unique transaction IDs", func(t *testing.T) {
		// Send two messages and verify they get different txn IDs.
		request := MatrixMessageRequest{
			Room: "!room1:bureau.local",
			Content: map[string]any{
				"msgtype": "m.text",
				"body":    "msg",
			},
		}
		body, _ := json.Marshal(request)

		resp1, _ := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		resp1.Body.Close()

		body, _ = json.Marshal(request)
		resp2, _ := client.Post("http://localhost/v1/matrix/message", "application/json", bytes.NewReader(body))
		resp2.Body.Close()

		// Find the two send requests in the mock.
		var sendPaths []string
		for _, recorded := range mock.recorded {
			if recorded.Method == "PUT" && strings.Contains(recorded.Path, "/send/") {
				sendPaths = append(sendPaths, recorded.Path)
			}
		}
		if len(sendPaths) < 2 {
			t.Fatalf("expected at least 2 send requests, got %d", len(sendPaths))
		}
		// Last two should have different transaction IDs.
		path1 := sendPaths[len(sendPaths)-2]
		path2 := sendPaths[len(sendPaths)-1]
		if path1 == path2 {
			t.Error("expected different transaction IDs for successive messages")
		}
	})
}

// setupMatrixProxyTestWithGrants creates a proxy test with the given grants
// configured on the handler. Returns the HTTP client, mock handler, and
// the server (so grants can be changed mid-test).
func setupMatrixProxyTestWithGrants(t *testing.T, grants []schema.Grant) (*http.Client, *mockMatrixHandler, *Server) {
	t.Helper()

	mock := newMockMatrixHandler()

	tempDir := t.TempDir()
	matrixSocket := filepath.Join(tempDir, "matrix.sock")
	matrixListener, err := net.Listen("unix", matrixSocket)
	if err != nil {
		t.Fatalf("listen on matrix socket: %v", err)
	}
	matrixServer := &http.Server{Handler: mock}
	go matrixServer.Serve(matrixListener)
	t.Cleanup(func() { matrixServer.Close() })

	agentSocket := filepath.Join(tempDir, "proxy.sock")
	server, err := NewServer(ServerConfig{
		SocketPath: agentSocket,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	matrixService, err := NewHTTPService(HTTPServiceConfig{
		Name:         matrixServiceName,
		UpstreamUnix: matrixSocket,
		InjectHeaders: map[string]string{
			"Authorization": "matrix-bearer",
		},
		Credential: testCredentials(t, map[string]string{
			"matrix-bearer": "Bearer syt_test_token",
		}),
	})
	if err != nil {
		t.Fatalf("create matrix service: %v", err)
	}
	server.RegisterHTTPService(matrixServiceName, matrixService)

	if grants != nil {
		server.SetGrants(grants)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	return client, mock, server
}

func TestMatrixJoinedRooms(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.joinedRoomsResponse = `{"joined_rooms":["!room1:bureau.local","!room2:bureau.local"]}`

	resp, err := client.Get("http://localhost/v1/matrix/joined-rooms")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string][]string
	json.NewDecoder(resp.Body).Decode(&result)
	rooms := result["joined_rooms"]
	if len(rooms) != 2 {
		t.Fatalf("expected 2 rooms, got %d", len(rooms))
	}
	if rooms[0] != "!room1:bureau.local" {
		t.Errorf("expected !room1:bureau.local, got %q", rooms[0])
	}
}

func TestMatrixGetRoomState(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.roomStateResponses["!room1:bureau.local"] = `[{"type":"m.room.topic","state_key":"","content":{"topic":"test"}}]`

	t.Run("get room state", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/room-state?room=" + url.QueryEscape("!room1:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var events []map[string]any
		json.NewDecoder(resp.Body).Decode(&events)
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0]["type"] != "m.room.topic" {
			t.Errorf("expected m.room.topic, got %v", events[0]["type"])
		}
	})

	t.Run("missing room parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/room-state")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("alias resolution", func(t *testing.T) {
		mock.resolveResponses["#bureau/fleet/prod/machine/ws:bureau.local"] = `{"room_id":"!room1:bureau.local"}`

		resp, err := client.Get("http://localhost/v1/matrix/room-state?room=" + url.QueryEscape("#bureau/fleet/prod/machine/ws:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})
}

func TestMatrixGetRoomMembers(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.membersResponses["!room1:bureau.local"] = `{"chunk":[{"type":"m.room.member","state_key":"@agent/test:bureau.local","sender":"@admin:bureau.local","content":{"membership":"join","displayname":"Test Agent"}}]}`

	t.Run("get room members", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/room-members?room=" + url.QueryEscape("!room1:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		chunk, ok := result["chunk"].([]any)
		if !ok || len(chunk) != 1 {
			t.Fatalf("expected 1 member event, got %v", result["chunk"])
		}
	})

	t.Run("missing room parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/room-members")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixMessages(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.messagesResponses["!room1:bureau.local"] = `{"start":"s1","end":"s2","chunk":[{"type":"m.room.message","sender":"@admin:bureau.local","content":{"msgtype":"m.text","body":"hello"}}]}`

	t.Run("fetch messages", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/messages?room=" + url.QueryEscape("!room1:bureau.local") + "&dir=b&limit=10")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		chunk, ok := result["chunk"].([]any)
		if !ok || len(chunk) != 1 {
			t.Fatalf("expected 1 message event, got %v", result["chunk"])
		}
	})

	t.Run("default direction is backward", func(t *testing.T) {
		mock.recorded = nil

		resp, err := client.Get("http://localhost/v1/matrix/messages?room=" + url.QueryEscape("!room1:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()

		// Verify the forwarded request includes dir=b in the query string.
		var messagesRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if strings.Contains(mock.recorded[i].Path, "/messages") {
				messagesRequest = &mock.recorded[i]
				break
			}
		}
		if messagesRequest == nil {
			t.Fatal("expected a request to /messages")
		}
		if !strings.Contains(messagesRequest.RawQuery, "dir=b") {
			t.Errorf("expected dir=b in query string, got %q", messagesRequest.RawQuery)
		}
	})

	t.Run("missing room parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/messages")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixThreadMessages(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.threadResponses["!room1:bureau.local/$root_event"] = `{"chunk":[{"type":"m.room.message","sender":"@agent:bureau.local","content":{"msgtype":"m.text","body":"reply"}}],"next_batch":"nb1"}`

	t.Run("fetch thread messages", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/thread-messages?room=" +
			url.QueryEscape("!room1:bureau.local") +
			"&thread=" + url.QueryEscape("$root_event") +
			"&limit=25")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		chunk, ok := result["chunk"].([]any)
		if !ok || len(chunk) != 1 {
			t.Fatalf("expected 1 thread event, got %v", result["chunk"])
		}
	})

	t.Run("missing required parameters", func(t *testing.T) {
		// Missing thread parameter.
		resp, err := client.Get("http://localhost/v1/matrix/thread-messages?room=" + url.QueryEscape("!room1:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("missing room parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/thread-messages?thread=$evt")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixGetDisplayName(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.displayNameResponse["@agent/test:bureau.local"] = `{"displayname":"Test Agent"}`

	t.Run("get display name", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/display-name?user=" + url.QueryEscape("@agent/test:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)
		if result["displayname"] != "Test Agent" {
			t.Errorf("expected displayname Test Agent, got %q", result["displayname"])
		}
	})

	t.Run("missing user parameter", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/display-name")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		resp, err := client.Get("http://localhost/v1/matrix/display-name?user=" + url.QueryEscape("@nonexistent:bureau.local"))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixCreateRoom(t *testing.T) {
	t.Run("with grant", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/create-room"}, Targets: []string{"*"}},
		}
		client, mock, _ := setupMatrixProxyTestWithGrants(t, grants)

		mock.createRoomResponse = `{"room_id":"!created:bureau.local"}`

		body, _ := json.Marshal(map[string]any{
			"name": "Test Room",
		})
		resp, err := client.Post("http://localhost/v1/matrix/room", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result map[string]string
		json.NewDecoder(resp.Body).Decode(&result)
		if result["room_id"] != "!created:bureau.local" {
			t.Errorf("expected room_id !created:bureau.local, got %q", result["room_id"])
		}

		// Verify the mock received a POST to /createRoom.
		var createRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "POST" && mock.recorded[i].Path == "/_matrix/client/v3/createRoom" {
				createRequest = &mock.recorded[i]
				break
			}
		}
		if createRequest == nil {
			t.Fatal("expected a POST to /createRoom")
		}
	})

	t.Run("without grant", func(t *testing.T) {
		// No grants configured — create room should be denied.
		client, _, _ := setupMatrixProxyTestWithGrants(t, nil)

		body, _ := json.Marshal(map[string]any{"name": "Test Room"})
		resp, err := client.Post("http://localhost/v1/matrix/room", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			respBody, _ := io.ReadAll(resp.Body)
			t.Errorf("expected 403, got %d: %s", resp.StatusCode, respBody)
		}
	})
}

func TestMatrixJoinRoom(t *testing.T) {
	t.Run("with grant", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/join"}, Targets: []string{"*"}},
		}
		client, mock, _ := setupMatrixProxyTestWithGrants(t, grants)

		mock.joinResponse["!target:bureau.local"] = `{"room_id":"!target:bureau.local"}`

		body, _ := json.Marshal(MatrixJoinRequest{Room: "!target:bureau.local"})
		resp, err := client.Post("http://localhost/v1/matrix/join", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify the mock received a POST to /join/{room}.
		var joinRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "POST" && strings.Contains(mock.recorded[i].Path, "/join/") {
				joinRequest = &mock.recorded[i]
				break
			}
		}
		if joinRequest == nil {
			t.Fatal("expected a POST to /join/")
		}
	})

	t.Run("without grant", func(t *testing.T) {
		client, _, _ := setupMatrixProxyTestWithGrants(t, nil)

		body, _ := json.Marshal(MatrixJoinRequest{Room: "!target:bureau.local"})
		resp, err := client.Post("http://localhost/v1/matrix/join", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			respBody, _ := io.ReadAll(resp.Body)
			t.Errorf("expected 403, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("missing room", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/join"}, Targets: []string{"*"}},
		}
		client, _, _ := setupMatrixProxyTestWithGrants(t, grants)

		body, _ := json.Marshal(MatrixJoinRequest{})
		resp, err := client.Post("http://localhost/v1/matrix/join", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixInviteUser(t *testing.T) {
	t.Run("with grant", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/invite"}, Targets: []string{"*"}},
		}
		client, mock, _ := setupMatrixProxyTestWithGrants(t, grants)

		body, _ := json.Marshal(MatrixInviteRequest{
			Room:   "!room1:bureau.local",
			UserID: "@agent/other:bureau.local",
		})
		resp, err := client.Post("http://localhost/v1/matrix/invite", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify the mock received a POST to /rooms/{roomId}/invite.
		var inviteRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "POST" && strings.Contains(mock.recorded[i].Path, "/invite") {
				inviteRequest = &mock.recorded[i]
				break
			}
		}
		if inviteRequest == nil {
			t.Fatal("expected a POST to /invite")
		}
		if !strings.Contains(inviteRequest.Body, "@agent/other:bureau.local") {
			t.Errorf("expected user_id in body, got %q", inviteRequest.Body)
		}
	})

	t.Run("without grant", func(t *testing.T) {
		client, _, _ := setupMatrixProxyTestWithGrants(t, nil)

		body, _ := json.Marshal(MatrixInviteRequest{
			Room:   "!room1:bureau.local",
			UserID: "@agent/other:bureau.local",
		})
		resp, err := client.Post("http://localhost/v1/matrix/invite", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			respBody, _ := io.ReadAll(resp.Body)
			t.Errorf("expected 403, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("missing fields", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/invite"}, Targets: []string{"*"}},
		}
		client, _, _ := setupMatrixProxyTestWithGrants(t, grants)

		body, _ := json.Marshal(MatrixInviteRequest{Room: "!room1:bureau.local"})
		resp, err := client.Post("http://localhost/v1/matrix/invite", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("alias resolution", func(t *testing.T) {
		grants := []schema.Grant{
			{Actions: []string{"matrix/invite"}, Targets: []string{"*"}},
		}
		client, mock, _ := setupMatrixProxyTestWithGrants(t, grants)

		mock.resolveResponses["#test/room:bureau.local"] = `{"room_id":"!resolved:bureau.local"}`

		body, _ := json.Marshal(MatrixInviteRequest{
			Room:   "#test/room:bureau.local",
			UserID: "@user:bureau.local",
		})
		resp, err := client.Post("http://localhost/v1/matrix/invite", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		// Verify the forwarded request used the resolved room ID.
		var inviteRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "POST" && strings.Contains(mock.recorded[i].Path, "/invite") {
				inviteRequest = &mock.recorded[i]
			}
		}
		if inviteRequest == nil {
			t.Fatal("expected a POST to /invite")
		}
		if !strings.Contains(inviteRequest.Path, "!resolved:bureau.local") {
			t.Errorf("expected resolved room ID in path, got %q", inviteRequest.Path)
		}
	})
}

func TestMatrixSendEvent(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	t.Run("send event", func(t *testing.T) {
		body, _ := json.Marshal(MatrixSendEventRequest{
			Room:      "!room1:bureau.local",
			EventType: "m.bureau.pipeline_request",
			Content:   map[string]any{"pipeline": "build", "ref": "main"},
		})
		resp, err := client.Post("http://localhost/v1/matrix/event", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}

		var result MatrixEventResponse
		json.NewDecoder(resp.Body).Decode(&result)
		if result.EventID == "" {
			t.Error("expected non-empty event_id")
		}

		// Verify the mock received a PUT to a send path with the right event type.
		var putRequest *recordedMatrixRequest
		for i := range mock.recorded {
			if mock.recorded[i].Method == "PUT" && strings.Contains(mock.recorded[i].Path, "/send/") {
				putRequest = &mock.recorded[i]
				break
			}
		}
		if putRequest == nil {
			t.Fatal("expected a PUT request to a send path")
		}
		if !strings.Contains(putRequest.Path, "m.bureau.pipeline_request") {
			t.Errorf("expected m.bureau.pipeline_request in path, got %q", putRequest.Path)
		}
		if !strings.Contains(putRequest.Body, `"pipeline"`) {
			t.Errorf("expected pipeline in body, got %q", putRequest.Body)
		}
	})

	t.Run("alias resolution", func(t *testing.T) {
		mock.resolveResponses["#workspace:bureau.local"] = `{"room_id":"!ws_room:bureau.local"}`

		body, _ := json.Marshal(MatrixSendEventRequest{
			Room:      "#workspace:bureau.local",
			EventType: "m.bureau.test",
			Content:   map[string]any{"value": true},
		})
		resp, err := client.Post("http://localhost/v1/matrix/event", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("missing fields", func(t *testing.T) {
		body, _ := json.Marshal(MatrixSendEventRequest{Room: "!room:bureau.local"})
		resp, err := client.Post("http://localhost/v1/matrix/event", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})
}

func TestMatrixSync(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	mock.syncResponse = `{"next_batch":"s_42","rooms":{"join":{"!config:bureau.local":{"timeline":{"events":[{"type":"m.room.message","sender":"@admin:bureau.local","content":{"msgtype":"m.text","body":"hello"}}]}}}}}`

	// Initial sync with timeout=0.
	resp, err := client.Get("http://localhost/v1/matrix/sync?timeout=0")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["next_batch"] != "s_42" {
		t.Errorf("next_batch = %v, want s_42", result["next_batch"])
	}

	// Verify the proxy forwarded query parameters correctly.
	if len(mock.recorded) == 0 {
		t.Fatal("no requests recorded by mock")
	}
	lastRequest := mock.recorded[len(mock.recorded)-1]
	if lastRequest.Path != "/_matrix/client/v3/sync" {
		t.Errorf("upstream path = %q, want /_matrix/client/v3/sync", lastRequest.Path)
	}
	if !strings.Contains(lastRequest.RawQuery, "timeout=0") {
		t.Errorf("upstream query %q should contain timeout=0", lastRequest.RawQuery)
	}
}

func TestMatrixSyncWithSince(t *testing.T) {
	client, mock := setupMatrixProxyTest(t)

	// Incremental sync with since token.
	resp, err := client.Get("http://localhost/v1/matrix/sync?since=s_initial&timeout=30000&filter=myfilter")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify all query parameters were forwarded.
	if len(mock.recorded) == 0 {
		t.Fatal("no requests recorded by mock")
	}
	lastRequest := mock.recorded[len(mock.recorded)-1]
	if !strings.Contains(lastRequest.RawQuery, "since=s_initial") {
		t.Errorf("upstream query %q should contain since=s_initial", lastRequest.RawQuery)
	}
	if !strings.Contains(lastRequest.RawQuery, "timeout=30000") {
		t.Errorf("upstream query %q should contain timeout=30000", lastRequest.RawQuery)
	}
	if !strings.Contains(lastRequest.RawQuery, "filter=myfilter") {
		t.Errorf("upstream query %q should contain filter=myfilter", lastRequest.RawQuery)
	}
}

func TestMatrixServiceNotConfigured(t *testing.T) {
	// Test that all endpoints return 503 when the "matrix" HTTP service
	// is not registered.
	tempDir := t.TempDir()
	agentSocket := filepath.Join(tempDir, "proxy.sock")

	server, err := NewServer(ServerConfig{
		SocketPath: agentSocket,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Do NOT register a "matrix" HTTP service.

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Shutdown(context.Background())

	testutil.RequireClosed(t, server.Ready(), 5*time.Second, "server ready")

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", agentSocket)
			},
		},
	}

	endpoints := []struct {
		method     string
		path       string
		body       string
		wantStatus int // expected status code
	}{
		{"GET", "/v1/matrix/whoami", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/resolve?alias=%23test:bureau.local", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/state?room=!room:bureau.local&type=m.room.topic&key=", "", http.StatusServiceUnavailable},
		{"POST", "/v1/matrix/state", `{"room":"!r:b","event_type":"m.test","content":{}}`, http.StatusServiceUnavailable},
		{"POST", "/v1/matrix/message", `{"room":"!r:b","content":{"msgtype":"m.text","body":"x"}}`, http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/joined-rooms", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/room-state?room=!room:bureau.local", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/room-members?room=!room:bureau.local", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/messages?room=!room:bureau.local", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/thread-messages?room=!room:bureau.local&thread=$evt1", "", http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/display-name?user=@user:bureau.local", "", http.StatusServiceUnavailable},
		// Gated endpoints return 403 (no grants) before reaching the matrix service check.
		{"POST", "/v1/matrix/room", `{"name":"test"}`, http.StatusForbidden},
		{"POST", "/v1/matrix/join", `{"room":"!room:bureau.local"}`, http.StatusForbidden},
		{"POST", "/v1/matrix/invite", `{"room":"!room:bureau.local","user_id":"@u:b"}`, http.StatusForbidden},
		// SendEvent is ungated — homeserver enforces permissions.
		{"POST", "/v1/matrix/event", `{"room":"!r:b","event_type":"m.test","content":{}}`, http.StatusServiceUnavailable},
		{"GET", "/v1/matrix/sync?timeout=0", "", http.StatusServiceUnavailable},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			var bodyReader io.Reader
			if endpoint.body != "" {
				bodyReader = bytes.NewReader([]byte(endpoint.body))
			}
			req, _ := http.NewRequest(endpoint.method, "http://localhost"+endpoint.path, bodyReader)
			if bodyReader != nil {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != endpoint.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("expected %d, got %d: %s", endpoint.wantStatus, resp.StatusCode, body)
			}
		})
	}
}

func TestForwardRequest(t *testing.T) {
	// Test the HTTPService.ForwardRequest method directly.

	// Create a test upstream server.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method,
			"path":   r.URL.Path,
			"auth":   r.Header.Get("Authorization"),
		})
	}))
	defer upstream.Close()

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:     "test-forward",
		Upstream: upstream.URL,
		InjectHeaders: map[string]string{
			"Authorization": "test-token",
		},
		Credential: testCredentials(t, map[string]string{
			"test-token": "Bearer abc123",
		}),
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}

	t.Run("GET request", func(t *testing.T) {
		response, err := service.ForwardRequest(context.Background(), "GET", "/test/path", nil)
		if err != nil {
			t.Fatalf("forward request failed: %v", err)
		}
		defer response.Body.Close()

		var result map[string]string
		json.NewDecoder(response.Body).Decode(&result)

		if result["method"] != "GET" {
			t.Errorf("expected GET, got %q", result["method"])
		}
		if result["path"] != "/test/path" {
			t.Errorf("expected /test/path, got %q", result["path"])
		}
		if result["auth"] != "Bearer abc123" {
			t.Errorf("expected Bearer abc123, got %q", result["auth"])
		}
	})

	t.Run("PUT request with body", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"test": true}`))
		response, err := service.ForwardRequest(context.Background(), "PUT", "/state/event", body)
		if err != nil {
			t.Fatalf("forward request failed: %v", err)
		}
		defer response.Body.Close()

		var result map[string]string
		json.NewDecoder(response.Body).Decode(&result)

		if result["method"] != "PUT" {
			t.Errorf("expected PUT, got %q", result["method"])
		}
		if result["auth"] != "Bearer abc123" {
			t.Errorf("expected Bearer abc123, got %q", result["auth"])
		}
	})

	t.Run("missing credentials", func(t *testing.T) {
		noCredService, err := NewHTTPService(HTTPServiceConfig{
			Name:     "no-creds",
			Upstream: upstream.URL,
			InjectHeaders: map[string]string{
				"Authorization": "missing-cred",
			},
		})
		if err != nil {
			t.Fatalf("create service: %v", err)
		}

		_, err = noCredService.ForwardRequest(context.Background(), "GET", "/test", nil)
		if err == nil {
			t.Fatal("expected error for missing credentials")
		}
		if !strings.Contains(err.Error(), "missing credentials") {
			t.Errorf("expected 'missing credentials' in error, got %q", err.Error())
		}
	})
}
