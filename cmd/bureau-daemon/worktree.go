// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Daemon handlers for workspace.worktree.add and workspace.worktree.remove.
// Both are async, ticket-driven operations: the handler validates parameters,
// publishes a transitional worktree state event, creates a pip- ticket via
// the ticket service, starts the executor immediately, and returns "accepted"
// with the ticket ID. Starting the executor inline avoids a dependency on
// the homeserver delivering the ticket state event back via /sync — the
// recovery watcher (processPipelineTickets) handles restart scenarios only.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
)

// handleWorkspaceWorktreeAdd validates parameters, publishes transitional
// "creating" state, creates a pip- ticket for the dev-worktree-init
// pipeline, and starts the executor immediately.
func handleWorkspaceWorktreeAdd(ctx context.Context, d *Daemon, roomID ref.RoomID, eventID ref.EventID, command schema.CommandMessage) (any, error) {
	if d.pipelineExecutorBinary == "" {
		return nil, fmt.Errorf("daemon not configured for pipeline execution (--pipeline-executor-binary not set)")
	}

	// Extract and validate the worktree path from parameters.
	worktreePath, _ := command.Parameters["path"].(string)
	if worktreePath == "" {
		return nil, fmt.Errorf("parameter 'path' is required (relative worktree path within the project)")
	}
	if err := validateWorktreePath(worktreePath); err != nil {
		return nil, err
	}

	branch, _ := command.Parameters["branch"].(string)

	// Derive the project (first path segment) from the workspace name.
	project := workspaceProject(command.Workspace)

	// Verify the bare repo exists on this machine.
	bareDir := d.workspaceProjectBareDir(command.Workspace)
	if err := requireDirectory(bareDir); err != nil {
		return nil, fmt.Errorf("project %q has no .bare directory: %w", project, err)
	}

	// Discover the ticket service.
	ticketSocketPath := d.findLocalTicketSocket()
	if ticketSocketPath == "" {
		return nil, fmt.Errorf("no ticket service running on this machine (required for pipeline execution)")
	}

	// Build an ephemeral Entity for the ticket creation token.
	localpart := d.worktreeLocalpart()
	pipelineEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, localpart)
	if err != nil {
		return nil, fmt.Errorf("invalid worktree localpart: %w", err)
	}

	pipelineRef := "bureau/pipeline:dev-worktree-init"
	pipelineVariables := map[string]string{
		"PROJECT":           project,
		"WORKTREE_PATH":     worktreePath,
		"BRANCH":            branch,
		"WORKSPACE_ROOM_ID": roomID.String(),
		"MACHINE":           d.machine.Localpart(),
	}

	// Publish the transitional "creating" state before creating the
	// ticket. This lets other principals gate on worktree existence
	// via StartCondition, and ensures the worktree has a state event
	// even if the ticket creation fails.
	if _, err := d.session.SendStateEvent(ctx, roomID, schema.EventTypeWorktree, worktreePath, workspace.WorktreeState{
		Status:       workspace.WorktreeStatusCreating,
		Project:      project,
		WorktreePath: worktreePath,
		Branch:       branch,
		Machine:      d.machine.Localpart(),
	}); err != nil {
		return nil, fmt.Errorf("failed to publish worktree creating state: %w", err)
	}

	ticketID, triggerBytes, err := d.createPipelineTicket(
		ctx, ticketSocketPath, pipelineEntity, roomID,
		pipelineRef, pipelineVariables)
	if err != nil {
		return nil, fmt.Errorf("creating pipeline ticket: %w", err)
	}

	// Start the executor immediately, bypassing the /sync round-trip.
	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(triggerBytes, &ticketContent); err != nil {
		return nil, fmt.Errorf("unmarshaling ticket content for executor: %w", err)
	}
	d.reconcileMu.Lock()
	d.startPipelineExecutor(ctx, roomID, ticketID, &ticketContent)
	d.reconcileMu.Unlock()

	d.logger.Info("workspace.worktree.add accepted",
		"room_id", roomID,
		"workspace", command.Workspace,
		"worktree_path", worktreePath,
		"branch", branch,
		"ticket_id", ticketID,
	)

	return map[string]any{
		"status":    "accepted",
		"ticket_id": ticketID,
		"room":      roomID.String(),
	}, nil
}

// handleWorkspaceWorktreeRemove validates parameters, publishes
// transitional "removing" state, creates a pip- ticket for the
// dev-worktree-deinit pipeline, and starts the executor immediately.
func handleWorkspaceWorktreeRemove(ctx context.Context, d *Daemon, roomID ref.RoomID, eventID ref.EventID, command schema.CommandMessage) (any, error) {
	if d.pipelineExecutorBinary == "" {
		return nil, fmt.Errorf("daemon not configured for pipeline execution (--pipeline-executor-binary not set)")
	}

	// Extract and validate the worktree path from parameters.
	worktreePath, _ := command.Parameters["path"].(string)
	if worktreePath == "" {
		return nil, fmt.Errorf("parameter 'path' is required (relative worktree path within the project)")
	}
	if err := validateWorktreePath(worktreePath); err != nil {
		return nil, err
	}

	// Mode defaults to "archive" — preserve uncommitted work.
	mode, _ := command.Parameters["mode"].(string)
	if mode == "" {
		mode = "archive"
	}
	if mode != "archive" && mode != "delete" {
		return nil, fmt.Errorf("parameter 'mode' must be \"archive\" or \"delete\", got %q", mode)
	}

	project := workspaceProject(command.Workspace)

	// Discover the ticket service.
	ticketSocketPath := d.findLocalTicketSocket()
	if ticketSocketPath == "" {
		return nil, fmt.Errorf("no ticket service running on this machine (required for pipeline execution)")
	}

	// Build an ephemeral Entity for the ticket creation token.
	localpart := d.worktreeLocalpart()
	pipelineEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, localpart)
	if err != nil {
		return nil, fmt.Errorf("invalid worktree localpart: %w", err)
	}

	pipelineRef := "bureau/pipeline:dev-worktree-deinit"
	pipelineVariables := map[string]string{
		"PROJECT":           project,
		"WORKTREE_PATH":     worktreePath,
		"MODE":              mode,
		"WORKSPACE_ROOM_ID": roomID.String(),
		"MACHINE":           d.machine.Localpart(),
	}

	// Publish the transitional "removing" state before creating the
	// ticket. The deinit pipeline's assert_state step verifies this
	// state still holds at execution time, preventing races where
	// removal was cancelled between queueing and execution.
	if _, err := d.session.SendStateEvent(ctx, roomID, schema.EventTypeWorktree, worktreePath, workspace.WorktreeState{
		Status:       workspace.WorktreeStatusRemoving,
		Project:      project,
		WorktreePath: worktreePath,
		Machine:      d.machine.Localpart(),
	}); err != nil {
		return nil, fmt.Errorf("failed to publish worktree removing state: %w", err)
	}

	ticketID, triggerBytes, err := d.createPipelineTicket(
		ctx, ticketSocketPath, pipelineEntity, roomID,
		pipelineRef, pipelineVariables)
	if err != nil {
		return nil, fmt.Errorf("creating pipeline ticket: %w", err)
	}

	// Start the executor immediately, bypassing the /sync round-trip.
	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(triggerBytes, &ticketContent); err != nil {
		return nil, fmt.Errorf("unmarshaling ticket content for executor: %w", err)
	}
	d.reconcileMu.Lock()
	d.startPipelineExecutor(ctx, roomID, ticketID, &ticketContent)
	d.reconcileMu.Unlock()

	d.logger.Info("workspace.worktree.remove accepted",
		"room_id", roomID,
		"workspace", command.Workspace,
		"worktree_path", worktreePath,
		"mode", mode,
		"ticket_id", ticketID,
	)

	return map[string]any{
		"status":    "accepted",
		"ticket_id": ticketID,
		"room":      roomID.String(),
	}, nil
}

// validateWorktreePath checks that a worktree path is safe for filesystem
// operations and shell interpolation. Delegates to principal.ValidateRelativePath
// for charset enforcement and segment validation.
func validateWorktreePath(path string) error {
	return principal.ValidateRelativePath(path, "worktree path")
}

// worktreeLocalpart generates a unique principal localpart for a
// worktree pipeline execution. Uses the daemon's shared ephemeral
// counter for short, unique IDs. The operation and worktree path
// are logged separately — the localpart just needs uniqueness.
func (d *Daemon) worktreeLocalpart() string {
	id := d.ephemeralCounter.Add(1)
	return fmt.Sprintf("worktree/%d", id)
}

// requireDirectory checks that a path exists and is a directory. Returns
// a descriptive error on failure.
func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", path)
	}
	return nil
}
