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
}

// Error returns the underlying error message. The category is not
// included in the string — it travels separately via the MCP errorInfo
// field, not in the text content block.
func (e *ToolError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error, allowing errors.Is and
// errors.As to walk the full chain through the ToolError wrapper.
func (e *ToolError) Unwrap() error { return e.Err }

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
