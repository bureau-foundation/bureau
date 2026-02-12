// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// TestStructuredAgentAPI exercises the proxy's structured /v1/ endpoints
// that agents use as their primary coordination API. These endpoints
// decouple sandboxed code from Matrix URL conventions — the proxy handles
// credential injection, alias resolution, and transaction ID generation.
//
// The test deploys a principal with a proxy, then exercises:
//   - GET /v1/identity       → agent discovers its own Matrix identity
//   - GET /v1/matrix/resolve → agent resolves a room alias to a room ID
//   - POST /v1/matrix/state  → agent publishes a state event
//   - GET /v1/matrix/state   → agent reads that state event back
//   - POST /v1/matrix/message → agent sends a message
//
// This proves credential injection (proxy holds the token, agent doesn't),
// structured request/response format, alias resolution within structured
// endpoints, and the full agent→proxy→homeserver→proxy→agent round trip.
func TestStructuredAgentAPI(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	ctx := t.Context()

	// Boot a machine with a proxy binary so we can deploy a principal.
	machine := newTestMachine(t, "machine/struct-api")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Register and deploy a principal. AllowJoin is needed so the agent
	// can join the test room through the raw /http/matrix/ proxy endpoint.
	agent := registerPrincipal(t, "test/struct-api", "struct-api-password")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:      agent,
			MatrixPolicy: map[string]any{"allow_join": true},
		}},
	})

	agentHTTP := proxyHTTPClient(proxySockets[agent.Localpart])

	// Create a room with an alias for alias-resolution tests, and with
	// state_default=0 so the agent can publish state events. In production
	// Bureau sets up workspace rooms with appropriate power levels; here
	// we lower the threshold so any room member can set state.
	roomAlias := "struct-api-test"
	testRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "Structured API Test Room",
		Alias:  roomAlias,
		Preset: "private_chat",
		Invite: []string{agent.UserID},
		PowerLevelContentOverride: map[string]any{
			"state_default": 0,
		},
	})
	if err != nil {
		t.Fatalf("create test room: %v", err)
	}
	testRoomID := testRoom.RoomID
	fullAlias := "#" + roomAlias + ":" + testServerName
	t.Logf("created test room %s (%s)", testRoomID, fullAlias)

	// Agent joins through the raw proxy endpoint (requires AllowJoin policy).
	proxyJoinRoom(t, agentHTTP, testRoomID)

	// --- Sub-test: GET /v1/identity ---
	t.Run("Identity", func(t *testing.T) {
		response, err := agentHTTP.Get("http://proxy/v1/identity")
		if err != nil {
			t.Fatalf("GET /v1/identity: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET /v1/identity status = %d: %s", response.StatusCode, body)
		}

		var identity struct {
			UserID     string `json:"user_id"`
			ServerName string `json:"server_name"`
		}
		if err := json.NewDecoder(response.Body).Decode(&identity); err != nil {
			t.Fatalf("decode identity: %v", err)
		}

		if identity.UserID != agent.UserID {
			t.Errorf("identity user_id = %q, want %q", identity.UserID, agent.UserID)
		}
		if identity.ServerName != testServerName {
			t.Errorf("identity server_name = %q, want %q", identity.ServerName, testServerName)
		}
	})

	// --- Sub-test: GET /v1/matrix/resolve ---
	t.Run("ResolveAlias", func(t *testing.T) {
		requestURL := "http://proxy/v1/matrix/resolve?alias=" + url.QueryEscape(fullAlias)
		response, err := agentHTTP.Get(requestURL)
		if err != nil {
			t.Fatalf("GET /v1/matrix/resolve: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET /v1/matrix/resolve status = %d: %s", response.StatusCode, body)
		}

		var result struct {
			RoomID string `json:"room_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode resolve response: %v", err)
		}

		if result.RoomID != testRoomID {
			t.Errorf("resolved room_id = %q, want %q", result.RoomID, testRoomID)
		}
	})

	// --- Sub-test: POST /v1/matrix/state ---
	const testEventType = "m.bureau.test"
	const testStateKey = "integration"
	testStateContent := map[string]any{
		"description": "structured API integration test",
		"timestamp":   time.Now().Unix(),
	}

	t.Run("PublishState", func(t *testing.T) {
		requestBody := map[string]any{
			"room":       testRoomID,
			"event_type": testEventType,
			"state_key":  testStateKey,
			"content":    testStateContent,
		}
		bodyBytes, _ := json.Marshal(requestBody)

		response, err := agentHTTP.Post("http://proxy/v1/matrix/state",
			"application/json", strings.NewReader(string(bodyBytes)))
		if err != nil {
			t.Fatalf("POST /v1/matrix/state: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST /v1/matrix/state status = %d: %s", response.StatusCode, body)
		}

		var result struct {
			EventID string `json:"event_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode put state response: %v", err)
		}
		if result.EventID == "" {
			t.Error("POST /v1/matrix/state returned empty event_id")
		}
		t.Logf("published state event: %s", result.EventID)
	})

	// --- Sub-test: GET /v1/matrix/state (by room ID) ---
	// The PUT returned 200, so the state event is persisted. Read it back
	// immediately — no delay needed.
	t.Run("ReadState", func(t *testing.T) {
		requestURL := "http://proxy/v1/matrix/state?" + url.Values{
			"room": {testRoomID},
			"type": {testEventType},
			"key":  {testStateKey},
		}.Encode()

		response, err := agentHTTP.Get(requestURL)
		if err != nil {
			t.Fatalf("GET /v1/matrix/state: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET /v1/matrix/state status = %d: %s", response.StatusCode, body)
		}

		var content map[string]any
		if err := json.NewDecoder(response.Body).Decode(&content); err != nil {
			t.Fatalf("decode get state response: %v", err)
		}

		description, _ := content["description"].(string)
		if description != "structured API integration test" {
			t.Errorf("state content description = %q, want %q",
				description, "structured API integration test")
		}
	})

	// --- Sub-test: GET /v1/matrix/state with room alias ---
	// The structured endpoints support room aliases in the room parameter.
	// The proxy resolves the alias to a room ID before forwarding.
	t.Run("ReadStateByAlias", func(t *testing.T) {
		requestURL := "http://proxy/v1/matrix/state?" + url.Values{
			"room": {fullAlias},
			"type": {testEventType},
			"key":  {testStateKey},
		}.Encode()

		response, err := agentHTTP.Get(requestURL)
		if err != nil {
			t.Fatalf("GET /v1/matrix/state (alias): %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET /v1/matrix/state (alias) status = %d: %s", response.StatusCode, body)
		}

		var content map[string]any
		if err := json.NewDecoder(response.Body).Decode(&content); err != nil {
			t.Fatalf("decode get state (alias) response: %v", err)
		}

		description, _ := content["description"].(string)
		if description != "structured API integration test" {
			t.Errorf("state content via alias description = %q, want %q",
				description, "structured API integration test")
		}
	})

	// --- Sub-test: POST /v1/matrix/state with room alias ---
	t.Run("PublishStateByAlias", func(t *testing.T) {
		requestBody := map[string]any{
			"room":       fullAlias,
			"event_type": testEventType,
			"state_key":  "alias-test",
			"content": map[string]any{
				"published_via": "alias",
			},
		}
		bodyBytes, _ := json.Marshal(requestBody)

		response, err := agentHTTP.Post("http://proxy/v1/matrix/state",
			"application/json", strings.NewReader(string(bodyBytes)))
		if err != nil {
			t.Fatalf("POST /v1/matrix/state (alias): %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST /v1/matrix/state (alias) status = %d: %s", response.StatusCode, body)
		}

		var result struct {
			EventID string `json:"event_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode put state (alias) response: %v", err)
		}
		if result.EventID == "" {
			t.Error("POST /v1/matrix/state (alias) returned empty event_id")
		}

		// Read it back by room ID to confirm the alias resolved correctly.
		readURL := "http://proxy/v1/matrix/state?" + url.Values{
			"room": {testRoomID},
			"type": {testEventType},
			"key":  {"alias-test"},
		}.Encode()
		readResponse, err := agentHTTP.Get(readURL)
		if err != nil {
			t.Fatalf("read back state published via alias: %v", err)
		}
		defer readResponse.Body.Close()

		if readResponse.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(readResponse.Body)
			t.Fatalf("read back status = %d: %s", readResponse.StatusCode, body)
		}

		var readContent map[string]any
		if err := json.NewDecoder(readResponse.Body).Decode(&readContent); err != nil {
			t.Fatalf("decode read back response: %v", err)
		}
		publishedVia, _ := readContent["published_via"].(string)
		if publishedVia != "alias" {
			t.Errorf("read back published_via = %q, want %q", publishedVia, "alias")
		}
	})

	// --- Sub-test: POST /v1/matrix/message ---
	messageBody := "Hello from structured API at " + time.Now().Format(time.RFC3339Nano)

	t.Run("SendMessage", func(t *testing.T) {
		requestBody := map[string]any{
			"room": testRoomID,
			"content": map[string]any{
				"msgtype": "m.text",
				"body":    messageBody,
			},
		}
		bodyBytes, _ := json.Marshal(requestBody)

		response, err := agentHTTP.Post("http://proxy/v1/matrix/message",
			"application/json", strings.NewReader(string(bodyBytes)))
		if err != nil {
			t.Fatalf("POST /v1/matrix/message: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST /v1/matrix/message status = %d: %s", response.StatusCode, body)
		}

		var result struct {
			EventID string `json:"event_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode send message response: %v", err)
		}
		if result.EventID == "" {
			t.Error("POST /v1/matrix/message returned empty event_id")
		}
		t.Logf("sent message: %s", result.EventID)
	})

	// --- Sub-test: POST /v1/matrix/message with room alias ---
	aliasMessageBody := "Hello via alias at " + time.Now().Format(time.RFC3339Nano)

	t.Run("SendMessageByAlias", func(t *testing.T) {
		requestBody := map[string]any{
			"room": fullAlias,
			"content": map[string]any{
				"msgtype": "m.text",
				"body":    aliasMessageBody,
			},
		}
		bodyBytes, _ := json.Marshal(requestBody)

		response, err := agentHTTP.Post("http://proxy/v1/matrix/message",
			"application/json", strings.NewReader(string(bodyBytes)))
		if err != nil {
			t.Fatalf("POST /v1/matrix/message (alias): %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("POST /v1/matrix/message (alias) status = %d: %s", response.StatusCode, body)
		}

		var result struct {
			EventID string `json:"event_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode send message (alias) response: %v", err)
		}
		if result.EventID == "" {
			t.Error("POST /v1/matrix/message (alias) returned empty event_id")
		}
	})

	// --- Sub-test: verify messages visible to admin ---
	// The sends returned 200 with event IDs, so the events are persisted.
	// Read immediately — no delay needed.
	t.Run("VerifyMessages", func(t *testing.T) {
		response, err := admin.RoomMessages(ctx, testRoomID, messaging.RoomMessagesOptions{
			Direction: "b",
			Limit:     50,
		})
		if err != nil {
			t.Fatalf("admin RoomMessages: %v", err)
		}

		var foundDirect, foundAlias bool
		for _, event := range response.Chunk {
			if event.Type != "m.room.message" || event.Sender != agent.UserID {
				continue
			}
			body, _ := event.Content["body"].(string)
			if body == messageBody {
				foundDirect = true
			}
			if body == aliasMessageBody {
				foundAlias = true
			}
		}

		if !foundDirect {
			t.Error("message sent via room ID not found in room timeline")
		}
		if !foundAlias {
			t.Error("message sent via room alias not found in room timeline")
		}
	})

	t.Log("structured agent API verified: identity → resolve → state (pub/read) → message, with alias resolution")
}
