// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// --- Test helpers ---

// openaiServer creates an httptest.Server that responds to OpenAI API
// endpoints. The handler validates the request and returns canned
// responses.
func openaiServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *OpenAIProvider) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	provider := NewOpenAI(server.URL+"/v1", OpenAIOptions{})
	t.Cleanup(func() { provider.Close() })

	return server, provider
}

// --- Non-streaming completion ---

func TestComplete_NonStreaming(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", request.URL.Path)
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}

		var body openaiChatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}

		if body.Model != "openai/gpt-4.1" {
			t.Errorf("Model = %q, want %q", body.Model, "openai/gpt-4.1")
		}
		if len(body.Messages) != 2 {
			t.Errorf("Messages length = %d, want 2", len(body.Messages))
		}
		if body.Stream {
			t.Error("Stream = true, want false")
		}

		response := openaiChatResponse{
			Model: "openai/gpt-4.1",
			Choices: []openaiChoice{
				{
					Message:      &openaiChoiceMessage{Content: "The function looks correct."},
					FinishReason: stringPointer("stop"),
				},
			},
			Usage: &openaiUsage{
				PromptTokens:     150,
				CompletionTokens: 42,
			},
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(response)
	})

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model: "openai/gpt-4.1",
		Messages: []model.Message{
			{Role: "system", Content: "You are a code reviewer."},
			{Role: "user", Content: "Review this function."},
		},
		Stream: false,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()

	if !stream.Next() {
		t.Fatal("expected one response chunk")
	}

	response := stream.Response()
	if response.Type != model.ResponseDone {
		t.Errorf("Type = %q, want %q", response.Type, model.ResponseDone)
	}
	if response.Content != "The function looks correct." {
		t.Errorf("Content = %q, want %q", response.Content, "The function looks correct.")
	}
	if response.Model != "openai/gpt-4.1" {
		t.Errorf("Model = %q, want %q", response.Model, "openai/gpt-4.1")
	}
	if response.Usage == nil {
		t.Fatal("Usage is nil")
	}
	if response.Usage.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150", response.Usage.InputTokens)
	}
	if response.Usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42", response.Usage.OutputTokens)
	}

	if stream.Next() {
		t.Error("expected no more chunks")
	}
	if err := stream.Err(); err != nil {
		t.Errorf("Err: %v", err)
	}
}

// --- Streaming completion ---

func TestComplete_Streaming(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		var body openaiChatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}

		if !body.Stream {
			t.Error("Stream = false, want true")
		}

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flush")
		}

		// Send three deltas and a done.
		chunks := []string{
			`{"model":"gpt-4.1","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"model":"gpt-4.1","choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"model":"gpt-4.1","choices":[{"delta":{"content":"!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}

		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	})

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model: "gpt-4.1",
		Messages: []model.Message{
			{Role: "user", Content: "Hello"},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()

	// Collect all chunks.
	var chunks []model.Response
	for stream.Next() {
		chunks = append(chunks, stream.Response())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}

	// First two are deltas.
	if chunks[0].Type != model.ResponseDelta {
		t.Errorf("chunks[0].Type = %q, want delta", chunks[0].Type)
	}
	if chunks[0].Content != "Hello" {
		t.Errorf("chunks[0].Content = %q, want %q", chunks[0].Content, "Hello")
	}

	if chunks[1].Type != model.ResponseDelta {
		t.Errorf("chunks[1].Type = %q, want delta", chunks[1].Type)
	}
	if chunks[1].Content != " world" {
		t.Errorf("chunks[1].Content = %q, want %q", chunks[1].Content, " world")
	}

	// Last chunk is done with usage.
	if chunks[2].Type != model.ResponseDone {
		t.Errorf("chunks[2].Type = %q, want done", chunks[2].Type)
	}
	if chunks[2].Content != "!" {
		t.Errorf("chunks[2].Content = %q, want %q", chunks[2].Content, "!")
	}
	if chunks[2].Usage == nil {
		t.Fatal("chunks[2].Usage is nil")
	}
	if chunks[2].Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", chunks[2].Usage.InputTokens)
	}
	if chunks[2].Usage.OutputTokens != 3 {
		t.Errorf("OutputTokens = %d, want 3", chunks[2].Usage.OutputTokens)
	}
}

// --- Streaming with usage in separate final chunk ---

func TestComplete_StreamingUsageSeparateChunk(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)

		// Some providers send usage in a separate final chunk with
		// no choices (just usage).
		chunks := []string{
			`{"model":"gpt-4.1","choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
			`{"model":"gpt-4.1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}

		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	})

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "Hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()

	var chunks []model.Response
	for stream.Next() {
		chunks = append(chunks, stream.Response())
	}

	// "Hi" delta, finish_reason="stop" done, usage-only done.
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}

	// Last chunk should have usage.
	last := chunks[2]
	if last.Type != model.ResponseDone {
		t.Errorf("last.Type = %q, want done", last.Type)
	}
	if last.Usage == nil {
		t.Fatal("last.Usage is nil")
	}
	if last.Usage.InputTokens != 5 {
		t.Errorf("InputTokens = %d, want 5", last.Usage.InputTokens)
	}
}

// --- Embeddings ---

func TestEmbed(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/embeddings" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}

		var body openaiEmbedRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}

		if body.Model != "nomic-embed-text-v1.5" {
			t.Errorf("Model = %q, want %q", body.Model, "nomic-embed-text-v1.5")
		}
		if len(body.Input) != 2 {
			t.Errorf("Input length = %d, want 2", len(body.Input))
		}

		response := openaiEmbedResponse{
			Model: "nomic-embed-text-v1.5",
			Data: []openaiEmbedObject{
				{Embedding: json.RawMessage(`[0.1, 0.2, 0.3]`)},
				{Embedding: json.RawMessage(`[0.4, 0.5, 0.6]`)},
			},
			Usage: &openaiUsage{PromptTokens: 24},
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(response)
	})

	result, err := provider.Embed(context.Background(), &EmbedRequest{
		Model: "nomic-embed-text-v1.5",
		Input: [][]byte{[]byte("hello world"), []byte("test input")},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(result.Embeddings) != 2 {
		t.Fatalf("Embeddings length = %d, want 2", len(result.Embeddings))
	}
	if result.Model != "nomic-embed-text-v1.5" {
		t.Errorf("Model = %q, want %q", result.Model, "nomic-embed-text-v1.5")
	}
	if result.Usage.InputTokens != 24 {
		t.Errorf("InputTokens = %d, want 24", result.Usage.InputTokens)
	}
}

// --- Extra headers ---

func TestComplete_ExtraHeaders(t *testing.T) {
	var capturedReferer string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		capturedReferer = request.Header.Get("HTTP-Referer")

		response := openaiChatResponse{
			Model:   "gpt-4.1",
			Choices: []openaiChoice{{Message: &openaiChoiceMessage{Content: "ok"}, FinishReason: stringPointer("stop")}},
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(response)
	}))
	t.Cleanup(server.Close)

	provider := NewOpenAI(server.URL+"/v1", OpenAIOptions{
		ExtraHeaders: map[string]string{
			"HTTP-Referer": "https://bureau.foundation",
		},
	})
	t.Cleanup(func() { provider.Close() })

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}

	if capturedReferer != "https://bureau.foundation" {
		t.Errorf("HTTP-Referer = %q, want %q", capturedReferer, "https://bureau.foundation")
	}
}

// --- Error handling ---

func TestComplete_HTTPError(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(writer).Encode(openaiErrorResponse{
			Error: struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			}{
				Message: "Rate limit exceeded",
				Type:    "rate_limit_error",
				Code:    "rate_limit_exceeded",
			},
		})
	})

	_, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "test"}},
	})
	if err == nil {
		t.Fatal("expected error for 429 response")
	}

	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should contain status code 429: %v", err)
	}
	if !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Errorf("error should contain error message: %v", err)
	}
}

func TestComplete_NonJSONError(t *testing.T) {
	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		writer.Write([]byte("internal server error"))
	})

	_, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "test"}},
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code 500: %v", err)
	}
}

// --- Multimodal messages ---

func TestConvertMessage_TextOnly(t *testing.T) {
	message := model.Message{
		Role:    "user",
		Content: "Hello world",
	}

	openaiMsg := convertMessage(message)

	data, err := json.Marshal(openaiMsg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Should produce simple string content, not array.
	if strings.Contains(string(data), `"type"`) {
		t.Errorf("text-only message should not have content parts: %s", data)
	}
}

func TestConvertMessage_WithAttachment(t *testing.T) {
	message := model.Message{
		Role:    "user",
		Content: "What's in this image?",
		Attachments: []model.Attachment{
			{ContentType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		},
	}

	openaiMsg := convertMessage(message)

	data, err := json.Marshal(openaiMsg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Should produce array content with text and image_url parts.
	if !strings.Contains(string(data), `"image_url"`) {
		t.Errorf("multimodal message should have image_url: %s", data)
	}
	if !strings.Contains(string(data), `"data:image/png;base64,`) {
		t.Errorf("image should be base64 data URI: %s", data)
	}
}

// --- Stream options ---

func TestComplete_StreamOptionsIncludeUsage(t *testing.T) {
	var capturedStreamOptions *openaiStreamOptions

	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		var body openaiChatRequest
		json.NewDecoder(request.Body).Decode(&body)
		capturedStreamOptions = body.StreamOptions

		// Return a minimal streaming response.
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		fmt.Fprintf(writer, "data: %s\n\n",
			`{"model":"gpt-4.1","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	})

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "test"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}

	if capturedStreamOptions == nil {
		t.Fatal("stream_options should be set for streaming requests")
	}
	if !capturedStreamOptions.IncludeUsage {
		t.Error("include_usage should be true")
	}
}

func TestComplete_NoStreamOptionsForNonStreaming(t *testing.T) {
	var capturedStreamOptions *openaiStreamOptions

	_, provider := openaiServer(t, func(writer http.ResponseWriter, request *http.Request) {
		var body openaiChatRequest
		json.NewDecoder(request.Body).Decode(&body)
		capturedStreamOptions = body.StreamOptions

		response := openaiChatResponse{
			Model:   "gpt-4.1",
			Choices: []openaiChoice{{Message: &openaiChoiceMessage{Content: "ok"}, FinishReason: stringPointer("stop")}},
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(response)
	})

	stream, err := provider.Complete(context.Background(), &CompleteRequest{
		Model:    "gpt-4.1",
		Messages: []model.Message{{Role: "user", Content: "test"}},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}

	if capturedStreamOptions != nil {
		t.Error("stream_options should not be set for non-streaming requests")
	}
}

func stringPointer(s string) *string {
	return &s
}
