// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

// CBOR wire protocol types for the model service's native API. These
// types flow over the service socket between agents and the model
// service. They use CBOR with keyasint encoding for compact binary
// transmission — integer map keys instead of field name strings.
//
// Binary data (images, audio, video, embedding vectors) is carried as
// raw byte slices, not base64-encoded strings. This is the primary
// advantage of CBOR over JSON for the model API: a 1MB image is 1MB
// on the wire, not 1.33MB.
//
// The HTTP compatibility layer (model-service.md) translates between
// provider-native JSON APIs and internal routing. These CBOR types are
// not used on the HTTP path — they are exclusively for Bureau agents
// speaking the native protocol.

// LatencyPolicy controls how the model service handles request
// scheduling. Agents choose the policy based on how urgently they need
// the response.
type LatencyPolicy string

const (
	// LatencyImmediate forwards the request to the provider without
	// delay. This is the default when no policy is specified. The
	// only mode available on the HTTP compatibility layer.
	LatencyImmediate LatencyPolicy = "immediate"

	// LatencyBatch allows the model service to accumulate requests
	// and send them as a single batch to the provider. The batch
	// accumulator fires when it reaches the provider's max batch
	// size or a configurable timeout (whichever comes first).
	// Suited for embedding workloads, background analysis, and
	// other latency-tolerant tasks.
	LatencyBatch LatencyPolicy = "batch"

	// LatencyBackground is the lowest priority. Requests yield to
	// immediate and batch traffic, using idle capacity. Suited for
	// large analysis jobs and bulk processing.
	LatencyBackground LatencyPolicy = "background"
)

// IsKnown reports whether p is one of the defined LatencyPolicy values.
func (p LatencyPolicy) IsKnown() bool {
	switch p {
	case LatencyImmediate, LatencyBatch, LatencyBackground:
		return true
	}
	return false
}

// ResponseType identifies the kind of message in a streamed model
// response. Non-streaming responses produce a single "done" message.
type ResponseType string

const (
	// ResponseDelta carries an incremental content fragment during
	// streaming. The agent appends each delta's content to build the
	// full response.
	ResponseDelta ResponseType = "delta"

	// ResponseDone signals completion. Contains the final model name,
	// token usage, and cost. For non-streaming requests, this is the
	// only response message and carries the full content.
	ResponseDone ResponseType = "done"

	// ResponseError signals a failure. The Error field contains a
	// human-readable description. No further messages follow.
	ResponseError ResponseType = "error"
)

// IsKnown reports whether t is one of the defined ResponseType values.
func (t ResponseType) IsKnown() bool {
	switch t {
	case ResponseDelta, ResponseDone, ResponseError:
		return true
	}
	return false
}

// Request is the top-level message an agent sends to the model service
// over the native CBOR API. The Action field determines which operation
// is performed and which fields are relevant:
//
//   - "complete": chat completion. Model, Messages, Stream, LatencyPolicy,
//     and ContinuationID are used. Input is ignored.
//   - "embed": embedding. Model, Input, and LatencyPolicy are used.
//     Messages, Stream, and ContinuationID are ignored.
type Request struct {
	// Action is the operation to perform: "complete" or "embed".
	Action string `cbor:"1,keyasint"`

	// Model is the alias name (state key in EventTypeModelAlias) to
	// route this request to. "auto" selects based on capabilities.
	Model string `cbor:"2,keyasint"`

	// Messages is the conversation for a completion request. Each
	// message has a role, text content, and optional binary attachments.
	Messages []Message `cbor:"3,keyasint,omitempty"`

	// Stream requests incremental delivery of the completion response.
	// When true, the model service sends ResponseDelta messages as
	// tokens arrive, followed by a final ResponseDone. When false,
	// a single ResponseDone contains the complete response.
	Stream bool `cbor:"4,keyasint,omitempty"`

	// LatencyPolicy controls request scheduling. Defaults to
	// LatencyImmediate when empty.
	LatencyPolicy LatencyPolicy `cbor:"5,keyasint,omitempty"`

	// ContinuationID resumes a prior conversation. The model service
	// maintains conversation state keyed by (agent, continuation_id).
	// Previous messages are automatically prepended. The agent does
	// not resend history. Continuations expire after a configurable
	// TTL (default: 1 hour).
	ContinuationID string `cbor:"6,keyasint,omitempty"`

	// Input is a list of byte slices for embedding requests. Each
	// element is the raw text to embed. Using byte slices rather than
	// strings allows non-UTF-8 binary input for future multimodal
	// embedding models.
	Input [][]byte `cbor:"7,keyasint,omitempty"`
}

// Message is a single turn in a completion conversation.
type Message struct {
	// Role identifies the speaker: "system", "user", or "assistant".
	Role string `cbor:"1,keyasint"`

	// Content is the text content of this message.
	Content string `cbor:"2,keyasint"`

	// Attachments are binary data associated with this message:
	// images, audio, video, documents. Carried as raw bytes in CBOR,
	// avoiding the 33% overhead of base64 encoding.
	Attachments []Attachment `cbor:"3,keyasint,omitempty"`
}

// Attachment is a binary blob attached to a message. The ContentType
// determines how the model service presents it to the provider (e.g.,
// as a base64 data URI for HTTP providers, or as raw bytes for
// CBOR-native providers like future IREE).
type Attachment struct {
	// ContentType is the MIME type (e.g., "image/png", "audio/wav",
	// "application/pdf").
	ContentType string `cbor:"1,keyasint"`

	// Data is the raw binary content. Not base64 — actual bytes.
	Data []byte `cbor:"2,keyasint"`
}

// Usage reports token consumption for a model API call.
type Usage struct {
	// InputTokens is the number of tokens in the input (prompt +
	// system message + attachments).
	InputTokens int64 `cbor:"1,keyasint"`

	// OutputTokens is the number of tokens in the model's response.
	// Zero for embedding requests.
	OutputTokens int64 `cbor:"2,keyasint,omitempty"`
}

// Response is a message from the model service to an agent for a
// completion request. Streaming completions produce multiple
// ResponseDelta messages followed by one ResponseDone. Non-streaming
// completions produce a single ResponseDone with the full content.
// Errors produce a single ResponseError.
type Response struct {
	// Type identifies this message: "delta", "done", or "error".
	Type ResponseType `cbor:"1,keyasint"`

	// Content is the text content. For delta messages, this is the
	// incremental fragment. For done messages (non-streaming), this
	// is the complete response text.
	Content string `cbor:"2,keyasint,omitempty"`

	// Model is the actual provider model used (e.g., "openai/gpt-4.1"),
	// as distinct from the alias the agent requested. Present only
	// in done messages.
	Model string `cbor:"3,keyasint,omitempty"`

	// Usage is the token consumption for this request. Present only
	// in done messages.
	Usage *Usage `cbor:"4,keyasint,omitempty"`

	// CostMicrodollars is the computed cost of this request in
	// microdollars, derived from Usage and the model's Pricing.
	// Present only in done messages.
	CostMicrodollars int64 `cbor:"5,keyasint,omitempty"`

	// Error is a human-readable error description. Present only in
	// error messages.
	Error string `cbor:"6,keyasint,omitempty"`
}

// EmbedResponse is the response for an embedding request. Unlike
// completion responses, embedding responses are not streamed — the
// full result arrives in a single message.
type EmbedResponse struct {
	// Embeddings is the list of embedding vectors, one per input.
	// Each vector is raw bytes (typically float32 little-endian),
	// not a JSON array of numbers. The caller is responsible for
	// interpreting the byte layout based on the model's specification.
	Embeddings [][]byte `cbor:"1,keyasint"`

	// Model is the actual provider model used.
	Model string `cbor:"2,keyasint"`

	// Usage is the token consumption for this request.
	Usage *Usage `cbor:"3,keyasint,omitempty"`

	// CostMicrodollars is the computed cost in microdollars.
	CostMicrodollars int64 `cbor:"4,keyasint,omitempty"`

	// Error is a human-readable error description. When non-empty,
	// Embeddings is nil and the request failed.
	Error string `cbor:"5,keyasint,omitempty"`
}

// UsageEvent is a telemetry record produced by the model service for
// every API call. These feed into the telemetry pipeline as spans,
// enabling per-project, per-agent, and per-model cost dashboards.
// Quota enforcement reads cumulative spend from these events.
type UsageEvent struct {
	// Project is the project that incurred the cost, extracted from
	// the requesting agent's service token.
	Project string `cbor:"1,keyasint"`

	// Agent is the full Matrix user ID of the requesting agent.
	Agent string `cbor:"2,keyasint"`

	// ModelAlias is the alias the agent requested (e.g., "codex").
	ModelAlias string `cbor:"3,keyasint"`

	// Provider is the backend that served the request.
	Provider string `cbor:"4,keyasint"`

	// ProviderModel is the specific model used at the provider.
	ProviderModel string `cbor:"5,keyasint"`

	// Account is the account name (state key in EventTypeModelAccount)
	// that provided the credential for this request.
	Account string `cbor:"6,keyasint"`

	// InputTokens is the number of input tokens consumed.
	InputTokens int64 `cbor:"7,keyasint"`

	// OutputTokens is the number of output tokens consumed.
	OutputTokens int64 `cbor:"8,keyasint,omitempty"`

	// CostMicrodollars is the computed cost of this request.
	CostMicrodollars int64 `cbor:"9,keyasint"`

	// LatencyMS is the end-to-end latency of the provider API call
	// in milliseconds.
	LatencyMS int64 `cbor:"10,keyasint"`

	// BatchSize is the number of requests that were batched together
	// in this provider API call. 1 for non-batched requests.
	BatchSize int `cbor:"11,keyasint"`
}
