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
