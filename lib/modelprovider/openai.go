// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/llm"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// OpenAIOptions configures an OpenAI-compatible provider.
type OpenAIOptions struct {
	// UnixSocket is the path to a Unix socket for local providers.
	// When set, the HTTP transport routes all connections to this
	// socket regardless of the endpoint hostname. The endpoint URL
	// is still used for path construction.
	UnixSocket string

	// ExtraHeaders are added to every request. Used for provider-specific
	// headers (e.g., OpenRouter's "HTTP-Referer" for rankings).
	ExtraHeaders map[string]string
}

// OpenAIProvider implements Provider for any backend speaking the
// OpenAI API (chat/completions and embeddings endpoints). This covers
// OpenRouter, local llama.cpp, vLLM, and direct OpenAI/Anthropic
// compatible endpoints.
//
// Transport differences (HTTPS vs Unix socket) and authentication
// differences (bearer token vs none) are configured at construction
// time. The provider itself is stateless — all request-specific data
// (model name, credential) comes from the request parameters.
type OpenAIProvider struct {
	httpClient   *http.Client
	endpoint     string // base URL (e.g., "https://openrouter.ai/api/v1")
	extraHeaders map[string]string
}

// NewOpenAI creates a provider for an OpenAI-compatible backend.
//
// The endpoint is the base URL including the API version prefix:
//   - "https://openrouter.ai/api/v1" for OpenRouter
//   - "http://localhost/v1" for local providers (with UnixSocket set)
//   - "https://api.openai.com/v1" for direct OpenAI
//
// The "/chat/completions" and "/embeddings" paths are appended to this.
func NewOpenAI(endpoint string, options OpenAIOptions) *OpenAIProvider {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	if options.UnixSocket != "" {
		socketPath := options.UnixSocket
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
	}

	return &OpenAIProvider{
		httpClient: &http.Client{
			Transport: transport,
			// No timeout: streaming responses are long-lived.
			Timeout: 0,
		},
		endpoint:     strings.TrimRight(endpoint, "/"),
		extraHeaders: options.ExtraHeaders,
	}
}

// Complete implements Provider.Complete. Sends a POST to
// {endpoint}/chat/completions and returns a CompletionStream.
func (provider *OpenAIProvider) Complete(ctx context.Context, request *CompleteRequest) (CompletionStream, error) {
	openaiMessages := make([]openaiMessage, len(request.Messages))
	for i, message := range request.Messages {
		openaiMessages[i] = convertMessage(message)
	}

	requestBody := openaiChatRequest{
		Model:    request.Model,
		Messages: openaiMessages,
		Stream:   request.Stream,
	}
	if request.Stream {
		// Request usage in the final streaming chunk. OpenAI and
		// compatible APIs include usage only when this option is set.
		requestBody.StreamOptions = &openaiStreamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("modelprovider: encoding chat request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, "POST",
		provider.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("modelprovider: creating chat request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	for key, value := range provider.extraHeaders {
		httpRequest.Header.Set(key, value)
	}

	response, err := provider.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("modelprovider: chat request failed: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, parseHTTPError(response)
	}

	if request.Stream {
		return newStreamingCompletion(response), nil
	}
	return newSingleCompletion(response)
}

// Embed implements Provider.Embed. Sends a POST to
// {endpoint}/embeddings and returns the complete result.
func (provider *OpenAIProvider) Embed(ctx context.Context, request *EmbedRequest) (*EmbedResult, error) {
	// OpenAI embeddings API accepts strings, not byte slices.
	inputs := make([]string, len(request.Input))
	for i, input := range request.Input {
		inputs[i] = string(input)
	}

	requestBody := openaiEmbedRequest{
		Model: request.Model,
		Input: inputs,
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("modelprovider: encoding embed request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, "POST",
		provider.endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("modelprovider: creating embed request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	for key, value := range provider.extraHeaders {
		httpRequest.Header.Set(key, value)
	}

	response, err := provider.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("modelprovider: embed request failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, parseHTTPError(response)
	}

	var openaiResponse openaiEmbedResponse
	if err := json.NewDecoder(response.Body).Decode(&openaiResponse); err != nil {
		return nil, fmt.Errorf("modelprovider: decoding embed response: %w", err)
	}

	return convertEmbedResponse(&openaiResponse), nil
}

// Close implements Provider.Close. Closes idle HTTP connections.
func (provider *OpenAIProvider) Close() error {
	provider.httpClient.CloseIdleConnections()
	return nil
}

// --- Streaming completion ---

// streamingCompletion reads SSE events from a streaming chat response.
type streamingCompletion struct {
	response *http.Response
	scanner  *llm.SSEScanner
	current  model.Response
	err      error
	done     bool
}

func newStreamingCompletion(response *http.Response) *streamingCompletion {
	return &streamingCompletion{
		response: response,
		scanner:  llm.NewSSEScanner(response.Body),
	}
}

func (stream *streamingCompletion) Next() bool {
	if stream.done {
		return false
	}

	for stream.scanner.Next() {
		event := stream.scanner.Event()

		// "[DONE]" signals the end of the stream.
		if event.Data == "[DONE]" {
			stream.done = true
			return false
		}

		var chunk openaiChatChunk
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			stream.err = fmt.Errorf("modelprovider: decoding stream chunk: %w", err)
			stream.done = true
			return false
		}

		response := convertStreamChunk(&chunk)
		if response == nil {
			// Empty delta (e.g., role-only or empty content). Skip.
			continue
		}

		stream.current = *response
		return true
	}

	if err := stream.scanner.Err(); err != nil {
		stream.err = fmt.Errorf("modelprovider: reading stream: %w", err)
	}
	stream.done = true
	return false
}

func (stream *streamingCompletion) Response() model.Response {
	return stream.current
}

func (stream *streamingCompletion) Err() error {
	return stream.err
}

func (stream *streamingCompletion) Close() error {
	return stream.response.Body.Close()
}

// --- Single (non-streaming) completion ---

// singleCompletion yields exactly one ResponseDone from a non-streaming
// chat response.
type singleCompletion struct {
	response model.Response
	consumed bool
}

func newSingleCompletion(response *http.Response) (*singleCompletion, error) {
	defer response.Body.Close()

	var openaiResponse openaiChatResponse
	if err := json.NewDecoder(response.Body).Decode(&openaiResponse); err != nil {
		return nil, fmt.Errorf("modelprovider: decoding chat response: %w", err)
	}

	return &singleCompletion{
		response: convertChatResponse(&openaiResponse),
	}, nil
}

func (stream *singleCompletion) Next() bool {
	if stream.consumed {
		return false
	}
	stream.consumed = true
	return true
}

func (stream *singleCompletion) Response() model.Response {
	return stream.response
}

func (stream *singleCompletion) Err() error {
	return nil
}

func (stream *singleCompletion) Close() error {
	return nil
}

// --- OpenAI API types (internal) ---

type openaiChatRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openaiMessage handles both simple text and multimodal content.
// When Content is a string, it serializes as "content": "text".
// When ContentParts is set, it serializes as "content": [...parts...].
type openaiMessage struct {
	Role         string              `json:"role"`
	Content      string              `json:"content,omitempty"`
	ContentParts []openaiContentPart `json:"-"` // marshaled via custom MarshalJSON
}

// MarshalJSON implements json.Marshaler. Produces the OpenAI message
// format: content is a string for text-only messages, an array for
// multimodal messages.
func (message openaiMessage) MarshalJSON() ([]byte, error) {
	if len(message.ContentParts) > 0 {
		type alias struct {
			Role    string              `json:"role"`
			Content []openaiContentPart `json:"content"`
		}
		return json.Marshal(alias{Role: message.Role, Content: message.ContentParts})
	}
	type alias struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	return json.Marshal(alias{Role: message.Role, Content: message.Content})
}

type openaiContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openaiImageURL `json:"image_url,omitempty"`
}

type openaiImageURL struct {
	URL string `json:"url"`
}

type openaiChatResponse struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

type openaiChoice struct {
	Message      *openaiChoiceMessage `json:"message,omitempty"`
	Delta        *openaiChoiceMessage `json:"delta,omitempty"`
	FinishReason *string              `json:"finish_reason"`
}

type openaiChoiceMessage struct {
	Content string `json:"content"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type openaiChatChunk struct {
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiEmbedResponse struct {
	Model string              `json:"model"`
	Data  []openaiEmbedObject `json:"data"`
	Usage *openaiUsage        `json:"usage,omitempty"`
}

type openaiEmbedObject struct {
	Embedding json.RawMessage `json:"embedding"`
}

type openaiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// --- Conversion functions ---

// convertMessage translates a Bureau model.Message to the OpenAI wire
// format. Text-only messages use the simple string content format;
// messages with attachments use the multimodal content parts format.
func convertMessage(message model.Message) openaiMessage {
	if len(message.Attachments) == 0 {
		return openaiMessage{
			Role:    message.Role,
			Content: message.Content,
		}
	}

	// Multimodal: text content + image attachments as data URIs.
	parts := make([]openaiContentPart, 0, 1+len(message.Attachments))
	if message.Content != "" {
		parts = append(parts, openaiContentPart{
			Type: "text",
			Text: message.Content,
		})
	}

	for _, attachment := range message.Attachments {
		dataURI := "data:" + attachment.ContentType + ";base64," +
			base64.StdEncoding.EncodeToString(attachment.Data)
		parts = append(parts, openaiContentPart{
			Type:     "image_url",
			ImageURL: &openaiImageURL{URL: dataURI},
		})
	}

	return openaiMessage{
		Role:         message.Role,
		ContentParts: parts,
	}
}

// convertChatResponse translates a non-streaming OpenAI response to
// a Bureau ResponseDone message.
func convertChatResponse(response *openaiChatResponse) model.Response {
	result := model.Response{
		Type:  model.ResponseDone,
		Model: response.Model,
	}

	if len(response.Choices) > 0 && response.Choices[0].Message != nil {
		result.Content = response.Choices[0].Message.Content
	}

	if response.Usage != nil {
		result.Usage = &model.Usage{
			InputTokens:  response.Usage.PromptTokens,
			OutputTokens: response.Usage.CompletionTokens,
		}
	}

	return result
}

// convertStreamChunk translates a streaming OpenAI chunk to a Bureau
// response. Returns nil if the chunk has no meaningful content (e.g.,
// role-only delta with empty content).
func convertStreamChunk(chunk *openaiChatChunk) *model.Response {
	if len(chunk.Choices) == 0 {
		// Usage-only chunk (final chunk with include_usage).
		if chunk.Usage != nil {
			return &model.Response{
				Type:  model.ResponseDone,
				Model: chunk.Model,
				Usage: &model.Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				},
			}
		}
		return nil
	}

	choice := chunk.Choices[0]

	// Final chunk: finish_reason is set.
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		response := &model.Response{
			Type:  model.ResponseDone,
			Model: chunk.Model,
		}
		if choice.Delta != nil {
			response.Content = choice.Delta.Content
		}
		if chunk.Usage != nil {
			response.Usage = &model.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		return response
	}

	// Delta chunk: incremental content.
	if choice.Delta != nil && choice.Delta.Content != "" {
		return &model.Response{
			Type:    model.ResponseDelta,
			Content: choice.Delta.Content,
		}
	}

	return nil
}

// convertEmbedResponse translates an OpenAI embedding response to a
// Bureau EmbedResult. The float arrays are kept as raw JSON bytes —
// the model service or agent is responsible for interpreting the
// numeric format.
func convertEmbedResponse(response *openaiEmbedResponse) *EmbedResult {
	embeddings := make([][]byte, len(response.Data))
	for i, object := range response.Data {
		embeddings[i] = []byte(object.Embedding)
	}

	result := &EmbedResult{
		Embeddings: embeddings,
		Model:      response.Model,
	}

	if response.Usage != nil {
		result.Usage = model.Usage{
			InputTokens:  response.Usage.PromptTokens,
			OutputTokens: response.Usage.CompletionTokens,
		}
	}

	return result
}

// parseHTTPError reads an error response body and returns a
// descriptive error. Attempts to parse OpenAI-format error JSON;
// falls back to the raw status text.
func parseHTTPError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return fmt.Errorf("modelprovider: HTTP %d (could not read error body: %v)",
			response.StatusCode, err)
	}

	var openaiError openaiErrorResponse
	if err := json.Unmarshal(body, &openaiError); err == nil && openaiError.Error.Message != "" {
		return fmt.Errorf("modelprovider: HTTP %d: %s (type=%s, code=%s)",
			response.StatusCode, openaiError.Error.Message,
			openaiError.Error.Type, openaiError.Error.Code)
	}

	return fmt.Errorf("modelprovider: HTTP %d: %s", response.StatusCode, string(body))
}
