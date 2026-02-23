// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestMCPServer verifies that MCP tool authorization works end-to-end when
// bureau-agent runs inside a real sandbox with daemon-resolved grants.
//
// The agent is deployed with a narrow grant: only command/matrix/send.
// A mock LLM drives the agent through three requests:
//
//  1. Return tool_use for bureau_matrix_send (authorized) — the agent
//     executes the tool, which posts a message to Matrix through the proxy.
//  2. Validate the tool_result, then return tool_use for bureau_template_list
//     (unauthorized — agent has no command/template/list grant).
//  3. Validate the tool_result contains "not authorized" (from the MCP
//     server's CallTool rejecting the unauthorized tool), return text.
//
// Verification flows through two paths:
//   - Matrix: admin watches for the message posted by the authorized tool
//   - Mock: validates the unauthorized tool's error content in the wire protocol
//
// This proves: grant flow from daemon → proxy → MCP server, authorized
// tool execution through the proxy, and unauthorized tool rejection — all
// from inside the sandbox where production agents run.
func TestMCPServer(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "mcp")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy bureau-agent with only command/matrix/send granted.
	// The daemon resolves this into grants and pushes them to the proxy,
	// where the MCP server reads them via GET /v1/grants.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
		Localpart: "agent/mcp-auth-test",
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{"command/matrix/send"}},
			},
		},
	})

	// Start the mock Anthropic server. The mock drives a 3-request sequence:
	// authorized tool → unauthorized tool → validate error.
	mock := newMockMCPAuthSequence(t, machine.ConfigRoomID)
	registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.Server.URL)

	// Watch for the authorized tool's Matrix message before sending the prompt.
	responseWatch := watchRoom(t, admin, machine.ConfigRoomID)
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTargetedTextMessage("Test MCP authorization", agent.Account.UserID)); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	// Wait for the bureau_matrix_send tool to post its message. This
	// proves the authorized tool executed through the proxy from inside
	// the sandbox.
	responseWatch.WaitForMessage(t, "mcp-auth-test", agent.Account.UserID)
	t.Log("authorized tool (bureau_matrix_send) posted message via proxy")

	// Wait for the mock to confirm all three LLM requests completed.
	// The mock validated on request 3 that the tool_result for the
	// unauthorized bureau_template_list call contained "not authorized".
	select {
	case <-mock.AllRequestsHandled:
		t.Log("all LLM requests handled — authorized + unauthorized tool cycle verified")
	case <-ctx.Done():
		t.Fatal("timed out waiting for all LLM requests to complete")
	}

	if mock.AuthorizationError() != "" {
		t.Fatal(mock.AuthorizationError())
	}
}

// mockMCPAuthServer wraps an httptest.Server with synchronization for the
// 3-request MCP authorization test sequence.
type mockMCPAuthServer struct {
	Server             *httptest.Server
	AllRequestsHandled <-chan struct{}

	// authorizationError is set if the mock detected a protocol violation
	// (e.g., tool_result for the unauthorized call didn't contain the
	// expected error). Read after AllRequestsHandled is closed.
	authorizationError string
	mutex              sync.Mutex
}

// AuthorizationError returns any validation failure detected by the mock.
// Safe to call after AllRequestsHandled is closed.
func (m *mockMCPAuthServer) AuthorizationError() string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.authorizationError
}

// newMockMCPAuthSequence creates a mock Anthropic Messages API server for
// the MCP authorization test. It handles three requests:
//
//   - Request 0: return tool_use for bureau_matrix_send (authorized).
//   - Request 1: validate tool_result for request 0, return tool_use for
//     bureau_template_list (unauthorized — agent lacks grant).
//   - Request 2: validate tool_result for request 1 contains "not authorized",
//     return text "done", signal AllRequestsHandled.
func newMockMCPAuthSequence(t *testing.T, configRoomID ref.RoomID) *mockMCPAuthServer {
	t.Helper()

	var (
		mutex     sync.Mutex
		callCount int
	)

	allDone := make(chan struct{})
	mock := &mockMCPAuthServer{AllRequestsHandled: allDone}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		current := callCount
		callCount++
		mutex.Unlock()

		// Parse the wire request to validate structure.
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
			// Return tool_use for bureau_matrix_send (authorized).
			toolInput, _ := json.Marshal(map[string]string{
				"room":    configRoomID.String(),
				"message": "mcp-auth-test",
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  150,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"tc_auth_00","name":"bureau_matrix_send","input":%s}`,
						toolInput)),
				},
			})

		case 1:
			// Validate tool_result for tc_auth_00 is present, then
			// return tool_use for bureau_template_list (unauthorized).
			if !hasToolResult(wireRequest.Messages, "tc_auth_00") {
				t.Error("mock: request 1 missing tool_result for tc_auth_00")
			}

			toolInput, _ := json.Marshal(map[string]string{
				"room": "#bureau/template:" + testServerName,
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  200,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"tc_auth_01","name":"bureau_template_list","input":%s}`,
						toolInput)),
				},
			})

		case 2:
			// Validate tool_result for tc_auth_01 contains "not authorized".
			// This proves the MCP server inside the sandbox rejected the
			// unauthorized tool call.
			toolResultContent := findToolResultContent(wireRequest.Messages, "tc_auth_01")
			if toolResultContent == "" {
				t.Error("mock: request 2 missing tool_result for tc_auth_01")
			} else if !strings.Contains(toolResultContent, "not authorized") {
				mock.mutex.Lock()
				mock.authorizationError = fmt.Sprintf(
					"mock: tool_result for unauthorized bureau_template_list should contain 'not authorized', got: %q",
					toolResultContent)
				mock.mutex.Unlock()
			}

			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "end_turn",
				InputTokens:  250,
				OutputTokens: 15,
				Content: []json.RawMessage{
					json.RawMessage(`{"type":"text","text":"Authorization test complete"}`),
				},
			})
			close(allDone)

		default:
			t.Errorf("mock: unexpected request %d", current)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	t.Cleanup(server.Close)
	mock.Server = server
	return mock
}

// findToolResultContent searches the message history for a tool_result
// with the given tool_use_id and returns its content string. Returns
// empty string if not found.
func findToolResultContent(messages []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}, toolUseID string) string {
	for _, message := range messages {
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return block.Content
			}
		}
	}
	return ""
}
