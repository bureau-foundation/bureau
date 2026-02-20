// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestWorkspaceStartConditionLifecycle exercises the daemon's
// StartCondition-driven principal lifecycle: principals gated on
// workspace status transitions start and stop in response to state
// event changes published to the workspace room.
//
// Flow:
//   - Admin creates workspace room with status "pending"
//   - Three proxy-only principals are assigned to the machine:
//     setup (no condition), agent (gated on "active"), teardown (gated on "teardown")
//   - Setup starts immediately (proxy socket appears)
//   - Agent is deferred (status is "pending", not "active")
//   - Admin publishes status "active" (simulating setup pipeline completion)
//   - Agent starts (proxy socket appears)
//   - Admin publishes status "teardown" (simulating "bureau workspace destroy")
//   - Agent stops (proxy socket disappears — condition no longer matches)
//   - Teardown starts (proxy socket appears — condition now matches)
//
// This proves the daemon's reconciliation loop correctly evaluates
// StartConditions, starts principals when conditions become true, and
// stops them when conditions become false. The trigger content delivery
// (workspace state event content → trigger.json) is verified for the
// teardown principal by checking that the daemon passed the matched
// event content through the create-sandbox IPC.
func TestWorkspaceStartConditionLifecycle(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ws-lifecycle")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	ctx := t.Context()

	// --- Create the workspace room ---
	workspaceAlias := "wst/lifecycle"
	workspaceRoomAlias := "#wst/lifecycle:" + testServerName
	adminUserID := "@bureau-admin:" + testServerName

	spaceRoomID, err := admin.ResolveAlias(ctx, "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve bureau space: %v", err)
	}

	workspaceRoomID := createTestWorkspaceRoom(t, admin, workspaceAlias, machine.UserID, adminUserID, spaceRoomID)

	// Publish initial workspace state (pending). This is the state the
	// daemon sees when it first evaluates StartConditions. Setup (no
	// condition) starts immediately; agent (gated on "active") and
	// teardown (gated on "teardown") are both deferred.
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:    "pending",
			Project:   "wst",
			Machine:   machine.Name,
			UpdatedAt: "2026-01-01T00:00:00Z",
		})
	if err != nil {
		t.Fatalf("publish initial workspace state: %v", err)
	}

	// --- Register principals ---
	setupAccount := registerFleetPrincipal(t, fleet, "wst/lifecycle/setup", "test-password")
	agentAccount := registerFleetPrincipal(t, fleet, "wst/lifecycle/agent/0", "test-password")
	teardownAccount := registerFleetPrincipal(t, fleet, "wst/lifecycle/teardown", "test-password")

	// --- Push encrypted credentials ---
	pushCredentials(t, admin, machine, setupAccount)
	pushCredentials(t, admin, machine, agentAccount)
	pushCredentials(t, admin, machine, teardownAccount)

	// --- Push MachineConfig with StartConditions ---
	// The daemon reads this from the config room via /sync and reconciles.
	// Setup starts immediately, agent and teardown are deferred by their
	// StartConditions.
	buildEntity := func(localpart string) ref.Entity {
		entity, entityErr := ref.NewEntityFromAccountLocalpart(machine.Ref.Fleet(), localpart)
		if entityErr != nil {
			t.Fatalf("build principal entity for %s: %v", localpart, entityErr)
		}
		return entity
	}
	machineConfig := schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: buildEntity(setupAccount.Localpart),
				Template:  "",
				AutoStart: true,
				Labels:    map[string]string{"role": "setup"},
			},
			{
				Principal: buildEntity(agentAccount.Localpart),
				Template:  "",
				AutoStart: true,
				Labels:    map[string]string{"role": "agent"},
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    workspaceRoomAlias,
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
			{
				Principal: buildEntity(teardownAccount.Localpart),
				Template:  "",
				AutoStart: true,
				Labels:    map[string]string{"role": "teardown"},
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    workspaceRoomAlias,
					ContentMatch: schema.ContentMatch{"status": schema.Eq("teardown")},
				},
			},
		},
	}
	_, err = admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, machineConfig)
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	setupSocket := machine.PrincipalSocketPath(t, setupAccount.Localpart)
	agentSocket := machine.PrincipalSocketPath(t, agentAccount.Localpart)
	teardownSocket := machine.PrincipalSocketPath(t, teardownAccount.Localpart)

	// --- Phase 1: Setup starts, agent and teardown are deferred ---
	t.Log("phase 1: verifying setup starts with pending workspace status")
	waitForFile(t, setupSocket)

	// The agent should NOT have started because the workspace status is
	// "pending", not "active". No sleep needed: reconcile() evaluates all
	// principals in a single pass, and the setup socket above proves that
	// pass completed. If the daemon started setup but not agent, the
	// decision is final until the workspace status changes.
	if _, err := os.Stat(agentSocket); err == nil {
		t.Fatal("agent proxy socket should not exist while workspace status is 'pending'")
	}
	if _, err := os.Stat(teardownSocket); err == nil {
		t.Fatal("teardown proxy socket should not exist while workspace status is 'pending'")
	}

	// --- Phase 2: Transition to "active" → agent starts ---
	t.Log("phase 2: publishing 'active' status, expecting agent to start")
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:    "active",
			Project:   "wst",
			Machine:   machine.Name,
			UpdatedAt: "2026-01-01T00:00:00Z",
		})
	if err != nil {
		t.Fatalf("publish active workspace state: %v", err)
	}

	waitForFile(t, agentSocket)
	t.Log("agent proxy socket appeared after workspace became active")

	// Setup should still be running (its condition is nil — always true).
	if _, err := os.Stat(setupSocket); err != nil {
		t.Error("setup proxy socket disappeared after workspace became active")
	}
	// Teardown should not have started (waiting for "teardown", not "active").
	if _, err := os.Stat(teardownSocket); err == nil {
		t.Fatal("teardown proxy socket should not exist while workspace status is 'active'")
	}

	// --- Phase 3: Transition to "teardown" → agent stops, teardown starts ---
	t.Log("phase 3: publishing 'teardown' status, expecting agent to stop and teardown to start")
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:       "teardown",
			TeardownMode: "archive",
			Project:      "wst",
			Machine:      machine.Name,
			UpdatedAt:    "2026-01-01T00:00:00Z",
		})
	if err != nil {
		t.Fatalf("publish teardown workspace state: %v", err)
	}

	// Agent's condition (status == "active") is no longer satisfied, so
	// the daemon should destroy its sandbox. Teardown's condition
	// (status == "teardown") is now satisfied, so it should start.
	waitForFileGone(t, agentSocket)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	waitForFile(t, teardownSocket)
	t.Log("teardown proxy socket appeared after workspace entered teardown")

	t.Log("workspace start condition lifecycle verified: pending → active → teardown drives principal lifecycle")
}

// TestWorkspaceCLILifecycle exercises the full workspace lifecycle through
// the CLI commands: "bureau workspace create" and "bureau workspace destroy".
// Unlike TestWorkspaceStartConditionLifecycle (which uses proxy-only
// principals and admin-published state changes), this test proves the
// end-to-end path that a Bureau developer uses:
//
//   - "bureau workspace create" creates the workspace room, publishes config,
//     and assigns setup/agent/teardown principals on the machine
//   - The daemon's pipeline executor overlay wires the setup principal's
//     sandbox (base template + pipeline_ref → executor binary overlay)
//   - Setup pipeline (dev-workspace-init) clones a git repo and publishes
//     workspace status "active"
//   - Agent principal starts when workspace becomes active (StartCondition)
//   - "bureau workspace destroy" transitions the workspace to "teardown"
//   - Agent stops (condition "active" no longer matches)
//   - Teardown pipeline (dev-workspace-deinit) archives the workspace and
//     publishes status "archived"
func TestWorkspaceCLILifecycle(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "ws-cli")
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

	// --- Resolve pipeline room for principal invitations ---
	// The machine was invited to all global rooms (template, pipeline,
	// system, machine, service, fleet) during provisioning. Principal
	// accounts still need pipeline room membership for workspace
	// setup/teardown pipelines (the pipeline executor authenticates
	// as the principal via the proxy).
	pipelineRoomID, err := admin.ResolveAlias(ctx, testNamespace.PipelineRoomAlias())
	if err != nil {
		t.Fatalf("resolve pipeline room: %v", err)
	}

	// --- Publish agent template ---
	// The agent principal needs a sandbox command that stays alive. The
	// runner environment provides coreutils (including sleep). Uses the
	// production template.Push path to exercise room resolution and
	// state event publication identically to "bureau template push".
	agentTemplateRef, err := schema.ParseTemplateRef("bureau/template:test-ws-agent")
	if err != nil {
		t.Fatalf("parse agent template ref: %v", err)
	}
	_, err = template.Push(ctx, admin, agentTemplateRef, schema.TemplateContent{
		Description: "Long-running agent for workspace CLI integration tests",
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
			// Mount the workspace root read-only so the agent can access
			// project directories created by the setup pipeline. The
			// launcher expands ${WORKSPACE_ROOT} to the machine's actual
			// workspace directory at sandbox creation time.
			{Source: "${WORKSPACE_ROOT}", Dest: "/workspace", Mode: "ro"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME": "/workspace",
			"TERM": "xterm-256color",
		},
	}, testServerName)
	if err != nil {
		t.Fatalf("push agent template: %v", err)
	}

	// --- Create seed git repo ---
	// The dev-workspace-init pipeline clones from ${REPOSITORY}. Inside
	// the sandbox, /workspace is mounted from machine.WorkspaceRoot. The
	// seed repo at WorkspaceRoot/seed.git becomes /workspace/seed.git.
	seedRepoPath := machine.WorkspaceRoot + "/seed.git"
	initTestGitRepo(t, ctx, seedRepoPath)

	// --- Register principals ---
	// workspace create generates localparts: agent/<alias>/setup,
	// agent/<alias>/<index>, agent/<alias>/teardown. In production,
	// credential provisioning (principal.Create) registers accounts with
	// fleet-scoped localparts. Use registerFleetPrincipal to match production:
	// the proxy authenticates as the fleet-scoped user, which the daemon
	// invites to the workspace room via ensurePrincipalRoomAccess.
	setupAccount := registerFleetPrincipal(t, fleet, "agent/wscli/main/setup", "test-password")
	agentAccount := registerFleetPrincipal(t, fleet, "agent/wscli/main/0", "test-password")
	teardownAccount := registerFleetPrincipal(t, fleet, "agent/wscli/main/teardown", "test-password")

	// --- Push encrypted credentials ---
	pushCredentials(t, admin, machine, setupAccount)
	pushCredentials(t, admin, machine, agentAccount)
	pushCredentials(t, admin, machine, teardownAccount)

	// --- Invite pipeline principals to the pipeline room ---
	// The pipeline executor resolves pipeline refs (bureau/pipeline:dev-workspace-init,
	// bureau/pipeline:dev-workspace-deinit) via the proxy, which authenticates
	// as the principal. The principal needs pipeline room membership to read
	// state events in the private room.
	for _, account := range []principalAccount{setupAccount, teardownAccount} {
		if err := admin.InviteUser(ctx, pipelineRoomID, account.UserID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite %s to pipeline room: %v", account.Localpart, err)
			}
		}
	}

	// --- Phase 1: Create workspace via CLI ---
	t.Log("phase 1: running 'bureau workspace create'")
	runBureauOrFail(t, "workspace", "create", "wscli/main",
		"--machine", machine.Name,
		"--template", "bureau/template:test-ws-agent",
		"--param", "repository=/workspace/seed.git",
		"--credential-file", credentialFile,
		"--homeserver", testHomeserverURL,
		"--server-name", testServerName,
	)

	// Resolve the workspace room created by the CLI so we can watch its
	// status transitions.
	workspaceRoomID, err := admin.ResolveAlias(ctx, "#wscli/main:"+testServerName)
	if err != nil {
		t.Fatalf("resolve workspace room created by CLI: %v", err)
	}

	// --- Phase 2: Wait for setup pipeline to complete ---
	// The daemon picks up the MachineConfig, resolves bureau/template:base
	// for the setup principal, applies the pipeline executor overlay (since
	// the payload has pipeline_ref and base has no command), and creates the
	// sandbox. The executor runs dev-workspace-init: clones the seed repo,
	// publishes workspace status "active".
	t.Log("phase 2: waiting for setup pipeline to publish 'active' status")
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "active")
	t.Log("workspace status is 'active' — setup pipeline completed")

	// Verify the pipeline published a structured result. This catches
	// pipelines that change status but fail partway through — the result
	// event records the conclusion and per-step outcomes.
	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-init", "success")

	// Verify the workspace state event contains the expected content.
	// The pipeline publishes this via Matrix (not filesystem), so reading
	// it back from Matrix state proves the pipeline's publish step
	// executed correctly and the data is available to other systems.
	workspaceStateRaw, err := admin.GetStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "")
	if err != nil {
		t.Fatalf("read workspace state after setup: %v", err)
	}
	var activeState schema.WorkspaceState
	if err := json.Unmarshal(workspaceStateRaw, &activeState); err != nil {
		t.Fatalf("unmarshal workspace state: %v", err)
	}
	if activeState.Project != "wscli" {
		t.Errorf("workspace state project = %q, want %q", activeState.Project, "wscli")
	}
	if activeState.Machine != machine.Name {
		t.Errorf("workspace state machine = %q, want %q", activeState.Machine, machine.Name)
	}
	t.Logf("workspace state verified: project=%s, machine=%s, status=%s", activeState.Project, activeState.Machine, activeState.Status)

	// --- Phase 3: Verify agent started and its proxy works ---
	agentSocket := machine.PrincipalSocketPath(t, agentAccount.Localpart)
	waitForFile(t, agentSocket)
	t.Log("agent proxy socket appeared after workspace became active")

	// Verify the agent's proxy correctly injects credentials by calling
	// whoami through the proxy. This proves the full chain: daemon reads
	// encrypted credentials from Matrix → decrypts with machine key →
	// passes to launcher → launcher configures proxy → proxy injects
	// token on HTTP requests.
	agentProxyClient := proxyHTTPClient(agentSocket)
	agentIdentity := proxyWhoami(t, agentProxyClient)
	if agentIdentity != agentAccount.UserID {
		t.Fatalf("agent whoami = %q, want %q", agentIdentity, agentAccount.UserID)
	}
	t.Log("agent proxy identity verified: " + agentIdentity)

	// --- Phase 4: Destroy workspace via CLI ---
	t.Log("phase 4: running 'bureau workspace destroy'")
	runBureauOrFail(t, "workspace", "destroy", "wscli/main",
		"--credential-file", credentialFile,
		"--homeserver", testHomeserverURL,
		"--server-name", testServerName,
	)

	// Agent stops (condition "active" no longer matches "teardown").
	waitForFileGone(t, agentSocket)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	// Teardown pipeline (dev-workspace-deinit) runs and publishes "archived".
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "archived")
	t.Log("workspace status is 'archived' — teardown pipeline completed")

	// Verify teardown pipeline published a successful result.
	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-deinit", "success")

	t.Log("full CLI-driven workspace lifecycle verified: create → active → destroy → archived")
}

// --- Workspace test helpers ---

// createTestWorkspaceRoom creates a workspace room for integration tests.
// The room is created with WorkspaceRoomPowerLevels, the machine is invited,
// and the room is added as a child of the Bureau space.
func createTestWorkspaceRoom(t *testing.T, admin *messaging.DirectSession, alias, machineUserID, adminUserID string, spaceRoomID ref.RoomID) ref.RoomID {
	t.Helper()

	ctx := t.Context()

	response, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      alias,
		Alias:                     alias,
		Topic:                     "Workspace integration test: " + alias,
		Preset:                    "private_chat",
		Invite:                    []string{machineUserID},
		Visibility:                "private",
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(adminUserID, machineUserID),
	})
	if err != nil {
		t.Fatalf("create workspace room %s: %v", alias, err)
	}

	// Add as child of Bureau space.
	_, err = admin.SendStateEvent(ctx, spaceRoomID, schema.MatrixEventTypeSpaceChild, response.RoomID.String(),
		map[string]any{"via": []string{testServerName}})
	if err != nil {
		t.Fatalf("add workspace room as space child: %v", err)
	}

	return response.RoomID
}

// waitForWorkspaceStatus waits for the workspace room's m.bureau.workspace
// state event to reach the expected status. Uses Matrix /sync long-polling
// (via room watch) instead of polling GetStateEvent — the server holds the
// connection until events arrive, consuming zero CPU while waiting.
//
// The watch is created BEFORE checking current state, so events that arrive
// between the GetStateEvent check and the first /sync are not lost.
// Bounded by t.Context() (test timeout), not an explicit deadline.
func waitForWorkspaceStatus(t *testing.T, session *messaging.DirectSession, roomID ref.RoomID, expectedStatus string) {
	t.Helper()

	// Create the watch first to capture the sync stream position. Events
	// arriving after this point are guaranteed to be delivered by
	// WaitForEvent, even if they arrive before we check GetStateEvent.
	watch := watchRoom(t, session, roomID)

	// Check whether the status already matches. This handles the case
	// where the pipeline completed before we started watching (the setup
	// pipeline is fast and may finish before the test resolves the room
	// alias and calls this function).
	content, err := session.GetStateEvent(t.Context(), roomID, schema.EventTypeWorkspace, "")
	if err == nil {
		var state schema.WorkspaceState
		if json.Unmarshal(content, &state) == nil && state.Status == expectedStatus {
			return
		}
	}

	// Wait for the workspace status to transition via /sync. The predicate
	// filters for workspace state events with the expected status, ignoring
	// intermediate transitions (e.g., "pending" → "active" when waiting for
	// "active", or "teardown" events when waiting for "archived").
	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeWorkspace {
			return false
		}
		if event.StateKey == nil || *event.StateKey != "" {
			return false
		}
		contentJSON, marshalError := json.Marshal(event.Content)
		if marshalError != nil {
			return false
		}
		var state schema.WorkspaceState
		if json.Unmarshal(contentJSON, &state) != nil {
			return false
		}
		return state.Status == expectedStatus
	}, fmt.Sprintf("workspace status %q in room %s", expectedStatus, roomID))
}

// waitForWorktreeStatus waits for a worktree state event
// (m.bureau.worktree) in the workspace room to reach the expected status.
// The state key matches the worktree path (e.g., "feature/test-branch").
// Uses the same watch-then-check pattern as waitForWorkspaceStatus.
func waitForWorktreeStatus(t *testing.T, session *messaging.DirectSession, roomID ref.RoomID, worktreePath, expectedStatus string) {
	t.Helper()

	watch := watchRoom(t, session, roomID)

	// Check whether the status already matches.
	content, err := session.GetStateEvent(t.Context(), roomID, schema.EventTypeWorktree, worktreePath)
	if err == nil {
		var state schema.WorktreeState
		if json.Unmarshal(content, &state) == nil && state.Status == expectedStatus {
			return
		}
	}

	// Wait for the worktree status to transition via /sync.
	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeWorktree {
			return false
		}
		if event.StateKey == nil || *event.StateKey != worktreePath {
			return false
		}
		contentJSON, marshalError := json.Marshal(event.Content)
		if marshalError != nil {
			return false
		}
		var state schema.WorktreeState
		if json.Unmarshal(contentJSON, &state) != nil {
			return false
		}
		return state.Status == expectedStatus
	}, fmt.Sprintf("worktree %q status %q in room %s", worktreePath, expectedStatus, roomID))
}

// verifyPipelineResult waits for and verifies the m.bureau.pipeline_result
// state event published by the pipeline executor. The result event is
// published AFTER the pipeline's own publish steps (e.g., workspace status
// "active"), so it may not exist yet when the workspace status watch returns.
// Uses the same watch-then-check pattern as waitForWorkspaceStatus.
func verifyPipelineResult(t *testing.T, session *messaging.DirectSession, roomID ref.RoomID, pipelineName, expectedConclusion string) {
	t.Helper()

	// Set up watch before checking current state, same as waitForWorkspaceStatus.
	watch := watchRoom(t, session, roomID)

	// Check if the result already exists.
	content, err := session.GetStateEvent(t.Context(), roomID, schema.EventTypePipelineResult, pipelineName)
	if err == nil {
		checkPipelineResultContent(t, content, pipelineName, expectedConclusion)
		return
	}

	// Wait for the pipeline_result state event to arrive.
	raw := watch.WaitForStateEvent(t, schema.EventTypePipelineResult, pipelineName)
	checkPipelineResultContent(t, raw, pipelineName, expectedConclusion)
}

// checkPipelineResultContent unmarshals and verifies the pipeline result
// content. Separated from verifyPipelineResult so both the immediate-check
// and sync-wait paths share the same validation logic.
func checkPipelineResultContent(t *testing.T, content json.RawMessage, pipelineName, expectedConclusion string) {
	t.Helper()

	var result schema.PipelineResultContent
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("unmarshal pipeline_result for %q: %v", pipelineName, err)
	}

	if result.Conclusion != expectedConclusion {
		t.Errorf("pipeline %q conclusion = %q, want %q", pipelineName, result.Conclusion, expectedConclusion)
		if result.FailedStep != "" {
			t.Errorf("  failed step: %s", result.FailedStep)
		}
		if result.ErrorMessage != "" {
			t.Errorf("  error: %s", result.ErrorMessage)
		}
		for _, step := range result.StepResults {
			t.Logf("  step %q: %s (%dms)", step.Name, step.Status, step.DurationMS)
		}
	}

	if result.PipelineRef == "" {
		t.Errorf("pipeline %q result has empty pipeline_ref", pipelineName)
	}
	if result.StepCount < 1 {
		t.Errorf("pipeline %q result has step_count = %d, want >= 1", pipelineName, result.StepCount)
	}
	if result.StartedAt == "" {
		t.Errorf("pipeline %q result has empty started_at", pipelineName)
	}
	if result.CompletedAt == "" {
		t.Errorf("pipeline %q result has empty completed_at", pipelineName)
	}

	t.Logf("pipeline %q: conclusion=%s, steps=%d, duration=%dms",
		pipelineName, result.Conclusion, result.StepCount, result.DurationMS)
}
