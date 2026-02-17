// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"fmt"
	"io"
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
