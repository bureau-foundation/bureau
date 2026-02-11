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

	// Verify dev-workspace-init is present and well-formed.
	var found bool
	for _, p := range pipelines {
		if p.Name == "dev-workspace-init" {
			found = true
			verifyDevWorkspaceInit(t, p)
			break
		}
	}
	if !found {
		names := make([]string, len(pipelines))
		for i, p := range pipelines {
			names[i] = p.Name
		}
		t.Fatalf("dev-workspace-init not found in pipelines: %v", names)
	}
}

func verifyDevWorkspaceInit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables are declared.
	requiredVariables := []string{"REPOSITORY", "PROJECT", "WORKSPACE_ROOM_ID"}
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

	// Last step should publish workspace ready.
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	if lastStep.Name != "publish-ready" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-ready")
	}
	if lastStep.Publish == nil {
		t.Error("last step should be a publish step")
	} else if lastStep.Publish.EventType != "m.bureau.workspace.ready" {
		t.Errorf("last step publish event_type = %q", lastStep.Publish.EventType)
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
