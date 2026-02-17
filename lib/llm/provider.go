// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Provider is the interface for LLM API backends. Implementations
// translate between the common types in this package and each
// vendor's wire format.
type Provider interface {
	// Complete sends a request and blocks until the full response
	// is available. Use this when streaming is not needed.
	Complete(ctx context.Context, request Request) (*Response, error)

	// Stream sends a request and returns an [EventStream] that yields
	// events as they arrive. The caller must call [EventStream.Close]
	// when done, even if iteration ended early.
	Stream(ctx context.Context, request Request) (*EventStream, error)
}

// nextFunc is the iteration function for an EventStream. Returns
// io.EOF when the stream is complete.
type nextFunc func() (StreamEvent, error)

// EventStream reads streaming events from an LLM response. It yields
// [StreamEvent] values via [Next] while accumulating the complete
// [Response] internally. After Next returns [io.EOF], call [Response]
// to retrieve the accumulated result.
//
// EventStream is not safe for concurrent use.
type EventStream struct {
	next     nextFunc
	closer   io.Closer
	response Response
	mutex    sync.Mutex
	done     bool
}

// NewEventStream creates an EventStream from a provider-specific
// iteration function and an io.Closer for the underlying resource
// (typically the HTTP response body).
//
// The next function must return (event, nil) for each event and
// (zero, io.EOF) when the stream is complete. The EventStream
// handles accumulation of the complete Response from the events.
func NewEventStream(next nextFunc, closer io.Closer) *EventStream {
	return &EventStream{
		next:   next,
		closer: closer,
	}
}

// Next returns the next event from the stream. Returns io.EOF when
// the stream is complete. After io.EOF, call [Response] to get the
// accumulated result.
//
// The caller should process events in a loop:
//
//	for {
//	    event, err := stream.Next()
//	    if err == io.EOF {
//	        break
//	    }
//	    if err != nil {
//	        return err
//	    }
//	    // process event
//	}
//	response := stream.Response()
func (stream *EventStream) Next() (StreamEvent, error) {
	if stream.done {
		return StreamEvent{}, io.EOF
	}

	event, err := stream.next()
	if err != nil {
		if err == io.EOF {
			stream.done = true
		}
		return event, err
	}

	stream.accumulate(event)
	return event, nil
}

// Response returns the accumulated complete response. Only valid
// after [Next] has returned [io.EOF]. Calling Response before the
// stream is complete returns whatever has been accumulated so far.
func (stream *EventStream) Response() Response {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	return stream.response
}

// Close releases the underlying resources (HTTP response body).
// Must be called when done with the stream, even if iteration
// ended early due to an error or cancellation.
func (stream *EventStream) Close() error {
	if stream.closer != nil {
		return stream.closer.Close()
	}
	return nil
}

// accumulate updates the internal Response from a stream event.
func (stream *EventStream) accumulate(event StreamEvent) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()

	switch event.Type {
	case EventContentBlockDone:
		stream.response.Content = append(stream.response.Content, event.ContentBlock)
	case EventDone:
		// StopReason and Usage are set by the provider's next function
		// via SetStopReason/SetUsage before emitting EventDone.
	}
}

// SetStopReason sets the stop reason on the accumulated response.
// Called by provider implementations during stream parsing.
func (stream *EventStream) SetStopReason(reason StopReason) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	stream.response.StopReason = reason
}

// SetUsage sets the usage statistics on the accumulated response.
// Called by provider implementations during stream parsing.
func (stream *EventStream) SetUsage(usage Usage) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	stream.response.Usage = usage
}

// SetModel sets the model name on the accumulated response.
// Called by provider implementations during stream parsing.
func (stream *EventStream) SetModel(model string) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	stream.response.Model = model
}

// AddOutputTokens increments the output token count. Called by
// provider implementations that receive usage incrementally
// (Anthropic's message_delta event includes only output_tokens).
func (stream *EventStream) AddOutputTokens(count int64) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	stream.response.Usage.OutputTokens += count
}

// ProviderError is returned when the LLM API responds with an error.
type ProviderError struct {
	// StatusCode is the HTTP status code.
	StatusCode int

	// Type is the provider-specific error type string
	// (e.g., "invalid_request_error", "rate_limit_error").
	Type string

	// Message is the human-readable error description.
	Message string
}

func (err *ProviderError) Error() string {
	if err.Type != "" {
		return fmt.Sprintf("llm: HTTP %d: %s: %s", err.StatusCode, err.Type, err.Message)
	}
	return fmt.Sprintf("llm: HTTP %d: %s", err.StatusCode, err.Message)
}

// IsRateLimited returns true if the error is a rate limit response (HTTP 429).
func (err *ProviderError) IsRateLimited() bool {
	return err.StatusCode == 429
}

// IsOverloaded returns true if the error is a server overload response (HTTP 529).
func (err *ProviderError) IsOverloaded() bool {
	return err.StatusCode == 529
}

// doProviderRequest marshals wireRequest as JSON, POSTs it to endpoint
// via httpClient, and returns the HTTP response. Returns a ProviderError
// for non-200 status codes. When streaming is true, the Accept header is
// set to text/event-stream.
//
// On success the caller is responsible for closing the response body.
// On error the body is already closed.
func doProviderRequest(ctx context.Context, httpClient *http.Client, endpoint string, wireRequest any, prefix string, streaming bool) (*http.Response, error) {
	body, err := json.Marshal(wireRequest)
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling request: %w", prefix, err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: creating request: %w", prefix, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if streaming {
		httpRequest.Header.Set("Accept", "text/event-stream")
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("%s: sending request: %w", prefix, err)
	}

	if httpResponse.StatusCode != http.StatusOK {
		defer httpResponse.Body.Close()
		return nil, readProviderError(httpResponse)
	}

	return httpResponse, nil
}

// wireResponse is implemented by pointer-to-struct types that can
// convert themselves from JSON wire format to the common Response.
type wireResponse[T any] interface {
	*T
	toResponse() *Response
}

// decodeResponse reads an HTTP response body as JSON into a
// provider-specific wire response type and converts it to the common
// Response. The HTTP response body is closed when this function returns.
func decodeResponse[T any, P wireResponse[T]](httpResponse *http.Response, prefix string) (*Response, error) {
	defer httpResponse.Body.Close()

	wireResp := P(new(T))
	if err := json.NewDecoder(httpResponse.Body).Decode(wireResp); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", prefix, err)
	}

	return wireResp.toResponse(), nil
}

// readProviderError parses an error response body in the common provider
// error format used by Anthropic, OpenAI, and compatible APIs:
// {"error":{"type":"...","message":"..."}}. Extra fields in the error
// object (such as OpenAI's "code" and "param") are silently ignored.
func readProviderError(httpResponse *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 4096))

	var wireError struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wireError) == nil && wireError.Error.Message != "" {
		return &ProviderError{
			StatusCode: httpResponse.StatusCode,
			Type:       wireError.Error.Type,
			Message:    wireError.Error.Message,
		}
	}

	return &ProviderError{
		StatusCode: httpResponse.StatusCode,
		Message:    string(body),
	}
}
