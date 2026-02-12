// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestWorkspaceCommands exercises the daemon's synchronous workspace command
// handlers (workspace.status, workspace.du, workspace.worktree.list,
// workspace.fetch) through the full stack: admin sends an m.bureau.command
// to the workspace room, daemon processes it via /sync, executes the handler,
// posts a threaded m.bureau.command_result reply.
//
// These are the "read" commands — they don't modify state, they just query
// the filesystem on the machine where the workspace lives. The test creates
// a real bare git repo in the workspace directory so that worktree.list and
// fetch have something to operate on.
func TestWorkspaceCommands(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/ws-cmds")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
	})

	ctx := t.Context()

	// --- Set up workspace filesystem ---
	// Create a bare git repo at <workspaceRoot>/testproj/.bare/
	// This is the standard Bureau layout: project root with a .bare/
	// directory containing the shared git object store.
	//
	// Strategy: init a normal repo in a temp directory, make an initial
	// commit on "main", then clone it bare into .bare/. This avoids the
	// problem of trying to add worktrees to an empty bare repo.
	projectDir := filepath.Join(machine.WorkspaceRoot, "testproj")
	bareDir := filepath.Join(projectDir, ".bare")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	seedRepo := t.TempDir()
	initTestGitRepo(t, ctx, seedRepo)

	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--bare", seedRepo, bareDir)
	cloneCmd.Stderr = os.Stderr
	if err := cloneCmd.Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}

	// --- Create workspace room ---
	workspaceAlias := "testproj"
	adminUserID := "@bureau-admin:" + testServerName

	spaceRoomID, err := admin.ResolveAlias(ctx, "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve bureau space: %v", err)
	}

	workspaceRoomID := createTestWorkspaceRoom(t, admin, workspaceAlias, machine.UserID, adminUserID, spaceRoomID)

	// Publish workspace state.
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:    "active",
			Project:   "testproj",
			Machine:   machine.Name,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("publish workspace state: %v", err)
	}

	// --- Test workspace.status ---
	t.Run("status", func(t *testing.T) {
		requestID := "ws-status-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.status testproj",
				Command:   "workspace.status",
				Workspace: "testproj",
				RequestID: requestID,
			})
		if err != nil {
			t.Fatalf("send workspace.status: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "success" {
			errorMsg, _ := result["error"].(string)
			t.Fatalf("workspace.status failed: %s", errorMsg)
		}

		resultMap, _ := result["result"].(map[string]any)
		if resultMap == nil {
			t.Fatal("workspace.status result is nil")
		}

		exists, _ := resultMap["exists"].(bool)
		if !exists {
			t.Error("workspace.status: exists = false, want true")
		}

		hasBareRepo, _ := resultMap["has_bare_repo"].(bool)
		if !hasBareRepo {
			t.Error("workspace.status: has_bare_repo = false, want true")
		}

		workspace, _ := resultMap["workspace"].(string)
		if workspace != "testproj" {
			t.Errorf("workspace.status: workspace = %q, want %q", workspace, "testproj")
		}
	})

	// --- Test workspace.du ---
	t.Run("du", func(t *testing.T) {
		requestID := "ws-du-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.du testproj",
				Command:   "workspace.du",
				Workspace: "testproj",
				RequestID: requestID,
			})
		if err != nil {
			t.Fatalf("send workspace.du: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "success" {
			errorMsg, _ := result["error"].(string)
			t.Fatalf("workspace.du failed: %s", errorMsg)
		}

		resultMap, _ := result["result"].(map[string]any)
		if resultMap == nil {
			t.Fatal("workspace.du result is nil")
		}

		size, _ := resultMap["size"].(string)
		if size == "" {
			t.Error("workspace.du: size is empty")
		}

		workspace, _ := resultMap["workspace"].(string)
		if workspace != "testproj" {
			t.Errorf("workspace.du: workspace = %q, want %q", workspace, "testproj")
		}
	})

	// --- Test workspace.worktree.list ---
	t.Run("worktree_list", func(t *testing.T) {
		requestID := "ws-wtlist-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.list testproj",
				Command:   "workspace.worktree.list",
				Workspace: "testproj",
				RequestID: requestID,
			})
		if err != nil {
			t.Fatalf("send workspace.worktree.list: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "success" {
			errorMsg, _ := result["error"].(string)
			t.Fatalf("workspace.worktree.list failed: %s", errorMsg)
		}

		resultMap, _ := result["result"].(map[string]any)
		if resultMap == nil {
			t.Fatal("workspace.worktree.list result is nil")
		}

		// The bare repo has worktrees listed by git (at minimum the bare
		// directory itself is listed as "(bare)").
		worktrees, _ := resultMap["worktrees"].([]any)
		if worktrees == nil {
			t.Error("workspace.worktree.list: worktrees is nil")
		}
	})

	// --- Test workspace.fetch ---
	t.Run("fetch", func(t *testing.T) {
		requestID := "ws-fetch-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.fetch testproj",
				Command:   "workspace.fetch",
				Workspace: "testproj",
				RequestID: requestID,
			})
		if err != nil {
			t.Fatalf("send workspace.fetch: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 60*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "success" {
			errorMsg, _ := result["error"].(string)
			t.Fatalf("workspace.fetch failed: %s", errorMsg)
		}

		resultMap, _ := result["result"].(map[string]any)
		if resultMap == nil {
			t.Fatal("workspace.fetch result is nil")
		}

		workspace, _ := resultMap["workspace"].(string)
		if workspace != "testproj" {
			t.Errorf("workspace.fetch: workspace = %q, want %q", workspace, "testproj")
		}
	})

	// --- Test workspace.status for nonexistent workspace ---
	t.Run("status_nonexistent", func(t *testing.T) {
		requestID := "ws-status-ne-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.status nonexistent",
				Command:   "workspace.status",
				Workspace: "nonexistent",
				RequestID: requestID,
			})
		if err != nil {
			t.Fatalf("send workspace.status: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "success" {
			t.Fatalf("workspace.status for nonexistent should succeed with exists=false, got status=%q", status)
		}

		resultMap, _ := result["result"].(map[string]any)
		exists, _ := resultMap["exists"].(bool)
		if exists {
			t.Error("workspace.status: exists = true for nonexistent workspace, want false")
		}
	})
}

// TestWorkspaceWorktreeHandlers exercises the daemon's async worktree
// command handlers (workspace.worktree.add, workspace.worktree.remove).
// These handlers validate parameters and spawn ephemeral pipeline executor
// sandboxes. The test verifies:
//
//   - Correct validation of the "path" parameter
//   - The "accepted" acknowledgment is posted immediately
//   - An ephemeral pipeline executor principal is created
//   - Invalid parameters are rejected with descriptive errors
//
// The pipeline executor may or may not succeed depending on whether git
// is available in the runner-env. This test focuses on the handler dispatch
// and parameter validation, not the pipeline execution itself (which is
// covered by TestPipelineExecution and TestWorkspacePipelineExecution).
func TestWorkspaceWorktreeHandlers(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/ws-worktree")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
	})

	ctx := t.Context()

	// --- Set up workspace filesystem with bare repo ---
	projectDir := filepath.Join(machine.WorkspaceRoot, "wtproj")
	bareDir := filepath.Join(projectDir, ".bare")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	seedRepo := t.TempDir()
	initTestGitRepo(t, ctx, seedRepo)

	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--bare", seedRepo, bareDir)
	cloneCmd.Stderr = os.Stderr
	if err := cloneCmd.Run(); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}

	// --- Create workspace room ---
	workspaceAlias := "wtproj"
	adminUserID := "@bureau-admin:" + testServerName

	spaceRoomID, err := admin.ResolveAlias(ctx, "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve bureau space: %v", err)
	}

	workspaceRoomID := createTestWorkspaceRoom(t, admin, workspaceAlias, machine.UserID, adminUserID, spaceRoomID)

	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:    "active",
			Project:   "wtproj",
			Machine:   machine.Name,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("publish workspace state: %v", err)
	}

	// --- Test worktree.add accepted ack ---
	// The daemon posts two results: an immediate "accepted" ack and
	// the async pipeline result. The pipeline may fail (git is not in
	// the integration-test-env), so we wait for both and verify the
	// accepted ack is present.
	t.Run("add_accepted", func(t *testing.T) {
		requestID := "wt-add-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.add feature/test-branch",
				Command:   "workspace.worktree.add",
				Workspace: "wtproj",
				RequestID: requestID,
				Parameters: map[string]any{
					"path":   "feature/test-branch",
					"branch": "main",
				},
			})
		if err != nil {
			t.Fatalf("send workspace.worktree.add: %v", err)
		}

		// Wait for both the accepted ack and the pipeline result.
		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 2, 60*time.Second)
		acceptedResult := findAcceptedResult(t, results)

		principalName, _ := acceptedResult["principal"].(string)
		if principalName == "" {
			t.Error("accepted result has empty principal")
		}
		t.Logf("worktree.add accepted, executor principal: %s", principalName)
	})

	// --- Test worktree.remove accepted ack ---
	t.Run("remove_accepted", func(t *testing.T) {
		requestID := "wt-rm-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.remove feature/test-branch",
				Command:   "workspace.worktree.remove",
				Workspace: "wtproj",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "feature/test-branch",
					"mode": "delete",
				},
			})
		if err != nil {
			t.Fatalf("send workspace.worktree.remove: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 2, 60*time.Second)
		acceptedResult := findAcceptedResult(t, results)

		principalName, _ := acceptedResult["principal"].(string)
		if principalName == "" {
			t.Error("accepted result has empty principal")
		}
		t.Logf("worktree.remove accepted, executor principal: %s", principalName)
	})

	// --- Test validation: missing path ---
	t.Run("add_missing_path", func(t *testing.T) {
		requestID := "wt-nopath-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:    schema.MsgTypeCommand,
				Body:       "workspace.worktree.add (no path)",
				Command:    "workspace.worktree.add",
				Workspace:  "wtproj",
				RequestID:  requestID,
				Parameters: map[string]any{
					// path intentionally omitted
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "error" {
			t.Fatalf("expected error for missing path, got status=%q", status)
		}

		errorMsg, _ := result["error"].(string)
		if errorMsg == "" {
			t.Error("error message is empty")
		}
		t.Logf("missing path correctly rejected: %s", errorMsg)
	})

	// --- Test validation: path traversal ---
	t.Run("add_path_traversal", func(t *testing.T) {
		requestID := "wt-trav-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.add ../escape",
				Command:   "workspace.worktree.add",
				Workspace: "wtproj",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "../escape",
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "error" {
			t.Fatalf("expected error for path traversal, got status=%q", status)
		}

		errorMsg, _ := result["error"].(string)
		if errorMsg == "" {
			t.Error("error message is empty")
		}
		t.Logf("path traversal correctly rejected: %s", errorMsg)
	})

	// --- Test validation: invalid remove mode ---
	t.Run("remove_invalid_mode", func(t *testing.T) {
		requestID := "wt-badmode-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.remove feature/x (bad mode)",
				Command:   "workspace.worktree.remove",
				Workspace: "wtproj",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "feature/x",
					"mode": "destroy-everything",
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "error" {
			t.Fatalf("expected error for invalid mode, got status=%q", status)
		}

		errorMsg, _ := result["error"].(string)
		if errorMsg == "" {
			t.Error("error message is empty")
		}
		t.Logf("invalid mode correctly rejected: %s", errorMsg)
	})

	// --- Test validation: missing workspace ---
	t.Run("add_missing_workspace", func(t *testing.T) {
		requestID := "wt-nows-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		_, err := admin.SendEvent(ctx, workspaceRoomID, "m.room.message",
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.add (no workspace)",
				Command:   "workspace.worktree.add",
				RequestID: requestID,
				// Workspace intentionally empty — needsWorkspace=true
				Parameters: map[string]any{
					"path": "feature/test",
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := waitForCommandResults(t, admin, workspaceRoomID, requestID, 1, 30*time.Second)
		result := results[0].Content

		status, _ := result["status"].(string)
		if status != "error" {
			t.Fatalf("expected error for missing workspace, got status=%q", status)
		}

		errorMsg, _ := result["error"].(string)
		if errorMsg == "" {
			t.Error("error message is empty")
		}
		t.Logf("missing workspace correctly rejected: %s", errorMsg)
	})
}

// --- Test helpers ---

// findAcceptedResult searches command result events for the "accepted"
// acknowledgment posted by async daemon handlers (worktree.add, worktree.remove,
// pipeline.execute). The accepted result has content.result.status == "accepted".
// Returns the inner result map (containing "status" and "principal" fields).
// Fatals if no accepted result is found.
func findAcceptedResult(t *testing.T, events []messaging.Event) map[string]any {
	t.Helper()

	for _, event := range events {
		resultField, _ := event.Content["result"].(map[string]any)
		if resultField == nil {
			continue
		}
		status, _ := resultField["status"].(string)
		if status == "accepted" {
			return resultField
		}
	}

	t.Fatalf("accepted result not found among %d command results", len(events))
	return nil
}

// initTestGitRepo creates a regular git repo with one commit on a "main"
// branch. Used as the seed for git clone --bare to create Bureau's .bare/
// directory layout. A regular repo is needed because git init --bare
// creates an empty repo with no HEAD, and most git operations (worktree
// add, fetch) require at least one commit.
func initTestGitRepo(t *testing.T, ctx context.Context, directory string) {
	t.Helper()

	for _, step := range []struct {
		args []string
		desc string
	}{
		{[]string{"init", "-b", "main", directory}, "git init"},
		{[]string{"-C", directory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", directory, "config", "user.email", "test@test"}, "git config user.email"},
	} {
		cmd := exec.CommandContext(ctx, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.desc, err)
		}
	}

	readmeFile := filepath.Join(directory, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test Project\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	for _, step := range []struct {
		args []string
		desc string
	}{
		{[]string{"-C", directory, "add", "README.md"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", "Initial commit"}, "git commit"},
	} {
		cmd := exec.CommandContext(ctx, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.desc, err)
		}
	}
}
