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

// Anthropic implements [Provider] for the Anthropic Messages API.
// Requests are sent to the proxy HTTP passthrough at
// /http/{serviceName}/v1/messages, where the proxy injects the API
// key and forwards to Anthropic's servers.
type Anthropic struct {
	httpClient  *http.Client
	serviceName string
}

// NewAnthropic creates an Anthropic provider. The httpClient should
// be the proxy client's transport (from [proxyclient.Client.HTTPClient])
// which dials the proxy Unix socket. The serviceName identifies the
// proxy HTTP service (e.g., "anthropic") that has the API key
// configured for credential injection.
func NewAnthropic(httpClient *http.Client, serviceName string) *Anthropic {
	return &Anthropic{
		httpClient:  httpClient,
		serviceName: serviceName,
	}
}

// Complete sends a non-streaming request and returns the full response.
func (provider *Anthropic) Complete(ctx context.Context, request Request) (*Response, error) {
	wireRequest := provider.buildRequest(request, false)

	httpResponse, err := doProviderRequest(ctx, provider.httpClient,
		provider.endpoint(), wireRequest, "llm/anthropic", false, request.ExtraHeaders)
	if err != nil {
		return nil, err
	}

	return decodeResponse[anthropicResponse](httpResponse, "llm/anthropic")
}

// Stream sends a streaming request and returns an [EventStream].
func (provider *Anthropic) Stream(ctx context.Context, request Request) (*EventStream, error) {
	wireRequest := provider.buildRequest(request, true)

	httpResponse, err := doProviderRequest(ctx, provider.httpClient,
		provider.endpoint(), wireRequest, "llm/anthropic", true, request.ExtraHeaders)
	if err != nil {
		return nil, err
	}

	return provider.newEventStream(httpResponse.Body), nil
}

// endpoint returns the proxy passthrough URL for the Anthropic Messages API.
func (provider *Anthropic) endpoint() string {
	return fmt.Sprintf("http://proxy/http/%s/v1/messages", provider.serviceName)
}

// buildRequest converts our types to Anthropic wire format.
func (provider *Anthropic) buildRequest(request Request, stream bool) anthropicRequest {
	wireRequest := anthropicRequest{
		Model:     request.Model,
		MaxTokens: request.MaxTokens,
		Stream:    stream,
	}

	if request.System != "" {
		wireRequest.System = request.System
	}
	if request.Temperature != nil {
		wireRequest.Temperature = request.Temperature
	}
	if len(request.StopSequences) > 0 {
		wireRequest.StopSequences = request.StopSequences
	}

	for _, message := range request.Messages {
		wireRequest.Messages = append(wireRequest.Messages, toAnthropicMessage(message))
	}

	for _, tool := range request.Tools {
		wireTool := anthropicTool{
			Name:         tool.Name,
			Description:  tool.Description,
			InputSchema:  tool.InputSchema,
			DeferLoading: tool.DeferLoading,
		}
		if tool.Type != "" {
			// Special provider-managed tools (e.g., tool search) send
			// their type but not description or schema — the provider
			// manages them internally.
			wireTool.Type = tool.Type
			wireTool.Description = ""
			wireTool.InputSchema = nil
		}
		wireRequest.Tools = append(wireRequest.Tools, wireTool)
	}

	return wireRequest
}

// newEventStream creates an EventStream that parses Anthropic SSE events.
func (provider *Anthropic) newEventStream(body io.ReadCloser) *EventStream {
	sseScanner := NewSSEScanner(body)

	// Partial state for accumulating content blocks during streaming.
	// Each content_block_start creates an entry; content_block_delta
	// appends to it; content_block_stop finalizes it.
	var partialBlocks []anthropicPartialBlock

	stream := NewEventStream(nil, body)

	stream.next = func() (StreamEvent, error) {
		for {
			if !sseScanner.Next() {
				if err := sseScanner.Err(); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: reading SSE: %w", err)
				}
				return StreamEvent{}, io.EOF
			}

			sseEvent := sseScanner.Event()

			switch sseEvent.Type {
			case "message_start":
				var envelope struct {
					Message struct {
						Model string         `json:"model"`
						Usage anthropicUsage `json:"usage"`
					} `json:"message"`
				}
				if err := json.Unmarshal([]byte(sseEvent.Data), &envelope); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: parsing message_start: %w", err)
				}
				stream.SetModel(envelope.Message.Model)
				stream.SetUsage(Usage{
					InputTokens:      envelope.Message.Usage.InputTokens,
					CacheReadTokens:  envelope.Message.Usage.CacheReadInputTokens,
					CacheWriteTokens: envelope.Message.Usage.CacheCreationInputTokens,
				})
				continue

			case "content_block_start":
				var envelope struct {
					Index        int                   `json:"index"`
					ContentBlock anthropicContentBlock `json:"content_block"`
				}
				if err := json.Unmarshal([]byte(sseEvent.Data), &envelope); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: parsing content_block_start: %w", err)
				}
				// Grow the partial blocks slice if needed.
				for len(partialBlocks) <= envelope.Index {
					partialBlocks = append(partialBlocks, anthropicPartialBlock{})
				}
				partial := anthropicPartialBlock{
					blockType: envelope.ContentBlock.Type,
					toolUseID: envelope.ContentBlock.ID,
					toolName:  envelope.ContentBlock.Name,
				}
				// server_tool_use uses ID for the tool use ID.
				// tool_search_tool_result uses ToolUseID to reference
				// the server_tool_use it responds to.
				if envelope.ContentBlock.ToolUseID != "" {
					partial.toolUseID = envelope.ContentBlock.ToolUseID
				}
				// Blocks that arrive fully formed (tool_search_tool_result)
				// carry their content in the start event, not via deltas.
				if len(envelope.ContentBlock.Content) > 0 {
					partial.rawContent = envelope.ContentBlock.Content
				}
				partialBlocks[envelope.Index] = partial
				continue

			case "content_block_delta":
				var envelope struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(sseEvent.Data), &envelope); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: parsing content_block_delta: %w", err)
				}

				if envelope.Index < len(partialBlocks) {
					block := &partialBlocks[envelope.Index]
					switch envelope.Delta.Type {
					case "text_delta":
						block.textContent.WriteString(envelope.Delta.Text)
						return StreamEvent{
							Type: EventTextDelta,
							Text: envelope.Delta.Text,
						}, nil
					case "input_json_delta":
						block.inputJSON.WriteString(envelope.Delta.PartialJSON)
						// Input JSON deltas are not surfaced as events — the
						// agent loop only cares about the complete tool_use
						// block, emitted on content_block_stop.
					}
				}
				continue

			case "content_block_stop":
				var envelope struct {
					Index int `json:"index"`
				}
				if err := json.Unmarshal([]byte(sseEvent.Data), &envelope); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: parsing content_block_stop: %w", err)
				}

				if envelope.Index < len(partialBlocks) {
					block := partialBlocks[envelope.Index]
					contentBlock := block.toContentBlock()
					return StreamEvent{
						Type:         EventContentBlockDone,
						ContentBlock: contentBlock,
					}, nil
				}
				continue

			case "message_delta":
				var envelope struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(sseEvent.Data), &envelope); err != nil {
					return StreamEvent{}, fmt.Errorf("llm/anthropic: parsing message_delta: %w", err)
				}
				stream.SetStopReason(mapAnthropicStopReason(envelope.Delta.StopReason))
				stream.AddOutputTokens(envelope.Usage.OutputTokens)
				continue

			case "message_stop":
				return StreamEvent{Type: EventDone}, nil

			case "ping":
				return StreamEvent{Type: EventPing}, nil

			case "error":
				var envelope struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(sseEvent.Data), &envelope) == nil {
					return StreamEvent{
						Type:  EventError,
						Error: fmt.Errorf("llm/anthropic: stream error: %s: %s", envelope.Error.Type, envelope.Error.Message),
					}, nil
				}
				return StreamEvent{
					Type:  EventError,
					Error: fmt.Errorf("llm/anthropic: stream error: %s", sseEvent.Data),
				}, nil

			default:
				// Unknown event types are silently skipped. Anthropic may
				// add new event types; defensive parsing prevents breakage.
				continue
			}
		}
	}

	return stream
}

// --- Anthropic wire types ---
//
// These map directly to the Anthropic Messages API JSON format.
// They are separate from the public types because the wire format
// uses snake_case, has provider-specific fields, and represents
// content blocks differently (single-level discriminated union
// vs. our nested struct approach).

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// anthropicPartialBlock tracks the state of a content block being
// assembled from streaming events.
type anthropicPartialBlock struct {
	blockType   string
	textContent strings.Builder
	inputJSON   strings.Builder
	toolUseID   string
	toolName    string
	// rawContent holds content for blocks that arrive fully formed
	// in content_block_start (e.g., tool_search_tool_result). These
	// blocks have no deltas — the complete content is in the start event.
	rawContent json.RawMessage
}

func (block *anthropicPartialBlock) toContentBlock() ContentBlock {
	switch block.blockType {
	case "text":
		return TextBlock(block.textContent.String())
	case "tool_use":
		return ToolUseBlock(
			block.toolUseID,
			block.toolName,
			json.RawMessage(block.inputJSON.String()),
		)
	case "server_tool_use":
		// Server tool use accumulates input JSON via deltas, same as
		// regular tool_use.
		return ContentBlock{
			Type: ContentServerToolUse,
			ServerToolUse: &ServerToolUse{
				ID:    block.toolUseID,
				Name:  block.toolName,
				Input: json.RawMessage(block.inputJSON.String()),
			},
		}
	case "tool_search_tool_result":
		// Tool search results arrive fully formed — use rawContent
		// captured from content_block_start, not from deltas.
		return ContentBlock{
			Type: ContentServerToolResult,
			ServerToolResult: &ServerToolResult{
				ToolUseID: block.toolUseID,
				Content:   block.rawContent,
			},
		}
	default:
		// Unknown block types are preserved as text with a type prefix.
		return TextBlock(fmt.Sprintf("[%s] %s", block.blockType, block.textContent.String()))
	}
}

// --- Wire type conversions ---

func toAnthropicMessage(message Message) anthropicMessage {
	wire := anthropicMessage{Role: string(message.Role)}
	for _, block := range message.Content {
		wire.Content = append(wire.Content, toAnthropicContentBlock(block))
	}
	return wire
}

func toAnthropicContentBlock(block ContentBlock) anthropicContentBlock {
	switch block.Type {
	case ContentText:
		return anthropicContentBlock{Type: "text", Text: block.Text}
	case ContentToolUse:
		if block.ToolUse != nil {
			return anthropicContentBlock{
				Type:  "tool_use",
				ID:    block.ToolUse.ID,
				Name:  block.ToolUse.Name,
				Input: block.ToolUse.Input,
			}
		}
	case ContentToolResult:
		if block.ToolResult != nil {
			// Content is a string, but the wire format expects
			// json.RawMessage. Marshal the string to a JSON string
			// value so the wire representation is correct.
			contentJSON, _ := json.Marshal(block.ToolResult.Content)
			return anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ToolResult.ToolUseID,
				Content:   contentJSON,
				IsError:   block.ToolResult.IsError,
			}
		}
	case ContentServerToolUse:
		if block.ServerToolUse != nil {
			return anthropicContentBlock{
				Type:  "server_tool_use",
				ID:    block.ServerToolUse.ID,
				Name:  block.ServerToolUse.Name,
				Input: block.ServerToolUse.Input,
			}
		}
	case ContentServerToolResult:
		if block.ServerToolResult != nil {
			return anthropicContentBlock{
				Type:      "tool_search_tool_result",
				ToolUseID: block.ServerToolResult.ToolUseID,
				Content:   block.ServerToolResult.Content,
			}
		}
	}
	return anthropicContentBlock{Type: string(block.Type)}
}

func (wireResponse *anthropicResponse) toResponse() *Response {
	response := &Response{
		StopReason: mapAnthropicStopReason(wireResponse.StopReason),
		Model:      wireResponse.Model,
		Usage: Usage{
			InputTokens:      wireResponse.Usage.InputTokens,
			OutputTokens:     wireResponse.Usage.OutputTokens,
			CacheReadTokens:  wireResponse.Usage.CacheReadInputTokens,
			CacheWriteTokens: wireResponse.Usage.CacheCreationInputTokens,
		},
	}
	for _, wireBlock := range wireResponse.Content {
		response.Content = append(response.Content, fromAnthropicContentBlock(wireBlock))
	}
	return response
}

func fromAnthropicContentBlock(wire anthropicContentBlock) ContentBlock {
	switch wire.Type {
	case "text":
		return TextBlock(wire.Text)
	case "tool_use":
		return ToolUseBlock(wire.ID, wire.Name, wire.Input)
	case "server_tool_use":
		return ContentBlock{
			Type: ContentServerToolUse,
			ServerToolUse: &ServerToolUse{
				ID:    wire.ID,
				Name:  wire.Name,
				Input: wire.Input,
			},
		}
	case "tool_search_tool_result":
		return ContentBlock{
			Type: ContentServerToolResult,
			ServerToolResult: &ServerToolResult{
				ToolUseID: wire.ToolUseID,
				Content:   wire.Content,
			},
		}
	default:
		return TextBlock(fmt.Sprintf("[%s] %s", wire.Type, wire.Text))
	}
}

func mapAnthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn":
		return StopReasonEndTurn
	case "tool_use":
		return StopReasonToolUse
	case "max_tokens":
		return StopReasonMaxTokens
	case "stop_sequence":
		return StopReasonStopSequence
	default:
		return StopReason(reason)
	}
}
