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

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
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

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "native-agent-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy bureau-agent with LLM config and MCP tool grants.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
		Localpart: "agent/native-e2e-test",
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":      "mock-model",
			"BUREAU_AGENT_SERVICE":    "anthropic",
			"BUREAU_AGENT_MAX_TOKENS": "1024",
		},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{schema.ActionCommandAll}},
			},
		},
	})

	// Start the mock Anthropic server AFTER agent-ready (proxy is confirmed
	// running) and BEFORE sending the Matrix message (which triggers the
	// first LLM call). The mock handles two requests:
	// 1. Tool use: returns a tool_use block for "bureau_matrix_send".
	// 2. Text response: validates tool_result, returns text, signals done.
	mock := newMockAnthropicToolSequence(t, machine.ConfigRoomID)

	// Register the mock on the proxy's admin socket.
	registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.URL)

	// Send a Matrix message to the agent. This is the first prompt:
	// the agent loop picks it up from stdin and calls the LLM.
	responseWatch := watchRoom(t, admin, machine.ConfigRoomID)
	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTargetedTextMessage("Echo hello for me", agent.Account.UserID)); err != nil {
		t.Fatalf("sending prompt to agent: %v", err)
	}

	// Wait for the bureau_matrix_send tool to post its message. The flow:
	// 1. Pump delivers "Echo hello for me" to agent stdin
	// 2. Agent loop calls LLM → mock returns tool_use for bureau_matrix_send
	// 3. Agent executes bureau_matrix_send via MCP → tool sends message to
	//    Matrix through the proxy-routed client (zero credentials in sandbox)
	// 4. This assertion catches the message from step 3
	responseWatch.WaitForMessage(t, "echo result: hello", agent.Account.UserID)
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

// TestNativeAgentContextTruncation exercises the full production pipeline
// for context truncation. The agent processes 5 turns, each producing one
// bureau_matrix_send tool call. A small context window (BUREAU_AGENT_CONTEXT_WINDOW)
// forces the Truncating context manager to evict older turn groups mid-conversation.
//
// The mock Anthropic server records per-request message counts and signals
// when truncation is observed (message count drops below a prior peak). This
// proves the entire chain:
//   - driver.go constructs the correct token budget from env vars
//   - CharEstimator calibrates from mock input_tokens values via EMA
//   - Truncating manager evicts oldest turn groups when over budget
//   - Conversation structure remains valid after eviction (role alternation,
//     tool_use/tool_result pairing preserved)
//   - Agent continues functioning correctly across truncation boundaries
func TestNativeAgentContextTruncation(t *testing.T) {
	t.Parallel()

	const turnCount = 5

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "native-truncation-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy bureau-agent with a context window that leaves a moderate
	// message budget after the real overhead estimate. Budget math:
	//   overhead ≈ 18000-20000 tokens (full Bureau tool catalog, varies
	//             with number of authorized commands)
	//   messageBudget = contextWindow - maxOutputTokens - overhead
	//                 = 30000 - 1024 - ~19000 ≈ ~10000 tokens
	//
	// The mock reports 1500 input_tokens per message (see
	// newMockMultiTurnToolSequence), so the estimator converges to a
	// ratio where each message costs ~1500 tokens. Truncation fires
	// when estimated tokens exceed the message budget: around 7-9
	// messages (turn 2-3 of 5).
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    testutil.DataBinary(t, "BUREAU_AGENT_BINARY"),
		Localpart: "agent/native-truncation-test",
		ExtraEnv: map[string]string{
			"BUREAU_AGENT_MODEL":          "mock-model",
			"BUREAU_AGENT_SERVICE":        "anthropic",
			"BUREAU_AGENT_MAX_TOKENS":     "1024",
			"BUREAU_AGENT_CONTEXT_WINDOW": "30000",
		},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{schema.ActionCommandAll}},
			},
		},
	})

	// Create the multi-turn mock and register it on the proxy.
	mock := newMockMultiTurnToolSequence(t, machine.ConfigRoomID, turnCount)
	registerProxyHTTPService(t, agent.AdminSocketPath, "anthropic", mock.URL)

	// Send turnCount messages, each triggering a full tool call cycle.
	// Each cycle: admin sends message → LLM returns tool_use → agent
	// executes bureau_matrix_send → LLM returns text → agent waits.
	for turn := 0; turn < turnCount; turn++ {
		responseWatch := watchRoom(t, admin, machine.ConfigRoomID)
		prompt := fmt.Sprintf("Turn %d: echo this", turn)
		if _, err := admin.SendMessage(ctx, machine.ConfigRoomID, messaging.NewTargetedTextMessage(prompt, agent.Account.UserID)); err != nil {
			t.Fatalf("sending prompt for turn %d: %v", turn, err)
		}

		expectedResponse := fmt.Sprintf("response from turn %d", turn)
		responseWatch.WaitForMessage(t, expectedResponse, agent.Account.UserID)
		t.Logf("turn %d: bureau_matrix_send posted message", turn)
	}

	// Wait for all LLM requests to complete.
	select {
	case <-mock.AllRequestsHandled:
		t.Log("all LLM requests handled")
	case <-ctx.Done():
		t.Fatal("timed out waiting for all LLM requests to complete")
	}

	// Verify truncation actually occurred. With a ~10000-token budget
	// and ~1500 tokens per message, the estimated context exceeds the
	// budget by turn 2-3, forcing the Truncating manager to evict
	// middle turn groups.
	select {
	case <-mock.TruncationObserved:
		t.Log("context truncation observed — manager evicted turn groups")
	case <-ctx.Done():
		t.Fatal("timed out waiting for truncation (budget may be too large)")
	}

	// Log the per-request message counts for diagnostic visibility.
	counts := mock.MessageCounts()
	t.Logf("per-request message counts: %v", counts)

	// Verify the message count pattern shows a decrease. Without
	// truncation, counts grow monotonically: 1, 3, 5, 7, 9, 11, ...
	// With truncation, at least one count is lower than a prior count.
	maxSeen := 0
	truncationCount := 0
	for _, count := range counts {
		if count < maxSeen {
			truncationCount++
		}
		if count > maxSeen {
			maxSeen = count
		}
	}
	if truncationCount == 0 {
		t.Errorf("no truncation detected in message counts %v; expected at least one decrease", counts)
	}
	t.Logf("truncation events in message counts: %d", truncationCount)
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
func newMockAnthropicToolSequence(t *testing.T, configRoomID ref.RoomID) *mockAnthropicServer {
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
				"room":    configRoomID.String(),
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
