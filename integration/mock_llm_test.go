// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// anthropicSSEResponse describes a single Anthropic Messages API streaming
// response. The mock server converts this into the SSE wire format
// (message_start, content_block_start/delta/stop, message_delta,
// message_stop). Each field maps directly to the Anthropic wire protocol:
// no interpretation, no abstraction leaking.
type anthropicSSEResponse struct {
	// Model is the model identifier (e.g., "mock-model").
	Model string

	// StopReason is the reason the response ended: "end_turn" for text
	// responses, "tool_use" for tool call responses.
	StopReason string

	// InputTokens and OutputTokens are the usage metrics.
	InputTokens  int64
	OutputTokens int64

	// Content is the list of content blocks. Each entry is a raw JSON
	// object matching Anthropic's content block format:
	//   {"type":"text","text":"Hello"}
	//   {"type":"tool_use","id":"tc_01","name":"echo","input":{"msg":"hi"}}
	Content []json.RawMessage
}

// writeSSE writes an anthropicSSEResponse as an SSE event stream to the
// ResponseWriter. The events follow the exact Anthropic wire protocol
// format that lib/llm.Anthropic.Stream parses.
func writeSSE(writer http.ResponseWriter, response anthropicSSEResponse) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")

	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// message_start: includes model and input usage.
	writeSSEEvent(writer, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    "msg_mock",
			"model": response.Model,
			"usage": map[string]any{
				"input_tokens":  response.InputTokens,
				"output_tokens": 0,
			},
		},
	})

	// Emit each content block as start/delta/stop.
	for index, rawBlock := range response.Content {
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		json.Unmarshal(rawBlock, &block)

		switch block.Type {
		case "text":
			// content_block_start with empty text.
			writeSSEEvent(writer, flusher, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			// Single text delta with the full text.
			writeSSEEvent(writer, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "text_delta", "text": block.Text},
			})

		case "tool_use":
			// content_block_start with tool metadata and empty input.
			writeSSEEvent(writer, flusher, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    block.ID,
					"name":  block.Name,
					"input": map[string]any{},
				},
			})
			// Single input_json_delta with the full input JSON.
			inputJSON, _ := json.Marshal(block.Input)
			writeSSEEvent(writer, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				},
			})
		}

		// content_block_stop.
		writeSSEEvent(writer, flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})
	}

	// message_delta: stop reason and output usage.
	writeSSEEvent(writer, flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": response.StopReason},
		"usage": map[string]any{"output_tokens": response.OutputTokens},
	})

	// message_stop.
	writeSSEEvent(writer, flusher, "message_stop", map[string]any{
		"type": "message_stop",
	})
}

// writeSSEEvent writes a single SSE event (event + data line).
func writeSSEEvent(writer http.ResponseWriter, flusher http.Flusher, eventType string, data any) {
	payload, _ := json.Marshal(data)
	fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", eventType, payload)
	flusher.Flush()
}

// mockToolStep describes a single tool invocation in a mock LLM sequence.
// The mock returns a tool_use response for this step, the agent executes
// the tool and sends back a tool_result, and the mock returns a text
// response acknowledging the result.
//
// The mock is purely a puppeteer — it tells the agent what to do but never
// participates in verification. All test assertions read outcomes from
// Matrix state events, which is the same observation path production
// systems use.
type mockToolStep struct {
	// ToolName is the MCP tool name (e.g., "bureau_ticket_create").
	ToolName string

	// ToolInput returns the tool input for this step. Use a closure to
	// capture values from the test scope (e.g., room IDs, ticket IDs
	// read from Matrix state events between steps).
	ToolInput func() map[string]any
}

// mockToolSequenceServer wraps an httptest.Server with a synchronization
// channel that signals when all expected LLM request pairs have been handled.
type mockToolSequenceServer struct {
	*httptest.Server

	// AllStepsCompleted is closed when the final text response (after the
	// last tool_result) has been sent. Waiting on this ensures the complete
	// tool→result→LLM cycle for every step has finished.
	AllStepsCompleted <-chan struct{}
}

// newMockToolSequence creates a mock Anthropic Messages API server that
// handles a multi-step tool sequence. Each step produces two HTTP requests:
//
//   - Even request (2*i): returns a tool_use for step i
//   - Odd request (2*i+1): validates tool_result is present, returns text
//
// After the final odd request, AllStepsCompleted is closed. The mock
// validates that the agent sends tool_result blocks (proving the agent
// protocol works) but does not extract or expose their content — all
// verification flows through Matrix state events.
func newMockToolSequence(t *testing.T, steps []mockToolStep) *mockToolSequenceServer {
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

		// Zero steps: return a single text response and signal completion.
		if len(steps) == 0 {
			if current > 0 {
				t.Errorf("mock: unexpected request %d (no steps configured, only 1 text response expected)", current)
				http.Error(writer, "unexpected request", http.StatusInternalServerError)
				return
			}
			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "end_turn",
				InputTokens:  100,
				OutputTokens: 20,
				Content: []json.RawMessage{
					json.RawMessage(`{"type":"text","text":"Hello! I'm a mock response."}`),
				},
			})
			close(allDone)
			return
		}

		stepIndex := current / 2
		isToolResult := current%2 == 1

		if stepIndex >= len(steps) {
			t.Errorf("mock: unexpected request %d (only %d steps configured)", current, len(steps))
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}

		if isToolResult {
			// Odd request: validate tool_result is present (proves the
			// agent executed the tool and sent the result back).
			toolCallID := fmt.Sprintf("tc_mock_%02d", stepIndex)
			if !hasToolResult(wireRequest.Messages, toolCallID) {
				t.Errorf("mock: request %d missing tool_result for %s", current, toolCallID)
			}

			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "end_turn",
				InputTokens:  200,
				OutputTokens: 15,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(`{"type":"text","text":"Step %d completed"}`, stepIndex)),
				},
			})

			// Signal completion after the last step's text response.
			if stepIndex == len(steps)-1 {
				close(allDone)
			}
		} else {
			// Even request: return tool_use.
			step := steps[stepIndex]
			toolInput := step.ToolInput()
			toolInputJSON, _ := json.Marshal(toolInput)
			toolCallID := fmt.Sprintf("tc_mock_%02d", stepIndex)

			writeSSE(writer, anthropicSSEResponse{
				Model:        wireRequest.Model,
				StopReason:   "tool_use",
				InputTokens:  150,
				OutputTokens: 30,
				Content: []json.RawMessage{
					json.RawMessage(fmt.Sprintf(
						`{"type":"tool_use","id":"%s","name":"%s","input":%s}`,
						toolCallID, step.ToolName, toolInputJSON,
					)),
				},
			})
		}
	}))

	t.Cleanup(server.Close)
	return &mockToolSequenceServer{
		Server:            server,
		AllStepsCompleted: allDone,
	}
}

// hasToolResult checks whether the message history contains a tool_result
// for the given tool_use_id. Used by the mock to validate that the agent
// executed the tool and sent the result back — a protocol-level assertion,
// not a content-level one.
func hasToolResult(messages []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}, toolUseID string) bool {
	for _, message := range messages {
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return true
			}
		}
	}
	return false
}

// registerProxyHTTPService registers an HTTP service on a proxy's admin
// socket. The proxy routes requests to /http/{serviceName}/... to the
// given upstream URL. This is the same wire format the daemon uses in
// configureConsumerProxy (PUT /v1/admin/services/{name}).
func registerProxyHTTPService(t *testing.T, adminSocketPath, serviceName, upstreamURL string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"upstream_url": upstreamURL,
	})
	if err != nil {
		t.Fatalf("marshaling service registration: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", adminSocketPath)
			},
		},
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		"http://localhost/v1/admin/services/"+serviceName,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("creating service registration request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("registering service %q on admin socket %s: %v", serviceName, adminSocketPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("service registration returned %d, want 201", response.StatusCode)
	}
}
