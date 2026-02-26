// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import "fmt"

// ErrorCategory classifies tool errors so that MCP clients can make
// programmatic decisions (retry, fix input, escalate) without parsing
// error message text.
type ErrorCategory string

const (
	// CategoryValidation indicates the caller provided invalid input:
	// missing required parameters, wrong argument count, unparseable
	// values. The caller should fix the input and retry.
	CategoryValidation ErrorCategory = "validation"

	// CategoryNotFound indicates a referenced resource does not exist:
	// unknown ticket ID, unresolved room alias, missing dependency.
	// Retrying with the same parameters will not help.
	CategoryNotFound ErrorCategory = "not_found"

	// CategoryForbidden indicates the caller lacks permission for the
	// requested operation. The caller should escalate or request access.
	CategoryForbidden ErrorCategory = "forbidden"

	// CategoryConflict indicates the operation conflicts with existing
	// state: duplicate resource, concurrent modification, already-exists.
	CategoryConflict ErrorCategory = "conflict"

	// CategoryTransient indicates a temporary failure: network error,
	// timeout, rate limit. The caller should back off and retry.
	CategoryTransient ErrorCategory = "transient"

	// CategoryInternal indicates an unexpected error: bugs, I/O
	// failures, parse errors on data the system produced. The caller
	// should report the error rather than retry.
	CategoryInternal ErrorCategory = "internal"
)

// ToolError is a categorized error returned by CLI commands. The MCP
// server inspects the Category to produce structured error metadata
// alongside the human-readable error text, enabling agents to make
// programmatic recovery decisions.
//
// ToolError wraps an inner error, preserving the full error chain for
// debugging while adding category metadata for the MCP layer. Use the
// category-specific constructors (Validation, NotFound, etc.) rather
// than constructing ToolError directly.
type ToolError struct {
	// Category classifies the error for programmatic handling.
	Category ErrorCategory

	// Err is the underlying error with the human-readable message.
	Err error

	// Hint is optional actionable guidance for the caller: what command
	// to run, what flag to pass, what prerequisite to check. When
	// non-empty, it is appended to the error string (separated by a
	// blank line) and exposed as a structured field in MCP errorInfo.
	Hint string
}

// Error returns the error message. When a hint is set, it is appended
// after a blank line so that both CLI and MCP consumers see the
// guidance. The category is not included — it travels separately via
// the MCP errorInfo field.
func (e *ToolError) Error() string {
	if e.Hint == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + "\n\n" + e.Hint
}

// Unwrap returns the underlying error, allowing errors.Is and
// errors.As to walk the full chain through the ToolError wrapper.
func (e *ToolError) Unwrap() error { return e.Err }

// WithHint attaches actionable guidance to the error. The hint tells
// the caller what to do: which command to run, which flag to pass,
// which prerequisite to install. Returns the same ToolError for
// chaining.
func (e *ToolError) WithHint(hint string) *ToolError {
	e.Hint = hint
	return e
}

// Validation creates a validation error: the caller provided bad input.
func Validation(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryValidation, Err: fmt.Errorf(format, args...)}
}

// NotFound creates a not-found error: a referenced resource does not exist.
func NotFound(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryNotFound, Err: fmt.Errorf(format, args...)}
}

// Forbidden creates a forbidden error: the caller lacks permission.
func Forbidden(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryForbidden, Err: fmt.Errorf(format, args...)}
}

// Conflict creates a conflict error: the operation conflicts with existing state.
func Conflict(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryConflict, Err: fmt.Errorf(format, args...)}
}

// Transient creates a transient error: a temporary failure that may succeed on retry.
func Transient(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryTransient, Err: fmt.Errorf(format, args...)}
}

// Internal creates an internal error: an unexpected failure, bug, or I/O error.
func Internal(format string, args ...any) *ToolError {
	return &ToolError{Category: CategoryInternal, Err: fmt.Errorf(format, args...)}
}
