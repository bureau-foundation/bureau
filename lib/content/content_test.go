// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestPipelines(t *testing.T) {
	t.Parallel()

	pipelines, err := Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(pipelines) == 0 {
		t.Fatal("expected at least one embedded pipeline")
	}

	// Index pipelines by name for targeted verification.
	byName := make(map[string]Pipeline, len(pipelines))
	for _, p := range pipelines {
		byName[p.Name] = p
	}

	// Verify all four dev pipelines are present.
	for _, name := range []string{"dev-workspace-init", "dev-workspace-deinit", "dev-worktree-init", "dev-worktree-deinit"} {
		if _, exists := byName[name]; !exists {
			names := make([]string, 0, len(pipelines))
			for _, p := range pipelines {
				names = append(names, p.Name)
			}
			t.Fatalf("%s not found in pipelines: %v", name, names)
		}
	}

	verifyDevWorkspaceInit(t, byName["dev-workspace-init"])
	verifyDevWorkspaceDeinit(t, byName["dev-workspace-deinit"])
	verifyDevWorktreeInit(t, byName["dev-worktree-init"])
	verifyDevWorktreeDeinit(t, byName["dev-worktree-deinit"])
}

func verifyDevWorkspaceInit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables are declared.
	requiredVariables := []string{"REPOSITORY", "PROJECT", "WORKSPACE_ROOM_ID", "MACHINE", "WORKSPACE_PATH"}
	for _, name := range requiredVariables {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing required variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// Verify step structure.
	if len(p.Content.Steps) < 3 {
		t.Fatalf("expected at least 3 steps, got %d", len(p.Content.Steps))
	}

	// First step should create the project directory.
	if p.Content.Steps[0].Name != "create-project-directory" {
		t.Errorf("first step name = %q, want %q", p.Content.Steps[0].Name, "create-project-directory")
	}

	// Last step should publish workspace active state.
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	if lastStep.Name != "publish-active" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-active")
	}
	if lastStep.Publish == nil {
		t.Error("last step should be a publish step")
	} else if lastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("last step publish event_type = %q", lastStep.Publish.EventType)
	}

	// on_failure should publish workspace failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	initOnFailure := p.Content.OnFailure[0]
	if initOnFailure.Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if initOnFailure.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("on_failure publish event_type = %q, want %q", initOnFailure.Publish.EventType, "m.bureau.workspace")
	}

	// Log should point to the workspace room.
	if p.Content.Log == nil {
		t.Error("Log should be configured")
	} else if p.Content.Log.Room == "" {
		t.Error("Log.Room should be set")
	}

	// SourceHash should be a valid hex-encoded SHA-256.
	if len(p.SourceHash) != sha256.Size*2 {
		t.Errorf("SourceHash length = %d, want %d", len(p.SourceHash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(p.SourceHash); err != nil {
		t.Errorf("SourceHash is not valid hex: %v", err)
	}
}

func verifyDevWorkspaceDeinit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables.
	requiredVariables := []string{"PROJECT", "WORKSPACE_ROOM_ID", "MACHINE"}
	for _, name := range requiredVariables {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing required variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// EVENT_teardown_mode should have a default of "archive". This variable
	// comes from the trigger event (workspace state with status "teardown"),
	// but the declaration provides a fallback for manual execution.
	modeVariable, exists := p.Content.Variables["EVENT_teardown_mode"]
	if !exists {
		t.Error("missing EVENT_teardown_mode variable declaration")
	} else if modeVariable.Default != "archive" {
		t.Errorf("EVENT_teardown_mode default = %q, want %q", modeVariable.Default, "archive")
	}

	// Should have enough steps for assert + validate + check + cleanup + publish (x2).
	if len(p.Content.Steps) < 6 {
		t.Fatalf("expected at least 6 steps, got %d", len(p.Content.Steps))
	}

	// First step should be the staleness guard (assert_state).
	firstStep := p.Content.Steps[0]
	if firstStep.Name != "assert-still-teardown" {
		t.Errorf("first step name = %q, want %q", firstStep.Name, "assert-still-teardown")
	}
	if firstStep.AssertState == nil {
		t.Error("first step should be an assert_state step")
	} else {
		if firstStep.AssertState.EventType != "m.bureau.workspace" {
			t.Errorf("assert_state event_type = %q, want %q", firstStep.AssertState.EventType, "m.bureau.workspace")
		}
		if firstStep.AssertState.Equals != "teardown" {
			t.Errorf("assert_state equals = %q, want %q", firstStep.AssertState.Equals, "teardown")
		}
		if firstStep.AssertState.OnMismatch != "abort" {
			t.Errorf("assert_state on_mismatch = %q, want %q", firstStep.AssertState.OnMismatch, "abort")
		}
	}

	// Second step should validate the MODE variable.
	if p.Content.Steps[1].Name != "validate-mode" {
		t.Errorf("second step name = %q, want %q", p.Content.Steps[1].Name, "validate-mode")
	}

	// Last two steps should be conditional publish steps (archive/delete).
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	secondLastStep := p.Content.Steps[len(p.Content.Steps)-2]
	if secondLastStep.Name != "publish-archived" {
		t.Errorf("second-to-last step name = %q, want %q", secondLastStep.Name, "publish-archived")
	}
	if lastStep.Name != "publish-removed" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-removed")
	}
	// Both should publish to the unified workspace event type.
	if secondLastStep.Publish == nil {
		t.Error("publish-archived should be a publish step")
	} else if secondLastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("publish-archived event_type = %q, want %q",
			secondLastStep.Publish.EventType, "m.bureau.workspace")
	}
	if lastStep.Publish == nil {
		t.Error("publish-removed should be a publish step")
	} else if lastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("publish-removed event_type = %q, want %q",
			lastStep.Publish.EventType, "m.bureau.workspace")
	}

	// on_failure should publish workspace failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	onFailure := p.Content.OnFailure[0]
	if onFailure.Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if onFailure.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("on_failure publish event_type = %q, want %q", onFailure.Publish.EventType, "m.bureau.workspace")
	}

	// Log should point to the workspace room.
	if p.Content.Log == nil {
		t.Error("Log should be configured")
	} else if p.Content.Log.Room == "" {
		t.Error("Log.Room should be set")
	}

	// SourceHash should be a valid hex-encoded SHA-256.
	if len(p.SourceHash) != sha256.Size*2 {
		t.Errorf("SourceHash length = %d, want %d", len(p.SourceHash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(p.SourceHash); err != nil {
		t.Errorf("SourceHash is not valid hex: %v", err)
	}
}

func TestPipelinesSourceHashStable(t *testing.T) {
	t.Parallel()

	// Calling Pipelines twice should produce identical hashes.
	first, err := Pipelines()
	if err != nil {
		t.Fatalf("first Pipelines call: %v", err)
	}
	second, err := Pipelines()
	if err != nil {
		t.Fatalf("second Pipelines call: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("pipeline count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceHash != second[i].SourceHash {
			t.Errorf("pipeline %q hash changed between calls: %s vs %s",
				first[i].Name, first[i].SourceHash, second[i].SourceHash)
		}
	}
}

func verifyDevWorktreeInit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables.
	for _, name := range []string{"PROJECT", "WORKTREE_PATH", "WORKSPACE_ROOM_ID", "MACHINE"} {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// Last step should publish worktree active state.
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	if lastStep.Name != "publish-active" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-active")
	}
	if lastStep.Publish == nil {
		t.Error("last step should be a publish step")
	} else {
		if lastStep.Publish.EventType != "m.bureau.worktree" {
			t.Errorf("publish event_type = %q, want %q", lastStep.Publish.EventType, "m.bureau.worktree")
		}
		if lastStep.Publish.StateKey != "${WORKTREE_PATH}" {
			t.Errorf("publish state_key = %q, want %q", lastStep.Publish.StateKey, "${WORKTREE_PATH}")
		}
	}

	// on_failure should publish worktree failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	if p.Content.OnFailure[0].Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if p.Content.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("on_failure publish event_type = %q", p.Content.OnFailure[0].Publish.EventType)
	}
}

func verifyDevWorktreeDeinit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// First step should be the staleness guard (assert_state).
	firstStep := p.Content.Steps[0]
	if firstStep.Name != "assert-still-removing" {
		t.Errorf("first step name = %q, want %q", firstStep.Name, "assert-still-removing")
	}
	if firstStep.AssertState == nil {
		t.Error("first step should be an assert_state step")
	} else {
		if firstStep.AssertState.EventType != "m.bureau.worktree" {
			t.Errorf("assert_state event_type = %q, want %q", firstStep.AssertState.EventType, "m.bureau.worktree")
		}
		if firstStep.AssertState.Equals != "removing" {
			t.Errorf("assert_state equals = %q, want %q", firstStep.AssertState.Equals, "removing")
		}
		if firstStep.AssertState.OnMismatch != "abort" {
			t.Errorf("assert_state on_mismatch = %q, want %q", firstStep.AssertState.OnMismatch, "abort")
		}
	}

	// Last two steps should be conditional publish steps (archived/removed).
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	secondLastStep := p.Content.Steps[len(p.Content.Steps)-2]
	if secondLastStep.Name != "publish-archived" {
		t.Errorf("second-to-last step name = %q, want %q", secondLastStep.Name, "publish-archived")
	}
	if lastStep.Name != "publish-removed" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-removed")
	}
	if secondLastStep.Publish != nil && secondLastStep.Publish.EventType != "m.bureau.worktree" {
		t.Errorf("publish-archived event_type = %q", secondLastStep.Publish.EventType)
	}
	if lastStep.Publish != nil && lastStep.Publish.EventType != "m.bureau.worktree" {
		t.Errorf("publish-removed event_type = %q", lastStep.Publish.EventType)
	}

	// on_failure should publish worktree failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	if p.Content.OnFailure[0].Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if p.Content.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("on_failure publish event_type = %q", p.Content.OnFailure[0].Publish.EventType)
	}
}

func TestPipelinesNamesUnique(t *testing.T) {
	t.Parallel()

	pipelines, err := Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	seen := make(map[string]bool, len(pipelines))
	for _, p := range pipelines {
		if seen[p.Name] {
			t.Errorf("duplicate pipeline name: %s", p.Name)
		}
		seen[p.Name] = true
	}
}
