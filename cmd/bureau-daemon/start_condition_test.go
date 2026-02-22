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
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Set up the workspace room with an active workspace event (condition is met).
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	// Configure a principal that gates on workspace status "active".
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Errorf("expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Set up the workspace room alias but do NOT publish the workspace event.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (condition not met), got %v", tracker.created)
	}
	if daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
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
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
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
	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Principal with no StartCondition.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/test"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Principal gates on a custom state event in its own config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/gated"),
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
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/gated", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Put the approval event in the config room.
	matrixState.setStateEvent(configRoomID, "m.bureau.approval", "agent/gated", map[string]string{
		"approved_by": "bureau-admin",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Principal references a room alias that doesn't exist.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/orphan"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspace,
					StateKey:  "",
					RoomAlias: ref.MustParseRoomAlias("#nonexistent/room:test.local"),
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/orphan", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (alias unresolvable), got %v", tracker.created)
	}
	if daemon.running[testEntity(t, fleet, "agent/orphan")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Workspace event exists but with "pending" status (not "active").
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "pending",
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
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
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
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
	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
		t.Error("principal should be running after content matches")
	}
}

// TestReconcile_ContentMatchArrayContains verifies that ContentMatch supports
// array containment: {"labels": "bug"} matches when the event's "labels" field
// is an array containing "bug".
func TestReconcile_ContentMatchArrayContains(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Ticket event with an array "labels" field containing "bug".
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-1", map[string]any{
		"title":  "Fix the widget",
		"labels": []any{"bug", "p1", "frontend"},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/bugfix"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-1",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("bug")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/bugfix", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "agent/bugfix" {
		t.Errorf("expected create-sandbox for agent/bugfix, got %v", tracker.created)
	}
}

// TestReconcile_ContentMatchArrayDoesNotContain verifies that ContentMatch
// correctly defers a principal when an array field does not contain the
// required value.
func TestReconcile_ContentMatchArrayDoesNotContain(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Ticket with labels that do NOT include "security".
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-2", map[string]any{
		"title":  "Improve performance",
		"labels": []any{"enhancement", "backend"},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/security"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-2",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("security")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/security", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (array does not contain value), got %v", tracker.created)
	}
}

// TestReconcile_ContentMatchArrayWithNonStringElements verifies that an array
// containing non-string elements (numbers, booleans, objects) does not match.
// Only string elements participate in containment checks.
func TestReconcile_ContentMatchArrayWithNonStringElements(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Array with non-string elements only.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-3", map[string]any{
		"title":  "Numeric labels",
		"labels": []any{42, true, map[string]any{"nested": "value"}},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/numeric"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-3",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("42")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/numeric", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (non-string array elements), got %v", tracker.created)
	}
}

// TestReconcile_ContentMatchMixedStringAndArray verifies that ContentMatch
// works correctly when one key matches against a string field and another
// matches against an array field. Both must match (AND semantics).
func TestReconcile_ContentMatchMixedStringAndArray(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-4", map[string]any{
		"title":  "Security bug",
		"labels": []any{"bug", "security", "p0"},
		"status": "open",
	})

	// Both conditions must match: status is string "open" AND labels contains "security".
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/secbug"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-4",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("open"), "labels": schema.Eq("security")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/secbug", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "agent/secbug" {
		t.Errorf("expected create-sandbox for agent/secbug, got %v", tracker.created)
	}
}

// TestReconcile_ContentMatchMixedStringAndArray_PartialFail verifies that when
// one ContentMatch key matches but the other doesn't, the principal is deferred.
func TestReconcile_ContentMatchMixedStringAndArray_PartialFail(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-5", map[string]any{
		"title":  "Feature request",
		"labels": []any{"enhancement", "backend"},
		"status": "open",
	})

	// Status matches ("open") but labels does not contain "security" → deferred.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/seconly"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-5",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("open"), "labels": schema.Eq("security")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/seconly", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (array match fails), got %v", tracker.created)
	}
}

// TestReconcile_ContentMatchEmptyArray verifies that an empty array never
// matches any ContentMatch value.
func TestReconcile_ContentMatchEmptyArray(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-6", map[string]any{
		"title":  "Unlabeled ticket",
		"labels": []any{},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/empty"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-6",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("bug")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/empty", schema.Credentials{
		Ciphertext: "encrypted-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls (empty array), got %v", tracker.created)
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Workspace is initially active — condition is met.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	// First reconcile: condition met → principal starts.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 1 || tracker.created[0] != "iree/amdgpu/pm" {
		t.Fatalf("first reconcile: expected create-sandbox for iree/amdgpu/pm, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
		t.Fatal("first reconcile: principal should be running")
	}
	tracker.mu.Unlock()

	// Simulate workspace transitioning to teardown.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "teardown",
		Project:   "iree",
		Machine:   machineName,
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
	if daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Workspace is initially active.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	// Two principals: one gated on workspace active, one unconditional.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
			{
				Principal: testEntity(t, fleet, "sysadmin/test"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				// No StartCondition — always runs.
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"sysadmin/test", schema.Credentials{
		Ciphertext: "encrypted-sysadmin-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	// First reconcile: both principals start.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 2 {
		t.Fatalf("first reconcile: expected 2 create-sandbox calls, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] || !daemon.running[testEntity(t, fleet, "sysadmin/test")] {
		t.Fatalf("first reconcile: both principals should be running, got %v", daemon.running)
	}
	tracker.mu.Unlock()

	// Workspace transitions to teardown — conditioned principal loses its condition.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    "teardown",
		Project:   "iree",
		Machine:   machineName,
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
	if daemon.running[testEntity(t, fleet, "iree/amdgpu/pm")] {
		t.Error("conditioned principal should have been stopped")
	}
	if !daemon.running[testEntity(t, fleet, "sysadmin/test")] {
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Set up a workspace event with status "teardown" and a teardown_mode field.
	// This is the event whose content should flow through as trigger content.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:       "teardown",
		TeardownMode: "archive",
		Project:      "iree",
		Machine:      machineName,
		UpdatedAt:    "2026-02-10T03:00:00Z",
	})

	// Configure a teardown principal gated on "teardown".
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "iree/amdgpu/inference/teardown"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("teardown")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"iree/amdgpu/inference/teardown", schema.Credentials{
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
	session, err := client.SessionFromToken(machine.UserID(), "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running[testEntity(t, fleet, "iree/amdgpu/inference/teardown")] {
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
	if triggerMap["machine"] != machineName {
		t.Errorf("trigger machine = %v, want %q", triggerMap["machine"], machineName)
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
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/always"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/always", schema.Credentials{
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
	session, err := client.SessionFromToken(machine.UserID(), "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running[testEntity(t, fleet, "agent/always")] {
		t.Fatal("principal should be running")
	}

	triggerMu.Lock()
	defer triggerMu.Unlock()

	if capturedTriggerContent != nil {
		t.Errorf("TriggerContent should be nil for unconditioned principal, got %s", string(capturedTriggerContent))
	}
}

// TestReconcile_ArrayContainmentTriggerContent verifies that when a
// StartCondition matches via array containment, the full event content
// (including the array field) is captured as TriggerContent and passed
// to the launcher's create-sandbox IPC request. This is the
// array-containment counterpart of TestReconcile_TriggerContentPassedToLauncher:
// the pipeline executor reads trigger.json to get event context, so the
// array values must be faithfully preserved.
func TestReconcile_ArrayContainmentTriggerContent(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Ticket event with an array field. The "labels" array contains "urgent"
	// which will satisfy the ContentMatch condition.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-100", map[string]any{
		"title":    "Critical production issue",
		"labels":   []any{"urgent", "production", "p0"},
		"status":   "open",
		"assignee": "oncall/primary",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/oncall"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-100",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("urgent")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/oncall", schema.Credentials{
		Ciphertext: "encrypted-oncall-credentials",
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
	session, err := client.SessionFromToken(machine.UserID(), "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running[testEntity(t, fleet, "agent/oncall")] {
		t.Fatal("agent/oncall should be running after array containment match")
	}

	triggerMu.Lock()
	defer triggerMu.Unlock()

	if capturedTriggerContent == nil {
		t.Fatal("TriggerContent should be non-nil when condition matches via array containment")
	}

	// Parse the trigger content and verify the full event is preserved,
	// including the array field that was matched against.
	var triggerMap map[string]any
	if err := json.Unmarshal(capturedTriggerContent, &triggerMap); err != nil {
		t.Fatalf("parsing TriggerContent: %v", err)
	}
	if triggerMap["title"] != "Critical production issue" {
		t.Errorf("trigger title = %v, want %q", triggerMap["title"], "Critical production issue")
	}
	if triggerMap["status"] != "open" {
		t.Errorf("trigger status = %v, want %q", triggerMap["status"], "open")
	}
	if triggerMap["assignee"] != "oncall/primary" {
		t.Errorf("trigger assignee = %v, want %q", triggerMap["assignee"], "oncall/primary")
	}

	// Verify the array field is preserved in the trigger content. JSON
	// unmarshal produces []any with string elements.
	labelsRaw, ok := triggerMap["labels"]
	if !ok {
		t.Fatal("trigger content missing 'labels' field")
	}
	labels, ok := labelsRaw.([]any)
	if !ok {
		t.Fatalf("trigger labels is %T, want []any", labelsRaw)
	}
	expectedLabels := []string{"urgent", "production", "p0"}
	if len(labels) != len(expectedLabels) {
		t.Fatalf("trigger labels length = %d, want %d", len(labels), len(expectedLabels))
	}
	for index, expected := range expectedLabels {
		if labels[index] != expected {
			t.Errorf("trigger labels[%d] = %v, want %q", index, labels[index], expected)
		}
	}
}

// TestReconcile_ArrayContainmentDeferredThenLaunches verifies the lifecycle
// when an array field initially does not contain the required value, then
// gets updated to include it. The first reconcile defers the principal;
// the second reconcile (after the array is updated) launches it.
func TestReconcile_ArrayContainmentDeferredThenLaunches(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Initial ticket has labels that do NOT include "reviewed".
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-200", map[string]any{
		"title":  "Needs code review",
		"labels": []any{"pending", "feature"},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/deployer"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-200",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("reviewed")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/deployer", schema.Credentials{
		Ciphertext: "encrypted-deployer-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	// First reconcile: "reviewed" not in array → deferred.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 0 {
		t.Errorf("first reconcile: expected no create-sandbox calls, got %v", tracker.created)
	}
	if daemon.running[testEntity(t, fleet, "agent/deployer")] {
		t.Error("first reconcile: agent/deployer should not be running")
	}
	tracker.mu.Unlock()

	// Simulate the review being completed: "reviewed" is now in the labels array.
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-200", map[string]any{
		"title":  "Needs code review",
		"labels": []any{"pending", "feature", "reviewed"},
		"status": "open",
	})

	// Second reconcile: "reviewed" now in array → launches.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.created) != 1 || tracker.created[0] != "agent/deployer" {
		t.Errorf("second reconcile: expected create-sandbox for agent/deployer, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "agent/deployer")] {
		t.Error("second reconcile: agent/deployer should be running after array now contains value")
	}
}

// TestReconcile_ArrayContainmentRunningPrincipalStopped verifies continuous
// enforcement with array containment: a principal that was running because
// its ContentMatch value was present in an array gets stopped when the array
// is updated to no longer contain that value. This mirrors the string-based
// TestReconcile_RunningPrincipalStoppedWhenConditionFails but for the array
// containment code path.
func TestReconcile_ArrayContainmentRunningPrincipalStopped(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Ticket initially has "active" in its tags array.
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-300", map[string]any{
		"title": "Active work item",
		"tags":  []any{"active", "sprint-42"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/worker"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-300",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"tags": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/worker", schema.Credentials{
		Ciphertext: "encrypted-worker-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	// First reconcile: "active" is in the tags array → principal starts.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 1 || tracker.created[0] != "agent/worker" {
		t.Fatalf("first reconcile: expected create-sandbox for agent/worker, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "agent/worker")] {
		t.Fatal("first reconcile: agent/worker should be running")
	}
	tracker.mu.Unlock()

	// Simulate the ticket being moved to "done": remove "active" from the tags.
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-300", map[string]any{
		"title": "Active work item",
		"tags":  []any{"done", "sprint-42"},
	})

	// Second reconcile: "active" is no longer in the tags → principal stopped.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.destroyed) != 1 || tracker.destroyed[0] != "agent/worker" {
		t.Errorf("second reconcile: expected destroy-sandbox for agent/worker, got %v", tracker.destroyed)
	}
	if daemon.running[testEntity(t, fleet, "agent/worker")] {
		t.Error("second reconcile: agent/worker should not be running after array no longer contains value")
	}
}

// TestReconcile_ArrayContainmentMultiplePrincipals verifies that multiple
// principals can gate on different values within the same array field of
// the same event. Each principal's ContentMatch specifies a different
// label, and only the principals whose required label is present in the
// array should start. This exercises the per-principal ContentMatch
// evaluation within a single reconcile cycle.
func TestReconcile_ArrayContainmentMultiplePrincipals(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Ticket with labels containing "bug" and "frontend" but NOT "backend".
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-400", map[string]any{
		"title":  "UI rendering glitch",
		"labels": []any{"bug", "frontend", "p1"},
		"status": "open",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/bugfixer"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-400",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("bug")},
				},
			},
			{
				Principal: testEntity(t, fleet, "agent/frontend"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-400",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("frontend")},
				},
			},
			{
				Principal: testEntity(t, fleet, "agent/backend"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-400",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"labels": schema.Eq("backend")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/bugfixer", schema.Credentials{
		Ciphertext: "encrypted-bugfixer-credentials",
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/frontend", schema.Credentials{
		Ciphertext: "encrypted-frontend-credentials",
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/backend", schema.Credentials{
		Ciphertext: "encrypted-backend-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	// agent/bugfixer and agent/frontend should start (their labels are in the array).
	// agent/backend should NOT start ("backend" is not in the array).
	if len(tracker.created) != 2 {
		t.Fatalf("expected 2 create-sandbox calls, got %d: %v", len(tracker.created), tracker.created)
	}

	createdSet := make(map[string]bool)
	for _, principal := range tracker.created {
		createdSet[principal] = true
	}

	if !createdSet["agent/bugfixer"] {
		t.Error("agent/bugfixer should have been created (labels contains 'bug')")
	}
	if !createdSet["agent/frontend"] {
		t.Error("agent/frontend should have been created (labels contains 'frontend')")
	}
	if createdSet["agent/backend"] {
		t.Error("agent/backend should NOT have been created (labels does not contain 'backend')")
	}

	if !daemon.running[testEntity(t, fleet, "agent/bugfixer")] {
		t.Error("agent/bugfixer should be running")
	}
	if !daemon.running[testEntity(t, fleet, "agent/frontend")] {
		t.Error("agent/frontend should be running")
	}
	if daemon.running[testEntity(t, fleet, "agent/backend")] {
		t.Error("agent/backend should NOT be running")
	}
}

// TestReconcile_ArrayContainmentFieldTypeChangesToArray verifies the
// transition from a string field to an array field across reconcile cycles.
// Initially the event field is a plain string (exact match works), then the
// field is updated to an array (array containment takes over). This tests
// that the reconcile logic handles the type change gracefully, restarting
// the principal when the underlying field type changes but the match
// semantics still hold.
func TestReconcile_ArrayContainmentFieldTypeChangesToArray(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		ticketRoomID   = "!tickets:test.local"
	)

	machine, fleet := testMachineSetup(t, "test", "test.local")
	machineName := machine.Localpart()
	fleetPrefix := fleet.Localpart() + "/"

	matrixState := newStartConditionTestState(t, fleet, configRoomID, templateRoomID)

	// Start with "category" as a plain string value "infrastructure".
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#tickets:test.local"), ticketRoomID)
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-500", map[string]any{
		"title":    "Server migration",
		"category": "infrastructure",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, fleet, "agent/infra"),
				Template:  "bureau/template:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeTicket,
					StateKey:     "TICKET-500",
					RoomAlias:    ref.MustParseRoomAlias("#tickets:test.local"),
					ContentMatch: schema.ContentMatch{"category": schema.Eq("infrastructure")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, fleetPrefix+"agent/infra", schema.Credentials{
		Ciphertext: "encrypted-infra-credentials",
	})

	daemon, tracker, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, machine, fleet)
	defer cleanup()

	// First reconcile: string match succeeds → principal starts.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	if len(tracker.created) != 1 || tracker.created[0] != "agent/infra" {
		t.Fatalf("first reconcile: expected create-sandbox for agent/infra, got %v", tracker.created)
	}
	if !daemon.running[testEntity(t, fleet, "agent/infra")] {
		t.Fatal("first reconcile: agent/infra should be running")
	}
	tracker.mu.Unlock()

	// Update the field from a string to an array that still contains the
	// value. The principal should remain running (condition is still satisfied).
	matrixState.setStateEvent(ticketRoomID, schema.EventTypeTicket, "TICKET-500", map[string]any{
		"title":    "Server migration",
		"category": []any{"infrastructure", "networking"},
	})

	// Second reconcile: field is now an array containing "infrastructure" →
	// condition still satisfied, principal stays running.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if len(tracker.destroyed) != 0 {
		t.Errorf("second reconcile: should not destroy (condition still met via array), destroyed %v", tracker.destroyed)
	}
	if !daemon.running[testEntity(t, fleet, "agent/infra")] {
		t.Error("second reconcile: agent/infra should still be running after field became array containing the value")
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
func newStartConditionTestState(t *testing.T, fleet ref.Fleet, configRoomID, templateRoomID string) *mockMatrixState {
	t.Helper()

	state := newMockMatrixState()

	state.setRoomAlias(fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	return state
}

// newStartConditionTestDaemon creates a Daemon backed by a mock Matrix server
// and a mock launcher. Returns the daemon, a tracker for created principals,
// and a cleanup function.
func newStartConditionTestDaemon(t *testing.T, matrixState *mockMatrixState, configRoomID string, machine ref.Machine, fleet ref.Fleet) (*Daemon, *principalTracker, func()) {
	t.Helper()

	matrixServer := httptest.NewServer(matrixState.handler())

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken(machine.UserID(), "test-token")
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

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
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
