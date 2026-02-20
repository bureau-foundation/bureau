// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// testMachine constructs a ref.Machine for use in tests. The machine
// localpart will be "bureau/fleet/default/machine/<name>" with server
// "bureau.local".
func testMachine(t *testing.T, name string) ref.Machine {
	t.Helper()
	namespace, err := ref.NewNamespace("bureau.local", "bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}
	fleet, err := ref.NewFleet(namespace, "default")
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	machine, err := ref.NewMachine(fleet, name)
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	return machine
}

func TestBuildPrincipalAssignments_SetupPayload(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "workstation")
	const testRoomID = "!workspace123:bureau.local"
	assignments, err := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		machine,
		testRoomID,
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	setup := assignments[0]
	if setup.Principal.Name() != "iree/amdgpu/inference/setup" {
		t.Errorf("setup principal name = %q, want %q", setup.Principal.Name(), "iree/amdgpu/inference/setup")
	}
	if setup.Labels["role"] != "setup" {
		t.Errorf("setup role label = %q, want %q", setup.Labels["role"], "setup")
	}
	// Setup gates on workspace status "pending" so the daemon stops it
	// after the pipeline publishes "active" (preventing a restart loop).
	if setup.StartCondition == nil {
		t.Fatal("setup StartCondition is nil")
	}
	if setup.StartCondition.ContentMatch["status"].StringValue() != "pending" {
		t.Errorf("setup ContentMatch[\"status\"] = %q, want %q",
			setup.StartCondition.ContentMatch["status"].StringValue(), "pending")
	}

	// Verify the payload contains all pipeline variables needed by
	// dev-workspace-init.jsonc, using UPPERCASE keys that match the
	// pipeline's variable declarations.
	if setup.Payload == nil {
		t.Fatal("setup Payload is nil")
	}
	if setup.Payload["pipeline_ref"] != "bureau/pipeline:dev-workspace-init" {
		t.Errorf("setup pipeline_ref = %v, want %q", setup.Payload["pipeline_ref"], "bureau/pipeline:dev-workspace-init")
	}
	if setup.Payload["REPOSITORY"] != "https://github.com/iree-org/iree.git" {
		t.Errorf("setup REPOSITORY = %v, want %q", setup.Payload["REPOSITORY"], "https://github.com/iree-org/iree.git")
	}
	if setup.Payload["PROJECT"] != "iree" {
		t.Errorf("setup PROJECT = %v, want %q", setup.Payload["PROJECT"], "iree")
	}
	if setup.Payload["WORKSPACE_ROOM_ID"] != testRoomID {
		t.Errorf("setup WORKSPACE_ROOM_ID = %v, want %q", setup.Payload["WORKSPACE_ROOM_ID"], testRoomID)
	}
	if setup.Payload["MACHINE"] != machine.Localpart() {
		t.Errorf("setup MACHINE = %v, want %q", setup.Payload["MACHINE"], machine.Localpart())
	}
	if len(setup.Payload) != 5 {
		t.Errorf("setup payload should have exactly 5 keys (pipeline_ref, REPOSITORY, PROJECT, WORKSPACE_ROOM_ID, MACHINE), got %d: %v",
			len(setup.Payload), setup.Payload)
	}
}

func TestBuildPrincipalAssignments_IncludesTeardown(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "workstation")
	const testRoomID = "!workspace456:bureau.local"
	assignments, err := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		machine,
		testRoomID,
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	// Expect: setup + 1 agent + teardown = 3 principals.
	if len(assignments) != 3 {
		t.Fatalf("expected 3 assignments (setup + agent + teardown), got %d: %v",
			len(assignments), localparts(assignments))
	}

	// Verify the teardown principal is the last one.
	teardown := assignments[len(assignments)-1]
	if teardown.Principal.Name() != "iree/amdgpu/inference/teardown" {
		t.Errorf("teardown principal name = %q, want %q", teardown.Principal.Name(), "iree/amdgpu/inference/teardown")
	}
	if teardown.Template != "bureau/template:base" {
		t.Errorf("teardown template = %q, want %q", teardown.Template, "bureau/template:base")
	}
	if !teardown.AutoStart {
		t.Error("teardown AutoStart should be true")
	}
	if teardown.Labels["role"] != "teardown" {
		t.Errorf("teardown role label = %q, want %q", teardown.Labels["role"], "teardown")
	}

	// Verify the payload contains static per-principal config. Dynamic
	// context (teardown mode) comes from the trigger event as
	// EVENT_teardown_mode, not from the payload.
	if teardown.Payload == nil {
		t.Fatal("teardown Payload is nil")
	}
	if teardown.Payload["pipeline_ref"] != "bureau/pipeline:dev-workspace-deinit" {
		t.Errorf("teardown pipeline_ref = %v, want %q", teardown.Payload["pipeline_ref"], "bureau/pipeline:dev-workspace-deinit")
	}
	if teardown.Payload["PROJECT"] != "iree" {
		t.Errorf("teardown PROJECT = %v, want %q", teardown.Payload["PROJECT"], "iree")
	}
	if teardown.Payload["WORKSPACE_ROOM_ID"] != testRoomID {
		t.Errorf("teardown WORKSPACE_ROOM_ID = %v, want %q", teardown.Payload["WORKSPACE_ROOM_ID"], testRoomID)
	}
	if teardown.Payload["MACHINE"] != machine.Localpart() {
		t.Errorf("teardown MACHINE = %v, want %q", teardown.Payload["MACHINE"], machine.Localpart())
	}
	if len(teardown.Payload) != 4 {
		t.Errorf("teardown payload should have exactly 4 keys (pipeline_ref, PROJECT, WORKSPACE_ROOM_ID, MACHINE), got %d: %v",
			len(teardown.Payload), teardown.Payload)
	}
}

func TestBuildPrincipalAssignments_SetupCondition(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "workstation")
	assignments, err := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		machine,
		"!room:bureau.local",
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	setup := assignments[0]
	if setup.StartCondition == nil {
		t.Fatal("setup StartCondition is nil")
	}

	condition := setup.StartCondition
	if condition.EventType != schema.EventTypeWorkspace {
		t.Errorf("condition EventType = %q, want %q", condition.EventType, schema.EventTypeWorkspace)
	}
	if condition.StateKey != "" {
		t.Errorf("condition StateKey = %q, want empty", condition.StateKey)
	}
	if condition.RoomAlias != "#iree/amdgpu/inference:bureau.local" {
		t.Errorf("condition RoomAlias = %q, want %q", condition.RoomAlias, "#iree/amdgpu/inference:bureau.local")
	}
	if condition.ContentMatch["status"].StringValue() != "pending" {
		t.Errorf("condition ContentMatch[\"status\"] = %q, want %q", condition.ContentMatch["status"].StringValue(), "pending")
	}
}

func TestBuildPrincipalAssignments_TeardownCondition(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "workstation")
	assignments, err := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		machine,
		"!room:bureau.local",
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	teardown := assignments[len(assignments)-1]
	if teardown.StartCondition == nil {
		t.Fatal("teardown StartCondition is nil")
	}

	condition := teardown.StartCondition
	if condition.EventType != schema.EventTypeWorkspace {
		t.Errorf("condition EventType = %q, want %q", condition.EventType, schema.EventTypeWorkspace)
	}
	if condition.StateKey != "" {
		t.Errorf("condition StateKey = %q, want empty", condition.StateKey)
	}
	if condition.RoomAlias != "#iree/amdgpu/inference:bureau.local" {
		t.Errorf("condition RoomAlias = %q, want %q", condition.RoomAlias, "#iree/amdgpu/inference:bureau.local")
	}
	if condition.ContentMatch["status"].StringValue() != "teardown" {
		t.Errorf("condition ContentMatch[\"status\"] = %q, want %q", condition.ContentMatch["status"].StringValue(), "teardown")
	}
}

func TestBuildPrincipalAssignments_AgentCondition(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "workstation")
	assignments, err := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		2,
		"bureau.local",
		machine,
		"!room:bureau.local",
		map[string]string{"branch": "main"},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	// setup (index 0) + 2 agents (1, 2) + teardown (3)
	if len(assignments) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(assignments))
	}

	// Setup gates on "pending".
	if assignments[0].StartCondition == nil {
		t.Fatal("setup StartCondition is nil")
	}
	if assignments[0].StartCondition.ContentMatch["status"].StringValue() != "pending" {
		t.Errorf("setup ContentMatch[\"status\"] = %q, want %q",
			assignments[0].StartCondition.ContentMatch["status"].StringValue(), "pending")
	}

	// Both agents gate on "active".
	for _, index := range []int{1, 2} {
		agent := assignments[index]
		if agent.StartCondition == nil {
			t.Errorf("agent %d StartCondition is nil", index)
			continue
		}
		if agent.StartCondition.ContentMatch["status"].StringValue() != "active" {
			t.Errorf("agent %d ContentMatch[\"status\"] = %q, want %q",
				index, agent.StartCondition.ContentMatch["status"].StringValue(), "active")
		}
	}

	// Teardown gates on "teardown".
	teardown := assignments[3]
	if teardown.StartCondition == nil {
		t.Fatal("teardown StartCondition is nil")
	}
	if teardown.StartCondition.ContentMatch["status"].StringValue() != "teardown" {
		t.Errorf("teardown ContentMatch[\"status\"] = %q, want %q",
			teardown.StartCondition.ContentMatch["status"].StringValue(), "teardown")
	}
}

func TestBuildPrincipalAssignments_ZeroAgents(t *testing.T) {
	t.Parallel()

	machine := testMachine(t, "laptop")
	assignments, err := buildPrincipalAssignments(
		"docs/writing",
		"bureau/template:base",
		0,
		"bureau.local",
		machine,
		"!room:bureau.local",
		map[string]string{"branch": "main"},
	)
	if err != nil {
		t.Fatalf("buildPrincipalAssignments: %v", err)
	}

	// setup + teardown, no agents
	if len(assignments) != 2 {
		t.Fatalf("expected 2 assignments (setup + teardown), got %d: %v",
			len(assignments), localparts(assignments))
	}
	if assignments[0].Labels["role"] != "setup" {
		t.Errorf("first assignment role = %q, want %q", assignments[0].Labels["role"], "setup")
	}
	if assignments[1].Labels["role"] != "teardown" {
		t.Errorf("second assignment role = %q, want %q", assignments[1].Labels["role"], "teardown")
	}
}

// localparts extracts the Principal name from each assignment for error messages.
func localparts(assignments []schema.PrincipalAssignment) []string {
	result := make([]string, len(assignments))
	for index, assignment := range assignments {
		result[index] = assignment.Principal.Name()
	}
	return result
}
