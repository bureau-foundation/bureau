// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// eventArtifactStorer is the subset of artifactstore.Client used by
// the event checkpoint tracker. Extracted as an interface for testability.
type eventArtifactStorer interface {
	Store(ctx context.Context, header *artifactstore.StoreHeader, content io.Reader) (*artifactstore.StoreResponse, error)
}

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
// The tracker uses the same commit chain infrastructure as the native
// agent's message-level checkpointing: CBOR artifacts in the CAS,
// commit metadata through the agent service. The difference is the
// payload: this tracker serializes []agentdriver.Event (the observation
// stream) rather than []llm.Message (the LLM conversation).
//
// Runtime checkpoint failures (transient store/network errors) are
// logged as warnings but do not interrupt the event pump. Setup
// failures (missing infrastructure) are caught at startup before the
// tracker is created.
type eventCheckpointTracker struct {
	artifactClient     eventArtifactStorer
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
// that both clients are present before calling this.
func newEventCheckpointTracker(
	artifactClient eventArtifactStorer,
	agentServiceClient eventContextCheckpointer,
	format string,
	sessionID string,
	template string,
	logger *slog.Logger,
) *eventCheckpointTracker {
	return &eventCheckpointTracker{
		artifactClient:     artifactClient,
		agentServiceClient: agentServiceClient,
		format:             format,
		sessionID:          sessionID,
		template:           template,
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

// checkpoint serializes events to CBOR, stores the artifact, and
// records the commit through the agent service. Updates tracker state
// on success.
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

	// Store as artifact in the CAS.
	header := &artifactstore.StoreHeader{
		Action:      "store",
		ContentType: "application/cbor",
		Size:        int64(len(data)),
		Data:        data,
		Labels:      []string{"event-delta"},
	}
	storeResponse, err := tracker.artifactClient.Store(ctx, header, nil)
	if err != nil {
		return fmt.Errorf("storing event delta artifact: %w", err)
	}

	// Record the commit through the agent service.
	checkpointResponse, err := tracker.agentServiceClient.CheckpointContext(
		ctx,
		CheckpointContextRequest{
			Parent:       tracker.currentContextID,
			CommitType:   commitType,
			ArtifactRef:  storeResponse.Ref,
			Format:       tracker.format,
			Template:     tracker.template,
			SessionID:    tracker.sessionID,
			Checkpoint:   trigger,
			MessageCount: len(events),
		},
	)
	if err != nil {
		return fmt.Errorf("recording event checkpoint: %w", err)
	}

	tracker.currentContextID = checkpointResponse.ID
	tracker.lastCheckpointIndex = len(tracker.allEvents)

	tracker.logger.Info("event checkpoint created",
		"commit_id", checkpointResponse.ID,
		"commit_type", commitType,
		"checkpoint", trigger,
		"event_count", len(events),
		"artifact_ref", storeResponse.Ref,
		"artifact_size", len(data),
	)

	return nil
}
