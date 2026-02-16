// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionLogWriteAndRead(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "session.jsonl")
	writer, err := NewSessionLogWriter(logPath)
	if err != nil {
		t.Fatalf("NewSessionLogWriter: %v", err)
	}

	// Write a sequence of events.
	events := []Event{
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 0, 0, time.UTC),
			Type:      EventTypeSystem,
			System:    &SystemEvent{Subtype: "init", Message: "agent starting"},
		},
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 1, 0, time.UTC),
			Type:      EventTypePrompt,
			Prompt:    &PromptEvent{Content: "Fix the bug", Source: "initial"},
		},
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 2, 0, time.UTC),
			Type:      EventTypeToolCall,
			ToolCall:  &ToolCallEvent{ID: "tc-1", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/test.go"}`)},
		},
		{
			Timestamp:  time.Date(2026, 2, 16, 10, 0, 3, 0, time.UTC),
			Type:       EventTypeToolResult,
			ToolResult: &ToolResultEvent{ID: "tc-1", Output: "package main"},
		},
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 4, 0, time.UTC),
			Type:      EventTypeResponse,
			Response:  &ResponseEvent{Content: "I see the bug"},
		},
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 5, 0, time.UTC),
			Type:      EventTypeError,
			Error:     &ErrorEvent{Message: "tool failed"},
		},
		{
			Timestamp: time.Date(2026, 2, 16, 10, 0, 6, 0, time.UTC),
			Type:      EventTypeMetric,
			Metric: &MetricEvent{
				InputTokens:  1000,
				OutputTokens: 500,
				CostUSD:      0.015,
				TurnCount:    3,
			},
		},
	}

	for _, event := range events {
		if writeError := writer.Write(event); writeError != nil {
			t.Fatalf("Write: %v", writeError)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back and verify JSONL format.
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("opening log: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var readEvents []Event
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("unmarshaling event: %v (line: %s)", err, scanner.Text())
		}
		readEvents = append(readEvents, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning: %v", err)
	}

	if len(readEvents) != len(events) {
		t.Fatalf("read %d events, wrote %d", len(readEvents), len(events))
	}

	// Verify specific fields round-trip correctly.
	if readEvents[0].Type != EventTypeSystem {
		t.Errorf("event[0].Type = %q, want system", readEvents[0].Type)
	}
	if readEvents[0].System.Subtype != "init" {
		t.Errorf("event[0].System.Subtype = %q, want init", readEvents[0].System.Subtype)
	}
	if readEvents[1].Prompt.Content != "Fix the bug" {
		t.Errorf("event[1].Prompt.Content = %q, want 'Fix the bug'", readEvents[1].Prompt.Content)
	}
	if readEvents[2].ToolCall.Name != "Read" {
		t.Errorf("event[2].ToolCall.Name = %q, want Read", readEvents[2].ToolCall.Name)
	}
	if readEvents[4].Response.Content != "I see the bug" {
		t.Errorf("event[4].Response.Content = %q, want 'I see the bug'", readEvents[4].Response.Content)
	}
	if readEvents[6].Metric.InputTokens != 1000 {
		t.Errorf("event[6].Metric.InputTokens = %d, want 1000", readEvents[6].Metric.InputTokens)
	}
}

func TestSessionLogSummary(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "summary.jsonl")
	writer, err := NewSessionLogWriter(logPath)
	if err != nil {
		t.Fatalf("NewSessionLogWriter: %v", err)
	}
	defer writer.Close()

	fixedTime := time.Date(2026, 2, 16, 12, 0, 0, 0, time.UTC)

	// Write tool calls, errors, and metrics.
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{Name: "Read"}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{Name: "Edit"}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeToolCall, ToolCall: &ToolCallEvent{Name: "Bash"}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeError, Error: &ErrorEvent{Message: "oops"}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeMetric, Metric: &MetricEvent{
		InputTokens:     2000,
		OutputTokens:    800,
		CacheReadTokens: 500,
		CostUSD:         0.03,
		TurnCount:       2,
	}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeMetric, Metric: &MetricEvent{
		InputTokens:      1500,
		OutputTokens:     600,
		CacheWriteTokens: 200,
		CostUSD:          0.02,
		TurnCount:        1,
	}})
	writer.Write(Event{Timestamp: fixedTime, Type: EventTypeResponse, Response: &ResponseEvent{Content: "done"}})

	summary := writer.Summary()

	if summary.EventCount != 7 {
		t.Errorf("EventCount = %d, want 7", summary.EventCount)
	}
	if summary.ToolCallCount != 3 {
		t.Errorf("ToolCallCount = %d, want 3", summary.ToolCallCount)
	}
	if summary.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", summary.ErrorCount)
	}
	if summary.InputTokens != 3500 {
		t.Errorf("InputTokens = %d, want 3500", summary.InputTokens)
	}
	if summary.OutputTokens != 1400 {
		t.Errorf("OutputTokens = %d, want 1400", summary.OutputTokens)
	}
	if summary.CacheReadTokens != 500 {
		t.Errorf("CacheReadTokens = %d, want 500", summary.CacheReadTokens)
	}
	if summary.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens = %d, want 200", summary.CacheWriteTokens)
	}
	if summary.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", summary.TurnCount)
	}
	// Cost should be approximately 0.05.
	if summary.CostUSD < 0.049 || summary.CostUSD > 0.051 {
		t.Errorf("CostUSD = %f, want ~0.05", summary.CostUSD)
	}
	if summary.Duration < 0 {
		t.Error("Duration should be non-negative")
	}
}

func TestSessionLogEmptySummary(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "empty.jsonl")
	writer, err := NewSessionLogWriter(logPath)
	if err != nil {
		t.Fatalf("NewSessionLogWriter: %v", err)
	}
	defer writer.Close()

	summary := writer.Summary()
	if summary.EventCount != 0 {
		t.Errorf("EventCount = %d, want 0", summary.EventCount)
	}
	if summary.ToolCallCount != 0 {
		t.Errorf("ToolCallCount = %d, want 0", summary.ToolCallCount)
	}
}
