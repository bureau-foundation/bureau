// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Command handler for m.bureau.command messages. The daemon receives
// commands via /sync timeline events, authorizes them via room power
// levels, dispatches to built-in handlers, and posts threaded
// m.bureau.command_result replies.
//
// Synchronous commands (workspace.list, workspace.status, etc.) execute
// directly in the daemon process. Async commands (pipeline.execute)
// return an "accepted" result immediately and run in a goroutine that
// posts threaded results when complete.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/git"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// commandDefinition describes a single built-in command: the minimum
// power level required to execute it, whether it requires a workspace
// target, and the handler function.
type commandDefinition struct {
	requiredPowerLevel int
	needsWorkspace     bool
	handler            func(ctx context.Context, d *Daemon, roomID ref.RoomID, eventID string,
		command schema.CommandMessage) (any, error)
}

// builtinCommands maps command names to their definitions. Commands not
// in this map are rejected with an "unknown command" error.
var builtinCommands = map[string]commandDefinition{
	"workspace.list":            {schema.PowerLevelReadOnly, false, handleWorkspaceList},
	"workspace.status":          {schema.PowerLevelReadOnly, true, handleWorkspaceStatus},
	"workspace.du":              {schema.PowerLevelReadOnly, true, handleWorkspaceDu},
	"workspace.worktree.list":   {schema.PowerLevelOperator, true, handleWorkspaceWorktreeList},
	"workspace.worktree.add":    {schema.PowerLevelOperator, true, handleWorkspaceWorktreeAdd},
	"workspace.worktree.remove": {schema.PowerLevelOperator, true, handleWorkspaceWorktreeRemove},
	"workspace.fetch":           {schema.PowerLevelOperator, true, handleWorkspaceFetch},
	"pipeline.execute":          {schema.PowerLevelAdmin, false, handlePipelineExecute},
}

// processCommandMessages scans timeline events for m.bureau.command
// messages and dispatches each to handleCommand. Called from
// processSyncResponse for every joined room that has timeline events.
func (d *Daemon) processCommandMessages(ctx context.Context, roomID ref.RoomID, events []messaging.Event) {
	for _, event := range events {
		// Only process m.room.message events.
		if event.Type != schema.MatrixEventTypeMessage {
			continue
		}

		// Extract msgtype from the event content map.
		msgtype, _ := event.Content["msgtype"].(string)
		if msgtype != schema.MsgTypeCommand {
			continue
		}

		// Skip messages sent by the daemon itself. This prevents
		// self-processing loops if the daemon's own messages appear
		// in the sync response (e.g., echoed command results).
		if event.Sender == d.machine.UserID() {
			continue
		}

		// Parse the event content into a CommandMessage. Re-marshal
		// to JSON and unmarshal into the struct to handle type
		// coercion (float64 numbers, nested maps) correctly.
		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			d.logger.Error("failed to marshal command event content",
				"room_id", roomID, "event_id", event.EventID, "error", err)
			continue
		}

		var command schema.CommandMessage
		if err := json.Unmarshal(contentJSON, &command); err != nil {
			d.logger.Error("failed to parse command message",
				"room_id", roomID, "event_id", event.EventID, "error", err)
			continue
		}

		d.handleCommand(ctx, roomID, event.EventID, event.Sender, command)
	}
}

// handleCommand is the per-command lifecycle: look up, validate,
// authorize, execute, and post the result.
func (d *Daemon) handleCommand(ctx context.Context, roomID ref.RoomID, eventID string, sender ref.UserID, command schema.CommandMessage) {
	start := time.Now()

	d.logger.Info("processing command",
		"room_id", roomID,
		"event_id", eventID,
		"sender", sender,
		"command", command.Command,
		"workspace", command.Workspace,
		"request_id", command.RequestID,
	)

	// Look up the command in the registry.
	definition, exists := builtinCommands[command.Command]
	if !exists {
		d.postCommandError(ctx, roomID, eventID, command, start,
			fmt.Sprintf("unknown command %q", command.Command))
		return
	}

	// Validate workspace target if required.
	if definition.needsWorkspace && command.Workspace == "" {
		d.postCommandError(ctx, roomID, eventID, command, start,
			fmt.Sprintf("command %q requires a workspace target", command.Command))
		return
	}

	// Validate workspace path to prevent path traversal.
	if command.Workspace != "" {
		if err := d.validateWorkspacePath(command.Workspace); err != nil {
			d.postCommandError(ctx, roomID, eventID, command, start, err.Error())
			return
		}
	}

	// Authorize the sender.
	if err := d.authorizeCommand(ctx, roomID, sender, definition.requiredPowerLevel); err != nil {
		d.postCommandError(ctx, roomID, eventID, command, start,
			fmt.Sprintf("authorization denied: %v", err))
		return
	}

	// Execute the handler.
	result, err := definition.handler(ctx, d, roomID, eventID, command)
	if err != nil {
		d.postCommandError(ctx, roomID, eventID, command, start, err.Error())
		return
	}

	d.postCommandResult(ctx, roomID, eventID, command, start, result)
}

// validateWorkspacePath checks that a workspace name is safe for
// filesystem operations and shell interpolation. Uses the shared
// principal.ValidateRelativePath for charset and segment validation,
// then adds containment checking to verify the resolved path falls
// under workspaceRoot.
func (d *Daemon) validateWorkspacePath(workspace string) error {
	if err := principal.ValidateRelativePath(workspace, "workspace name"); err != nil {
		return err
	}

	// Verify containment: the resolved path must be under workspaceRoot.
	resolved := filepath.Join(d.workspaceRoot, workspace)
	relative, err := filepath.Rel(d.workspaceRoot, resolved)
	if err != nil {
		return fmt.Errorf("invalid workspace path: %w", err)
	}
	if strings.HasPrefix(relative, "..") {
		return fmt.Errorf("workspace path escapes root: %q", workspace)
	}

	return nil
}

// workspacePath returns the full filesystem path for a workspace name
// under the daemon's workspaceRoot. Caller must have already validated
// the workspace name via validateWorkspacePath.
func (d *Daemon) workspacePath(workspace string) string {
	return filepath.Join(d.workspaceRoot, workspace)
}

// authorizeCommand reads the room's m.room.power_levels state event and
// checks that the sender's power level meets the required threshold.
// Returns an error if authorization fails or power levels cannot be read.
func (d *Daemon) authorizeCommand(ctx context.Context, roomID ref.RoomID, sender ref.UserID, requiredLevel int) error {
	raw, err := d.session.GetStateEvent(ctx, roomID, schema.MatrixEventTypePowerLevels, "")
	if err != nil {
		// If the room has no power levels event (unlikely but possible),
		// treat as denied rather than defaulting to open.
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return fmt.Errorf("room %s has no power levels configured", roomID)
		}
		return fmt.Errorf("reading power levels for room %s: %w", roomID, err)
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(raw, &powerLevels); err != nil {
		return fmt.Errorf("parsing power levels for room %s: %w", roomID, err)
	}

	senderLevel := getUserPowerLevel(powerLevels, sender.String())
	if int(senderLevel) < requiredLevel {
		return fmt.Errorf("sender %s has power level %d, command requires %d",
			sender, int(senderLevel), requiredLevel)
	}

	return nil
}

// getUserPowerLevel extracts a user's power level from a parsed
// m.room.power_levels event content. Returns users_default if the user
// has no explicit entry.
func getUserPowerLevel(powerLevels map[string]any, userID string) float64 {
	usersDefault := getNumericField(powerLevels, "users_default")

	users, ok := powerLevels["users"].(map[string]any)
	if !ok {
		return usersDefault
	}

	level, ok := users[userID].(float64)
	if !ok {
		return usersDefault
	}
	return level
}

// getNumericField extracts a float64 field from a JSON-decoded map.
// Returns 0 if the field is missing or not a number.
func getNumericField(data map[string]any, key string) float64 {
	value, ok := data[key].(float64)
	if !ok {
		return 0
	}
	return value
}

// postCommandResult posts a successful command result as a threaded
// reply to the original command message.
func (d *Daemon) postCommandResult(ctx context.Context, roomID ref.RoomID, commandEventID string, command schema.CommandMessage, start time.Time, result any) {
	durationMilliseconds := time.Since(start).Milliseconds()

	resultJSON, err := json.Marshal(result)
	if err != nil {
		d.logger.Error("failed to marshal command result",
			"room_id", roomID,
			"command", command.Command,
			"error", err,
		)
		return
	}

	message := schema.CommandResultMessage{
		MsgType:    schema.MsgTypeCommandResult,
		Body:       fmt.Sprintf("%s: completed in %dms", command.Command, durationMilliseconds),
		Status:     "success",
		Result:     json.RawMessage(resultJSON),
		DurationMS: durationMilliseconds,
		RequestID:  command.RequestID,
		RelatesTo:  schema.NewThreadRelation(commandEventID),
	}

	if _, err := d.sendEventRetry(ctx, roomID, schema.MatrixEventTypeMessage, message); err != nil {
		d.logger.Error("failed to post command result",
			"room_id", roomID,
			"command", command.Command,
			"error", err,
		)
	}
}

// postCommandError posts a command error as a threaded reply to the
// original command message.
func (d *Daemon) postCommandError(ctx context.Context, roomID ref.RoomID, commandEventID string, command schema.CommandMessage, start time.Time, errorMessage string) {
	durationMilliseconds := time.Since(start).Milliseconds()

	d.logger.Warn("command failed",
		"room_id", roomID,
		"command", command.Command,
		"error", errorMessage,
	)

	message := schema.CommandResultMessage{
		MsgType:    schema.MsgTypeCommandResult,
		Body:       fmt.Sprintf("%s: error: %s", command.Command, errorMessage),
		Status:     "error",
		Error:      errorMessage,
		DurationMS: durationMilliseconds,
		RequestID:  command.RequestID,
		RelatesTo:  schema.NewThreadRelation(commandEventID),
	}

	if _, err := d.sendEventRetry(ctx, roomID, schema.MatrixEventTypeMessage, message); err != nil {
		d.logger.Error("failed to post command error",
			"room_id", roomID,
			"command", command.Command,
			"error", err,
		)
	}
}

// --- Built-in command handlers ---

// handleWorkspaceList lists local workspace directories under the
// daemon's workspaceRoot (e.g., /var/bureau/workspace/).
func handleWorkspaceList(ctx context.Context, d *Daemon, roomID ref.RoomID, _ string, command schema.CommandMessage) (any, error) {
	entries, err := os.ReadDir(d.workspaceRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{
				"workspaces": []string{},
			}, nil
		}
		return nil, fmt.Errorf("reading workspace root %s: %w", d.workspaceRoot, err)
	}

	var workspaces []string
	for _, entry := range entries {
		if entry.IsDir() {
			workspaces = append(workspaces, entry.Name())
		}
	}

	// Ensure the JSON output has an empty array, not null.
	if workspaces == nil {
		workspaces = []string{}
	}

	return map[string]any{
		"workspaces": workspaces,
	}, nil
}

// handleWorkspaceStatus reports the existence and basic metadata of a
// workspace directory.
func handleWorkspaceStatus(ctx context.Context, d *Daemon, roomID ref.RoomID, _ string, command schema.CommandMessage) (any, error) {
	path := d.workspacePath(command.Workspace)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{
				"workspace": command.Workspace,
				"exists":    false,
			}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	result := map[string]any{
		"workspace": command.Workspace,
		"exists":    true,
		"is_dir":    info.IsDir(),
	}

	// Check for .bare directory (indicates git workspace).
	bareDir := filepath.Join(path, ".bare")
	if _, err := os.Stat(bareDir); err == nil {
		result["has_bare_repo"] = true
	}

	return result, nil
}

// handleWorkspaceDu runs du -sh on the workspace directory and returns
// the output.
func handleWorkspaceDu(ctx context.Context, d *Daemon, roomID ref.RoomID, _ string, command schema.CommandMessage) (any, error) {
	path := d.workspacePath(command.Workspace)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace %q does not exist", command.Workspace)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "du", "-sh", path)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("du -sh %s: %w (stderr: %s)", path, err, strings.TrimSpace(stderr.String()))
	}

	output := strings.TrimSpace(stdout.String())

	// Parse the du output: "123M\t/path/to/workspace"
	parts := strings.SplitN(output, "\t", 2)
	size := output
	if len(parts) >= 1 {
		size = parts[0]
	}

	return map[string]any{
		"workspace": command.Workspace,
		"size":      size,
		"raw":       output,
	}, nil
}

// handleWorkspaceWorktreeList runs git worktree list on the workspace's
// .bare directory and returns the output.
func handleWorkspaceWorktreeList(ctx context.Context, d *Daemon, roomID ref.RoomID, _ string, command schema.CommandMessage) (any, error) {
	path := d.workspacePath(command.Workspace)
	bareDir := filepath.Join(path, ".bare")

	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace %q has no .bare directory (not a git workspace)", command.Workspace)
	}

	repo := git.NewRepository(bareDir)
	rawOutput, err := repo.Run(ctx, "worktree", "list")
	if err != nil {
		return nil, err
	}

	output := strings.TrimSpace(rawOutput)
	var worktrees []string
	if output != "" {
		worktrees = strings.Split(output, "\n")
	}

	// Ensure the JSON output has an empty array, not null.
	if worktrees == nil {
		worktrees = []string{}
	}

	return map[string]any{
		"workspace": command.Workspace,
		"worktrees": worktrees,
		"raw":       output,
	}, nil
}

// handleWorkspaceFetch runs git fetch --all in the workspace's .bare
// directory, using flock to serialize concurrent fetches.
func handleWorkspaceFetch(ctx context.Context, d *Daemon, roomID ref.RoomID, _ string, command schema.CommandMessage) (any, error) {
	path := d.workspacePath(command.Workspace)
	bareDir := filepath.Join(path, ".bare")

	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace %q has no .bare directory (not a git workspace)", command.Workspace)
	}

	lockPath := filepath.Join(bareDir, "bureau.lock")
	repo := git.NewRepository(bareDir)
	output, err := repo.RunLocked(ctx, lockPath, "fetch", "--all")
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"workspace": command.Workspace,
		"output":    output,
	}, nil
}
