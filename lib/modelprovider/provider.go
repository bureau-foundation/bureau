// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package modelprovider defines the Provider interface for model
// inference backends and provides implementations for OpenAI-compatible
// APIs (covering OpenRouter, local llama.cpp, vLLM, and any other
// backend speaking the OpenAI HTTP protocol).
//
// The model service resolves aliases and selects accounts; this package
// handles the actual HTTP transport, request serialization, response
// parsing, and streaming.
//
// model-service.md defines the full design.
package modelprovider

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// Provider sends inference requests to a backend and returns results.
// Implementations handle protocol translation between Bureau's internal
// types and the backend's wire format.
//
// A single Provider instance may serve many concurrent requests. The
// credential is per-request (not per-provider) because different
// projects may use different API keys for the same backend.
type Provider interface {
	// Complete sends a chat completion request and returns a stream
	// of response chunks. The final chunk has Type=ResponseDone with
	// usage statistics. For non-streaming requests (Stream=false),
	// the stream yields exactly one ResponseDone chunk.
	//
	// The caller must call Close on the returned stream when done,
	// even if iteration completed naturally. This releases the
	// underlying HTTP response body and connection.
	Complete(ctx context.Context, request *CompleteRequest) (CompletionStream, error)

	// Embed sends an embedding request and returns the result.
	// Embedding responses are not streamed — the complete result
	// arrives in a single response.
	Embed(ctx context.Context, request *EmbedRequest) (*EmbedResult, error)

	// Close releases resources held by the provider (HTTP connection
	// pools, etc.).
	Close() error
}

// CompleteRequest carries the parameters for a chat completion.
// The model service resolves the alias and selects the credential
// before constructing this request — the provider only sees the
// backend-specific model name and the credential to use.
type CompleteRequest struct {
	// Model is the backend-specific model identifier (e.g.,
	// "openai/gpt-4.1" for OpenRouter, "nomic-embed-text-v1.5"
	// for local llama.cpp). Already resolved from the alias.
	Model string

	// Messages is the conversation to complete.
	Messages []model.Message

	// Stream requests incremental token delivery. When true, the
	// CompletionStream yields multiple ResponseDelta chunks followed
	// by a ResponseDone. When false, a single ResponseDone contains
	// the complete response.
	Stream bool

	// Credential is the API key or bearer token for authentication.
	// Empty for providers with AuthMethodNone (local inference).
	// The model service selects this from the project's account.
	Credential string
}

// EmbedRequest carries the parameters for an embedding request.
type EmbedRequest struct {
	// Model is the backend-specific model identifier.
	Model string

	// Input is the list of texts to embed. Each element is raw bytes
	// (typically UTF-8 text).
	Input [][]byte

	// Credential is the API key for authentication (empty for local).
	Credential string
}

// EmbedResult is the successful result of an embedding request.
type EmbedResult struct {
	// Embeddings is the list of embedding vectors, one per input.
	// Each vector is raw float bytes (the caller interprets the
	// layout based on the model's specification).
	Embeddings [][]byte

	// Model is the actual model identifier used by the backend.
	Model string

	// Usage is the token consumption.
	Usage model.Usage
}

// CompletionStream reads incremental completion responses from a
// provider. The caller iterates with Next()/Response()/Err(),
// following the same scanner pattern as bufio.Scanner and
// llm.SSEScanner.
//
// A stream must always be closed, even after Next() returns false.
type CompletionStream interface {
	// Next advances to the next response chunk. Returns false when
	// the stream ends or an error occurs. The first call to Next
	// blocks until the provider sends the first chunk.
	Next() bool

	// Response returns the current response chunk. Only valid after
	// Next returns true. The chunk is a model.Response with Type
	// set to ResponseDelta or ResponseDone.
	Response() model.Response

	// Err returns the error that stopped iteration, or nil if the
	// stream completed cleanly (the final ResponseDone was received).
	Err() error

	// Close releases resources (HTTP response body, connections).
	// Must be called even if Next() returned false naturally.
	Close() error
}
