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
	templatedef "github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestExtraInherits verifies deployment-time template mix-in composition:
// a principal's main template has no proxy services, but ExtraInherits
// references a separate template that provides a proxy service with a
// response interceptor. The daemon resolves and merges the extra template,
// the proxy registers the service, and the interceptor fires correctly.
//
// This validates the full ExtraInherits pipeline: PrincipalAssignment
// wire format → daemon resolveExtraInherits → templatedef.Merge →
// proxy admin API → interceptor → Matrix event.
func TestExtraInherits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	namespace := setupTestNamespace(t)
	admin := namespace.Admin
	fleet := createTestFleet(t, admin, namespace)
	machine := newTestMachine(t, fleet, "extrainh")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Create a workspace room for interceptor events.
	workspaceRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      "ExtraInherits Test Workspace",
		Preset:                    "private_chat",
		Invite:                    []string{machine.UserID.String()},
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(admin.UserID(), machine.UserID),
	})
	if err != nil {
		t.Fatalf("create workspace room: %v", err)
	}

	// Mock upstream that returns a JSON response simulating a GitHub PR.
	mockResponse := map[string]any{
		"number":   float64(99),
		"html_url": "https://example.com/org/repo/pull/99",
		"title":    "ExtraInherits PR",
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

	// Publish the mix-in template: a proxy service with interceptor,
	// no command, no filesystem — a pure capability fragment.
	grantTemplateAccess(t, admin, machine)

	mixinTemplateRef, err := schema.ParseTemplateRef(
		namespace.Namespace.TemplateRoomAliasLocalpart() + ":extra-api-mixin")
	if err != nil {
		t.Fatalf("parse mixin template ref: %v", err)
	}
	_, err = templatedef.Push(ctx, admin, mixinTemplateRef, schema.TemplateContent{
		Description: "API proxy mix-in with response interceptor",
		ProxyServices: map[string]schema.TemplateProxyService{
			"mockforge": {
				Upstream: mockServer.URL,
				ResponseInterceptors: []schema.ResponseInterceptor{
					{
						Method:      "POST",
						PathPattern: `^/repos/([^/]+)/([^/]+)/pulls$`,
						EventType:   "m.bureau.test_extra_inherits",
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
	}, testServer)
	if err != nil {
		t.Fatalf("push mixin template: %v", err)
	}

	// Watch workspace room BEFORE deploying the agent.
	workspaceWatch := watchRoom(t, admin, workspaceRoom.RoomID)

	// Deploy an agent whose main template has NO proxy services,
	// but whose PrincipalAssignment includes ExtraInherits pointing
	// at the mix-in template.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    resolvedBinary(t, "TEST_AGENT_BINARY"),
		Localpart: "agent/extra-inherits-test",
		ExtraInherits: []string{
			mixinTemplateRef.String(),
		},
		Payload: map[string]any{
			"WORKSPACE_ROOM_ID": workspaceRoom.RoomID.String(),
		},
		SkipWaitForReady: true,
	})

	// Wait for the daemon to register the "mockforge" proxy service.
	// This service comes from the extra-inherited template, not the
	// main template — proving that ExtraInherits merge works.
	waitForProxyService(t, agent.AdminSocketPath, "mockforge")

	proxyClient := proxyHTTPClient(agent.ProxySocketPath)

	// POST through the proxy — the interceptor should fire.
	postRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://proxy/http/mockforge/repos/testorg/testrepo/pulls",
		strings.NewReader(`{"title":"ExtraInherits PR","head":"feature","base":"main"}`),
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

	if postResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(postResponse.Body)
		t.Fatalf("expected status 201, got %d: %s", postResponse.StatusCode, body)
	}

	var responseBody map[string]any
	if err := json.NewDecoder(postResponse.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if responseBody["title"] != "ExtraInherits PR" {
		t.Errorf("response body title = %v, want %q", responseBody["title"], "ExtraInherits PR")
	}

	// Wait for the interceptor event in the workspace room.
	interceptorEvent := workspaceWatch.WaitForEvent(t, func(event messaging.Event) bool {
		return event.Type == "m.bureau.test_extra_inherits"
	}, "interceptor event from ExtraInherits proxy service")

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
	if content["title"] != "ExtraInherits PR" {
		t.Errorf("event title = %v, want %q", content["title"], "ExtraInherits PR")
	}
	if content["entity_url"] != "https://example.com/org/repo/pull/99" {
		t.Errorf("event entity_url = %v, want %q", content["entity_url"], "https://example.com/org/repo/pull/99")
	}

	// Type preservation: entity_number should be float64(99).
	entityNumber, ok := content["entity_number"].(float64)
	if !ok {
		t.Errorf("event entity_number type = %T, want float64", content["entity_number"])
	} else if entityNumber != 99 {
		t.Errorf("event entity_number = %v, want 99", entityNumber)
	}

	agentDisplay, ok := content["agent_display"].(string)
	if !ok || agentDisplay == "" {
		t.Errorf("event agent_display = %v, want non-empty string", content["agent_display"])
	}
}
