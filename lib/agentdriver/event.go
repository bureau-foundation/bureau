// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"encoding/json"
	"time"
)

// EventType classifies session log events.
type EventType string

const (
	// EventTypePrompt is the initial prompt or injected message sent to the agent.
	EventTypePrompt EventType = "prompt"

	// EventTypeToolCall is a tool invocation by the agent.
	EventTypeToolCall EventType = "tool_call"

	// EventTypeToolResult is the result returned from a tool invocation.
	EventTypeToolResult EventType = "tool_result"

	// EventTypeResponse is a text response from the agent.
	EventTypeResponse EventType = "response"

	// EventTypeMetric is a summary metric event (tokens, cost, duration).
	EventTypeMetric EventType = "metric"

	// EventTypeOutput is raw output that doesn't map to a structured type.
	EventTypeOutput EventType = "output"

	// EventTypeError is an error event from the agent or wrapper.
	EventTypeError EventType = "error"

	// EventTypeSystem is a system-level event (init, shutdown, config).
	EventTypeSystem EventType = "system"
)

// Event is a structured session log entry. Each event has a timestamp, type,
// and type-specific data. Events are serialized as JSONL for session logs.
type Event struct {
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`

	// Type classifies the event.
	Type EventType `json:"type"`

	// Prompt is set for EventTypePrompt events.
	Prompt *PromptEvent `json:"prompt,omitempty"`

	// ToolCall is set for EventTypeToolCall events.
	ToolCall *ToolCallEvent `json:"tool_call,omitempty"`

	// ToolResult is set for EventTypeToolResult events.
	ToolResult *ToolResultEvent `json:"tool_result,omitempty"`

	// Response is set for EventTypeResponse events.
	Response *ResponseEvent `json:"response,omitempty"`

	// Metric is set for EventTypeMetric events.
	Metric *MetricEvent `json:"metric,omitempty"`

	// Output is set for EventTypeOutput events.
	Output *OutputEvent `json:"output,omitempty"`

	// Error is set for EventTypeError events.
	Error *ErrorEvent `json:"error,omitempty"`

	// System is set for EventTypeSystem events.
	System *SystemEvent `json:"system,omitempty"`
}

// PromptEvent records a prompt sent to the agent.
type PromptEvent struct {
	// Content is the prompt text.
	Content string `json:"content"`

	// Source distinguishes the initial prompt from injected messages.
	// Values: "initial", "injected".
	Source string `json:"source"`
}

// ToolCallEvent records a tool invocation by the agent.
type ToolCallEvent struct {
	// ID is the tool call identifier (runtime-specific, e.g., Claude's tool_use ID).
	ID string `json:"id,omitempty"`

	// Name is the tool name (e.g., "Read", "Bash", "Edit").
	Name string `json:"name"`

	// Input is the tool input, preserved as raw JSON.
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResultEvent records the result of a tool invocation.
type ToolResultEvent struct {
	// ID matches the corresponding ToolCallEvent.ID.
	ID string `json:"id,omitempty"`

	// IsError indicates the tool call failed.
	IsError bool `json:"is_error,omitempty"`

	// Output is the tool result text (truncated for large outputs).
	Output string `json:"output,omitempty"`
}

// ResponseEvent records a text response from the agent.
type ResponseEvent struct {
	// Content is the response text.
	Content string `json:"content"`
}

// MetricEvent records summary metrics from the agent session.
type MetricEvent struct {
	// InputTokens is the total input token count.
	InputTokens int64 `json:"input_tokens,omitempty"`

	// OutputTokens is the total output token count.
	OutputTokens int64 `json:"output_tokens,omitempty"`

	// CacheReadTokens is the count of tokens read from cache.
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`

	// CacheWriteTokens is the count of tokens written to cache.
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`

	// CostUSD is the total cost in USD.
	CostUSD float64 `json:"cost_usd,omitempty"`

	// DurationSeconds is the session wall-clock duration.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`

	// TurnCount is the number of agent turns (API round-trips).
	TurnCount int64 `json:"turn_count,omitempty"`
}

// OutputEvent records raw output that doesn't map to a structured event type.
type OutputEvent struct {
	// Raw is the original output, preserved as raw JSON.
	Raw json.RawMessage `json:"raw"`
}

// ErrorEvent records an error.
type ErrorEvent struct {
	// Message is the error description.
	Message string `json:"message"`
}

// SystemEvent records system-level events.
type SystemEvent struct {
	// Subtype further classifies the system event (e.g., "init", "shutdown").
	Subtype string `json:"subtype"`

	// Message is an optional human-readable description.
	Message string `json:"message,omitempty"`
}
