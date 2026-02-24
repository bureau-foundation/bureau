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

	// EventTypeThinking is a reasoning/thinking block from the agent.
	// Contains the agent's chain-of-thought reasoning and an optional
	// cryptographic signature for verification.
	EventTypeThinking EventType = "thinking"
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

	// Thinking is set for EventTypeThinking events.
	Thinking *ThinkingEvent `json:"thinking,omitempty"`
}

// PromptEvent records a prompt sent to the agent.
type PromptEvent struct {
	// Content is the prompt text.
	Content string `json:"content"`

	// Source distinguishes the origin of the prompt.
	// Values: "initial" (the first prompt at session start),
	// "injected" (message injected via the messaging system),
	// "user" (human input via stdin, used by Claude Code driver).
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

	// ServerTool distinguishes built-in server tools (web search,
	// file search) from MCP/user-defined tools.
	ServerTool bool `json:"server_tool,omitempty"`
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

	// Status is the session outcome. Values: "success",
	// "error_max_turns", "error_during_execution",
	// "error_max_budget_usd". Empty for agents that don't report
	// session outcome status.
	Status string `json:"status,omitempty"`
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
	// Subtype further classifies the system event. Known subtypes:
	// "init" (session startup with configuration), "shutdown"
	// (graceful termination), "compact_boundary" (full context
	// compaction), "microcompact_boundary" (lighter compaction
	// variant), "context_truncated" (hard truncation).
	Subtype string `json:"subtype"`

	// Message is an optional human-readable description.
	Message string `json:"message,omitempty"`

	// Metadata captures the full structured payload of the system event
	// as raw JSON. For compact_boundary events this contains
	// {"trigger":"auto","pre_tokens":128000}; for init events it contains
	// session_id, tools, model, etc. Consumers unmarshal on demand.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// ThinkingEvent records a reasoning/thinking block from the agent.
type ThinkingEvent struct {
	// Content is the agent's chain-of-thought reasoning text.
	Content string `json:"content"`

	// Signature is a cryptographic signature for the thinking block,
	// used for verification by the LLM provider. Present when the
	// provider includes signatures in thinking output.
	Signature string `json:"signature,omitempty"`
}
