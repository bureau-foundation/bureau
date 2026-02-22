// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/agentdriver"
)

// Sample stream-json output from Claude Code (representative fragments).
const sampleStreamJSON = `{"type":"system","subtype":"init","session_id":"abc123","tools":["Read","Edit","Bash"],"message":"Claude Code starting"}
{"type":"assistant","subtype":"text","text":"I'll read the file first."}
{"type":"assistant","subtype":"tool_use","tool_use_id":"tu-1","name":"Read","input":{"file_path":"/tmp/test.go"}}
{"type":"tool","subtype":"result","tool_use_id":"tu-1","content":"package main\n\nfunc main() {}","is_error":false}
{"type":"assistant","subtype":"text","text":"The file looks good."}
{"type":"result","subtype":"success","cost_usd":0.015,"input_tokens":2500,"output_tokens":800,"cache_read_input_tokens":500,"num_turns":3,"duration_ms":4500}
`

func TestParseOutputEventTypes(t *testing.T) {
	t.Parallel()

	driver := &claudeDriver{}
	events := make(chan agentdriver.Event, 64)
	reader := strings.NewReader(sampleStreamJSON)

	err := driver.ParseOutput(context.Background(), reader, events)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	close(events)

	var collected []agentdriver.Event
	for event := range events {
		collected = append(collected, event)
	}

	if len(collected) != 6 {
		t.Fatalf("got %d events, want 6", len(collected))
	}

	// Event 0: system init.
	if collected[0].Type != agentdriver.EventTypeSystem {
		t.Errorf("event[0].Type = %q, want system", collected[0].Type)
	}
	if collected[0].System.Subtype != "init" {
		t.Errorf("event[0].System.Subtype = %q, want init", collected[0].System.Subtype)
	}
	if collected[0].System.Message != "Claude Code starting" {
		t.Errorf("event[0].System.Message = %q, want 'Claude Code starting'", collected[0].System.Message)
	}

	// Event 1: assistant text.
	if collected[1].Type != agentdriver.EventTypeResponse {
		t.Errorf("event[1].Type = %q, want response", collected[1].Type)
	}
	if collected[1].Response.Content != "I'll read the file first." {
		t.Errorf("event[1].Response.Content = %q", collected[1].Response.Content)
	}

	// Event 2: tool use.
	if collected[2].Type != agentdriver.EventTypeToolCall {
		t.Errorf("event[2].Type = %q, want tool_call", collected[2].Type)
	}
	if collected[2].ToolCall.Name != "Read" {
		t.Errorf("event[2].ToolCall.Name = %q, want Read", collected[2].ToolCall.Name)
	}
	if collected[2].ToolCall.ID != "tu-1" {
		t.Errorf("event[2].ToolCall.ID = %q, want tu-1", collected[2].ToolCall.ID)
	}

	// Event 3: tool result.
	if collected[3].Type != agentdriver.EventTypeToolResult {
		t.Errorf("event[3].Type = %q, want tool_result", collected[3].Type)
	}
	if collected[3].ToolResult.ID != "tu-1" {
		t.Errorf("event[3].ToolResult.ID = %q, want tu-1", collected[3].ToolResult.ID)
	}
	if collected[3].ToolResult.IsError {
		t.Error("event[3].ToolResult.IsError should be false")
	}
	if !strings.Contains(collected[3].ToolResult.Output, "package main") {
		t.Errorf("event[3].ToolResult.Output should contain 'package main', got %q", collected[3].ToolResult.Output)
	}

	// Event 4: second assistant text.
	if collected[4].Type != agentdriver.EventTypeResponse {
		t.Errorf("event[4].Type = %q, want response", collected[4].Type)
	}

	// Event 5: result metrics.
	if collected[5].Type != agentdriver.EventTypeMetric {
		t.Errorf("event[5].Type = %q, want metric", collected[5].Type)
	}
	if collected[5].Metric.InputTokens != 2500 {
		t.Errorf("event[5].Metric.InputTokens = %d, want 2500", collected[5].Metric.InputTokens)
	}
	if collected[5].Metric.OutputTokens != 800 {
		t.Errorf("event[5].Metric.OutputTokens = %d, want 800", collected[5].Metric.OutputTokens)
	}
	if collected[5].Metric.CacheReadTokens != 500 {
		t.Errorf("event[5].Metric.CacheReadTokens = %d, want 500", collected[5].Metric.CacheReadTokens)
	}
	if collected[5].Metric.CostUSD < 0.014 || collected[5].Metric.CostUSD > 0.016 {
		t.Errorf("event[5].Metric.CostUSD = %f, want ~0.015", collected[5].Metric.CostUSD)
	}
	if collected[5].Metric.TurnCount != 3 {
		t.Errorf("event[5].Metric.TurnCount = %d, want 3", collected[5].Metric.TurnCount)
	}
	// duration_ms = 4500 → 4.5 seconds
	if collected[5].Metric.DurationSeconds < 4.4 || collected[5].Metric.DurationSeconds > 4.6 {
		t.Errorf("event[5].Metric.DurationSeconds = %f, want ~4.5", collected[5].Metric.DurationSeconds)
	}
}

func TestParseOutputMalformedLine(t *testing.T) {
	t.Parallel()

	driver := &claudeDriver{}
	events := make(chan agentdriver.Event, 64)
	reader := strings.NewReader("not valid json\n{\"type\":\"system\",\"subtype\":\"init\"}\n")

	err := driver.ParseOutput(context.Background(), reader, events)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	close(events)

	var collected []agentdriver.Event
	for event := range events {
		collected = append(collected, event)
	}

	if len(collected) != 2 {
		t.Fatalf("got %d events, want 2", len(collected))
	}

	// Malformed line should produce an output event with raw content.
	if collected[0].Type != agentdriver.EventTypeOutput {
		t.Errorf("malformed line should produce output event, got %q", collected[0].Type)
	}

	// Valid line should still parse correctly.
	if collected[1].Type != agentdriver.EventTypeSystem {
		t.Errorf("valid line should produce system event, got %q", collected[1].Type)
	}
}

func TestParseOutputUnknownType(t *testing.T) {
	t.Parallel()

	driver := &claudeDriver{}
	events := make(chan agentdriver.Event, 64)
	reader := strings.NewReader(`{"type":"future_event","data":"something new"}` + "\n")

	err := driver.ParseOutput(context.Background(), reader, events)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	close(events)

	var collected []agentdriver.Event
	for event := range events {
		collected = append(collected, event)
	}

	if len(collected) != 1 {
		t.Fatalf("got %d events, want 1", len(collected))
	}

	// Unknown types should produce output events with raw JSON preserved.
	if collected[0].Type != agentdriver.EventTypeOutput {
		t.Errorf("unknown type should produce output event, got %q", collected[0].Type)
	}
	if collected[0].Output == nil {
		t.Fatal("output event should have Output field set")
	}
	if !strings.Contains(string(collected[0].Output.Raw), "future_event") {
		t.Errorf("raw output should contain the original JSON, got %s", collected[0].Output.Raw)
	}
}

func TestParseOutputEmptyLines(t *testing.T) {
	t.Parallel()

	driver := &claudeDriver{}
	events := make(chan agentdriver.Event, 64)
	reader := strings.NewReader("\n\n{\"type\":\"system\",\"subtype\":\"init\"}\n\n")

	err := driver.ParseOutput(context.Background(), reader, events)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	close(events)

	var collected []agentdriver.Event
	for event := range events {
		collected = append(collected, event)
	}

	// Empty lines should be skipped, only the system event should appear.
	if len(collected) != 1 {
		t.Fatalf("got %d events, want 1 (empty lines should be skipped)", len(collected))
	}
}

func TestParseOutputToolError(t *testing.T) {
	t.Parallel()

	driver := &claudeDriver{}
	events := make(chan agentdriver.Event, 64)
	reader := strings.NewReader(`{"type":"tool","subtype":"result","tool_use_id":"tu-2","content":"permission denied","is_error":true}` + "\n")

	err := driver.ParseOutput(context.Background(), reader, events)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	close(events)

	var collected []agentdriver.Event
	for event := range events {
		collected = append(collected, event)
	}

	if len(collected) != 1 {
		t.Fatalf("got %d events, want 1", len(collected))
	}
	if !collected[0].ToolResult.IsError {
		t.Error("tool result should have IsError=true")
	}
	if collected[0].ToolResult.Output != "permission denied" {
		t.Errorf("tool result output = %q, want 'permission denied'", collected[0].ToolResult.Output)
	}
}

func TestExtractStringField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     string
		field    string
		expected string
	}{
		{"existing field", `{"message":"hello"}`, "message", "hello"},
		{"missing field", `{"other":"value"}`, "message", ""},
		{"non-string field", `{"count":42}`, "count", ""},
		{"invalid json", `not json`, "message", ""},
		{"nested object", `{"message":"hello","nested":{"key":"value"}}`, "message", "hello"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			result := extractStringField([]byte(testCase.data), testCase.field)
			if result != testCase.expected {
				t.Errorf("extractStringField(%q, %q) = %q, want %q",
					testCase.data, testCase.field, result, testCase.expected)
			}
		})
	}
}
