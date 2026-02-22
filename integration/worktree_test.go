// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
	"github.com/bureau-foundation/bureau/lib/templatedef"
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

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ws-cmds")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
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
	adminUserID := admin.UserID()

	spaceRoomID, err := admin.ResolveAlias(ctx, ref.MustParseRoomAlias("#bureau:"+testServerName))
	if err != nil {
		t.Fatalf("resolve bureau space: %v", err)
	}

	workspaceRoomID := createTestWorkspaceRoom(t, admin, workspaceAlias, machine.UserID, adminUserID, spaceRoomID)

	// Publish workspace state.
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", workspace.WorkspaceState{
			Status:    "active",
			Project:   "testproj",
			Machine:   machine.Name,
			UpdatedAt: "2026-01-01T00:00:00Z",
		})
	if err != nil {
		t.Fatalf("publish workspace state: %v", err)
	}

	// --- Test workspace.status ---
	t.Run("status", func(t *testing.T) {
		requestID := "ws-status-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "ws-du-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "ws-wtlist-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "ws-fetch-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "ws-status-ne-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
// These handlers validate parameters and create pip- tickets via the
// ticket service. The test verifies:
//
//   - Correct validation of the "path" parameter
//   - The "accepted" acknowledgment is posted immediately with a ticket ID
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

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ws-worktree")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Set up workspace filesystem with bare repo ---
	// The bare repo must exist before sending worktree commands — the
	// handler verifies its presence on disk.
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

	// --- Deploy ticket service and create workspace via production API ---
	// The ticket service must be running before workspace.Create so the
	// CLI can discover it via the service directory and invite it to the
	// workspace room.
	deployTicketService(t, admin, fleet, machine, "ws-wt-hdlr")

	wsResult := createWorkspace(t, admin, machine.Ref, "wtproj/main",
		"bureau/template:base", nil)
	workspaceRoomID := wsResult.RoomID

	// --- Test worktree.add accepted ack ---
	t.Run("add_accepted", func(t *testing.T) {
		requestID := "wt-add-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.add feature/test-branch",
				Command:   "workspace.worktree.add",
				Workspace: "wtproj/main",
				RequestID: requestID,
				Parameters: map[string]any{
					"path":   "feature/test-branch",
					"branch": "main",
				},
			})
		if err != nil {
			t.Fatalf("send workspace.worktree.add: %v", err)
		}

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
		acceptedContent := findAcceptedEvent(t, results)
		innerResult, _ := acceptedContent["result"].(map[string]any)

		ticketID, _ := innerResult["ticket_id"].(string)
		if ticketID == "" {
			t.Error("accepted result has empty ticket_id")
		}
		t.Logf("worktree.add accepted, ticket: %s", ticketID)
	})

	// --- Test worktree.remove accepted ack ---
	t.Run("remove_accepted", func(t *testing.T) {
		requestID := "wt-rm-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.remove feature/test-branch",
				Command:   "workspace.worktree.remove",
				Workspace: "wtproj/main",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "feature/test-branch",
					"mode": "delete",
				},
			})
		if err != nil {
			t.Fatalf("send workspace.worktree.remove: %v", err)
		}

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
		acceptedContent := findAcceptedEvent(t, results)
		innerResult, _ := acceptedContent["result"].(map[string]any)

		ticketID, _ := innerResult["ticket_id"].(string)
		if ticketID == "" {
			t.Error("accepted result has empty ticket_id")
		}
		t.Logf("worktree.remove accepted, ticket: %s", ticketID)
	})

	// --- Test validation: missing path ---
	t.Run("add_missing_path", func(t *testing.T) {
		requestID := "wt-nopath-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
			schema.CommandMessage{
				MsgType:    schema.MsgTypeCommand,
				Body:       "workspace.worktree.add (no path)",
				Command:    "workspace.worktree.add",
				Workspace:  "wtproj/main",
				RequestID:  requestID,
				Parameters: map[string]any{
					// path intentionally omitted
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "wt-trav-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.add ../escape",
				Command:   "workspace.worktree.add",
				Workspace: "wtproj/main",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "../escape",
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "wt-badmode-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
			schema.CommandMessage{
				MsgType:   schema.MsgTypeCommand,
				Body:      "workspace.worktree.remove feature/x (bad mode)",
				Command:   "workspace.worktree.remove",
				Workspace: "wtproj/main",
				RequestID: requestID,
				Parameters: map[string]any{
					"path": "feature/x",
					"mode": "destroy-everything",
				},
			})
		if err != nil {
			t.Fatalf("send command: %v", err)
		}

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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
		requestID := "wt-nows-" + t.Name()
		resultWatch := watchRoom(t, admin, workspaceRoomID)

		_, err := admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
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

		results := resultWatch.WaitForCommandResults(t, requestID, 1)
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

// TestWorkspaceWorktreeLifecycle exercises the full workspace-to-worktree
// lifecycle: workspace create, worktree add (with pipeline completion),
// host filesystem verification, worktree remove, and workspace destroy.
//
// This is the end-to-end test that proves:
//   - The worktree init pipeline (dev-worktree-init) creates a git worktree
//     inside a bwrap sandbox with workspace mounts and publishes "active" state
//   - The worktree deinit pipeline (dev-worktree-deinit) removes the worktree
//     and publishes "removed" state
//   - The workspace mount chain works: host workspace dir → launcher
//     variable expansion → bwrap bind-mount → pipeline executor → git worktree
//   - The full lifecycle integrates cleanly: workspace create → setup →
//     agent start → worktree add → worktree remove → workspace destroy
func TestWorkspaceWorktreeLifecycle(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ws-wt-lifecycle")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	runnerEnv := findRunnerEnv(t)

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    runnerEnv,
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Ticket service ---
	// Pipeline executors (workspace setup/teardown and worktree
	// add/remove) create pip- tickets. Deploy before workspace
	// creation so the daemon discovers the service via the service
	// directory. Ticket config is published to the workspace room
	// after the CLI creates it.
	deployTicketService(t, admin, fleet, machine, "ws-wt")

	// Resolve the pipeline room for principal invitations. The machine
	// itself was invited to all global rooms (template, pipeline, system,
	// machine, service, fleet) during provisioning (startMachineLauncher).
	pipelineRoomID := resolvePipelineRoom(t, admin)

	// --- Publish agent template ---
	agentTemplateRef, err := schema.ParseTemplateRef("bureau/template:test-wt-agent")
	if err != nil {
		t.Fatalf("parse agent template ref: %v", err)
	}
	_, err = templatedef.Push(ctx, admin, agentTemplateRef, schema.TemplateContent{
		Description: "Long-running agent for worktree lifecycle integration tests",
		Command:     []string{"sleep", "infinity"},
		Environment: runnerEnv,
		Namespaces:  &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Dest: "/tmp", Type: "tmpfs"},
			{Source: "${WORKSPACE_ROOT}", Dest: "/workspace", Mode: "ro"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME": "/workspace",
			"TERM": "xterm-256color",
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push agent template: %v", err)
	}

	// --- Create seed git repo ---
	seedRepoPath := machine.WorkspaceRoot + "/seed.git"
	initTestGitRepo(t, ctx, seedRepoPath)

	// --- Register principals ---
	// workspace create generates localparts: agent/<alias>/setup,
	// agent/<alias>/<index>, agent/<alias>/teardown. Use fleet-scoped
	// registration so the proxy authenticates as the fleet-scoped user
	// that the daemon invites to the workspace room.
	setupAccount := registerFleetPrincipal(t, fleet, "agent/wswt/main/setup", "test-password")
	agentAccount := registerFleetPrincipal(t, fleet, "agent/wswt/main/0", "test-password")
	teardownAccount := registerFleetPrincipal(t, fleet, "agent/wswt/main/teardown", "test-password")

	// --- Push encrypted credentials ---
	pushCredentials(t, admin, machine, setupAccount)
	pushCredentials(t, admin, machine, agentAccount)
	pushCredentials(t, admin, machine, teardownAccount)

	// --- Invite pipeline principals to the pipeline room ---
	for _, account := range []principalAccount{setupAccount, teardownAccount} {
		if err := admin.InviteUser(ctx, pipelineRoomID, account.UserID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite %s to pipeline room: %v", account.Localpart, err)
			}
		}
	}

	// --- Phase 1: Create workspace via API ---
	t.Log("phase 1: creating workspace")
	createWorkspace(t, admin, machine.Ref, "wswt/main", "bureau/template:test-wt-agent",
		map[string]string{"repository": "/workspace/seed.git"})

	workspaceRoomID, err := admin.ResolveAlias(ctx, ref.MustParseRoomAlias("#wswt/main:"+testServerName))
	if err != nil {
		t.Fatalf("resolve workspace room: %v", err)
	}

	// --- Phase 2: Wait for setup pipeline → workspace active ---
	t.Log("phase 2: waiting for setup pipeline to publish 'active' status")
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "active")
	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-init", "success")
	t.Log("workspace is active — setup pipeline completed")

	// --- Phase 3: Agent starts ---
	agentSocket := machine.PrincipalSocketPath(t, agentAccount.Localpart)
	waitForFile(t, agentSocket)

	agentProxyClient := proxyHTTPClient(agentSocket)
	agentIdentity := proxyWhoami(t, agentProxyClient)
	if agentIdentity != agentAccount.UserID.String() {
		t.Fatalf("agent whoami = %q, want %q", agentIdentity, agentAccount.UserID)
	}
	t.Log("agent started with verified proxy identity: " + agentIdentity)

	// --- Phase 4: Add worktree ---
	t.Log("phase 4: adding worktree feature/test-branch")
	addRequestID := "wt-add-lifecycle"
	addWatch := watchRoom(t, admin, workspaceRoomID)

	_, err = admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "workspace.worktree.add feature/test-branch",
			Command:   "workspace.worktree.add",
			Workspace: "wswt",
			RequestID: addRequestID,
			Parameters: map[string]any{
				"path":   "feature/test-branch",
				"branch": "main",
			},
		})
	if err != nil {
		t.Fatalf("send workspace.worktree.add: %v", err)
	}

	// Wait for the accepted acknowledgment. The daemon creates a pip-
	// ticket and returns immediately; the ticket watcher creates the
	// executor sandbox from the next /sync.
	addResults := addWatch.WaitForCommandResults(t, addRequestID, 1)
	addAccepted := findAcceptedEvent(t, addResults)

	addResult, _ := addAccepted["result"].(map[string]any)
	addTicketID, _ := addResult["ticket_id"].(string)
	if addTicketID == "" {
		t.Fatal("worktree.add accepted result has empty ticket_id")
	}
	t.Logf("worktree.add accepted, ticket: %s", addTicketID)

	// Verify the worktree state event reached "active".
	waitForWorktreeStatus(t, admin, workspaceRoomID, "feature/test-branch", "active")
	t.Log("worktree state is 'active'")

	// --- Phase 5: Verify worktree on disk ---
	worktreeDir := filepath.Join(machine.WorkspaceRoot, "wswt", "feature/test-branch")
	readmePath := filepath.Join(worktreeDir, "README.md")

	if _, err := os.Stat(worktreeDir); err != nil {
		t.Fatalf("worktree directory does not exist on host: %v", err)
	}
	if _, err := os.Stat(readmePath); err != nil {
		t.Fatalf("README.md not found in worktree: %v", err)
	}
	t.Log("worktree exists on disk with README.md from seed repo")

	// --- Phase 6: Remove worktree ---
	t.Log("phase 6: removing worktree feature/test-branch")
	removeRequestID := "wt-remove-lifecycle"
	removeWatch := watchRoom(t, admin, workspaceRoomID)

	_, err = admin.SendEvent(ctx, workspaceRoomID, schema.MatrixEventTypeMessage,
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "workspace.worktree.remove feature/test-branch",
			Command:   "workspace.worktree.remove",
			Workspace: "wswt",
			RequestID: removeRequestID,
			Parameters: map[string]any{
				"path": "feature/test-branch",
				"mode": "delete",
			},
		})
	if err != nil {
		t.Fatalf("send workspace.worktree.remove: %v", err)
	}

	// Wait for the accepted acknowledgment.
	removeResults := removeWatch.WaitForCommandResults(t, removeRequestID, 1)
	removeAccepted := findAcceptedEvent(t, removeResults)

	removeResult, _ := removeAccepted["result"].(map[string]any)
	removeTicketID, _ := removeResult["ticket_id"].(string)
	if removeTicketID == "" {
		t.Fatal("worktree.remove accepted result has empty ticket_id")
	}
	t.Logf("worktree.remove accepted, ticket: %s", removeTicketID)

	// Verify the worktree state event reached "removed".
	waitForWorktreeStatus(t, admin, workspaceRoomID, "feature/test-branch", "removed")
	t.Log("worktree state is 'removed'")

	// --- Phase 7: Verify worktree removed from disk ---
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists after removal: %v", err)
	}
	t.Log("worktree directory removed from disk")

	// --- Phase 8: Destroy workspace ---
	t.Log("phase 8: destroying workspace")
	destroyWorkspace(t, admin, "wswt/main")

	waitForFileGone(t, agentSocket)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	waitForWorkspaceStatus(t, admin, workspaceRoomID, "archived")
	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-deinit", "success")
	t.Log("workspace lifecycle complete: create → active → worktree add → worktree remove → destroy → archived")
}

// --- Test helpers ---
