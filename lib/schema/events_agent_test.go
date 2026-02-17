// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// --- AgentSessionContent ---

func TestAgentSessionContentRoundTrip(t *testing.T) {
	original := AgentSessionContent{
		Version:                  1,
		ActiveSessionID:          "session-12345",
		ActiveSessionStartedAt:   "2026-02-17T10:00:00Z",
		LatestSessionID:          "session-12344",
		LatestSessionArtifactRef: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		SessionIndexArtifactRef:  "f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "active_session_id", "session-12345")
	assertField(t, raw, "active_session_started_at", "2026-02-17T10:00:00Z")

	var decoded AgentSessionContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestAgentSessionContentOmitsEmptyOptionals(t *testing.T) {
	session := AgentSessionContent{
		Version: 1,
	}

	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"active_session_id",
		"active_session_started_at",
		"latest_session_id",
		"latest_session_artifact_ref",
		"session_index_artifact_ref",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestAgentSessionContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		content AgentSessionContent
		wantErr string
	}{
		{
			name:    "valid_empty",
			content: AgentSessionContent{Version: 1},
			wantErr: "",
		},
		{
			name: "valid_with_active_session",
			content: AgentSessionContent{
				Version:                1,
				ActiveSessionID:        "session-123",
				ActiveSessionStartedAt: "2026-02-17T10:00:00Z",
			},
			wantErr: "",
		},
		{
			name: "valid_with_completed_session",
			content: AgentSessionContent{
				Version:                  1,
				LatestSessionID:          "session-123",
				LatestSessionArtifactRef: "abc123",
			},
			wantErr: "",
		},
		{
			name:    "version_zero",
			content: AgentSessionContent{Version: 0},
			wantErr: "version must be >= 1",
		},
		{
			name: "active_id_without_timestamp",
			content: AgentSessionContent{
				Version:         1,
				ActiveSessionID: "session-123",
			},
			wantErr: "active_session_id and active_session_started_at must both be set or both be empty",
		},
		{
			name: "active_timestamp_without_id",
			content: AgentSessionContent{
				Version:                1,
				ActiveSessionStartedAt: "2026-02-17T10:00:00Z",
			},
			wantErr: "active_session_id and active_session_started_at must both be set or both be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestAgentSessionContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", AgentSessionVersion, false},
		{"older_version", AgentSessionVersion - 1, false},
		{"newer_version", AgentSessionVersion + 1, true},
		{"far_future_version", AgentSessionVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := AgentSessionContent{Version: test.version}
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestAgentSessionContentForwardCompatibility(t *testing.T) {
	v2JSON := `{
		"version": 2,
		"active_session_id": "session-42",
		"active_session_started_at": "2026-02-17T10:00:00Z",
		"new_v2_field": "unknown to v1"
	}`

	var content AgentSessionContent
	if err := json.Unmarshal([]byte(v2JSON), &content); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.ActiveSessionID != "session-42" {
		t.Errorf("ActiveSessionID = %q, want %q", content.ActiveSessionID, "session-42")
	}

	if err := content.CanModify(); err == nil {
		t.Fatal("CanModify() = nil for v2 event, want error")
	}

	remarshaled, _ := json.Marshal(content)
	var raw map[string]any
	json.Unmarshal(remarshaled, &raw)
	if _, exists := raw["new_v2_field"]; exists {
		t.Error("new_v2_field survived round-trip through v1 struct (unexpected)")
	}
}

// --- AgentContextContent ---

func TestAgentContextContentRoundTrip(t *testing.T) {
	original := AgentContextContent{
		Version:            1,
		ContextArtifactRef: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		SessionID:          "session-12345",
		MessageCount:       42,
		TokenCount:         150000,
		UpdatedAt:          "2026-02-17T11:30:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "message_count", float64(42))
	assertField(t, raw, "token_count", float64(150000))

	var decoded AgentContextContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestAgentContextContentOmitsEmptyOptionals(t *testing.T) {
	content := AgentContextContent{
		Version: 1,
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"context_artifact_ref",
		"session_id",
		"updated_at",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestAgentContextContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		content AgentContextContent
		wantErr string
	}{
		{
			name:    "valid_empty",
			content: AgentContextContent{Version: 1},
			wantErr: "",
		},
		{
			name: "valid_with_context",
			content: AgentContextContent{
				Version:            1,
				ContextArtifactRef: "abc123",
				SessionID:          "session-42",
				MessageCount:       10,
				TokenCount:         50000,
			},
			wantErr: "",
		},
		{
			name:    "version_zero",
			content: AgentContextContent{Version: 0},
			wantErr: "version must be >= 1",
		},
		{
			name: "artifact_ref_without_session_id",
			content: AgentContextContent{
				Version:            1,
				ContextArtifactRef: "abc123",
			},
			wantErr: "session_id is required when context_artifact_ref is set",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestAgentContextContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", AgentContextVersion, false},
		{"older_version", AgentContextVersion - 1, false},
		{"newer_version", AgentContextVersion + 1, true},
		{"far_future_version", AgentContextVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := AgentContextContent{Version: test.version}
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

// --- AgentMetricsContent ---

func TestAgentMetricsContentRoundTrip(t *testing.T) {
	original := AgentMetricsContent{
		Version:                  1,
		TotalInputTokens:         1500000,
		TotalOutputTokens:        250000,
		TotalCacheReadTokens:     800000,
		TotalCacheWriteTokens:    200000,
		TotalCostMilliUSD:        4500,
		TotalToolCalls:           350,
		TotalTurns:               120,
		TotalErrors:              5,
		TotalSessionCount:        15,
		TotalDurationSeconds:     7200,
		MetricsDetailArtifactRef: "f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2",
		LastSessionID:            "session-12345",
		LastUpdatedAt:            "2026-02-17T12:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "total_input_tokens", float64(1500000))
	assertField(t, raw, "total_cost_milliusd", float64(4500))
	assertField(t, raw, "total_session_count", float64(15))
	assertField(t, raw, "total_duration_seconds", float64(7200))

	var decoded AgentMetricsContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestAgentMetricsContentOmitsEmptyOptionals(t *testing.T) {
	metrics := AgentMetricsContent{
		Version: 1,
	}

	data, err := json.Marshal(metrics)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"metrics_detail_artifact_ref",
		"last_session_id",
		"last_updated_at",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestAgentMetricsContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		content AgentMetricsContent
		wantErr string
	}{
		{
			name:    "valid_zeroed",
			content: AgentMetricsContent{Version: 1},
			wantErr: "",
		},
		{
			name: "valid_with_data",
			content: AgentMetricsContent{
				Version:           1,
				TotalInputTokens:  100000,
				TotalOutputTokens: 25000,
				TotalCostMilliUSD: 500,
				TotalSessionCount: 3,
			},
			wantErr: "",
		},
		{
			name:    "version_zero",
			content: AgentMetricsContent{Version: 0},
			wantErr: "version must be >= 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestAgentMetricsContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", AgentMetricsVersion, false},
		{"older_version", AgentMetricsVersion - 1, false},
		{"newer_version", AgentMetricsVersion + 1, true},
		{"far_future_version", AgentMetricsVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := AgentMetricsContent{Version: test.version}
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestAgentMetricsContentForwardCompatibility(t *testing.T) {
	v2JSON := `{
		"version": 2,
		"total_input_tokens": 500000,
		"total_output_tokens": 100000,
		"total_cost_milliusd": 1200,
		"total_session_count": 5,
		"total_duration_seconds": 3600,
		"new_v2_counter": 42
	}`

	var content AgentMetricsContent
	if err := json.Unmarshal([]byte(v2JSON), &content); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.TotalInputTokens != 500000 {
		t.Errorf("TotalInputTokens = %d, want 500000", content.TotalInputTokens)
	}

	if err := content.CanModify(); err == nil {
		t.Fatal("CanModify() = nil for v2 event, want error")
	}

	remarshaled, _ := json.Marshal(content)
	var raw map[string]any
	json.Unmarshal(remarshaled, &raw)
	if _, exists := raw["new_v2_counter"]; exists {
		t.Error("new_v2_counter survived round-trip through v1 struct (unexpected)")
	}
}

// --- Event type constants ---

func TestAgentEventTypeConstants(t *testing.T) {
	// Verify event type constants use the m.bureau.* namespace.
	for _, eventType := range []struct {
		name     string
		constant string
	}{
		{"AgentSession", EventTypeAgentSession},
		{"AgentContext", EventTypeAgentContext},
		{"AgentMetrics", EventTypeAgentMetrics},
	} {
		if !strings.HasPrefix(eventType.constant, "m.bureau.") {
			t.Errorf("%s = %q, must start with m.bureau.", eventType.name, eventType.constant)
		}
	}
}
