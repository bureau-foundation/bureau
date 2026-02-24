// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// anthropicTestServer creates a test HTTP server and returns an
// Anthropic provider connected to it.
func anthropicTestServer(t *testing.T, handler http.Handler) *Anthropic {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &Anthropic{
		httpClient: &http.Client{
			Transport: &testTransport{
				server:    server,
				transport: http.DefaultTransport,
			},
		},
		serviceName: "anthropic",
	}
}

// testTransport rewrites requests to target the test server, mimicking
// the proxy socket transport.
type testTransport struct {
	server    *httptest.Server
	transport http.RoundTripper
}

func (transport *testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.URL.Scheme = "http"
	request.URL.Host = transport.server.Listener.Addr().String()
	return transport.transport.RoundTrip(request)
}

func TestAnthropicComplete(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		// Verify request format.
		var wireRequest struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Messages  []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&wireRequest); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		if wireRequest.Model != "claude-sonnet-4-5-20250929" {
			t.Errorf("model = %q, want claude-sonnet-4-5-20250929", wireRequest.Model)
		}
		if wireRequest.MaxTokens != 1024 {
			t.Errorf("max_tokens = %d, want 1024", wireRequest.MaxTokens)
		}
		if wireRequest.System != "You are helpful." {
			t.Errorf("system = %q, want 'You are helpful.'", wireRequest.System)
		}
		if wireRequest.Stream {
			t.Error("stream should be false for Complete")
		}
		if length := len(wireRequest.Messages); length != 1 {
			t.Errorf("messages length = %d, want 1", length)
		}
		if length := len(wireRequest.Tools); length != 1 {
			t.Errorf("tools length = %d, want 1", length)
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":   "msg_test",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Hello! How can I help?"},
			},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":                100,
				"output_tokens":               15,
				"cache_read_input_tokens":     50,
				"cache_creation_input_tokens": 0,
			},
		})
	})

	provider := anthropicTestServer(t, mux)

	temperature := 0.7
	response, err := provider.Complete(context.Background(), Request{
		Model:       "claude-sonnet-4-5-20250929",
		System:      "You are helpful.",
		MaxTokens:   1024,
		Temperature: &temperature,
		Messages:    []Message{UserMessage("Hello")},
		Tools: []ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if response.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason = %q, want end_turn", response.StopReason)
	}
	if response.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("Model = %q, want claude-sonnet-4-5-20250929", response.Model)
	}
	if response.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", response.Usage.InputTokens)
	}
	if response.Usage.OutputTokens != 15 {
		t.Errorf("OutputTokens = %d, want 15", response.Usage.OutputTokens)
	}
	if response.Usage.CacheReadTokens != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", response.Usage.CacheReadTokens)
	}
	if text := response.TextContent(); text != "Hello! How can I help?" {
		t.Errorf("TextContent = %q, want 'Hello! How can I help?'", text)
	}
}

func TestAnthropicCompleteToolUse(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":   "msg_tools",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Let me check the weather."},
				{
					"type":  "tool_use",
					"id":    "toolu_01",
					"name":  "get_weather",
					"input": map[string]string{"location": "San Francisco"},
				},
			},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 80, "output_tokens": 30},
		})
	})

	provider := anthropicTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("What's the weather in SF?")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if response.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %q, want tool_use", response.StopReason)
	}

	if length := len(response.Content); length != 2 {
		t.Fatalf("content blocks = %d, want 2", length)
	}
	if response.Content[0].Type != ContentText {
		t.Errorf("block[0].Type = %q, want text", response.Content[0].Type)
	}
	if response.Content[1].Type != ContentToolUse {
		t.Fatalf("block[1].Type = %q, want tool_use", response.Content[1].Type)
	}

	toolUses := response.ToolUses()
	if length := len(toolUses); length != 1 {
		t.Fatalf("ToolUses = %d, want 1", length)
	}
	if toolUses[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", toolUses[0].Name)
	}
	if toolUses[0].ID != "toolu_01" {
		t.Errorf("tool ID = %q, want toolu_01", toolUses[0].ID)
	}
}

func TestAnthropicCompleteError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]string{
				"type":    "rate_limit_error",
				"message": "Rate limit exceeded",
			},
		})
	})

	provider := anthropicTestServer(t, mux)

	_, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Hello")},
	})
	if err == nil {
		t.Fatal("expected error for 429 response")
	}

	providerErr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *ProviderError", err)
	}
	if providerErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", providerErr.StatusCode)
	}
	if providerErr.Type != "rate_limit_error" {
		t.Errorf("Type = %q, want rate_limit_error", providerErr.Type)
	}
	if !providerErr.IsRateLimited() {
		t.Error("IsRateLimited should be true")
	}
}

func TestAnthropicStreamText(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		// Verify streaming was requested.
		var wireRequest struct {
			Stream bool `json:"stream"`
		}
		body, _ := io.ReadAll(request.Body)
		json.Unmarshal(body, &wireRequest)
		if !wireRequest.Stream {
			t.Error("stream should be true for Stream()")
		}

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_stream","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":50,"output_tokens":0,"cache_read_input_tokens":10}}}` + "\n\n",
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := anthropicTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Hello")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer eventStream.Close()

	var textDeltas []string
	var contentBlocks []ContentBlock
	var doneCount int

	for {
		event, err := eventStream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}

		switch event.Type {
		case EventTextDelta:
			textDeltas = append(textDeltas, event.Text)
		case EventContentBlockDone:
			contentBlocks = append(contentBlocks, event.ContentBlock)
		case EventDone:
			doneCount++
		case EventPing:
			// ignore
		case EventError:
			t.Fatalf("stream error: %v", event.Error)
		}
	}

	// Verify text deltas arrived.
	if length := len(textDeltas); length != 2 {
		t.Fatalf("text deltas = %d, want 2", length)
	}
	if textDeltas[0] != "Hello" {
		t.Errorf("delta[0] = %q, want Hello", textDeltas[0])
	}
	if textDeltas[1] != " world" {
		t.Errorf("delta[1] = %q, want ' world'", textDeltas[1])
	}

	// Verify completed content blocks.
	if length := len(contentBlocks); length != 1 {
		t.Fatalf("content blocks = %d, want 1", length)
	}
	if contentBlocks[0].Type != ContentText {
		t.Errorf("block type = %q, want text", contentBlocks[0].Type)
	}
	if contentBlocks[0].Text != "Hello world" {
		t.Errorf("block text = %q, want 'Hello world'", contentBlocks[0].Text)
	}

	if doneCount != 1 {
		t.Errorf("done events = %d, want 1", doneCount)
	}

	// Verify accumulated response.
	response := eventStream.Response()
	if response.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason = %q, want end_turn", response.StopReason)
	}
	if response.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("Model = %q, want claude-sonnet-4-5-20250929", response.Model)
	}
	if response.Usage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", response.Usage.InputTokens)
	}
	if response.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", response.Usage.OutputTokens)
	}
	if response.Usage.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", response.Usage.CacheReadTokens)
	}
	if text := response.TextContent(); text != "Hello world" {
		t.Errorf("TextContent = %q, want 'Hello world'", text)
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_tool","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":80,"output_tokens":0}}}` + "\n\n",
			// Text block first.
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking weather."}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",
			// Tool use block.
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_stream","name":"get_weather","input":{}}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":1}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := anthropicTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Weather in SF?")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer eventStream.Close()

	var contentBlocks []ContentBlock
	for {
		event, err := eventStream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if event.Type == EventContentBlockDone {
			contentBlocks = append(contentBlocks, event.ContentBlock)
		}
	}

	if length := len(contentBlocks); length != 2 {
		t.Fatalf("content blocks = %d, want 2", length)
	}

	// First block: text.
	if contentBlocks[0].Type != ContentText {
		t.Errorf("block[0].Type = %q, want text", contentBlocks[0].Type)
	}
	if contentBlocks[0].Text != "Checking weather." {
		t.Errorf("block[0].Text = %q, want 'Checking weather.'", contentBlocks[0].Text)
	}

	// Second block: tool_use.
	if contentBlocks[1].Type != ContentToolUse {
		t.Fatalf("block[1].Type = %q, want tool_use", contentBlocks[1].Type)
	}
	toolUse := contentBlocks[1].ToolUse
	if toolUse.ID != "toolu_stream" {
		t.Errorf("tool ID = %q, want toolu_stream", toolUse.ID)
	}
	if toolUse.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", toolUse.Name)
	}

	// Verify the accumulated input JSON from deltas.
	var input map[string]string
	if err := json.Unmarshal(toolUse.Input, &input); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if input["location"] != "SF" {
		t.Errorf("tool input location = %q, want SF", input["location"])
	}

	// Verify accumulated response.
	response := eventStream.Response()
	if response.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %q, want tool_use", response.StopReason)
	}
	if response.Usage.OutputTokens != 25 {
		t.Errorf("OutputTokens = %d, want 25", response.Usage.OutputTokens)
	}

	responseToolUses := response.ToolUses()
	if length := len(responseToolUses); length != 1 {
		t.Fatalf("response ToolUses = %d, want 1", length)
	}
}

func TestAnthropicStreamError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]string{
				"type":    "overloaded_error",
				"message": "Overloaded",
			},
		})
	})

	provider := anthropicTestServer(t, mux)

	_, err := provider.Stream(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Hello")},
	})
	if err == nil {
		t.Fatal("expected error for 503 response")
	}

	providerErr, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *ProviderError", err)
	}
	if providerErr.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", providerErr.StatusCode)
	}
}

func TestAnthropicToolResultMessage(t *testing.T) {
	t.Parallel()

	// Verify that tool result messages are serialized correctly in
	// the Anthropic wire format.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		var wireRequest struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
					Content   string `json:"content"`
					IsError   bool   `json:"is_error"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&wireRequest); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		// Should have 3 messages: user, assistant (with tool_use), user (with tool_result).
		if length := len(wireRequest.Messages); length != 3 {
			t.Errorf("messages = %d, want 3", length)
		} else {
			toolResultMsg := wireRequest.Messages[2]
			if toolResultMsg.Role != "user" {
				t.Errorf("tool result role = %q, want user", toolResultMsg.Role)
			}
			if length := len(toolResultMsg.Content); length != 1 {
				t.Errorf("tool result content blocks = %d, want 1", length)
			} else {
				block := toolResultMsg.Content[0]
				if block.Type != "tool_result" {
					t.Errorf("block type = %q, want tool_result", block.Type)
				}
				if block.ToolUseID != "toolu_01" {
					t.Errorf("tool_use_id = %q, want toolu_01", block.ToolUseID)
				}
				if block.Content != "72°F and sunny" {
					t.Errorf("content = %q, want '72°F and sunny'", block.Content)
				}
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":          "msg_final",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]any{{"type": "text", "text": "It's sunny!"}},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 120, "output_tokens": 10},
		})
	})

	provider := anthropicTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages: []Message{
			UserMessage("Weather in SF?"),
			{
				Role: RoleAssistant,
				Content: []ContentBlock{
					TextBlock("Let me check."),
					ToolUseBlock("toolu_01", "get_weather", json.RawMessage(`{"location":"SF"}`)),
				},
			},
			ToolResultMessage(ToolResult{
				ToolUseID: "toolu_01",
				Content:   "72°F and sunny",
			}),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text := response.TextContent(); text != "It's sunny!" {
		t.Errorf("TextContent = %q, want 'It's sunny!'", text)
	}
}

func TestAnthropicStreamServerToolUse(t *testing.T) {
	t.Parallel()

	// Simulates Anthropic's tool search: the model's response contains
	// server_tool_use and tool_search_tool_result blocks alongside
	// a normal tool_use block. The server blocks should flow through
	// as ContentServerToolUse/ContentServerToolResult, while ToolUses()
	// returns only the regular tool_use.

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_search","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n",

			// Block 0: server_tool_use (tool search invocation).
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"tool_search_tool_bm25_20251119"}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"weather\"}"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",

			// Block 1: tool_search_tool_result (fully formed, no deltas).
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_01","content":{"tool_names":["get_weather","get_forecast"]}}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":1}` + "\n\n",

			// Block 2: regular tool_use (the model decided to call get_weather).
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_02","name":"get_weather","input":{}}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"location\":\"NYC\"}"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":2}` + "\n\n",

			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := anthropicTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Weather in NYC?")},
		Tools: []ToolDefinition{
			{
				Name:         "get_weather",
				Description:  "Get the weather",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
				DeferLoading: true,
			},
			{
				Type: "tool_search_tool_bm25_20251119",
				Name: "tool_search_tool_bm25_20251119",
			},
		},
		ExtraHeaders: map[string]string{
			"anthropic-beta": "advanced-tool-use-2025-11-20",
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer eventStream.Close()

	var contentBlocks []ContentBlock
	for {
		event, err := eventStream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if event.Type == EventContentBlockDone {
			contentBlocks = append(contentBlocks, event.ContentBlock)
		}
	}

	if length := len(contentBlocks); length != 3 {
		t.Fatalf("content blocks = %d, want 3", length)
	}

	// Block 0: server_tool_use.
	if contentBlocks[0].Type != ContentServerToolUse {
		t.Fatalf("block[0].Type = %q, want server_tool_use", contentBlocks[0].Type)
	}
	serverToolUse := contentBlocks[0].ServerToolUse
	if serverToolUse.ID != "srvtoolu_01" {
		t.Errorf("server_tool_use.ID = %q, want srvtoolu_01", serverToolUse.ID)
	}
	if serverToolUse.Name != "tool_search_tool_bm25_20251119" {
		t.Errorf("server_tool_use.Name = %q, want tool_search_tool_bm25_20251119", serverToolUse.Name)
	}
	var searchInput map[string]string
	if err := json.Unmarshal(serverToolUse.Input, &searchInput); err != nil {
		t.Fatalf("unmarshal server_tool_use.Input: %v", err)
	}
	if searchInput["query"] != "weather" {
		t.Errorf("server_tool_use query = %q, want weather", searchInput["query"])
	}

	// Block 1: tool_search_tool_result.
	if contentBlocks[1].Type != ContentServerToolResult {
		t.Fatalf("block[1].Type = %q, want server_tool_result", contentBlocks[1].Type)
	}
	serverResult := contentBlocks[1].ServerToolResult
	if serverResult.ToolUseID != "srvtoolu_01" {
		t.Errorf("server_tool_result.ToolUseID = %q, want srvtoolu_01", serverResult.ToolUseID)
	}
	var searchResult struct {
		ToolNames []string `json:"tool_names"`
	}
	if err := json.Unmarshal(serverResult.Content, &searchResult); err != nil {
		t.Fatalf("unmarshal server_tool_result.Content: %v", err)
	}
	if length := len(searchResult.ToolNames); length != 2 {
		t.Errorf("tool_names length = %d, want 2", length)
	}

	// Block 2: regular tool_use.
	if contentBlocks[2].Type != ContentToolUse {
		t.Fatalf("block[2].Type = %q, want tool_use", contentBlocks[2].Type)
	}
	if contentBlocks[2].ToolUse.Name != "get_weather" {
		t.Errorf("tool_use.Name = %q, want get_weather", contentBlocks[2].ToolUse.Name)
	}

	// ToolUses() should only return the regular tool_use, not server blocks.
	response := eventStream.Response()
	toolUses := response.ToolUses()
	if length := len(toolUses); length != 1 {
		t.Fatalf("ToolUses() = %d, want 1 (only regular tool_use)", length)
	}
	if toolUses[0].Name != "get_weather" {
		t.Errorf("ToolUses()[0].Name = %q, want get_weather", toolUses[0].Name)
	}
}

func TestAnthropicCompleteServerToolUse(t *testing.T) {
	t.Parallel()

	// Non-streaming response with server tool use blocks.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":   "msg_search_complete",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":  "server_tool_use",
					"id":    "srvtoolu_10",
					"name":  "tool_search_tool_bm25_20251119",
					"input": map[string]string{"query": "ticket"},
				},
				{
					"type":        "tool_search_tool_result",
					"tool_use_id": "srvtoolu_10",
					"content":     map[string]any{"tool_names": []string{"bureau_ticket_create"}},
				},
				{
					"type":  "tool_use",
					"id":    "toolu_10",
					"name":  "bureau_ticket_create",
					"input": map[string]any{"title": "Fix bug"},
				},
			},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 90, "output_tokens": 35},
		})
	})

	provider := anthropicTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Create a bug ticket")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if length := len(response.Content); length != 3 {
		t.Fatalf("content blocks = %d, want 3", length)
	}

	if response.Content[0].Type != ContentServerToolUse {
		t.Errorf("block[0].Type = %q, want server_tool_use", response.Content[0].Type)
	}
	if response.Content[1].Type != ContentServerToolResult {
		t.Errorf("block[1].Type = %q, want server_tool_result", response.Content[1].Type)
	}
	if response.Content[2].Type != ContentToolUse {
		t.Errorf("block[2].Type = %q, want tool_use", response.Content[2].Type)
	}

	toolUses := response.ToolUses()
	if length := len(toolUses); length != 1 {
		t.Fatalf("ToolUses() = %d, want 1", length)
	}
}

func TestAnthropicServerBlockConversationReplay(t *testing.T) {
	t.Parallel()

	// Verify that server tool use blocks survive round-trip through
	// conversation history: ContentBlock → anthropicContentBlock →
	// wire JSON → back. This is critical because the model expects
	// to see its own server_tool_use and tool_search_tool_result
	// blocks in subsequent requests.

	var capturedRequest json.RawMessage

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		capturedRequest = body

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":          "msg_replay",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]any{{"type": "text", "text": "Done."}},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 200, "output_tokens": 5},
		})
	})

	provider := anthropicTestServer(t, mux)

	// Build a conversation that includes server blocks in the
	// assistant turn (as if the previous response had tool search).
	_, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages: []Message{
			UserMessage("Weather?"),
			{
				Role: RoleAssistant,
				Content: []ContentBlock{
					{
						Type: ContentServerToolUse,
						ServerToolUse: &ServerToolUse{
							ID:    "srvtoolu_replay",
							Name:  "tool_search_tool_bm25_20251119",
							Input: json.RawMessage(`{"query":"weather"}`),
						},
					},
					{
						Type: ContentServerToolResult,
						ServerToolResult: &ServerToolResult{
							ToolUseID: "srvtoolu_replay",
							Content:   json.RawMessage(`{"tool_names":["get_weather"]}`),
						},
					},
					ToolUseBlock("toolu_replay", "get_weather", json.RawMessage(`{"location":"NYC"}`)),
				},
			},
			ToolResultMessage(ToolResult{
				ToolUseID: "toolu_replay",
				Content:   "72F sunny",
			}),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Parse the captured request and verify server blocks were
	// serialized correctly.
	var wireRequest struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id,omitempty"`
				Name      string          `json:"name,omitempty"`
				Input     json.RawMessage `json:"input,omitempty"`
				ToolUseID string          `json:"tool_use_id,omitempty"`
				Content   json.RawMessage `json:"content,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedRequest, &wireRequest); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}

	if length := len(wireRequest.Messages); length != 3 {
		t.Fatalf("messages = %d, want 3", length)
	}

	assistantMsg := wireRequest.Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Fatalf("message[1].role = %q, want assistant", assistantMsg.Role)
	}
	if length := len(assistantMsg.Content); length != 3 {
		t.Fatalf("assistant content blocks = %d, want 3", length)
	}

	// server_tool_use should be replayed with correct wire type.
	if assistantMsg.Content[0].Type != "server_tool_use" {
		t.Errorf("block[0].type = %q, want server_tool_use", assistantMsg.Content[0].Type)
	}
	if assistantMsg.Content[0].ID != "srvtoolu_replay" {
		t.Errorf("block[0].id = %q, want srvtoolu_replay", assistantMsg.Content[0].ID)
	}
	if assistantMsg.Content[0].Name != "tool_search_tool_bm25_20251119" {
		t.Errorf("block[0].name = %q, want tool_search_tool_bm25_20251119", assistantMsg.Content[0].Name)
	}

	// tool_search_tool_result should be replayed.
	if assistantMsg.Content[1].Type != "tool_search_tool_result" {
		t.Errorf("block[1].type = %q, want tool_search_tool_result", assistantMsg.Content[1].Type)
	}
	if assistantMsg.Content[1].ToolUseID != "srvtoolu_replay" {
		t.Errorf("block[1].tool_use_id = %q, want srvtoolu_replay", assistantMsg.Content[1].ToolUseID)
	}

	// tool_use should also be present.
	if assistantMsg.Content[2].Type != "tool_use" {
		t.Errorf("block[2].type = %q, want tool_use", assistantMsg.Content[2].Type)
	}
}

func TestAnthropicDeferLoadingWireFormat(t *testing.T) {
	t.Parallel()

	// Verify that DeferLoading and Type fields serialize correctly
	// on the wire.
	var capturedRequest json.RawMessage

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		capturedRequest = body

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":          "msg_defer",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 50, "output_tokens": 2},
		})
	})

	provider := anthropicTestServer(t, mux)

	_, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("hello")},
		Tools: []ToolDefinition{
			{
				Name:         "always_visible",
				Description:  "Always in context",
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				DeferLoading: false,
			},
			{
				Name:         "deferred_tool",
				Description:  "Only loaded on search match",
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				DeferLoading: true,
			},
			{
				Type: "tool_search_tool_bm25_20251119",
				Name: "tool_search_tool_bm25_20251119",
			},
		},
		ExtraHeaders: map[string]string{
			"anthropic-beta": "advanced-tool-use-2025-11-20",
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var wireRequest struct {
		Tools []struct {
			Type         string          `json:"type,omitempty"`
			Name         string          `json:"name"`
			Description  string          `json:"description,omitempty"`
			InputSchema  json.RawMessage `json:"input_schema,omitempty"`
			DeferLoading bool            `json:"defer_loading,omitempty"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(capturedRequest, &wireRequest); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}

	if length := len(wireRequest.Tools); length != 3 {
		t.Fatalf("tools = %d, want 3", length)
	}

	// Tool 0: normal, not deferred.
	if wireRequest.Tools[0].DeferLoading {
		t.Error("tools[0].defer_loading should be false")
	}
	if wireRequest.Tools[0].Description != "Always in context" {
		t.Errorf("tools[0].description = %q, want 'Always in context'", wireRequest.Tools[0].Description)
	}

	// Tool 1: deferred.
	if !wireRequest.Tools[1].DeferLoading {
		t.Error("tools[1].defer_loading should be true")
	}
	if wireRequest.Tools[1].Description != "Only loaded on search match" {
		t.Errorf("tools[1].description = %q, want 'Only loaded on search match'", wireRequest.Tools[1].Description)
	}

	// Tool 2: special type (search tool) — description/schema omitted.
	if wireRequest.Tools[2].Type != "tool_search_tool_bm25_20251119" {
		t.Errorf("tools[2].type = %q, want tool_search_tool_bm25_20251119", wireRequest.Tools[2].Type)
	}
	if wireRequest.Tools[2].Description != "" {
		t.Errorf("tools[2].description = %q, want empty (special tool)", wireRequest.Tools[2].Description)
	}
	if wireRequest.Tools[2].InputSchema != nil {
		t.Errorf("tools[2].input_schema = %s, want nil (special tool)", wireRequest.Tools[2].InputSchema)
	}
}

func TestAnthropicExtraHeaders(t *testing.T) {
	t.Parallel()

	var capturedHeaders http.Header

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		capturedHeaders = request.Header.Clone()

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":          "msg_headers",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 1},
		})
	})

	provider := anthropicTestServer(t, mux)

	_, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("hello")},
		ExtraHeaders: map[string]string{
			"anthropic-beta": "advanced-tool-use-2025-11-20",
			"x-custom":       "test-value",
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := capturedHeaders.Get("anthropic-beta"); got != "advanced-tool-use-2025-11-20" {
		t.Errorf("anthropic-beta header = %q, want advanced-tool-use-2025-11-20", got)
	}
	if got := capturedHeaders.Get("x-custom"); got != "test-value" {
		t.Errorf("x-custom header = %q, want test-value", got)
	}
	if got := capturedHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestAnthropicStreamThinking(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		// Simulate a response with a thinking block followed by a text block.
		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_think","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n",
			// Thinking block start.
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n",
			// Thinking delta.
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me reason"}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" about this carefully."}}` + "\n\n",
			// Signature delta.
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_abc123"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",
			// Text block.
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The answer is 42."}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":1}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := anthropicTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("What is the meaning of life?")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer eventStream.Close()

	var contentBlocks []ContentBlock
	for {
		event, err := eventStream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if event.Type == EventContentBlockDone {
			contentBlocks = append(contentBlocks, event.ContentBlock)
		}
	}

	// Two content blocks: thinking + text.
	if length := len(contentBlocks); length != 2 {
		t.Fatalf("content blocks = %d, want 2", length)
	}

	// Verify thinking block.
	if contentBlocks[0].Type != ContentThinking {
		t.Errorf("block[0].Type = %q, want thinking", contentBlocks[0].Type)
	}
	if contentBlocks[0].Thinking == nil {
		t.Fatal("block[0].Thinking is nil")
	}
	if contentBlocks[0].Thinking.Content != "Let me reason about this carefully." {
		t.Errorf("thinking content = %q", contentBlocks[0].Thinking.Content)
	}
	if contentBlocks[0].Thinking.Signature != "sig_abc123" {
		t.Errorf("thinking signature = %q, want sig_abc123", contentBlocks[0].Thinking.Signature)
	}

	// Verify text block.
	if contentBlocks[1].Type != ContentText {
		t.Errorf("block[1].Type = %q, want text", contentBlocks[1].Type)
	}
	if contentBlocks[1].Text != "The answer is 42." {
		t.Errorf("text = %q", contentBlocks[1].Text)
	}

	// Verify accumulated response.
	response := eventStream.Response()
	if thinking := response.ThinkingContent(); thinking != "Let me reason about this carefully." {
		t.Errorf("ThinkingContent = %q", thinking)
	}
	if text := response.TextContent(); text != "The answer is 42." {
		t.Errorf("TextContent = %q", text)
	}
}

func TestAnthropicCompleteThinking(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":   "msg_think_complete",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":      "thinking",
					"thinking":  "Step by step analysis...",
					"signature": "sig_xyz789",
				},
				{
					"type": "text",
					"text": "Here is my answer.",
				},
			},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 80, "output_tokens": 30},
		})
	})

	provider := anthropicTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Analyze this")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if length := len(response.Content); length != 2 {
		t.Fatalf("content blocks = %d, want 2", length)
	}

	// Thinking block.
	if response.Content[0].Type != ContentThinking {
		t.Errorf("block[0].Type = %q, want thinking", response.Content[0].Type)
	}
	if response.Content[0].Thinking == nil {
		t.Fatal("block[0].Thinking is nil")
	}
	if response.Content[0].Thinking.Content != "Step by step analysis..." {
		t.Errorf("thinking content = %q", response.Content[0].Thinking.Content)
	}
	if response.Content[0].Thinking.Signature != "sig_xyz789" {
		t.Errorf("thinking signature = %q", response.Content[0].Thinking.Signature)
	}

	// Text block.
	if response.Content[1].Type != ContentText {
		t.Errorf("block[1].Type = %q, want text", response.Content[1].Type)
	}
	if response.Content[1].Text != "Here is my answer." {
		t.Errorf("text = %q", response.Content[1].Text)
	}

	if thinking := response.ThinkingContent(); thinking != "Step by step analysis..." {
		t.Errorf("ThinkingContent = %q", thinking)
	}
}

func TestAnthropicThinkingConversationReplay(t *testing.T) {
	t.Parallel()

	// Verify that thinking blocks survive round-trip through
	// conversation history. The API requires thinking blocks to be
	// sent back in subsequent requests for signature verification.

	var capturedRequest json.RawMessage

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/anthropic/v1/messages", func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		capturedRequest = body

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":          "msg_replay_thinking",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]any{{"type": "text", "text": "Follow-up."}},
			"model":       "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 150, "output_tokens": 5},
		})
	})

	provider := anthropicTestServer(t, mux)

	// Build a conversation where the previous assistant response
	// contained a thinking block.
	_, err := provider.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 1024,
		Messages: []Message{
			UserMessage("Think about this"),
			{
				Role: RoleAssistant,
				Content: []ContentBlock{
					ThinkingContentBlock("My reasoning process...", "sig_replay_test"),
					TextBlock("Here is my conclusion."),
				},
			},
			UserMessage("Can you elaborate?"),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Parse the captured wire request to verify thinking block
	// serialization.
	var wireRequest struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text,omitempty"`
				Thinking  string `json:"thinking,omitempty"`
				Signature string `json:"signature,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedRequest, &wireRequest); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}

	if length := len(wireRequest.Messages); length != 3 {
		t.Fatalf("messages = %d, want 3", length)
	}

	assistantMessage := wireRequest.Messages[1]
	if assistantMessage.Role != "assistant" {
		t.Fatalf("message[1].role = %q, want assistant", assistantMessage.Role)
	}
	if length := len(assistantMessage.Content); length != 2 {
		t.Fatalf("assistant content blocks = %d, want 2", length)
	}

	// Thinking block must be serialized with correct wire fields.
	thinkingBlock := assistantMessage.Content[0]
	if thinkingBlock.Type != "thinking" {
		t.Errorf("block[0].type = %q, want thinking", thinkingBlock.Type)
	}
	if thinkingBlock.Thinking != "My reasoning process..." {
		t.Errorf("block[0].thinking = %q", thinkingBlock.Thinking)
	}
	if thinkingBlock.Signature != "sig_replay_test" {
		t.Errorf("block[0].signature = %q, want sig_replay_test", thinkingBlock.Signature)
	}

	// Text block follows.
	textBlock := assistantMessage.Content[1]
	if textBlock.Type != "text" {
		t.Errorf("block[1].type = %q, want text", textBlock.Type)
	}
	if textBlock.Text != "Here is my conclusion." {
		t.Errorf("block[1].text = %q", textBlock.Text)
	}
}
