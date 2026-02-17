// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestStructuredAgentAPI exercises the proxy's /v1/ structured endpoints
// when called by bureau-agent via MCP tools from inside a real sandbox.
// This proves the full agent → MCP → proxyclient → proxy → Matrix path
// that production agents use.
//
// The test deploys a bureau-agent with grants for state/set, state/get,
// and message send, then drives it through three chained tool calls in
// a single LLM turn:
//
//  1. bureau_matrix_state_set — publish a custom state event to the test room
//  2. bureau_matrix_state_get — read the state event back using a room alias
//     (proves alias resolution through proxy/proxyclient)
//  3. bureau_matrix_send — send a message using a room alias
//
// All three tool calls happen within one agent turn: the mock returns
// tool_use after each tool_result (not end_turn text), so the agent
// executes all tools before the conversation turn ends. This matches
// how real LLMs chain tool calls.
//
// Verification reads Matrix state events and messages from the admin
// session, confirming credential injection, state write, alias
// resolution, and message delivery.
func TestStructuredAgentAPI(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := createFleetRoom(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, "machine/struct-api")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Deploy bureau-agent with grants for state operations and message send.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
		Localpart: "agent/struct-api-test",
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{
					"command/matrix/send",
					"command/matrix/state/set",
					"command/matrix/state/get",
				}},
			},
		},
	})

	// Create a test room with an alias for alias-resolution tests. Set
	// state_default=0 so the agent (a regular member) can publish state
	// events. In production, Bureau configures workspace rooms with
	// appropriate power levels; here we lower the threshold.
	roomAlias := "struct-api-test"
	testRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "Structured API Test Room",
		Alias:  roomAlias,
		Preset: "private_chat",
		Invite: []string{agent.Account.UserID},
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

	// Join the agent to the test room so it can publish state and send
	// messages. This uses a direct session (the agent is already registered
	// and has a token from deployAgent).
	joinConfigRoom(t, admin, testRoomID, agent.Account)

	// Set up the mock LLM that chains three tool calls in one turn.
	stateContent := `{"source":"agent","verified":true}`
	messageBody := testutil.UniqueID("structured-api-test")

	mock := newMockStructuredAPISequence(t, testRoomID, fullAlias, stateContent, messageBody)
	registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.Server.URL)

	// Send the prompt that triggers the agent loop.
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTextMessage("Test structured API")); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	// Wait for all mock LLM requests to complete.
	select {
	case <-mock.AllRequestsHandled:
		t.Log("all LLM requests handled — tool chain completed")
	case <-ctx.Done():
		t.Fatal("timed out waiting for LLM requests to complete")
	}

	// Verify the state event was published by the agent through the proxy.
	var stateEventContent map[string]any
	rawContent, err := admin.GetStateEvent(ctx, testRoomID, "m.bureau.test", "integration")
	if err != nil {
		t.Fatalf("read state event: %v", err)
	}
	if err := json.Unmarshal(rawContent, &stateEventContent); err != nil {
		t.Fatalf("decode state event content: %v", err)
	}

	source, _ := stateEventContent["source"].(string)
	if source != "agent" {
		t.Errorf("state event source = %q, want %q", source, "agent")
	}
	verified, _ := stateEventContent["verified"].(bool)
	if !verified {
		t.Errorf("state event verified = %v, want true", verified)
	}
	t.Log("state event verified: agent published via proxy, read back via alias")

	// Verify the message was sent by the agent through the proxy.
	response, err := admin.RoomMessages(ctx, testRoomID, messaging.RoomMessagesOptions{
		Direction: "b",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("admin RoomMessages: %v", err)
	}

	var foundMessage bool
	for _, event := range response.Chunk {
		if event.Type != schema.MatrixEventTypeMessage || event.Sender != agent.Account.UserID {
			continue
		}
		body, _ := event.Content["body"].(string)
		if body == messageBody {
			foundMessage = true
			break
		}
	}
	if !foundMessage {
		t.Error("message sent by agent via alias not found in room timeline")
	}

	t.Log("structured agent API verified: state set → state get (alias) → message send (alias)")
}

// mockStructuredAPIServer wraps an httptest.Server for the structured API
// test's chained tool sequence.
type mockStructuredAPIServer struct {
	Server             *httptest.Server
	AllRequestsHandled <-chan struct{}
}

// newMockStructuredAPISequence creates a mock Anthropic Messages API server
// that chains three tool calls in a single conversation turn:
//
//   - Request 0: return tool_use for bureau_matrix_state_set
//   - Request 1: validate tool_result, return tool_use for bureau_matrix_state_get
//   - Request 2: validate tool_result, return tool_use for bureau_matrix_send
//   - Request 3: validate tool_result, return text, signal done
//
// Each odd request returns another tool_use (not end_turn text), keeping
// the agent in the same conversation turn. Only the final request returns
// end_turn text, which completes the turn.
func newMockStructuredAPISequence(t *testing.T, testRoomID, fullAlias, stateContent, messageBody string) *mockStructuredAPIServer {
	t.Helper()

	var (
		mutex     sync.Mutex
		callCount int
	)

	allDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		current := callCount
		callCount++
		mutex.Unlock()

		var wireRequest struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&wireRequest); err != nil {
			http.Error(writer, "bad request body", http.StatusBadRequest)
			return
		}
		if !wireRequest.Stream {
			http.Error(writer, "expected stream=true", http.StatusBadRequest)
			return
		}

		switch current {
		case 0:
			// Initial prompt — return tool_use for bureau_matrix_state_set.
			toolInput, _ := json.Marshal(map[string]any{
				"room":       testRoomID,
				"event_type": "m.bureau.test",
				"state_key":  "integration",
				"body":       stateContent,
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  150,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"tc_api_00","name":"bureau_matrix_state_set","input":%s}`,
						toolInput)),
				},
			})

		case 1:
			// state_set tool_result — return tool_use for bureau_matrix_state_get.
			if !hasToolResult(wireRequest.Messages, "tc_api_00") {
				t.Error("mock: request 1 missing tool_result for tc_api_00")
			}
			toolInput, _ := json.Marshal(map[string]any{
				"room":       fullAlias,
				"event_type": "m.bureau.test",
				"state_key":  "integration",
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  200,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"tc_api_01","name":"bureau_matrix_state_get","input":%s}`,
						toolInput)),
				},
			})

		case 2:
			// state_get tool_result — return tool_use for bureau_matrix_send.
			if !hasToolResult(wireRequest.Messages, "tc_api_01") {
				t.Error("mock: request 2 missing tool_result for tc_api_01")
			}
			toolInput, _ := json.Marshal(map[string]any{
				"room":    fullAlias,
				"message": messageBody,
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  250,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"tc_api_02","name":"bureau_matrix_send","input":%s}`,
						toolInput)),
				},
			})

		case 3:
			// send tool_result — return text to complete the turn.
			if !hasToolResult(wireRequest.Messages, "tc_api_02") {
				t.Error("mock: request 3 missing tool_result for tc_api_02")
			}
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "end_turn",
				InputTokens:  300,
				OutputTokens: 15,
				Content: []json.RawMessage{
					json.RawMessage(`{"type":"text","text":"All structured API operations completed"}`),
				},
			})
			close(allDone)

		default:
			t.Errorf("mock: unexpected request %d", current)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	t.Cleanup(server.Close)
	return &mockStructuredAPIServer{
		Server:             server,
		AllRequestsHandled: allDone,
	}
}
