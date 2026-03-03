// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"429 rate limit", &messaging.MatrixError{StatusCode: 429, Code: messaging.ErrCodeLimitExceeded}, true},
		{"500 server error", &messaging.MatrixError{StatusCode: 500, Code: messaging.ErrCodeUnknown}, true},
		{"502 bad gateway", &messaging.MatrixError{StatusCode: 502, Code: messaging.ErrCodeUnknown}, true},
		{"403 forbidden", &messaging.MatrixError{StatusCode: 403, Code: messaging.ErrCodeForbidden}, false},
		{"404 not found", &messaging.MatrixError{StatusCode: 404, Code: messaging.ErrCodeNotFound}, false},
		{"400 bad request", &messaging.MatrixError{StatusCode: 400, Code: messaging.ErrCodeInvalidParam}, false},
		{"connection refused", fmt.Errorf("dial tcp: connection refused"), true},
		{"connection reset", fmt.Errorf("read tcp: connection reset by peer"), true},
		{"EOF", fmt.Errorf("unexpected EOF"), true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := isTransientError(test.err)
			if got != test.transient {
				t.Errorf("isTransientError(%v) = %v, want %v", test.err, got, test.transient)
			}
		})
	}
}

func TestRetryAfterFromError(t *testing.T) {
	t.Parallel()

	t.Run("429 with retry_after_ms", func(t *testing.T) {
		err := &messaging.MatrixError{
			StatusCode:   429,
			Code:         messaging.ErrCodeLimitExceeded,
			RetryAfterMS: 5000,
		}
		got := retryAfterFromError(err)
		if got != 5*time.Second {
			t.Errorf("retryAfterFromError = %v, want 5s", got)
		}
	})

	t.Run("429 without retry_after_ms", func(t *testing.T) {
		err := &messaging.MatrixError{
			StatusCode: 429,
			Code:       messaging.ErrCodeLimitExceeded,
		}
		got := retryAfterFromError(err)
		if got != 0 {
			t.Errorf("retryAfterFromError = %v, want 0", got)
		}
	})

	t.Run("non-429 matrix error", func(t *testing.T) {
		err := &messaging.MatrixError{
			StatusCode: 500,
			Code:       messaging.ErrCodeUnknown,
		}
		got := retryAfterFromError(err)
		if got != 0 {
			t.Errorf("retryAfterFromError = %v, want 0", got)
		}
	})

	t.Run("non-matrix error", func(t *testing.T) {
		err := fmt.Errorf("connection reset")
		got := retryAfterFromError(err)
		if got != 0 {
			t.Errorf("retryAfterFromError = %v, want 0", got)
		}
	})
}
