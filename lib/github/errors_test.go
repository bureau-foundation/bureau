// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"testing"
)

func TestAPIError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *APIError
		expected string
	}{
		{
			name: "simple message",
			err: &APIError{
				StatusCode: 404,
				Message:    "Not Found",
			},
			expected: "github: HTTP 404: Not Found",
		},
		{
			name: "with validation errors using messages",
			err: &APIError{
				StatusCode: 422,
				Message:    "Validation Failed",
				Errors: []ValidationError{
					{Resource: "Issue", Field: "title", Message: "is required"},
				},
			},
			expected: "github: HTTP 422: Validation Failed; Issue.title: is required",
		},
		{
			name: "with validation errors using codes",
			err: &APIError{
				StatusCode: 422,
				Message:    "Validation Failed",
				Errors: []ValidationError{
					{Resource: "Issue", Field: "title", Code: "missing_field"},
				},
			},
			expected: "github: HTTP 422: Validation Failed; Issue.title: missing_field",
		},
		{
			name: "multiple validation errors",
			err: &APIError{
				StatusCode: 422,
				Message:    "Validation Failed",
				Errors: []ValidationError{
					{Resource: "Issue", Field: "title", Code: "missing_field"},
					{Resource: "Issue", Field: "body", Message: "is too long"},
				},
			},
			expected: "github: HTTP 422: Validation Failed; Issue.title: missing_field; Issue.body: is too long",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.err.Error()
			if got != test.expected {
				t.Errorf("got %q, want %q", got, test.expected)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{StatusCode: 404, Message: "Not Found"}) {
		t.Error("expected IsNotFound for 404")
	}
	if IsNotFound(&APIError{StatusCode: 403, Message: "Forbidden"}) {
		t.Error("unexpected IsNotFound for 403")
	}
	if IsNotFound(fmt.Errorf("network error")) {
		t.Error("unexpected IsNotFound for non-APIError")
	}
}

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "429 response",
			err:      &APIError{StatusCode: 429, Message: "Too Many Requests"},
			expected: true,
		},
		{
			name:     "403 rate limit exceeded",
			err:      &APIError{StatusCode: 403, Message: "API rate limit exceeded for installation ID 12345"},
			expected: true,
		},
		{
			name:     "403 abuse detection",
			err:      &APIError{StatusCode: 403, Message: "You have triggered an abuse detection mechanism"},
			expected: true,
		},
		{
			name:     "403 permission denied",
			err:      &APIError{StatusCode: 403, Message: "Resource not accessible by integration"},
			expected: false,
		},
		{
			name:     "non-APIError",
			err:      fmt.Errorf("network error"),
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRateLimited(test.err); got != test.expected {
				t.Errorf("IsRateLimited = %v, want %v", got, test.expected)
			}
		})
	}
}

func TestIsValidationFailed(t *testing.T) {
	if !IsValidationFailed(&APIError{StatusCode: 422, Message: "Validation Failed"}) {
		t.Error("expected IsValidationFailed for 422")
	}
	if IsValidationFailed(&APIError{StatusCode: 400, Message: "Bad Request"}) {
		t.Error("unexpected IsValidationFailed for 400")
	}
}

func TestIsConflict(t *testing.T) {
	if !IsConflict(&APIError{StatusCode: 409, Message: "Conflict"}) {
		t.Error("expected IsConflict for 409")
	}
	if IsConflict(&APIError{StatusCode: 422, Message: "Validation Failed"}) {
		t.Error("unexpected IsConflict for 422")
	}
}

func TestAPIError_WrappedInFmt(t *testing.T) {
	// Verify classification works through fmt.Errorf wrapping.
	original := &APIError{StatusCode: 404, Message: "Not Found"}
	wrapped := fmt.Errorf("getting issue: %w", original)
	if !IsNotFound(wrapped) {
		t.Error("IsNotFound should see through fmt.Errorf wrapping")
	}
}
