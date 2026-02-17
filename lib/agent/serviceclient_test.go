// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"
	"time"
)

func TestEndSessionRequestFromSummary(t *testing.T) {
	summary := SessionSummary{
		EventCount:       42,
		InputTokens:      1500,
		OutputTokens:     800,
		CacheReadTokens:  200,
		CacheWriteTokens: 100,
		CostUSD:          0.0035,
		ToolCallCount:    5,
		ErrorCount:       1,
		TurnCount:        3,
		Duration:         90500 * time.Millisecond,
	}

	request := EndSessionRequestFromSummary("session-123", summary, "artifact-ref-abc")

	if request.SessionID != "session-123" {
		t.Errorf("SessionID = %q, want %q", request.SessionID, "session-123")
	}
	if request.SessionLogArtifactRef != "artifact-ref-abc" {
		t.Errorf("SessionLogArtifactRef = %q, want %q", request.SessionLogArtifactRef, "artifact-ref-abc")
	}
	if request.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want %d", request.InputTokens, 1500)
	}
	if request.OutputTokens != 800 {
		t.Errorf("OutputTokens = %d, want %d", request.OutputTokens, 800)
	}
	if request.CacheReadTokens != 200 {
		t.Errorf("CacheReadTokens = %d, want %d", request.CacheReadTokens, 200)
	}
	if request.CacheWriteTokens != 100 {
		t.Errorf("CacheWriteTokens = %d, want %d", request.CacheWriteTokens, 100)
	}
	if request.CostUSD != 0.0035 {
		t.Errorf("CostUSD = %f, want %f", request.CostUSD, 0.0035)
	}
	if request.ToolCalls != 5 {
		t.Errorf("ToolCalls = %d, want %d", request.ToolCalls, 5)
	}
	if request.Errors != 1 {
		t.Errorf("Errors = %d, want %d", request.Errors, 1)
	}
	if request.Turns != 3 {
		t.Errorf("Turns = %d, want %d", request.Turns, 3)
	}
	// 90500ms rounds to 91 seconds.
	if request.DurationSeconds != 91 {
		t.Errorf("DurationSeconds = %d, want %d", request.DurationSeconds, 91)
	}
}

func TestEndSessionRequestFromSummary_EmptyArtifactRef(t *testing.T) {
	request := EndSessionRequestFromSummary("session-456", SessionSummary{}, "")

	if request.SessionLogArtifactRef != "" {
		t.Errorf("SessionLogArtifactRef = %q, want empty", request.SessionLogArtifactRef)
	}
	if request.DurationSeconds != 0 {
		t.Errorf("DurationSeconds = %d, want 0", request.DurationSeconds)
	}
}

func TestNewAgentServiceClient_EmptySocketPath(t *testing.T) {
	_, err := NewAgentServiceClient("", "/some/token")
	if err == nil {
		t.Fatal("expected error for empty socket path, got nil")
	}
}

func TestClientSideValidation(t *testing.T) {
	// Create a client with a non-existent socket. We're only testing
	// client-side validation — these calls should fail before dialing.
	client := NewAgentServiceClientFromToken("/nonexistent/agent.sock", []byte("fake-token"))
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"StartSession empty ID", func() error {
			return client.StartSession(ctx, "")
		}},
		{"EndSession empty ID", func() error {
			return client.EndSession(ctx, EndSessionRequest{})
		}},
		{"SetContext empty key", func() error {
			return client.SetContext(ctx, SetContextRequest{ArtifactRef: "ref", ContentType: "text/plain"})
		}},
		{"SetContext empty artifact_ref", func() error {
			return client.SetContext(ctx, SetContextRequest{Key: "k", ContentType: "text/plain"})
		}},
		{"SetContext empty content_type", func() error {
			return client.SetContext(ctx, SetContextRequest{Key: "k", ArtifactRef: "ref"})
		}},
		{"GetContext empty key", func() error {
			_, err := client.GetContext(ctx, "", "")
			return err
		}},
		{"DeleteContext empty key", func() error {
			return client.DeleteContext(ctx, "")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
