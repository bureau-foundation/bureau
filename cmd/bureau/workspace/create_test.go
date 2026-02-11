// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestBuildPrincipalAssignments_IncludesTeardown(t *testing.T) {
	t.Parallel()

	assignments := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		"machine/workstation",
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)

	// Expect: setup + 1 agent + teardown = 3 principals.
	if len(assignments) != 3 {
		t.Fatalf("expected 3 assignments (setup + agent + teardown), got %d: %v",
			len(assignments), localparts(assignments))
	}

	// Verify the teardown principal is the last one.
	teardown := assignments[len(assignments)-1]
	if teardown.Localpart != "iree/amdgpu/inference/teardown" {
		t.Errorf("teardown localpart = %q, want %q", teardown.Localpart, "iree/amdgpu/inference/teardown")
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

	// Verify the payload contains the expected fields.
	if teardown.Payload == nil {
		t.Fatal("teardown Payload is nil")
	}
	if teardown.Payload["pipeline_ref"] != "bureau/pipeline:dev-workspace-deinit" {
		t.Errorf("teardown pipeline_ref = %v, want %q", teardown.Payload["pipeline_ref"], "bureau/pipeline:dev-workspace-deinit")
	}
	if teardown.Payload["MACHINE"] != "machine/workstation" {
		t.Errorf("teardown MACHINE = %v, want %q", teardown.Payload["MACHINE"], "machine/workstation")
	}
	workspaceRoom, ok := teardown.Payload["WORKSPACE_ROOM"].(string)
	if !ok || workspaceRoom != "#iree/amdgpu/inference:bureau.local" {
		t.Errorf("teardown WORKSPACE_ROOM = %v, want %q", teardown.Payload["WORKSPACE_ROOM"], "#iree/amdgpu/inference:bureau.local")
	}
}

func TestBuildPrincipalAssignments_TeardownCondition(t *testing.T) {
	t.Parallel()

	assignments := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		1,
		"bureau.local",
		"machine/workstation",
		map[string]string{
			"repository": "https://github.com/iree-org/iree.git",
			"branch":     "main",
		},
	)

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
	if condition.ContentMatch["status"] != "teardown" {
		t.Errorf("condition ContentMatch[\"status\"] = %q, want %q", condition.ContentMatch["status"], "teardown")
	}
}

func TestBuildPrincipalAssignments_AgentCondition(t *testing.T) {
	t.Parallel()

	assignments := buildPrincipalAssignments(
		"iree/amdgpu/inference",
		"bureau/template:base",
		2,
		"bureau.local",
		"machine/workstation",
		map[string]string{"branch": "main"},
	)

	// setup (index 0) + 2 agents (1, 2) + teardown (3)
	if len(assignments) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(assignments))
	}

	// Setup principal has no condition.
	if assignments[0].StartCondition != nil {
		t.Error("setup principal should have no StartCondition")
	}

	// Both agents gate on "active".
	for _, index := range []int{1, 2} {
		agent := assignments[index]
		if agent.StartCondition == nil {
			t.Errorf("agent %d StartCondition is nil", index)
			continue
		}
		if agent.StartCondition.ContentMatch["status"] != "active" {
			t.Errorf("agent %d ContentMatch[\"status\"] = %q, want %q",
				index, agent.StartCondition.ContentMatch["status"], "active")
		}
	}

	// Teardown gates on "teardown".
	teardown := assignments[3]
	if teardown.StartCondition == nil {
		t.Fatal("teardown StartCondition is nil")
	}
	if teardown.StartCondition.ContentMatch["status"] != "teardown" {
		t.Errorf("teardown ContentMatch[\"status\"] = %q, want %q",
			teardown.StartCondition.ContentMatch["status"], "teardown")
	}
}

func TestBuildPrincipalAssignments_ZeroAgents(t *testing.T) {
	t.Parallel()

	assignments := buildPrincipalAssignments(
		"docs/writing",
		"bureau/template:base",
		0,
		"bureau.local",
		"machine/laptop",
		map[string]string{"branch": "main"},
	)

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

// localparts extracts the Localpart field from each assignment for error messages.
func localparts(assignments []schema.PrincipalAssignment) []string {
	result := make([]string, len(assignments))
	for index, assignment := range assignments {
		result[index] = assignment.Localpart
	}
	return result
}
