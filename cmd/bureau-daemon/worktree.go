// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Daemon handlers for workspace.worktree.add and workspace.worktree.remove.
// Both are async, pipeline-based operations: the handler validates parameters,
// returns an "accepted" result immediately, and launches an ephemeral pipeline
// executor sandbox in a goroutine. The pipeline executor performs the actual
// git worktree operations, posts threaded progress updates, and the goroutine
// posts the final result when the executor exits.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// handleWorkspaceWorktreeAdd validates parameters and spawns an async pipeline
// executor to create a git worktree. The executor runs the
// dev-worktree-init pipeline, which handles bare repo verification, worktree
// creation, submodule init, and project-level worktree-init scripts.
func handleWorkspaceWorktreeAdd(ctx context.Context, d *Daemon, roomID, eventID string, command schema.CommandMessage) (any, error) {
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
	// The bare repo lives at /workspace/<project>/.bare/ regardless
	// of how deep the workspace alias is.
	project, _, _ := strings.Cut(command.Workspace, "/")
	if project == "" {
		project = command.Workspace
	}

	// Verify the bare repo exists on this machine.
	bareDir := filepath.Join(d.workspaceRoot, project, ".bare")
	if err := requireDirectory(bareDir); err != nil {
		return nil, fmt.Errorf("project %q has no .bare directory: %w", project, err)
	}

	// Build the pipeline localpart for the ephemeral executor sandbox.
	localpart := worktreeLocalpart("add", worktreePath)
	pipelineRef := "bureau/pipeline:dev-worktree-init"

	// Build a synthetic command with pipeline variables as parameters.
	// The executor reads these from the sandbox payload.
	pipelineCommand := schema.CommandMessage{
		MsgType:   schema.MsgTypeCommand,
		Body:      command.Body,
		Command:   "pipeline.execute",
		RequestID: command.RequestID,
		Parameters: map[string]any{
			"pipeline":          pipelineRef,
			"PROJECT":           project,
			"WORKTREE_PATH":     worktreePath,
			"BRANCH":            branch,
			"WORKSPACE_ROOM_ID": roomID,
			"MACHINE":           d.machineName,
		},
	}

	d.logger.Info("workspace.worktree.add accepted",
		"room_id", roomID,
		"workspace", command.Workspace,
		"worktree_path", worktreePath,
		"branch", branch,
		"localpart", localpart,
	)

	go d.executePipeline(d.shutdownCtx, roomID, eventID, pipelineCommand, localpart, pipelineRef)

	return map[string]any{
		"status":    "accepted",
		"principal": localpart,
	}, nil
}

// handleWorkspaceWorktreeRemove validates parameters and spawns an async
// pipeline executor to remove a git worktree. The executor runs the
// dev-worktree-deinit pipeline, which handles mode validation, optional
// archiving of uncommitted changes, project-level cleanup scripts, and
// the actual worktree removal.
func handleWorkspaceWorktreeRemove(ctx context.Context, d *Daemon, roomID, eventID string, command schema.CommandMessage) (any, error) {
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

	project, _, _ := strings.Cut(command.Workspace, "/")
	if project == "" {
		project = command.Workspace
	}

	// Build the pipeline localpart for the ephemeral executor sandbox.
	localpart := worktreeLocalpart("remove", worktreePath)
	pipelineRef := "bureau/pipeline:dev-worktree-deinit"

	pipelineCommand := schema.CommandMessage{
		MsgType:   schema.MsgTypeCommand,
		Body:      command.Body,
		Command:   "pipeline.execute",
		RequestID: command.RequestID,
		Parameters: map[string]any{
			"pipeline":          pipelineRef,
			"PROJECT":           project,
			"WORKTREE_PATH":     worktreePath,
			"MODE":              mode,
			"WORKSPACE_ROOM_ID": roomID,
			"MACHINE":           d.machineName,
		},
	}

	d.logger.Info("workspace.worktree.remove accepted",
		"room_id", roomID,
		"workspace", command.Workspace,
		"worktree_path", worktreePath,
		"mode", mode,
		"localpart", localpart,
	)

	go d.executePipeline(d.shutdownCtx, roomID, eventID, pipelineCommand, localpart, pipelineRef)

	return map[string]any{
		"status":    "accepted",
		"principal": localpart,
	}, nil
}

// validateWorktreePath checks that a worktree path is safe for filesystem
// operations. Rejects absolute paths, path traversal (..), hidden segments
// (leading dot), and empty segments.
func validateWorktreePath(path string) error {
	if filepath.IsAbs(path) {
		return fmt.Errorf("worktree path must not be absolute: %q", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("worktree path must not contain '..': %q", path)
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("worktree path contains empty segment: %q", path)
		}
		if segment[0] == '.' {
			return fmt.Errorf("worktree path segment %q starts with '.' (hidden)", segment)
		}
	}
	return nil
}

// worktreeLocalpart generates a unique principal localpart for a worktree
// pipeline execution. Follows the same pattern as pipelineLocalpart but
// uses a "worktree/<operation>/" prefix for readability.
func worktreeLocalpart(operation, path string) string {
	timestamp := time.Now().UnixMilli()
	label := sanitizeLabel(path)
	return fmt.Sprintf("worktree/%s/%s/%d", operation, label, timestamp)
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
