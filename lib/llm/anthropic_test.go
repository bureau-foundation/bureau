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
