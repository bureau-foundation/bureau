// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"errors"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// Schema version constants for agent service events. CanModify methods
// compare the event's Version field against these constants to prevent
// silent data loss during rolling upgrades.
const (
	// AgentSessionVersion is the current schema version for
	// AgentSessionContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentSessionVersion = 1

	// AgentContextVersion is the current schema version for
	// AgentContextContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentContextVersion = 1

	// AgentMetricsVersion is the current schema version for
	// AgentMetricsContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	AgentMetricsVersion = 1
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
	// Room: #<machine-localpart>:<server>
	EventTypeAgentSession ref.EventType = "m.bureau.agent_session"

	// EventTypeAgentContext tracks the LLM conversation context for
	// session resumption. Contains an artifact ref to the serialized
	// conversation history and summary metadata (message count, token
	// count). The agent service writes this at session end so the
	// next session can resume the conversation.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #<machine-localpart>:<server>
	EventTypeAgentContext ref.EventType = "m.bureau.agent_context"

	// EventTypeAgentMetrics tracks aggregated metrics across all
	// sessions for a principal. Running totals for tokens, cost,
	// tool calls, errors, and duration live directly in the Matrix
	// event for fast reads. A detailed per-session metrics breakdown
	// is stored as an artifact.
	//
	// State key: principal localpart (e.g., "iree/amdgpu/pm")
	// Room: #<machine-localpart>:<server>
	EventTypeAgentMetrics ref.EventType = "m.bureau.agent_metrics"
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

	// LatestContextCommitID is the ctx-* identifier of the most
	// recent context commit from the latest completed session. Used
	// for session resumption — Run() reads this to materialize the
	// previous conversation context and chain new checkpoints from
	// the same tip.
	LatestContextCommitID string `json:"latest_context_commit_id,omitempty"`

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
// event. Acts as a key-value index mapping named context keys to
// artifact refs. Keys are hierarchical strings using forward-slash
// separators (e.g., "conversation", "summary/2026-02-16",
// "notes/architecture"). Values are artifact refs plus summary metadata,
// stored as ContextEntry structs.
//
// The agent service is a metadata index — callers store content in the
// artifact service directly and then pass the resulting ref to the agent
// service via set-context. This keeps the agent service ephemeral
// (no artifact client dependency) and enforces write-through ordering:
// content exists in the artifact service before the ref appears here.
type AgentContextContent struct {
	// Version is the schema version (see AgentContextVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds AgentContextVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// Entries maps context keys to their artifact metadata. Keys are
	// hierarchical strings with forward-slash separators. An empty
	// map means no context has been stored for this principal.
	Entries map[string]ContextEntry `json:"entries,omitempty"`
}

// ContextEntry describes a single named context artifact in the index.
// The artifact content itself lives in the artifact service; this entry
// holds only the ref and enough metadata for callers to decide whether
// to fetch the full content.
type ContextEntry struct {
	// ArtifactRef is the artifact ref (BLAKE3 hex hash or short ref)
	// pointing to the content in the artifact service.
	ArtifactRef string `json:"artifact_ref"`

	// Size is the uncompressed content size in bytes.
	Size int64 `json:"size"`

	// ContentType is the MIME type of the stored content (e.g.,
	// "application/json", "text/markdown").
	ContentType string `json:"content_type"`

	// ModifiedAt is the ISO 8601 timestamp when this entry was last
	// written or updated.
	ModifiedAt string `json:"modified_at"`

	// SessionID is the session that produced this context entry.
	// Required for conversation context entries (key "conversation")
	// where it enables consistency checking during resume. Optional
	// for other entry types.
	SessionID string `json:"session_id,omitempty"`

	// MessageCount is the number of messages in the stored context.
	// Only meaningful for conversation context entries; zero for
	// other types.
	MessageCount int `json:"message_count,omitempty"`

	// TokenCount is the approximate token count of the stored
	// context. Only meaningful for conversation context entries;
	// zero for other types.
	TokenCount int64 `json:"token_count,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (content *AgentContextContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("agent context: version must be >= 1, got %d", content.Version)
	}
	for key, entry := range content.Entries {
		if err := entry.Validate(key); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks that all required fields of a ContextEntry are present.
func (entry *ContextEntry) Validate(key string) error {
	if entry.ArtifactRef == "" {
		return fmt.Errorf("agent context: entry %q: artifact_ref is required", key)
	}
	if entry.ContentType == "" {
		return fmt.Errorf("agent context: entry %q: content_type is required", key)
	}
	if entry.ModifiedAt == "" {
		return fmt.Errorf("agent context: entry %q: modified_at is required", key)
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
