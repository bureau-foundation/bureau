// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"errors"
	"fmt"
)

// Agent service event type constants. These events live in per-machine
// config rooms (alongside PrincipalAssignment and Credentials events)
// and are read/written by the agent service. The state key for each is
// the principal's localpart.
//
// The agent service stores bulk content (session logs, conversation
// context, detailed metrics) in the artifact service and writes only
// artifact refs and small summary data to Matrix state events. This
// keeps Matrix events compact while providing full historical data
// through the artifact service. The agent service uses write-through
// ordering: content is stored in the artifact service first, and only
// after the artifact ref is confirmed does the Matrix state event get
// updated. This guarantees no dangling refs.
const (
	// EventTypeAgentSession tracks agent session lifecycle. Contains
	// the active session ID (if running), the most recent completed
	// session's artifact ref (for resume), and a pointer to a session
	// index artifact listing all historical sessions.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #bureau/config/<machine-localpart>:<server>
	EventTypeAgentSession = "m.bureau.agent_session"

	// EventTypeAgentContext tracks the LLM conversation context for
	// session resumption. Contains an artifact ref to the serialized
	// conversation history and summary metadata (message count, token
	// count). The agent service writes this at session end so the
	// next session can resume the conversation.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #bureau/config/<machine-localpart>:<server>
	EventTypeAgentContext = "m.bureau.agent_context"

	// EventTypeAgentMetrics tracks aggregated metrics across all
	// sessions for a principal. Running totals for tokens, cost,
	// tool calls, errors, and duration live directly in the Matrix
	// event for fast reads. A detailed per-session metrics breakdown
	// is stored as an artifact.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #bureau/config/<machine-localpart>:<server>
	EventTypeAgentMetrics = "m.bureau.agent_metrics"
)

// AgentSessionContent is the content of an EventTypeAgentSession state
// event. Tracks the lifecycle of agent sessions for a single principal.
//
// When no session has ever run, all fields except Version are zero/empty.
// The agent service updates this event at session start (setting
// ActiveSessionID) and session end (clearing ActiveSessionID, updating
// LatestSessionID and artifact refs).
type AgentSessionContent struct {
	// Version is the schema version (see AgentSessionVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds AgentSessionVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// ActiveSessionID is the ID of the currently running session.
	// Empty when no session is active. The agent service sets this
	// when a session starts and clears it when the session ends.
	ActiveSessionID string `json:"active_session_id,omitempty"`

	// ActiveSessionStartedAt is the ISO 8601 timestamp when the
	// active session started. Empty when no session is active.
	ActiveSessionStartedAt string `json:"active_session_started_at,omitempty"`

	// LatestSessionID is the ID of the most recently completed
	// session. Used for session resume — the agent can look up this
	// session's context to continue the conversation.
	LatestSessionID string `json:"latest_session_id,omitempty"`

	// LatestSessionArtifactRef is the artifact ref (BLAKE3 hex hash)
	// pointing to the most recently completed session's JSONL log in
	// the artifact service.
	LatestSessionArtifactRef string `json:"latest_session_artifact_ref,omitempty"`

	// SessionIndexArtifactRef is the artifact ref pointing to the
	// session index — a JSONL file listing all sessions for this
	// principal with their IDs, timestamps, summary stats, and
	// artifact refs. Updated after each session completes.
	SessionIndexArtifactRef string `json:"session_index_artifact_ref,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (content *AgentSessionContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("agent session: version must be >= 1, got %d", content.Version)
	}
	// ActiveSessionID and ActiveSessionStartedAt must be set together.
	if (content.ActiveSessionID != "") != (content.ActiveSessionStartedAt != "") {
		return errors.New("agent session: active_session_id and active_session_started_at must both be set or both be empty")
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
func (content *AgentSessionContent) CanModify() error {
	if content.Version > AgentSessionVersion {
		return fmt.Errorf(
			"agent session version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			content.Version, AgentSessionVersion,
		)
	}
	return nil
}

// AgentContextContent is the content of an EventTypeAgentContext state
// event. Tracks the LLM conversation context for session resumption.
//
// The conversation context itself (the full messages array) is stored
// as an artifact. This event holds only the ref and summary metadata
// needed to decide whether to resume and how large the context is.
type AgentContextContent struct {
	// Version is the schema version (see AgentContextVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds AgentContextVersion, the modification is
	// refused to prevent silent field loss.
	Version int `json:"version"`

	// ContextArtifactRef is the artifact ref (BLAKE3 hex hash)
	// pointing to the serialized LLM conversation context. The format
	// is a JSON array of message objects (matching the provider's
	// message schema). The agent service writes this at session end
	// for resume.
	ContextArtifactRef string `json:"context_artifact_ref,omitempty"`

	// SessionID is the session that produced this context snapshot.
	// Used for consistency checking during resume — if the session ID
	// doesn't match expectations, the context may be stale or from a
	// different session lineage.
	SessionID string `json:"session_id,omitempty"`

	// MessageCount is the number of messages in the stored context.
	MessageCount int `json:"message_count"`

	// TokenCount is the approximate token count of the stored context.
	// This is the estimate from the context manager, not a precise
	// count from the tokenizer.
	TokenCount int64 `json:"token_count"`

	// UpdatedAt is the ISO 8601 timestamp of the last context update.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (content *AgentContextContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("agent context: version must be >= 1, got %d", content.Version)
	}
	// If a context artifact ref is set, the session ID that produced
	// it must also be recorded for consistency checking.
	if content.ContextArtifactRef != "" && content.SessionID == "" {
		return errors.New("agent context: session_id is required when context_artifact_ref is set")
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event.
func (content *AgentContextContent) CanModify() error {
	if content.Version > AgentContextVersion {
		return fmt.Errorf(
			"agent context version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			content.Version, AgentContextVersion,
		)
	}
	return nil
}

// AgentMetricsContent is the content of an EventTypeAgentMetrics state
// event. Contains lifetime aggregated counters across all sessions for
// a principal. The agent service updates this event after each session
// completes by adding the session's metrics to the running totals.
//
// All numeric values are integers for Matrix canonical JSON compatibility
// (which forbids fractional numbers). Cost is expressed in milliUSD
// (thousandths of a US dollar).
type AgentMetricsContent struct {
	// Version is the schema version (see AgentMetricsVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds AgentMetricsVersion, the modification is
	// refused to prevent silent field loss.
	Version int `json:"version"`

	// TotalInputTokens is the sum of input tokens across all sessions.
	TotalInputTokens int64 `json:"total_input_tokens"`

	// TotalOutputTokens is the sum of output tokens across all sessions.
	TotalOutputTokens int64 `json:"total_output_tokens"`

	// TotalCacheReadTokens is the sum of cache-read tokens across all
	// sessions.
	TotalCacheReadTokens int64 `json:"total_cache_read_tokens"`

	// TotalCacheWriteTokens is the sum of cache-write tokens across all
	// sessions.
	TotalCacheWriteTokens int64 `json:"total_cache_write_tokens"`

	// TotalCostMilliUSD is the total cost in thousandths of a US dollar.
	// Divide by 1000 for USD (e.g., 4500 = $4.50). Integer for Matrix
	// canonical JSON compatibility.
	TotalCostMilliUSD int64 `json:"total_cost_milliusd"`

	// TotalToolCalls is the sum of tool invocations across all sessions.
	TotalToolCalls int64 `json:"total_tool_calls"`

	// TotalTurns is the sum of LLM conversation turns (API round-trips)
	// across all sessions.
	TotalTurns int64 `json:"total_turns"`

	// TotalErrors is the sum of error events across all sessions.
	TotalErrors int64 `json:"total_errors"`

	// TotalSessionCount is the number of completed sessions.
	TotalSessionCount int64 `json:"total_session_count"`

	// TotalDurationSeconds is the total wall-clock time across all
	// sessions, in seconds. Integer for Matrix canonical JSON.
	TotalDurationSeconds int64 `json:"total_duration_seconds"`

	// MetricsDetailArtifactRef is the artifact ref pointing to a
	// detailed metrics breakdown — per-session stats, time-series
	// data, model usage distribution, etc. Updated after each session
	// completes.
	MetricsDetailArtifactRef string `json:"metrics_detail_artifact_ref,omitempty"`

	// LastSessionID identifies the session that last updated these
	// metrics. Used for idempotency — if a session's metrics have
	// already been incorporated (matching LastSessionID), the update
	// is skipped.
	LastSessionID string `json:"last_session_id,omitempty"`

	// LastUpdatedAt is the ISO 8601 timestamp of the last metrics update.
	LastUpdatedAt string `json:"last_updated_at,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (content *AgentMetricsContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("agent metrics: version must be >= 1, got %d", content.Version)
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event.
func (content *AgentMetricsContent) CanModify() error {
	if content.Version > AgentMetricsVersion {
		return fmt.Errorf(
			"agent metrics version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			content.Version, AgentMetricsVersion,
		)
	}
	return nil
}
