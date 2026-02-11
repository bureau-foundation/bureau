// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestReconcile_StartConditionMet verifies that a principal with a
// satisfied StartCondition launches normally.
func TestReconcile_StartConditionMet(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Set up the workspace room with an active workspace event (condition is met).
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	// Configure a principal that gates on workspace status "active".
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Errorf("expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should be running after start condition is met")
	}
}

// TestReconcile_StartConditionNotMet verifies that a principal with an
// unsatisfied StartCondition is deferred (no create-sandbox call).
func TestReconcile_StartConditionNotMet(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Set up the workspace room alias but do NOT publish the workspace event.
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (condition not met), got %v", tracker.created)
	}
	if daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should not be running when start condition is not met")
	}
}

// TestReconcile_StartConditionDeferredThenLaunches verifies the lifecycle:
// first reconcile defers the principal (event missing), then the event appears,
// and the second reconcile launches it.
func TestReconcile_StartConditionDeferredThenLaunches(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	// First reconcile: event missing → deferred.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 0 {
		t.Errorf("first reconcile: expected no create-sandbox calls, got %v", tracker.created)
	}
	tracker.mu.Unlock()

	// Simulate the setup principal publishing workspace active status.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T01:00:00Z",
	})

	// Second reconcile: event now exists → launches.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Errorf("second reconcile: expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should be running after start condition becomes satisfied")
	}
}

// TestReconcile_NoStartCondition verifies that a principal without a
// StartCondition launches normally (no gating).
func TestReconcile_NoStartCondition(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal with no StartCondition.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "agent/test" {
		t.Errorf("expected create-sandbox for agent/test, got %v", tracker.created)
	}
}

// TestReconcile_StartConditionConfigRoom verifies that a StartCondition with
// an empty RoomAlias checks the config room (where MachineConfig lives).
func TestReconcile_StartConditionConfigRoom(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal gates on a custom state event in its own config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/gated",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: "m.bureau.approval",
					StateKey:  "agent/gated",
					// RoomAlias empty → checks configRoomID
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/gated", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Put the approval event in the config room.
	matrixState.setStateEvent(configRoomID, "m.bureau.approval", "agent/gated", map[string]string{
		"approved_by": "bureau-admin",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "agent/gated" {
		t.Errorf("expected create-sandbox for agent/gated, got %v", tracker.created)
	}
}

// TestReconcile_StartConditionUnresolvableAlias verifies that when the
// room alias in a StartCondition doesn't resolve, the principal is deferred.
func TestReconcile_StartConditionUnresolvableAlias(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal references a room alias that doesn't exist.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/orphan",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspace,
					StateKey:  "",
					RoomAlias: "#nonexistent/room:test.local",
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/orphan", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (alias unresolvable), got %v", tracker.created)
	}
	if daemon.running["agent/orphan"] {
		t.Error("principal should not be running when room alias is unresolvable")
	}
}

// TestReconcile_StartConditionContentMismatch verifies that a principal is
// deferred when the state event exists but the content doesn't match the
// ContentMatch criteria. This is the key workspace lifecycle scenario:
// the event exists from "bureau workspace create" with status "pending",
// but agents should only launch when status becomes "active".
func TestReconcile_StartConditionContentMismatch(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Workspace event exists but with "pending" status (not "active").
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "pending",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	// First reconcile: event exists but status is "pending" → deferred.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 0 {
		t.Errorf("first reconcile: expected no create-sandbox calls (content mismatch), got %v", tracker.created)
	}
	tracker.mu.Unlock()

	// Update the workspace event to "active".
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T01:00:00Z",
	})

	// Second reconcile: content now matches → launches.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Errorf("second reconcile: expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should be running after content matches")
	}
}

// TestReconcile_RunningPrincipalStoppedWhenConditionFails verifies continuous
// enforcement: a principal that was running because its StartCondition was
// satisfied gets stopped when the condition becomes false. This is the core
// lifecycle mechanism — when a workspace status changes from "active" to
// "teardown", agents gated on "active" are excluded from the desired set
// and the "destroy unneeded" pass stops them.
func TestReconcile_RunningPrincipalStoppedWhenConditionFails(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Workspace is initially active — condition is met.
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	// First reconcile: condition met → principal starts.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Fatalf("first reconcile: expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Fatal("first reconcile: principal should be running")
	}
	tracker.mu.Unlock()

	// Simulate workspace transitioning to teardown.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "teardown",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T02:00:00Z",
	})

	// Second reconcile: condition no longer met → principal stopped.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.destroyed) != 1 || tracker.destroyed[0] != "iree/amdgpu/pm" {
		t.Errorf("second reconcile: expected destroy-sandbox for iree/amdgpu/pm, got %v", tracker.destroyed)
	}
	if daemon.running["iree/amdgpu/pm"] {
		t.Error("second reconcile: principal should no longer be running")
	}
}

// TestReconcile_ConditionFalseDoesNotStopUnconditionedPrincipal verifies that
// when a conditioned principal's StartCondition becomes false, only that
// principal is stopped — unconditioned principals on the same machine continue
// running undisturbed.
func TestReconcile_ConditionFalseDoesNotStopUnconditionedPrincipal(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Workspace is initially active.
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	// Two principals: one gated on workspace active, one unconditional.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "active"},
				},
			},
			{
				Localpart: "sysadmin/test",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				// No StartCondition — always runs.
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "sysadmin/test", schema.Credentials{
		Ciphertext: "encrypted-sysadmin-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	// First reconcile: both principals start.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 2 {
		t.Fatalf("first reconcile: expected 2 create-sandbox calls, got %v", tracker.created)
	}
	if !daemon.running["iree/amdgpu/pm"] || !daemon.running["sysadmin/test"] {
		t.Fatalf("first reconcile: both principals should be running, got %v", daemon.running)
	}
	tracker.mu.Unlock()

	// Workspace transitions to teardown — conditioned principal loses its condition.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "teardown",
		Project:   "iree",
		Machine:   "machine/test",
		UpdatedAt: "2026-02-10T02:00:00Z",
	})

	// Second reconcile: conditioned principal stops, unconditioned survives.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.destroyed) != 1 || tracker.destroyed[0] != "iree/amdgpu/pm" {
		t.Errorf("second reconcile: expected destroy-sandbox for iree/amdgpu/pm only, got %v", tracker.destroyed)
	}
	if daemon.running["iree/amdgpu/pm"] {
		t.Error("conditioned principal should have been stopped")
	}
	if !daemon.running["sysadmin/test"] {
		t.Error("unconditioned principal should still be running")
	}
}

// TestReconcile_TriggerContentPassedToLauncher verifies that when a
// StartCondition-gated principal launches, the create-sandbox IPC request
// includes the full content of the matched state event as TriggerContent.
// This is the foundation of event-driven parameter passing: the teardown
// pipeline reads ${EVENT_teardown_mode} from trigger.json, which comes
// from the workspace state event that satisfied its StartCondition.
func TestReconcile_TriggerContentPassedToLauncher(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Set up a workspace event with status "teardown" and a teardown_mode field.
	// This is the event whose content should flow through as trigger content.
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:       "teardown",
		TeardownMode: "archive",
		Project:      "iree",
		Machine:      "machine/test",
		UpdatedAt:    "2026-02-10T03:00:00Z",
	})

	// Configure a teardown principal gated on "teardown".
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/inference/teardown",
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: map[string]string{"status": "teardown"},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/inference/teardown", schema.Credentials{
		Ciphertext: "encrypted-teardown-credentials",
	})

	// Use a custom mock launcher that captures TriggerContent from the IPC request.
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedTriggerContent json.RawMessage
	var triggerMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		triggerMu.Lock()
		defer triggerMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedTriggerContent = request.TriggerContent
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	daemon := &Daemon{
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastCredentials:     make(map[string]string),
		lastVisibility:      make(map[string][]string),
		lastMatrixPolicy:    make(map[string]*schema.MatrixPolicy),
		lastObservePolicy:   make(map[string]*schema.ObservePolicy),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return nil
		},
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["iree/amdgpu/inference/teardown"] {
		t.Fatal("teardown principal should be running")
	}

	// Verify the trigger content was passed through.
	triggerMu.Lock()
	defer triggerMu.Unlock()

	if capturedTriggerContent == nil {
		t.Fatal("TriggerContent should be non-nil for a StartCondition-gated principal")
	}

	// Parse the trigger content and verify it contains the workspace state fields.
	var triggerMap map[string]any
	if err := json.Unmarshal(capturedTriggerContent, &triggerMap); err != nil {
		t.Fatalf("parsing TriggerContent: %v", err)
	}
	if triggerMap["status"] != "teardown" {
		t.Errorf("trigger status = %v, want %q", triggerMap["status"], "teardown")
	}
	if triggerMap["teardown_mode"] != "archive" {
		t.Errorf("trigger teardown_mode = %v, want %q", triggerMap["teardown_mode"], "archive")
	}
	if triggerMap["project"] != "iree" {
		t.Errorf("trigger project = %v, want %q", triggerMap["project"], "iree")
	}
	if triggerMap["machine"] != "machine/test" {
		t.Errorf("trigger machine = %v, want %q", triggerMap["machine"], "machine/test")
	}
}

// TestReconcile_NoTriggerContentForUnconditionedPrincipal verifies that
// principals without a StartCondition have nil TriggerContent in the IPC
// request. Only event-triggered principals get trigger content.
func TestReconcile_NoTriggerContentForUnconditionedPrincipal(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/always",
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/always", schema.Credentials{
		Ciphertext: "encrypted-always-credentials",
	})

	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedTriggerContent json.RawMessage
	var triggerMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		triggerMu.Lock()
		defer triggerMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedTriggerContent = request.TriggerContent
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	daemon := &Daemon{
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastCredentials:     make(map[string]string),
		lastVisibility:      make(map[string][]string),
		lastMatrixPolicy:    make(map[string]*schema.MatrixPolicy),
		lastObservePolicy:   make(map[string]*schema.ObservePolicy),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return nil
		},
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["agent/always"] {
		t.Fatal("principal should be running")
	}

	triggerMu.Lock()
	defer triggerMu.Unlock()

	if capturedTriggerContent != nil {
		t.Errorf("TriggerContent should be nil for unconditioned principal, got %s", string(capturedTriggerContent))
	}
}

// --- Test helpers ---

// principalTracker is a thread-safe tracker for principals passed to
// create-sandbox and destroy-sandbox IPC calls.
type principalTracker struct {
	mu        sync.Mutex
	created   []string
	destroyed []string
}

// newStartConditionTestState creates a mock Matrix state with a base template.
// The caller sets MachineConfig and StartCondition per test.
func newStartConditionTestState(t *testing.T, configRoomID, templateRoomID, machineName string) *mockMatrixState {
	t.Helper()

	state := newMockMatrixState()

	state.setRoomAlias("#bureau/template:test.local", templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	return state
}

// newStartConditionTestDaemon creates a Daemon backed by a mock Matrix server
// and a mock launcher. Returns the daemon, a tracker for created principals,
// and a cleanup function.
func newStartConditionTestDaemon(t *testing.T, matrixState *mockMatrixState, configRoomID, serverName, machineName string) (*Daemon, *principalTracker, func()) {
	t.Helper()

	matrixServer := httptest.NewServer(matrixState.handler())

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	tracker := &principalTracker{}
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		switch request.Action {
		case "create-sandbox":
			tracker.created = append(tracker.created, request.Principal)
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		case "destroy-sandbox":
			tracker.destroyed = append(tracker.destroyed, request.Principal)
			return launcherIPCResponse{OK: true}
		default:
			return launcherIPCResponse{OK: true}
		}
	})

	daemon := &Daemon{
		runDir:              principal.DefaultRunDir,
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastCredentials:     make(map[string]string),
		lastVisibility:      make(map[string][]string),
		lastMatrixPolicy:    make(map[string]*schema.MatrixPolicy),
		lastObservePolicy:   make(map[string]*schema.ObservePolicy),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		previousSpecs:       make(map[string]*schema.SandboxSpec),
		lastTemplates:       make(map[string]*schema.TemplateContent),
		healthMonitors:      make(map[string]*healthMonitor),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return nil
		},
	}

	cleanup := func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
		listener.Close()
		session.Close()
		matrixServer.Close()
	}

	return daemon, tracker, cleanup
}
