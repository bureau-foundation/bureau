// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package llm provides a provider-agnostic interface for Large Language
// Model APIs with streaming and tool-use support.
//
// The primary abstraction is [Provider], which supports both blocking
// completion and streaming responses. Provider implementations translate
// between the common types in this package and each vendor's wire format.
//
// All HTTP requests go through a caller-supplied [http.Client], which in
// Bureau agents is the proxy client's transport — dialing the proxy Unix
// socket for automatic credential injection. The LLM library never
// handles API keys, TLS configuration, or connection management.
//
// Streaming uses Server-Sent Events (SSE), parsed by [SSEScanner].
// The [EventStream] type wraps a streaming response, yielding
// [StreamEvent] values as they arrive while accumulating the complete
// [Response] internally.
//
// Current provider implementations:
//   - [Anthropic]: Claude models via the Messages API (/v1/messages)
package llm
