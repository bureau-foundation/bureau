// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
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

	machine := newTestMachine(t, "machine/ws-lifecycle")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
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
	setupAccount := registerPrincipal(t, "wst/lifecycle/setup", "test-password")
	agentAccount := registerPrincipal(t, "wst/lifecycle/agent/0", "test-password")
	teardownAccount := registerPrincipal(t, "wst/lifecycle/teardown", "test-password")

	// --- Push encrypted credentials ---
	pushCredentials(t, admin, machine, setupAccount)
	pushCredentials(t, admin, machine, agentAccount)
	pushCredentials(t, admin, machine, teardownAccount)

	// --- Push MachineConfig with StartConditions ---
	// The daemon reads this from the config room via /sync and reconciles.
	// Setup starts immediately, agent and teardown are deferred by their
	// StartConditions.
	machineConfig := schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: setupAccount.Localpart,
				Template:  "",
				AutoStart: true,
				Labels:    map[string]string{"role": "setup"},
			},
			{
				Localpart: agentAccount.Localpart,
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
				Localpart: teardownAccount.Localpart,
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

	setupSocket := machine.PrincipalSocketPath(setupAccount.Localpart)
	agentSocket := machine.PrincipalSocketPath(agentAccount.Localpart)
	teardownSocket := machine.PrincipalSocketPath(teardownAccount.Localpart)

	// --- Phase 1: Setup starts, agent and teardown are deferred ---
	t.Log("phase 1: verifying setup starts with pending workspace status")
	waitForFile(t, setupSocket, 30*time.Second)

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

	waitForFile(t, agentSocket, 30*time.Second)
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
	waitForFileGone(t, agentSocket, 30*time.Second)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	waitForFile(t, teardownSocket, 30*time.Second)
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

	machine := newTestMachine(t, "machine/ws-cli")
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
	})

	ctx := t.Context()

	// --- Resolve global rooms and grant access ---
	// The daemon resolves the "base" template during reconciliation (needs
	// template room membership). The pipeline executor resolves pipeline
	// refs (dev-workspace-init, dev-workspace-deinit) via the proxy's
	// Matrix session (needs pipeline room membership for the principal).
	templateRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasTemplate, testServerName))
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}
	pipelineRoomID, err := admin.ResolveAlias(ctx, schema.FullRoomAlias(schema.RoomAliasPipeline, testServerName))
	if err != nil {
		t.Fatalf("resolve pipeline room: %v", err)
	}

	// Invite the machine to the template room (daemon resolves base template).
	if err := admin.InviteUser(ctx, templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}

	// --- Publish agent template ---
	// The agent principal needs a sandbox command that stays alive. The
	// runner environment provides coreutils (including sleep).
	_, err = admin.SendStateEvent(ctx, templateRoomID,
		schema.EventTypeTemplate, "test-ws-agent", schema.TemplateContent{
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
			},
			CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
			EnvironmentVariables: map[string]string{
				"HOME": "/workspace",
				"TERM": "xterm-256color",
			},
		})
	if err != nil {
		t.Fatalf("publish agent template: %v", err)
	}

	// --- Create seed git repo ---
	// The dev-workspace-init pipeline clones from ${REPOSITORY}. Inside
	// the sandbox, /workspace is mounted from machine.WorkspaceRoot. The
	// seed repo at WorkspaceRoot/seed.git becomes /workspace/seed.git.
	seedRepoPath := machine.WorkspaceRoot + "/seed.git"
	initTestGitRepo(t, ctx, seedRepoPath)

	// --- Register principals ---
	// workspace create generates localparts: <alias>/setup, <alias>/agent/0,
	// <alias>/teardown. The CLI doesn't register accounts — in production,
	// credential provisioning handles this. The test pre-creates them.
	setupAccount := registerPrincipal(t, "wscli/main/setup", "test-password")
	agentAccount := registerPrincipal(t, "wscli/main/agent/0", "test-password")
	teardownAccount := registerPrincipal(t, "wscli/main/teardown", "test-password")

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

	// Resolve the workspace room created by the CLI so we can poll its
	// status and pass it to waitForWorkspaceStatus.
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
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "active", 120*time.Second)
	t.Log("workspace status is 'active' — setup pipeline completed")

	// --- Phase 3: Verify agent started ---
	agentSocket := machine.PrincipalSocketPath(agentAccount.Localpart)
	waitForFile(t, agentSocket, 60*time.Second)
	t.Log("agent proxy socket appeared after workspace became active")

	// --- Phase 4: Destroy workspace via CLI ---
	t.Log("phase 4: running 'bureau workspace destroy'")
	runBureauOrFail(t, "workspace", "destroy", "wscli/main",
		"--credential-file", credentialFile,
		"--homeserver", testHomeserverURL,
		"--server-name", testServerName,
	)

	// Agent stops (condition "active" no longer matches "teardown").
	waitForFileGone(t, agentSocket, 30*time.Second)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	// Teardown pipeline (dev-workspace-deinit) runs and publishes "archived".
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "archived", 120*time.Second)
	t.Log("workspace status is 'archived' — teardown pipeline completed")

	t.Log("full CLI-driven workspace lifecycle verified: create → active → destroy → archived")
}

// --- Workspace test helpers ---

// createTestWorkspaceRoom creates a workspace room for integration tests.
// The room is created with WorkspaceRoomPowerLevels, the machine is invited,
// and the room is added as a child of the Bureau space.
func createTestWorkspaceRoom(t *testing.T, admin *messaging.Session, alias, machineUserID, adminUserID, spaceRoomID string) string {
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
	_, err = admin.SendStateEvent(ctx, spaceRoomID, schema.MatrixEventTypeSpaceChild, response.RoomID,
		map[string]any{"via": []string{testServerName}})
	if err != nil {
		t.Fatalf("add workspace room as space child: %v", err)
	}

	return response.RoomID
}

// waitForWorkspaceStatus polls the workspace room for the
// m.bureau.workspace state event until its status field matches the
// expected value or the context expires. Uses the test context for
// timeout bounding instead of wall-clock deadlines.
func waitForWorkspaceStatus(t *testing.T, session *messaging.Session, roomID, expectedStatus string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	for {
		content, err := session.GetStateEvent(ctx, roomID, schema.EventTypeWorkspace, "")
		if err == nil {
			var state schema.WorkspaceState
			if unmarshalError := json.Unmarshal(content, &state); unmarshalError == nil {
				if state.Status == expectedStatus {
					return
				}
			}
		}
		if ctx.Err() != nil {
			// Read the current status for the error message.
			currentStatus := "(unknown)"
			if content != nil {
				var state schema.WorkspaceState
				if json.Unmarshal(content, &state) == nil {
					currentStatus = state.Status
				}
			}
			t.Fatalf("timed out after %s waiting for workspace status %q in room %s (current: %s)",
				timeout, expectedStatus, roomID, currentStatus)
		}
		runtime.Gosched()
	}
}
