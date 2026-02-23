// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/llm"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// artifactStorer is the subset of artifactstore.Client used by the
// checkpoint tracker. Extracted as an interface for testability.
type artifactStorer interface {
	Store(ctx context.Context, header *artifactstore.StoreHeader, content io.Reader) (*artifactstore.StoreResponse, error)
}

// contextCheckpointer is the subset of AgentServiceClient used by the
// checkpoint tracker. Extracted as an interface for testability.
type contextCheckpointer interface {
	CheckpointContext(ctx context.Context, request agentdriver.CheckpointContextRequest) (*agentdriver.CheckpointContextResponse, error)
}

// checkpointTracker manages context checkpoint state and execution for
// the agent loop. It maintains a complete message history independently
// of the context manager (which provides the LLM windowing view) and
// creates checkpoints by serializing message deltas to CBOR, storing
// them as artifacts, and recording commits through the agent service.
//
// The tracker is best-effort: checkpoint failures are logged but do not
// interrupt the agent loop. An agent that crashes because it cannot
// persist is worse than one that continues without persistence.
type checkpointTracker struct {
	artifactClient     artifactStorer
	agentServiceClient contextCheckpointer
	sessionID          string
	template           string
	logger             *slog.Logger

	// currentContextID is the ctx-* identifier of the most recent
	// checkpoint commit. Empty until the first checkpoint succeeds.
	// Used as the parent for the next checkpoint.
	currentContextID string

	// lastCheckpointIndex is the index into allMessages at which the
	// last checkpoint was created. The next delta is
	// allMessages[lastCheckpointIndex:].
	lastCheckpointIndex int

	// allMessages is the complete conversation history, maintained
	// independently of the context manager. The context manager's job
	// is to decide what the LLM sees; the checkpoint tracker's job is
	// persistence. These are orthogonal concerns.
	allMessages []llm.Message

	// enabled is true when both clients are available.
	enabled bool
}

// newCheckpointTracker creates a tracker from the loop's dependencies.
// If either client is nil, checkpointing is disabled and the tracker
// becomes a no-op.
func newCheckpointTracker(
	artifactClient artifactStorer,
	agentServiceClient contextCheckpointer,
	sessionID string,
	template string,
	logger *slog.Logger,
) *checkpointTracker {
	enabled := artifactClient != nil && agentServiceClient != nil
	if !enabled {
		logger.Info("context checkpointing disabled (missing artifact or agent service client)")
	}
	return &checkpointTracker{
		artifactClient:     artifactClient,
		agentServiceClient: agentServiceClient,
		sessionID:          sessionID,
		template:           template,
		logger:             logger,
		enabled:            enabled,
	}
}

// appendMessage records a message in the tracker's history. Call this
// alongside each contextManager.Append to keep the tracker in sync.
func (tracker *checkpointTracker) appendMessage(message llm.Message) {
	tracker.allMessages = append(tracker.allMessages, message)
}

// checkpointDelta creates a delta checkpoint of all messages appended
// since the last checkpoint. No-op if there are no new messages or
// checkpointing is disabled. Errors are logged as warnings — the agent
// loop continues regardless.
func (tracker *checkpointTracker) checkpointDelta(ctx context.Context, trigger string) {
	delta := tracker.allMessages[tracker.lastCheckpointIndex:]
	if len(delta) == 0 || !tracker.enabled {
		return
	}
	if err := tracker.checkpoint(ctx, trigger, agent.CommitTypeDelta, delta); err != nil {
		tracker.logger.Warn("checkpoint failed, continuing without persistence",
			"checkpoint", trigger,
			"error", err,
		)
	}
}

// checkpointCompaction creates the two-commit compaction sequence:
// first a pre-compaction delta (everything since last checkpoint),
// then a compaction commit (the post-truncation surviving messages).
// The compaction commit's parent is the pre-compaction delta, so the
// chain preserves the full history: materialization stops at the
// compaction node, but walking past it reaches the pre-compaction
// content.
func (tracker *checkpointTracker) checkpointCompaction(ctx context.Context, survivingMessages []llm.Message) {
	if !tracker.enabled {
		return
	}

	// Pre-compaction delta: everything since last checkpoint, including
	// messages about to be evicted from the LLM's view.
	delta := tracker.allMessages[tracker.lastCheckpointIndex:]
	if len(delta) > 0 {
		if err := tracker.checkpoint(ctx, agent.CheckpointCompaction, agent.CommitTypeDelta, delta); err != nil {
			tracker.logger.Warn("pre-compaction delta checkpoint failed",
				"error", err,
			)
			return
		}
	}

	// Compaction commit: the surviving messages after truncation. Acts
	// as the conversation prefix during materialization.
	if len(survivingMessages) > 0 {
		if err := tracker.checkpoint(ctx, agent.CheckpointCompaction, agent.CommitTypeCompaction, survivingMessages); err != nil {
			tracker.logger.Warn("compaction checkpoint failed",
				"error", err,
			)
		}
	}
}

// checkpoint serializes messages to CBOR, stores the artifact, and
// records the commit through the agent service. Updates tracker state
// on success.
func (tracker *checkpointTracker) checkpoint(
	ctx context.Context,
	trigger string,
	commitType string,
	messages []llm.Message,
) error {
	// Serialize to CBOR (bureau-agent-v1 format: CBOR-encoded
	// []llm.Message array).
	data, err := codec.Marshal(messages)
	if err != nil {
		return fmt.Errorf("encoding messages to CBOR: %w", err)
	}

	// Store as artifact in the CAS.
	header := &artifactstore.StoreHeader{
		Action:      "store",
		ContentType: "application/cbor",
		Size:        int64(len(data)),
		Data:        data,
		Labels:      []string{"context-delta"},
	}
	storeResponse, err := tracker.artifactClient.Store(ctx, header, nil)
	if err != nil {
		return fmt.Errorf("storing context delta artifact: %w", err)
	}

	// Record the commit through the agent service.
	checkpointResponse, err := tracker.agentServiceClient.CheckpointContext(
		ctx,
		agentdriver.CheckpointContextRequest{
			Parent:       tracker.currentContextID,
			CommitType:   commitType,
			ArtifactRef:  storeResponse.Ref,
			Format:       "bureau-agent-v1",
			Template:     tracker.template,
			SessionID:    tracker.sessionID,
			Checkpoint:   trigger,
			MessageCount: len(messages),
		},
	)
	if err != nil {
		return fmt.Errorf("recording context checkpoint: %w", err)
	}

	tracker.currentContextID = checkpointResponse.ID
	tracker.lastCheckpointIndex = len(tracker.allMessages)

	tracker.logger.Info("context checkpoint created",
		"commit_id", checkpointResponse.ID,
		"commit_type", commitType,
		"checkpoint", trigger,
		"message_count", len(messages),
		"artifact_ref", storeResponse.Ref,
		"artifact_size", len(data),
	)

	return nil
}
