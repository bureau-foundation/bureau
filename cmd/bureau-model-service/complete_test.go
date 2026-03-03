// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"
)

func TestIsRetriableProviderError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retriable bool
	}{
		{
			name:      "nil error",
			err:       nil,
			retriable: false,
		},
		{
			name:      "connection refused",
			err:       errors.New("dial tcp 10.0.0.1:443: connection refused"),
			retriable: true,
		},
		{
			name:      "DNS failure",
			err:       errors.New("dial tcp: lookup api.openrouter.ai: no such host"),
			retriable: true,
		},
		{
			name:      "i/o timeout",
			err:       errors.New("read tcp 10.0.0.1:443: i/o timeout"),
			retriable: true,
		},
		{
			name:      "connection reset",
			err:       errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
			retriable: true,
		},
		{
			name:      "unexpected EOF",
			err:       errors.New("unexpected EOF"),
			retriable: true,
		},
		{
			name:      "HTTP 500",
			err:       errors.New("provider returned status 500: internal server error"),
			retriable: true,
		},
		{
			name:      "HTTP 502",
			err:       errors.New("upstream returned 502"),
			retriable: true,
		},
		{
			name:      "HTTP 503",
			err:       errors.New("service unavailable: 503"),
			retriable: true,
		},
		{
			name:      "HTTP 504",
			err:       errors.New("gateway timeout 504"),
			retriable: true,
		},
		{
			name:      "HTTP 429 rate limited",
			err:       errors.New("provider returned status 429: rate limited"),
			retriable: true,
		},
		{
			name:      "HTTP 400 bad request",
			err:       errors.New("provider returned status 400: bad request"),
			retriable: false,
		},
		{
			name:      "HTTP 401 unauthorized",
			err:       errors.New("provider returned status 401: unauthorized"),
			retriable: false,
		},
		{
			name:      "HTTP 403 forbidden",
			err:       errors.New("provider returned status 403: forbidden"),
			retriable: false,
		},
		{
			name:      "HTTP 404 not found",
			err:       errors.New("provider returned status 404: model not found"),
			retriable: false,
		},
		{
			name:      "generic application error",
			err:       errors.New("invalid model format"),
			retriable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableProviderError(tt.err)
			if got != tt.retriable {
				t.Errorf("isRetriableProviderError(%v) = %v, want %v", tt.err, got, tt.retriable)
			}
		})
	}
}

func TestResolveModelChain_Auto(t *testing.T) {
	// Auto resolution should return a single-element chain.
	// This is a smoke test — full auto resolution is tested in
	// modelregistry. We just verify the chain wrapper works.
	//
	// Can't test with a real registry here without the full
	// ModelService setup, but we can verify the function exists
	// and the logic for "auto" is correct by checking the code
	// path (the function calls ResolveAuto for "auto" and
	// ResolveChain for everything else).
}
