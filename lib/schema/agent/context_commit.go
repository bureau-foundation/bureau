// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// ContextCommitVersion is the current schema version for
// ContextCommitContent events. Increment when adding fields that
// existing code must not silently drop during read-modify-write.
const ContextCommitVersion = 1

// EventTypeAgentContextCommit is the event type identifier for context
// commit metadata. Context commits track individual snapshots in an
// agent's understanding chain. Each commit holds metadata about a
// single delta (or compaction summary, or full snapshot) stored in the
// artifact service, plus provenance linking it to a parent commit,
// agent template, principal, and session.
//
// Context commits form chains through parent links, analogous to git
// commits. The full conversation at any point is the concatenation of
// all deltas from root to that commit.
//
// Commit metadata is stored as CBOR artifacts in the artifact service,
// tagged as "ctx/<commitID>" (e.g., "ctx/ctx-a1b2c3d4"). The event
// type constant is retained for schema identification and wire format
// compatibility.
const EventTypeAgentContextCommit ref.EventType = "m.bureau.agent_context_commit"

// Context commit type constants. These describe the relationship
// between the commit's artifact and the conversation chain.
const (
	// CommitTypeDelta indicates the artifact contains new
	// conversation entries since the parent commit.
	CommitTypeDelta = "delta"

	// CommitTypeCompaction indicates the artifact contains a
	// summary replacing the prefix chain. Materialization stops
	// here by default and uses the summary as the conversation
	// prefix. The full pre-compaction history remains reachable
	// by walking past the compaction node.
	CommitTypeCompaction = "compaction"

	// CommitTypeSnapshot indicates the artifact contains a full
	// materialized context stored as a single artifact. Used for
	// caching materialized chains or for contexts imported from
	// external systems.
	CommitTypeSnapshot = "snapshot"
)

// Checkpoint trigger constants. These describe what caused the
// context commit to be created.
const (
	// CheckpointTurnBoundary is set when the checkpoint happens
	// after the assistant finishes responding, before tool
	// execution begins.
	CheckpointTurnBoundary = "turn_boundary"

	// CheckpointToolCall is set when the checkpoint happens after
	// a tool returns its result.
	CheckpointToolCall = "tool_call"

	// CheckpointCompaction is set when the context manager
	// compacts the conversation. The pre-compaction delta is
	// saved as a separate commit before the compaction commit.
	CheckpointCompaction = "compaction"

	// CheckpointSessionEnd is set when the sandbox lifecycle ends
	// (graceful exit or crash recovery).
	CheckpointSessionEnd = "session_end"

	// CheckpointExplicit is set when the agent or operator
	// requests a checkpoint via the agent service.
	CheckpointExplicit = "explicit"
)

// ContextCommitContent is the metadata for a context commit. Each
// context commit is a node in an agent's understanding chain: a delta
// artifact plus metadata linking it to its parent commit, the agent
// template that produced it, and the runtime context in which it was
// created.
//
// Both the commit metadata and its delta artifact live in the CAS
// (content-addressed storage). The metadata is stored as a CBOR
// artifact tagged as "ctx/<commitID>"; the delta content is stored
// separately and referenced by ArtifactRef.
//
// Summary is the one mutable field: it can be added or updated after
// creation by a batch summarization service. All other fields are
// immutable once written. Metadata updates re-store the full content
// and overwrite the tag.
type ContextCommitContent struct {
	// Version is the schema version (see ContextCommitVersion).
	// Code that modifies a commit must call CanModify() first; if
	// Version exceeds ContextCommitVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by the CBOR
	// decoder).
	Version int `json:"version"`

	// Parent is the ctx-* ID of the parent commit in the chain.
	// Empty for root commits (the start of a new conversation).
	// Two commits sharing a parent creates a branch — independent
	// conversations diverging from a shared starting point.
	Parent string `json:"parent,omitempty"`

	// CommitType describes the artifact's role in the chain:
	// "delta" (new entries since parent), "compaction" (summary
	// replacing the prefix chain), or "snapshot" (full materialized
	// context). See CommitType* constants.
	CommitType string `json:"commit_type"`

	// ArtifactRef is the BLAKE3 content-addressed ref pointing to
	// the delta (or compaction summary, or full snapshot) in the
	// artifact service.
	ArtifactRef string `json:"artifact_ref"`

	// Format is the serialization format of the delta artifact.
	// Determines how deltas are concatenated during materialization
	// and how translation works across runtimes. Values:
	// "events-v1" (CBOR-encoded []agentdriver.Event, used by all
	// runtimes for observation-level checkpoints),
	// "bureau-agent-v1" (CBOR-encoded []llm.Message, native agent
	// conversation-level snapshots), "claude-code-v1" (raw JSONL
	// byte ranges, for importing historical Claude Code sessions).
	Format string `json:"format"`

	// Template is the agent template that produced this commit.
	// Structural identity: what kind of agent this is. Stored as
	// the template ref string (e.g., "bureau/template:code-reviewer").
	Template string `json:"template,omitempty"`

	// Principal is the Matrix user ID of the principal that executed
	// this commit. Informative: where the agent happened to run.
	Principal ref.UserID `json:"principal,omitempty"`

	// Machine is the Matrix user ID of the machine the principal
	// ran on. Informative: which machine provided compute.
	Machine ref.UserID `json:"machine,omitempty"`

	// SessionID identifies the session within which this commit was
	// created. Links context commits to session lifecycle events.
	SessionID string `json:"session_id,omitempty"`

	// Checkpoint describes what triggered this commit. One of the
	// Checkpoint* constants: "turn_boundary", "tool_call",
	// "compaction", "session_end", "explicit".
	Checkpoint string `json:"checkpoint"`

	// TicketID is the ticket being worked when this commit was
	// created. Empty if the agent was not working on a ticket.
	TicketID string `json:"ticket_id,omitempty"`

	// ThreadID is the Matrix event ID of the thread this commit
	// is part of. Empty if the agent was not in a thread
	// conversation.
	ThreadID string `json:"thread_id,omitempty"`

	// Summary is a human-readable description of the agent's
	// understanding at this point. Mutable: can be added or
	// updated after creation by a batch summarization service
	// or operator. The delta artifact is immutable; the summary
	// is metadata that annotates the chain for browsing.
	Summary string `json:"summary,omitempty"`

	// MessageCount is the number of messages in this delta.
	// Zero for compaction commits (which contain a summary,
	// not messages).
	MessageCount int `json:"message_count,omitempty"`

	// TokenCount is the approximate token count of this delta's
	// content.
	TokenCount int64 `json:"token_count,omitempty"`

	// CreatedAt is the ISO 8601 timestamp when this commit was
	// created.
	CreatedAt string `json:"created_at"`
}

// Validate checks that all required fields are present and well-formed.
func (content *ContextCommitContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("context commit: version must be >= 1, got %d", content.Version)
	}
	switch content.CommitType {
	case CommitTypeDelta, CommitTypeCompaction, CommitTypeSnapshot:
		// Valid.
	case "":
		return fmt.Errorf("context commit: commit_type is required")
	default:
		return fmt.Errorf("context commit: unknown commit_type %q", content.CommitType)
	}
	if content.ArtifactRef == "" {
		return fmt.Errorf("context commit: artifact_ref is required")
	}
	if content.Format == "" {
		return fmt.Errorf("context commit: format is required")
	}
	switch content.Checkpoint {
	case CheckpointTurnBoundary, CheckpointToolCall, CheckpointCompaction,
		CheckpointSessionEnd, CheckpointExplicit:
		// Valid.
	case "":
		return fmt.Errorf("context commit: checkpoint is required")
	default:
		return fmt.Errorf("context commit: unknown checkpoint %q", content.Checkpoint)
	}
	if content.CreatedAt == "" {
		return fmt.Errorf("context commit: created_at is required")
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this commit. Returns nil if safe, or an
// error explaining why modification would risk data loss.
func (content *ContextCommitContent) CanModify() error {
	if content.Version > ContextCommitVersion {
		return fmt.Errorf(
			"context commit version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this commit",
			content.Version, ContextCommitVersion,
		)
	}
	return nil
}

// contextCommitIDLength is the number of hex characters used in
// generated ctx-* IDs. 8 hex chars = 4 bytes = ~4 billion
// possibilities. Combined with the artifact ref (a unique BLAKE3
// hash) as an input, collisions are effectively impossible.
const contextCommitIDLength = 8

// GenerateContextCommitID produces a deterministic ctx-* identifier
// from the commit's defining inputs. The ID is a SHA-256 hash of the
// concatenated inputs, truncated to contextCommitIDLength hex
// characters and prefixed with "ctx-".
//
// The inputs that define a context commit's identity are:
//   - parent: the parent commit ID (empty string for root commits)
//   - artifactRef: the BLAKE3 ref of the delta artifact
//   - createdAt: the ISO 8601 creation timestamp
//   - template: the agent template identifier
//
// The artifact ref alone is unique (content-addressed), but including
// the other inputs ties the ID to the full provenance, making the
// relationship between ID and commit unambiguous.
func GenerateContextCommitID(parent, artifactRef, createdAt, template string) string {
	hash := sha256.New()
	hash.Write([]byte(parent))
	hash.Write([]byte{0}) // null separator
	hash.Write([]byte(artifactRef))
	hash.Write([]byte{0})
	hash.Write([]byte(createdAt))
	hash.Write([]byte{0})
	hash.Write([]byte(template))
	digest := hash.Sum(nil)
	return "ctx-" + hex.EncodeToString(digest[:])[:contextCommitIDLength]
}
