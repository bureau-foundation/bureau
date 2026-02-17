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

	template "github.com/bureau-foundation/bureau/lib/template"
)

// TestNativeAgentEndToEnd exercises the bureau-agent binary running in a
// real Bureau sandbox with a mock Anthropic API. It proves the full vertical
// slice: Matrix message delivery → LLM call through proxy HTTP passthrough
// → MCP tool execution (bureau_matrix_send) → tool posts message to Matrix
// via the proxy-routed client → tool result fed back to LLM.
//
// The mock Anthropic server returns a two-request sequence: first a
// tool_use response requesting bureau_matrix_send, then a text response
// after validating the tool_result was included in the conversation.
//
// Determinism is guaranteed by three production improvements:
//   - No payload prompt: the agent loop waits for a Matrix message before
//     calling the LLM, so the mock is registered before any LLM traffic.
//   - agent-ready after pump: the message pump captures its /sync position
//     before "agent-ready" is sent, so messages sent after agent-ready are
//     guaranteed to be delivered.
//   - Admin socket registration: the mock upstream is registered on the
//     proxy's admin socket (same mechanism the daemon uses).
func TestNativeAgentEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := createFleetRoom(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, "machine/native-agent-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Publish a template for bureau-agent. No payload prompt — the agent
	// waits for a Matrix message before calling the LLM.
	agentBinary := testutil.DataBinary(t, "BUREAU_AGENT_BINARY")
	grantTemplateAccess(t, admin, machine)

	ref, err := schema.ParseTemplateRef("bureau/template:native-agent-test")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = template.Push(ctx, admin, ref, schema.TemplateContent{
		Description: "Native agent for end-to-end testing",
		Command:     []string{agentBinary},
		Namespaces: &schema.TemplateNamespaces{
			PID: true,
		},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Source: agentBinary, Dest: agentBinary, Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME":                    "/workspace",
			"TERM":                    "xterm-256color",
			"BUREAU_PROXY_SOCKET":     "${PROXY_SOCKET}",
			"BUREAU_MACHINE_NAME":     "${MACHINE_NAME}",
			"BUREAU_SERVER_NAME":      "${SERVER_NAME}",
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
	}, testServerName)
	if err != nil {
		t.Fatalf("push native-agent template: %v", err)
	}

	// Register the agent principal with grants for MCP tool execution.
	agent := registerPrincipal(t, "agent/native-e2e-test", "test-password")
	pushCredentials(t, admin, machine, agent)

	// The agent sends messages to the config room (via bureau_matrix_send
	// through the proxy). Set up membership before the sandbox starts.
	if err := admin.InviteUser(ctx, machine.ConfigRoomID, agent.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite agent to config room: %v", err)
		}
	}
	agentSession := principalSession(t, agent)
	if _, err := agentSession.JoinRoom(ctx, machine.ConfigRoomID); err != nil {
		t.Fatalf("agent join config room: %v", err)
	}
	agentSession.Close()

	// Set up room watch BEFORE deploying so we don't miss agent-ready.
	readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Deploy with command/** grants so the agent can execute MCP tools.
	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: "bureau/template:native-agent-test",
			Authorization: &schema.AuthorizationPolicy{
				Grants: []schema.Grant{
					{Actions: []string{"command/**"}},
				},
			},
		}},
	})

	// Wait for proxy socket (sandbox created, proxy running).
	proxySocketPath := machine.PrincipalSocketPath(agent.Localpart)
	waitForFile(t, proxySocketPath)
	t.Logf("proxy socket appeared: %s", proxySocketPath)

	// Wait for "agent-ready". This means:
	// - Proxy is running (BuildContext succeeded)
	// - Message pump has captured its /sync position
	// - Any message sent now will be delivered to the agent
	readyWatch.WaitForMessage(t, "agent-ready", agent.UserID)
	t.Log("agent sent ready signal — pump is listening")

	// Start the mock Anthropic server AFTER agent-ready (proxy is confirmed
	// running) and BEFORE sending the Matrix message (which triggers the
	// first LLM call). The mock handles two requests:
	// 1. Tool use: returns a tool_use block for "bureau_matrix_send".
	// 2. Text response: validates tool_result, returns text, signals done.
	mock := newMockAnthropicToolSequence(t, machine.ConfigRoomID)

	// Register the mock on the proxy's admin socket.
	adminSocketPath := machine.PrincipalAdminSocketPath(agent.Localpart)
	registerProxyHTTPService(t, adminSocketPath, "anthropic", mock.URL)
	t.Logf("mock Anthropic registered on admin socket: %s", adminSocketPath)

	// Send a Matrix message to the agent. This is the first prompt:
	// the agent loop picks it up from stdin and calls the LLM.
	responseWatch := watchRoom(t, admin, machine.ConfigRoomID)
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTextMessage("Echo hello for me")); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	// Wait for the bureau_matrix_send tool to post its message. The flow:
	// 1. Pump delivers "Echo hello for me" to agent stdin
	// 2. Agent loop calls LLM → mock returns tool_use for bureau_matrix_send
	// 3. Agent executes bureau_matrix_send via MCP → tool sends message to
	//    Matrix through the proxy-routed client (zero credentials in sandbox)
	// 4. This assertion catches the message from step 3
	responseWatch.WaitForMessage(t, "echo result: hello", agent.UserID)
	t.Log("bureau_matrix_send tool posted message — proxy pipeline verified")

	// Wait for the mock to confirm the second LLM call completed. This
	// ensures the tool_result was fed back to the LLM (the mock validates
	// the tool_result is present in the conversation on request 2).
	select {
	case <-mock.AllRequestsHandled:
		t.Log("all LLM requests handled — full tool→result→LLM cycle verified")
	case <-ctx.Done():
		t.Fatal("timed out waiting for all LLM requests to complete")
	}

	// The agent loop is now blocking on waitForMessage() for the next
	// stdin message (the agent is long-running). The test returns here
	// and t.Cleanup tears down the launcher, which kills the sandbox.
}

// mockAnthropicServer wraps an httptest.Server with a synchronization
// channel that signals when all expected LLM requests have been handled.
type mockAnthropicServer struct {
	*httptest.Server

	// AllRequestsHandled is closed when the final expected request has
	// been fully processed. Waiting on this ensures the complete
	// tool→result→LLM cycle has finished before test teardown.
	AllRequestsHandled <-chan struct{}
}

// newMockAnthropicToolSequence creates a mock Anthropic Messages API
// server that handles a two-request sequence:
//
//  1. Tool use: returns a tool_use block for "bureau_matrix_send" with
//     input {"room": configRoomID, "message": "echo result: hello"}.
//  2. Text response: validates that the conversation contains a
//     tool_result for the tool call, returns "I echoed hello for you",
//     then signals AllRequestsHandled.
//
// The configRoomID parameter is injected into the tool_use input so the
// tool sends its message to the correct Matrix room.
func newMockAnthropicToolSequence(t *testing.T, configRoomID string) *mockAnthropicServer {
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

		// Validate request body is well-formed.
		var wireRequest struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id,omitempty"`
					Content   string `json:"content,omitempty"`
				} `json:"content"`
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
			// Request 1: return a tool_use for "bureau_matrix_send".
			// The input includes the actual config room ID so the tool
			// sends the message to the right room through the proxy.
			toolInput, _ := json.Marshal(map[string]string{
				"room":    configRoomID,
				"message": "echo result: hello",
			})
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  150,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(`{"type":"tool_use","id":"tc_mock_01","name":"bureau_matrix_send","input":%s}`, toolInput)),
				},
			})

		case 1:
			// Request 2: validate the conversation contains a tool_result
			// for tc_mock_01, then return a text response.
			hasToolResult := false
			for _, message := range wireRequest.Messages {
				for _, block := range message.Content {
					if block.Type == "tool_result" && block.ToolUseID == "tc_mock_01" {
						hasToolResult = true
					}
				}
			}
			if !hasToolResult {
				t.Error("mock: request 2 missing tool_result for tc_mock_01")
			}

			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "end_turn",
				InputTokens:  200,
				OutputTokens: 15,
				Content: []json.RawMessage{
					json.RawMessage(`{"type":"text","text":"I echoed hello for you"}`),
				},
			})

			// Signal that all expected requests have been handled.
			close(allDone)

		default:
			t.Errorf("mock: unexpected request %d", current+1)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	t.Cleanup(server.Close)
	return &mockAnthropicServer{
		Server:             server,
		AllRequestsHandled: allDone,
	}
}
