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

// openaiTestServer creates a test HTTP server and returns an OpenAI
// provider connected to it.
func openaiTestServer(t *testing.T, handler http.Handler) *OpenAI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &OpenAI{
		httpClient: &http.Client{
			Transport: &testTransport{
				server:    server,
				transport: http.DefaultTransport,
			},
		},
		serviceName: "openai",
	}
}

func TestOpenAIComplete(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		// Verify request format.
		var wireRequest struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name       string          `json:"name"`
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&wireRequest); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		if wireRequest.Model != "gpt-4o" {
			t.Errorf("model = %q, want gpt-4o", wireRequest.Model)
		}
		if wireRequest.MaxTokens != 1024 {
			t.Errorf("max_tokens = %d, want 1024", wireRequest.MaxTokens)
		}
		if wireRequest.Stream {
			t.Error("stream should be false for Complete")
		}

		// Should have 2 messages: system + user.
		if length := len(wireRequest.Messages); length != 2 {
			t.Errorf("messages length = %d, want 2", length)
		} else {
			if wireRequest.Messages[0].Role != "system" {
				t.Errorf("messages[0].role = %q, want system", wireRequest.Messages[0].Role)
			}
			var systemContent string
			json.Unmarshal(wireRequest.Messages[0].Content, &systemContent)
			if systemContent != "You are helpful." {
				t.Errorf("system content = %q, want 'You are helpful.'", systemContent)
			}
			if wireRequest.Messages[1].Role != "user" {
				t.Errorf("messages[1].role = %q, want user", wireRequest.Messages[1].Role)
			}
		}

		// Verify tools use OpenAI format.
		if length := len(wireRequest.Tools); length != 1 {
			t.Errorf("tools length = %d, want 1", length)
		} else {
			tool := wireRequest.Tools[0]
			if tool.Type != "function" {
				t.Errorf("tool.type = %q, want function", tool.Type)
			}
			if tool.Function.Name != "get_weather" {
				t.Errorf("tool.function.name = %q, want get_weather", tool.Function.Name)
			}
			// Verify it's "parameters" not "input_schema".
			if tool.Function.Parameters == nil {
				t.Error("tool.function.parameters is nil")
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":    "chatcmpl-test",
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "Hello! How can I help?",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     100,
				"completion_tokens": 15,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 50,
				},
			},
		})
	})

	provider := openaiTestServer(t, mux)

	temperature := 0.7
	response, err := provider.Complete(context.Background(), Request{
		Model:       "gpt-4o",
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
	if response.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", response.Model)
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

func TestOpenAICompleteToolUse(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":    "chatcmpl-tools",
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "Let me check the weather.",
					"tool_calls": []map[string]any{{
						"id":   "call_01",
						"type": "function",
						"function": map[string]string{
							"name":      "get_weather",
							"arguments": `{"location":"San Francisco"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{
				"prompt_tokens":     80,
				"completion_tokens": 30,
			},
		})
	})

	provider := openaiTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "gpt-4o",
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
	if toolUses[0].ID != "call_01" {
		t.Errorf("tool ID = %q, want call_01", toolUses[0].ID)
	}

	var input map[string]string
	if err := json.Unmarshal(toolUses[0].Input, &input); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if input["location"] != "San Francisco" {
		t.Errorf("tool input location = %q, want San Francisco", input["location"])
	}
}

func TestOpenAICompleteError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]string{
				"type":    "rate_limit_error",
				"message": "Rate limit exceeded",
				"code":    "rate_limit_exceeded",
			},
		})
	})

	provider := openaiTestServer(t, mux)

	_, err := provider.Complete(context.Background(), Request{
		Model:     "gpt-4o",
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

func TestOpenAIStreamText(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		// Verify streaming was requested.
		var wireRequest struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		body, _ := io.ReadAll(request.Body)
		json.Unmarshal(body, &wireRequest)
		if !wireRequest.Stream {
			t.Error("stream should be true for Stream()")
		}
		if wireRequest.StreamOptions == nil || !wireRequest.StreamOptions.IncludeUsage {
			t.Error("stream_options.include_usage should be true")
		}

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		events := []string{
			`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := openaiTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-4o",
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
	if response.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", response.Model)
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

func TestOpenAIStreamToolUse(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		events := []string{
			// Text content first.
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Checking weather."},"finish_reason":null}]}` + "\n\n",
			// Tool call start: ID and function name.
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_stream","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
			// Tool call argument deltas.
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":"}}]},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]}` + "\n\n",
			// Finish with tool_calls reason.
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
			`data: {"id":"chatcmpl-2","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":80,"completion_tokens":25}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := openaiTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-4o",
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
	if toolUse.ID != "call_stream" {
		t.Errorf("tool ID = %q, want call_stream", toolUse.ID)
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

func TestOpenAIToolResultMessage(t *testing.T) {
	t.Parallel()

	// Verify that tool result messages are serialized correctly in
	// the OpenAI wire format: assistant tool_calls become the
	// tool_calls array, tool results become role:"tool" messages.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		var wireRequest struct {
			Messages []struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&wireRequest); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		// Should have 4 messages: system, user, assistant (with tool_calls), tool.
		if length := len(wireRequest.Messages); length != 4 {
			t.Errorf("messages = %d, want 4", length)
		} else {
			// Message 0: system prompt.
			if wireRequest.Messages[0].Role != "system" {
				t.Errorf("messages[0].role = %q, want system", wireRequest.Messages[0].Role)
			}

			// Message 1: user message.
			if wireRequest.Messages[1].Role != "user" {
				t.Errorf("messages[1].role = %q, want user", wireRequest.Messages[1].Role)
			}

			// Message 2: assistant with tool_calls.
			assistantMsg := wireRequest.Messages[2]
			if assistantMsg.Role != "assistant" {
				t.Errorf("messages[2].role = %q, want assistant", assistantMsg.Role)
			}
			if length := len(assistantMsg.ToolCalls); length != 1 {
				t.Errorf("assistant tool_calls = %d, want 1", length)
			} else {
				if assistantMsg.ToolCalls[0].ID != "call_01" {
					t.Errorf("tool_call.id = %q, want call_01", assistantMsg.ToolCalls[0].ID)
				}
				if assistantMsg.ToolCalls[0].Type != "function" {
					t.Errorf("tool_call.type = %q, want function", assistantMsg.ToolCalls[0].Type)
				}
				if assistantMsg.ToolCalls[0].Function.Name != "get_weather" {
					t.Errorf("tool_call.function.name = %q, want get_weather", assistantMsg.ToolCalls[0].Function.Name)
				}
			}

			// Message 3: tool result (NOT a user message).
			toolResultMsg := wireRequest.Messages[3]
			if toolResultMsg.Role != "tool" {
				t.Errorf("messages[3].role = %q, want tool", toolResultMsg.Role)
			}
			if toolResultMsg.ToolCallID != "call_01" {
				t.Errorf("tool_call_id = %q, want call_01", toolResultMsg.ToolCallID)
			}
			var toolContent string
			json.Unmarshal(toolResultMsg.Content, &toolContent)
			if toolContent != "72°F and sunny" {
				t.Errorf("tool content = %q, want '72°F and sunny'", toolContent)
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"id":    "chatcmpl-final",
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "It's sunny!",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     120,
				"completion_tokens": 10,
			},
		})
	})

	provider := openaiTestServer(t, mux)

	response, err := provider.Complete(context.Background(), Request{
		Model:     "gpt-4o",
		System:    "You are helpful.",
		MaxTokens: 1024,
		Messages: []Message{
			UserMessage("Weather in SF?"),
			{
				Role: RoleAssistant,
				Content: []ContentBlock{
					TextBlock("Let me check."),
					ToolUseBlock("call_01", "get_weather", json.RawMessage(`{"location":"SF"}`)),
				},
			},
			ToolResultMessage(ToolResult{
				ToolUseID: "call_01",
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

func TestOpenAIStreamMultipleToolCalls(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		// Two tool calls multiplexed by index.
		events := []string{
			// First tool call: ID and name.
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
			// Second tool call: ID and name.
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
			// Arguments for first tool call.
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":\"SF\"}"}}]},"finish_reason":null}]}` + "\n\n",
			// Arguments for second tool call.
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"timezone\":\"PST\"}"}}]},"finish_reason":null}]}` + "\n\n",
			// Finish.
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
			`data: {"id":"chatcmpl-3","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":60,"completion_tokens":40}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}

		for _, event := range events {
			fmt.Fprint(writer, event)
			flusher.Flush()
		}
	})

	provider := openaiTestServer(t, mux)

	eventStream, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages:  []Message{UserMessage("Weather and time in SF?")},
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

	// Should have exactly 2 tool_use blocks (no text block since
	// no text content was streamed).
	if length := len(contentBlocks); length != 2 {
		t.Fatalf("content blocks = %d, want 2", length)
	}

	// First tool call.
	if contentBlocks[0].Type != ContentToolUse {
		t.Fatalf("block[0].Type = %q, want tool_use", contentBlocks[0].Type)
	}
	if contentBlocks[0].ToolUse.ID != "call_a" {
		t.Errorf("block[0].ID = %q, want call_a", contentBlocks[0].ToolUse.ID)
	}
	if contentBlocks[0].ToolUse.Name != "get_weather" {
		t.Errorf("block[0].Name = %q, want get_weather", contentBlocks[0].ToolUse.Name)
	}
	var inputA map[string]string
	if err := json.Unmarshal(contentBlocks[0].ToolUse.Input, &inputA); err != nil {
		t.Fatalf("unmarshal tool A input: %v", err)
	}
	if inputA["location"] != "SF" {
		t.Errorf("tool A location = %q, want SF", inputA["location"])
	}

	// Second tool call.
	if contentBlocks[1].Type != ContentToolUse {
		t.Fatalf("block[1].Type = %q, want tool_use", contentBlocks[1].Type)
	}
	if contentBlocks[1].ToolUse.ID != "call_b" {
		t.Errorf("block[1].ID = %q, want call_b", contentBlocks[1].ToolUse.ID)
	}
	if contentBlocks[1].ToolUse.Name != "get_time" {
		t.Errorf("block[1].Name = %q, want get_time", contentBlocks[1].ToolUse.Name)
	}
	var inputB map[string]string
	if err := json.Unmarshal(contentBlocks[1].ToolUse.Input, &inputB); err != nil {
		t.Fatalf("unmarshal tool B input: %v", err)
	}
	if inputB["timezone"] != "PST" {
		t.Errorf("tool B timezone = %q, want PST", inputB["timezone"])
	}

	// Verify accumulated response.
	response := eventStream.Response()
	if response.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %q, want tool_use", response.StopReason)
	}
	if response.Usage.InputTokens != 60 {
		t.Errorf("InputTokens = %d, want 60", response.Usage.InputTokens)
	}
	if response.Usage.OutputTokens != 40 {
		t.Errorf("OutputTokens = %d, want 40", response.Usage.OutputTokens)
	}
}

func TestOpenAIStreamError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /http/openai/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]string{
				"type":    "server_error",
				"message": "Service unavailable",
			},
		})
	})

	provider := openaiTestServer(t, mux)

	_, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-4o",
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
