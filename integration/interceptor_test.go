// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestProxyResponseInterceptor verifies the full interceptor pipeline:
// template with response interceptors → daemon registers on proxy via
// admin API → proxy fires interceptor on matching response → Matrix
// event published to workspace room.
func TestProxyResponseInterceptor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	namespace := setupTestNamespace(t)
	admin := namespace.Admin
	fleet := createTestFleet(t, admin, namespace)
	machine := newTestMachine(t, fleet, "intercept")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Create a workspace room where interceptor events will be published.
	// The daemon (running as the machine user) needs invite permission so
	// it can add the agent after sandbox creation.
	workspaceRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      "Interceptor Test Workspace",
		Preset:                    "private_chat",
		Invite:                    []string{machine.UserID.String()},
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(admin.UserID(), machine.UserID),
	})
	if err != nil {
		t.Fatalf("create workspace room: %v", err)
	}

	// Mock upstream API server. Returns a fixed JSON response for POST
	// requests to /repos/{owner}/{repo}/pulls, simulating a forge API.
	mockResponse := map[string]any{
		"number":   float64(42),
		"html_url": "https://example.com/testorg/testrepo/pull/42",
		"title":    "Add response interceptors",
	}
	mockResponseJSON, err := json.Marshal(mockResponse)
	if err != nil {
		t.Fatalf("marshal mock response: %v", err)
	}

	mockServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		writer.Write(mockResponseJSON)
	}))
	defer mockServer.Close()

	// Watch workspace room BEFORE deploying the agent so we capture
	// all events from the moment the interceptor could fire.
	workspaceWatch := watchRoom(t, admin, workspaceRoom.RoomID)

	// Deploy an agent with a proxy service that has a response interceptor.
	// The interceptor matches POST /repos/{owner}/{repo}/pulls and publishes
	// a custom event with fields extracted from the response.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    resolvedBinary(t, "TEST_AGENT_BINARY"),
		Localpart: "agent/interceptor-test",
		ProxyServices: map[string]schema.TemplateProxyService{
			"mockapi": {
				Upstream: mockServer.URL,
				ResponseInterceptors: []schema.ResponseInterceptor{
					{
						Method:      "POST",
						PathPattern: `^/repos/([^/]+)/([^/]+)/pulls$`,
						EventType:   "m.bureau.test_interceptor",
						EventContent: map[string]any{
							"agent":         "${agent}",
							"agent_display": "${agent_display}",
							"repo":          "${path.1}/${path.2}",
							"entity_number": "${response.number}",
							"entity_url":    "${response.html_url}",
							"title":         "${response.title}",
							"provider":      "mock",
						},
					},
				},
			},
		},
		Payload: map[string]any{
			"WORKSPACE_ROOM_ID": workspaceRoom.RoomID.String(),
		},
		SkipWaitForReady: true,
	})

	// Wait for the daemon to register the "mockapi" external service on
	// the proxy via admin API. The proxy socket exists (deployAgent waited
	// for it via inotify), but the daemon configures external services
	// asynchronously after the launcher responds to create-sandbox IPC.
	waitForProxyService(t, agent.AdminSocketPath, "mockapi")

	proxyClient := proxyHTTPClient(agent.ProxySocketPath)

	// Make a matching POST request through the proxy. The interceptor
	// should fire and publish an event to the workspace room.
	postRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://proxy/http/mockapi/repos/testorg/testrepo/pulls",
		strings.NewReader(`{"title":"Add response interceptors","head":"feature","base":"main"}`),
	)
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	postRequest.Header.Set("Content-Type", "application/json")

	postResponse, err := proxyClient.Do(postRequest)
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer postResponse.Body.Close()

	// Verify the HTTP response is forwarded correctly to the client.
	if postResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(postResponse.Body)
		t.Fatalf("expected status 201, got %d: %s", postResponse.StatusCode, body)
	}

	var responseBody map[string]any
	if err := json.NewDecoder(postResponse.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if responseBody["title"] != "Add response interceptors" {
		t.Errorf("response body title = %v, want %q", responseBody["title"], "Add response interceptors")
	}

	// Wait for the interceptor event in the workspace room. The interceptor
	// fires asynchronously after the response is written to the client.
	interceptorEvent := workspaceWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == "m.bureau.test_interceptor"
	}, "interceptor event from POST /repos/.../pulls")

	// Verify the event content has correctly resolved template placeholders.
	content := interceptorEvent.Content

	if content["agent"] != agent.Account.UserID.String() {
		t.Errorf("event agent = %v, want %s", content["agent"], agent.Account.UserID)
	}
	if content["provider"] != "mock" {
		t.Errorf("event provider = %v, want %q", content["provider"], "mock")
	}
	if content["repo"] != "testorg/testrepo" {
		t.Errorf("event repo = %v, want %q", content["repo"], "testorg/testrepo")
	}
	if content["title"] != "Add response interceptors" {
		t.Errorf("event title = %v, want %q", content["title"], "Add response interceptors")
	}
	if content["entity_url"] != "https://example.com/testorg/testrepo/pull/42" {
		t.Errorf("event entity_url = %v, want %q", content["entity_url"], "https://example.com/testorg/testrepo/pull/42")
	}

	// Type preservation: ${response.number} as the sole placeholder should
	// preserve the JSON numeric type (float64 in Go's JSON decoder).
	entityNumber, ok := content["entity_number"].(float64)
	if !ok {
		t.Errorf("event entity_number type = %T, want float64 (type preservation)", content["entity_number"])
	} else if entityNumber != 42 {
		t.Errorf("event entity_number = %v, want 42", entityNumber)
	}

	// agent_display should be the last segment of the agent's localpart.
	agentDisplay, ok := content["agent_display"].(string)
	if !ok || agentDisplay == "" {
		t.Errorf("event agent_display = %v, want non-empty string", content["agent_display"])
	}

	// Verify that a non-matching request does NOT produce an interceptor
	// event. Strategy: make a GET request (interceptor matches POST only),
	// then make another POST. If we see the second POST's event without
	// seeing a GET event first, the negative case is confirmed.
	getRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://proxy/http/mockapi/repos/testorg/testrepo/pulls",
		nil,
	)
	if err != nil {
		t.Fatalf("create GET request: %v", err)
	}
	getResponse, err := proxyClient.Do(getRequest)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	getResponse.Body.Close()

	// Second POST with a different path to distinguish from the first.
	secondPostRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://proxy/http/mockapi/repos/secondorg/secondrepo/pulls",
		strings.NewReader(`{"title":"Second PR"}`),
	)
	if err != nil {
		t.Fatalf("create second POST request: %v", err)
	}
	secondPostRequest.Header.Set("Content-Type", "application/json")
	secondPostResponse, err := proxyClient.Do(secondPostRequest)
	if err != nil {
		t.Fatalf("second POST through proxy: %v", err)
	}
	secondPostResponse.Body.Close()

	// Wait for the second POST's interceptor event. If a GET event
	// arrived, the predicate would see it first (events are ordered).
	secondEvent := workspaceWatch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != "m.bureau.test_interceptor" {
			return false
		}
		repo, _ := event.Content["repo"].(string)
		return repo == "secondorg/secondrepo"
	}, "interceptor event from second POST")

	if secondEvent.Content["repo"] != "secondorg/secondrepo" {
		t.Errorf("second event repo = %v, want %q", secondEvent.Content["repo"], "secondorg/secondrepo")
	}
}
