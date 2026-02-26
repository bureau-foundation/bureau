// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// eventContextCheckpointer is the subset of AgentServiceClient used
// by the event checkpoint tracker. Extracted as an interface for
// testability.
type eventContextCheckpointer interface {
	CheckpointContext(ctx context.Context, request CheckpointContextRequest) (*CheckpointContextResponse, error)
}

// eventCheckpointTracker manages event checkpoint state and execution
// for the Run() event pump. It accumulates events from the pump and
// creates CBOR delta checkpoints at meaningful boundaries (turn
// boundaries, compaction events, session end).
//
// The tracker sends CBOR-encoded event deltas to the agent service,
// which stores them as artifacts and records commit metadata in Matrix.
// This keeps the agent's sandbox surface area minimal: the agent only
// needs the agent service socket, not direct artifact service access.
// The agent service owns both the data plane (artifact storage) and
// the control plane (commit chain) for checkpoints, ensuring coherent
// responses to queries without cross-service coordination.
//
// Runtime checkpoint failures (transient network errors) are logged as
// warnings but do not interrupt the event pump. Setup failures (missing
// infrastructure) are caught at startup before the tracker is created.
type eventCheckpointTracker struct {
	agentServiceClient eventContextCheckpointer
	format             string
	sessionID          string
	template           string
	logger             *slog.Logger

	// allEvents is the complete event history from the pump.
	allEvents []Event

	// lastCheckpointIndex is the index into allEvents at which the
	// last checkpoint was created. The next delta is
	// allEvents[lastCheckpointIndex:].
	lastCheckpointIndex int

	// currentContextID is the ctx-* identifier of the most recent
	// checkpoint commit. Empty until the first checkpoint succeeds.
	// Used as the parent for the next checkpoint.
	currentContextID string
}

// newEventCheckpointTracker creates a tracker for Run()-level event
// checkpointing. All arguments are required — the caller validates
// that the agent service client is present before calling this.
//
// initialContextID is the ctx-* identifier to use as the parent for
// the first checkpoint. When resuming from a previous session, this
// is the tip commit of the previous session's chain, ensuring new
// checkpoints extend the existing chain rather than starting a new
// root. Empty for a fresh session (first checkpoint has no parent).
func newEventCheckpointTracker(
	agentServiceClient eventContextCheckpointer,
	format string,
	sessionID string,
	template string,
	initialContextID string,
	logger *slog.Logger,
) *eventCheckpointTracker {
	return &eventCheckpointTracker{
		agentServiceClient: agentServiceClient,
		format:             format,
		sessionID:          sessionID,
		template:           template,
		currentContextID:   initialContextID,
		logger:             logger,
	}
}

// appendEvent records an event in the tracker's history. Call this
// for every event received from the event pump.
func (tracker *eventCheckpointTracker) appendEvent(event Event) {
	tracker.allEvents = append(tracker.allEvents, event)
}

// checkpointDelta creates a delta checkpoint of all events appended
// since the last checkpoint. No-op if there are no new events.
// Errors are logged as warnings — the event pump continues regardless.
func (tracker *eventCheckpointTracker) checkpointDelta(ctx context.Context, trigger string) {
	delta := tracker.allEvents[tracker.lastCheckpointIndex:]
	if len(delta) == 0 {
		return
	}
	if err := tracker.checkpoint(ctx, trigger, agent.CommitTypeDelta, delta); err != nil {
		tracker.logger.Warn("event checkpoint failed, continuing without persistence",
			"checkpoint", trigger,
			"error", err,
		)
	}
}

// checkpoint serializes events to CBOR, sends them to the agent
// service for artifact storage and commit recording, and updates
// tracker state on success.
func (tracker *eventCheckpointTracker) checkpoint(
	ctx context.Context,
	trigger string,
	commitType string,
	events []Event,
) error {
	// Serialize to CBOR (events-v1 format: CBOR-encoded
	// []agentdriver.Event array).
	data, err := codec.Marshal(events)
	if err != nil {
		return fmt.Errorf("encoding events to CBOR: %w", err)
	}

	// Send the CBOR bytes to the agent service. The agent service
	// stores them as an artifact and records the commit metadata
	// atomically (from the caller's perspective).
	checkpointResponse, err := tracker.agentServiceClient.CheckpointContext(
		ctx,
		CheckpointContextRequest{
			Parent:       tracker.currentContextID,
			CommitType:   commitType,
			Data:         data,
			Format:       tracker.format,
			Template:     tracker.template,
			SessionID:    tracker.sessionID,
			Checkpoint:   trigger,
			MessageCount: len(events),
		},
	)
	if err != nil {
		return fmt.Errorf("checkpoint-context: %w", err)
	}

	tracker.currentContextID = checkpointResponse.ID
	tracker.lastCheckpointIndex = len(tracker.allEvents)

	tracker.logger.Info("event checkpoint created",
		"commit_id", checkpointResponse.ID,
		"commit_type", commitType,
		"checkpoint", trigger,
		"event_count", len(events),
		"data_size", len(data),
	)

	return nil
}
