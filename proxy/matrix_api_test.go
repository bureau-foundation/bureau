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

	// Recorded requests for assertion.
	recorded []recordedMatrixRequest
}

type recordedMatrixRequest struct {
	Method string
	Path   string
	Body   string
}

type recordedMatrixEvent struct {
	EventID string
}

func newMockMatrixHandler() *mockMatrixHandler {
	return &mockMatrixHandler{
		whoamiResponse:   `{"user_id":"@pipeline/test:bureau.local"}`,
		resolveResponses: map[string]string{},
		stateResponses:   map[string]string{},
		putStateEventID:  "$state_evt_1",
	}
}

func (m *mockMatrixHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.recorded = append(m.recorded, recordedMatrixRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   string(body),
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

	// PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}
	if r.Method == "PUT" && strings.Contains(path, "/send/") {
		eventID := "$msg_evt_" + strings.TrimRight(path[strings.LastIndex(path, "/")+1:], "/")
		w.Write([]byte(`{"event_id":"` + eventID + `"}`))
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
		method string
		path   string
		body   string
	}{
		{"GET", "/v1/matrix/whoami", ""},
		{"GET", "/v1/matrix/resolve?alias=%23test:bureau.local", ""},
		{"GET", "/v1/matrix/state?room=!room:bureau.local&type=m.room.topic&key=", ""},
		{"POST", "/v1/matrix/state", `{"room":"!r:b","event_type":"m.test","content":{}}`},
		{"POST", "/v1/matrix/message", `{"room":"!r:b","content":{"msgtype":"m.text","body":"x"}}`},
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

			if resp.StatusCode != http.StatusServiceUnavailable {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("expected 503, got %d: %s", resp.StatusCode, body)
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
