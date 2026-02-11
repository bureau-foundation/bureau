// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// Request is the JSON request format for proxy calls.
type Request struct {
	Service string   `json:"service"`          // Service name from config
	Args    []string `json:"args"`             // Arguments (interpretation depends on service type)
	Input   string   `json:"input,omitempty"`  // Optional stdin input for the command
	Stream  bool     `json:"stream,omitempty"` // If true, stream output as newline-delimited JSON
}

// Response is the JSON response format for non-streaming calls.
type Response struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// StreamChunk is a single chunk in a streaming response.
// Streaming uses newline-delimited JSON with chunked transfer encoding.
type StreamChunk struct {
	Type string `json:"type"` // "stdout", "stderr", or "exit"
	Data string `json:"data,omitempty"`
	Code int    `json:"code,omitempty"` // Only for type="exit"
}

// Service is the interface for proxied services.
// Different service types (CLI, HTTP, etc.) implement this interface.
type Service interface {
	// Name returns the service name for logging.
	Name() string

	// Execute processes a request and returns a result.
	// Output is buffered and returned when the command completes.
	// The input parameter provides optional stdin data for the command.
	Execute(ctx context.Context, args []string, input string) (*ExecutionResult, error)
}

// StreamingService is an optional interface for services that support streaming.
// Services that implement this interface can stream stdout/stderr as they arrive.
type StreamingService interface {
	Service

	// ExecuteStream runs the command and streams output to the provided writers.
	// Returns the exit code when complete.
	// The input parameter provides optional stdin data for the command.
	ExecuteStream(ctx context.Context, args []string, input string, stdout, stderr io.Writer) (int, error)
}

// ExecutionResult holds the result of executing a service request.
type ExecutionResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Filter validates whether a request should be allowed.
// Different filter types can implement different validation logic.
type Filter interface {
	// Check returns nil if the request is allowed, or an error explaining why not.
	Check(args []string) error
}

// CredentialSource provides credentials for services.
// This abstraction allows different credential backends (env, systemd, vault, etc.)
//
// Get returns a borrowed *secret.Buffer — the source retains ownership and the
// caller must NOT close it. Returns nil when the credential is not found.
//
// Close releases all mmap-backed buffers held by the source. The creator of
// a CredentialSource is responsible for calling Close; consumers (services)
// borrow references and must not close the source.
type CredentialSource interface {
	Get(name string) *secret.Buffer
	Close() error
}
