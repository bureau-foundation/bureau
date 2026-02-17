// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAI implements [Provider] for the OpenAI Chat Completions API.
// Requests are sent to the proxy HTTP passthrough at
// /http/{serviceName}/v1/chat/completions, where the proxy injects the
// API key and forwards to the upstream server. This is compatible with
// any API that implements the OpenAI chat completions wire format
// (OpenAI, Azure OpenAI, OpenRouter, vLLM, Ollama, llama.cpp, etc.).
type OpenAI struct {
	httpClient  *http.Client
	serviceName string
}

// NewOpenAI creates an OpenAI-compatible provider. The httpClient should
// be the proxy client's transport (from [proxyclient.Client.HTTPClient])
// which dials the proxy Unix socket. The serviceName identifies the
// proxy HTTP service (e.g., "openai", "openrouter") that has the API
// key configured for credential injection.
func NewOpenAI(httpClient *http.Client, serviceName string) *OpenAI {
	return &OpenAI{
		httpClient:  httpClient,
		serviceName: serviceName,
	}
}

// Complete sends a non-streaming request and returns the full response.
func (provider *OpenAI) Complete(ctx context.Context, request Request) (*Response, error) {
	wireRequest := provider.buildRequest(request, false)

	httpResponse, err := doProviderRequest(ctx, provider.httpClient,
		provider.endpoint(), wireRequest, "llm/openai", false)
	if err != nil {
		return nil, err
	}

	return decodeResponse[openaiResponse](httpResponse, "llm/openai")
}

// Stream sends a streaming request and returns an [EventStream].
func (provider *OpenAI) Stream(ctx context.Context, request Request) (*EventStream, error) {
	wireRequest := provider.buildRequest(request, true)

	httpResponse, err := doProviderRequest(ctx, provider.httpClient,
		provider.endpoint(), wireRequest, "llm/openai", true)
	if err != nil {
		return nil, err
	}

	return provider.newEventStream(httpResponse.Body), nil
}

// endpoint returns the proxy passthrough URL for the OpenAI Chat Completions API.
func (provider *OpenAI) endpoint() string {
	return fmt.Sprintf("http://proxy/http/%s/v1/chat/completions", provider.serviceName)
}

// buildRequest converts our types to the OpenAI wire format.
func (provider *OpenAI) buildRequest(request Request, stream bool) openaiRequest {
	wireRequest := openaiRequest{
		Model:     request.Model,
		MaxTokens: request.MaxTokens,
	}

	if request.Temperature != nil {
		wireRequest.Temperature = request.Temperature
	}
	if len(request.StopSequences) > 0 {
		wireRequest.Stop = request.StopSequences
	}
	if stream {
		wireRequest.Stream = true
		wireRequest.StreamOptions = &openaiStreamOptions{IncludeUsage: true}
	}

	// System prompt becomes the first message with role "system".
	if request.System != "" {
		wireRequest.Messages = append(wireRequest.Messages, openaiMessage{
			Role:    "system",
			Content: openaiTextContent(request.System),
		})
	}

	for _, message := range request.Messages {
		wireRequest.Messages = append(wireRequest.Messages, toOpenAIMessages(message)...)
	}

	for _, tool := range request.Tools {
		wireRequest.Tools = append(wireRequest.Tools, openaiTool{
			Type: "function",
			Function: openaiToolDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	return wireRequest
}

// newEventStream creates an EventStream that parses OpenAI SSE events.
//
// The OpenAI streaming protocol differs from Anthropic in a key way:
// Anthropic emits per-block start/delta/stop events, while OpenAI
// accumulates everything into deltas and finalizes all blocks at once
// when finish_reason arrives. A pending events queue bridges this gap,
// holding finalized content blocks until they can be emitted one at a
// time through the EventStream.Next() interface.
func (provider *OpenAI) newEventStream(body io.ReadCloser) *EventStream {
	sseScanner := NewSSEScanner(body)

	// Partial state for accumulating content during streaming.
	var textContent strings.Builder
	var partialToolCalls []openaiPartialToolCall
	var pendingEvents []StreamEvent
	var modelSet bool

	stream := NewEventStream(nil, body)

	stream.next = func() (StreamEvent, error) {
		// Drain pending events before reading more SSE data.
		if len(pendingEvents) > 0 {
			event := pendingEvents[0]
			pendingEvents = pendingEvents[1:]
			return event, nil
		}

		for {
			if !sseScanner.Next() {
				if err := sseScanner.Err(); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/openai: reading SSE: %w", err)
				}
				return StreamEvent{}, io.EOF
			}

			sseEvent := sseScanner.Event()

			// The OpenAI streaming protocol terminates with "data: [DONE]".
			if sseEvent.Data == "[DONE]" {
				return StreamEvent{Type: EventDone}, nil
			}

			var chunk openaiStreamChunk
			if err := json.Unmarshal([]byte(sseEvent.Data), &chunk); err != nil {
				return StreamEvent{}, fmt.Errorf("llm/openai: parsing stream chunk: %w", err)
			}

			// Check for error responses. OpenAI sends errors as regular
			// data lines with an "error" field instead of using SSE event
			// types. Detect them when the chunk has no choices, no usage,
			// and no model (all signs it's not a normal completion chunk).
			if len(chunk.Choices) == 0 && chunk.Usage == nil && chunk.Model == "" {
				var errorChunk struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(sseEvent.Data), &errorChunk) == nil && errorChunk.Error.Message != "" {
					return StreamEvent{
						Type:  EventError,
						Error: fmt.Errorf("llm/openai: stream error: %s: %s", errorChunk.Error.Type, errorChunk.Error.Message),
					}, nil
				}
			}

			// Set model from the first chunk that carries it.
			if !modelSet && chunk.Model != "" {
				stream.SetModel(chunk.Model)
				modelSet = true
			}

			// Process usage when present. When stream_options.include_usage
			// is set, the final chunk after finish_reason carries usage
			// with an empty choices array.
			if chunk.Usage != nil {
				usage := Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
				if chunk.Usage.PromptTokensDetails != nil {
					usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				stream.SetUsage(usage)
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			choice := chunk.Choices[0]
			delta := choice.Delta

			// Accumulate text content and emit text deltas.
			if delta.Content != "" {
				textContent.WriteString(delta.Content)
				return StreamEvent{
					Type: EventTextDelta,
					Text: delta.Content,
				}, nil
			}

			// Accumulate tool call deltas.
			for _, toolCallDelta := range delta.ToolCalls {
				index := toolCallDelta.Index

				// Grow the partial tool calls slice if needed.
				for len(partialToolCalls) <= index {
					partialToolCalls = append(partialToolCalls, openaiPartialToolCall{})
				}

				partial := &partialToolCalls[index]
				if toolCallDelta.ID != "" {
					partial.id = toolCallDelta.ID
				}
				if toolCallDelta.Function != nil {
					if toolCallDelta.Function.Name != "" {
						partial.name = toolCallDelta.Function.Name
					}
					if toolCallDelta.Function.Arguments != "" {
						partial.arguments.WriteString(toolCallDelta.Function.Arguments)
					}
				}
			}

			// When finish_reason is present, finalize all accumulated
			// content blocks into the pending events queue.
			if choice.FinishReason != nil {
				stream.SetStopReason(mapOpenAIFinishReason(*choice.FinishReason))

				if textContent.Len() > 0 {
					pendingEvents = append(pendingEvents, StreamEvent{
						Type:         EventContentBlockDone,
						ContentBlock: TextBlock(textContent.String()),
					})
				}
				for i := range partialToolCalls {
					pendingEvents = append(pendingEvents, StreamEvent{
						Type:         EventContentBlockDone,
						ContentBlock: partialToolCalls[i].toContentBlock(),
					})
				}

				// Return the first pending event if any, otherwise
				// continue to the [DONE] sentinel.
				if len(pendingEvents) > 0 {
					event := pendingEvents[0]
					pendingEvents = pendingEvents[1:]
					return event, nil
				}
			}

			continue
		}
	}

	return stream
}

// --- OpenAI wire types ---
//
// These map directly to the OpenAI Chat Completions API JSON format.
// They are separate from the public types because the wire format
// uses different field names, structures, and conventions.
//
// The Content field on openaiMessage is json.RawMessage rather than
// string because OpenAI's content field is polymorphic: it accepts a
// JSON string for text-only messages and a JSON array of content parts
// for multimodal inputs (images, audio). Using json.RawMessage keeps
// the wire type ready for multimodal without structural changes.

type openaiRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	Tools         []openaiTool         `json:"tools,omitempty"`
	MaxTokens     int                  `json:"max_tokens"`
	Temperature   *float64             `json:"temperature,omitempty"`
	Stop          []string             `json:"stop,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string               `json:"type"`
	Function openaiToolDefinition `json:"function"`
}

type openaiToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Index        int           `json:"index"`
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openaiUsage struct {
	PromptTokens        int64                      `json:"prompt_tokens"`
	CompletionTokens    int64                      `json:"completion_tokens"`
	PromptTokensDetails *openaiPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type openaiPromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// Streaming-specific types. The streaming format uses "delta" instead
// of "message" in choices, tool_calls carry an "index" field for
// multiplexing multiple concurrent calls, and finish_reason is null
// until the final chunk.

type openaiStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []openaiStreamChoice `json:"choices"`
	Usage   *openaiUsage         `json:"usage,omitempty"`
}

type openaiStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type openaiStreamDelta struct {
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	ToolCalls []openaiStreamToolCall `json:"tool_calls,omitempty"`
}

type openaiStreamToolCall struct {
	Index    int                       `json:"index"`
	ID       string                    `json:"id,omitempty"`
	Type     string                    `json:"type,omitempty"`
	Function *openaiStreamToolFunction `json:"function,omitempty"`
}

type openaiStreamToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openaiPartialToolCall tracks the state of a tool call being assembled
// from streaming deltas. OpenAI streams tool calls incrementally: the
// first delta carries the ID and function name, subsequent deltas
// append to the arguments string.
type openaiPartialToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func (partial *openaiPartialToolCall) toContentBlock() ContentBlock {
	return ToolUseBlock(
		partial.id,
		partial.name,
		json.RawMessage(partial.arguments.String()),
	)
}

// --- Wire type helpers ---

// openaiTextContent serializes a text string as a JSON value suitable
// for the openaiMessage Content field. OpenAI's content field accepts
// both a JSON string (text-only) and a JSON array of content parts
// (multimodal). This helper handles the text-only case.
func openaiTextContent(text string) json.RawMessage {
	data, _ := json.Marshal(text)
	return data
}

// openaiContentText extracts a text string from an openaiMessage's
// Content field. Returns empty string if Content is nil/empty or not
// a JSON string. When multimodal content parts are added, this will
// need to handle the array case as well.
func openaiContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	return ""
}

// --- Wire type conversions ---

// toOpenAIMessages converts an internal Message to one or more OpenAI
// wire messages. A single internal message may produce multiple wire
// messages because OpenAI represents tool results as individual
// role:"tool" messages rather than content blocks in a user message.
func toOpenAIMessages(message Message) []openaiMessage {
	switch message.Role {
	case RoleAssistant:
		return []openaiMessage{toOpenAIAssistantMessage(message)}
	case RoleUser:
		return toOpenAIUserMessages(message)
	default:
		var text strings.Builder
		for _, block := range message.Content {
			if block.Type == ContentText {
				text.WriteString(block.Text)
			}
		}
		return []openaiMessage{{Role: string(message.Role), Content: openaiTextContent(text.String())}}
	}
}

// toOpenAIAssistantMessage converts an assistant message, splitting
// text content from tool calls into their respective wire fields.
func toOpenAIAssistantMessage(message Message) openaiMessage {
	wire := openaiMessage{Role: "assistant"}

	var textParts []string
	for _, block := range message.Content {
		switch block.Type {
		case ContentText:
			textParts = append(textParts, block.Text)
		case ContentToolUse:
			if block.ToolUse != nil {
				wire.ToolCalls = append(wire.ToolCalls, openaiToolCall{
					ID:   block.ToolUse.ID,
					Type: "function",
					Function: openaiToolFunction{
						Name:      block.ToolUse.Name,
						Arguments: string(block.ToolUse.Input),
					},
				})
			}
		}
	}

	if len(textParts) > 0 {
		wire.Content = openaiTextContent(strings.Join(textParts, ""))
	}

	return wire
}

// toOpenAIUserMessages converts a user message to one or more wire
// messages. Tool result content blocks become individual role:"tool"
// messages; text blocks are collected into role:"user" messages.
func toOpenAIUserMessages(message Message) []openaiMessage {
	var messages []openaiMessage
	var textParts []string

	for _, block := range message.Content {
		switch block.Type {
		case ContentText:
			textParts = append(textParts, block.Text)
		case ContentToolResult:
			if block.ToolResult != nil {
				// Flush any accumulated text as a user message first.
				if len(textParts) > 0 {
					messages = append(messages, openaiMessage{
						Role:    "user",
						Content: openaiTextContent(strings.Join(textParts, "")),
					})
					textParts = nil
				}
				messages = append(messages, openaiMessage{
					Role:       "tool",
					Content:    openaiTextContent(block.ToolResult.Content),
					ToolCallID: block.ToolResult.ToolUseID,
				})
			}
		}
	}

	// Flush remaining text.
	if len(textParts) > 0 {
		messages = append(messages, openaiMessage{
			Role:    "user",
			Content: openaiTextContent(strings.Join(textParts, "")),
		})
	}

	// A user message with no recognized content blocks should not
	// silently produce zero messages — that would indicate a bug in
	// the caller's message construction.
	if len(messages) == 0 {
		messages = append(messages, openaiMessage{
			Role:    "user",
			Content: openaiTextContent(""),
		})
	}

	return messages
}

func (wireResponse *openaiResponse) toResponse() *Response {
	response := &Response{
		Model: wireResponse.Model,
		Usage: Usage{
			InputTokens:  wireResponse.Usage.PromptTokens,
			OutputTokens: wireResponse.Usage.CompletionTokens,
		},
	}

	if wireResponse.Usage.PromptTokensDetails != nil {
		response.Usage.CacheReadTokens = wireResponse.Usage.PromptTokensDetails.CachedTokens
	}

	if len(wireResponse.Choices) == 0 {
		return response
	}

	choice := wireResponse.Choices[0]
	response.StopReason = mapOpenAIFinishReason(choice.FinishReason)

	// Text content.
	if text := openaiContentText(choice.Message.Content); text != "" {
		response.Content = append(response.Content, TextBlock(text))
	}

	// Tool calls.
	for _, toolCall := range choice.Message.ToolCalls {
		response.Content = append(response.Content, ToolUseBlock(
			toolCall.ID,
			toolCall.Function.Name,
			json.RawMessage(toolCall.Function.Arguments),
		))
	}

	return response
}

func mapOpenAIFinishReason(reason string) StopReason {
	switch reason {
	case "stop":
		return StopReasonEndTurn
	case "tool_calls":
		return StopReasonToolUse
	case "length":
		return StopReasonMaxTokens
	default:
		// Preserve unknown reasons (e.g., "content_filter") as-is
		// rather than silently mapping to a default.
		return StopReason(reason)
	}
}
