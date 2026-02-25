// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"encoding/json"
	"testing"
)

func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func TestProjectConfigGitBackedRoundTrip(t *testing.T) {
	original := ProjectConfig{
		Repository:    "https://github.com/iree-org/iree.git",
		WorkspacePath: "iree",
		DefaultBranch: "main",
		Worktrees: map[string]WorktreeConfig{
			"amdgpu/inference": {Branch: "feature/amdgpu-inference", Description: "AMDGPU inference pipeline"},
			"amdgpu/pm":        {Branch: "feature/amdgpu-pm"},
			"remoting":         {Branch: "main"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "repository", "https://github.com/iree-org/iree.git")
	assertField(t, raw, "workspace_path", "iree")
	assertField(t, raw, "default_branch", "main")

	worktrees, ok := raw["worktrees"].(map[string]any)
	if !ok {
		t.Fatal("worktrees field missing or wrong type")
	}
	if len(worktrees) != 3 {
		t.Fatalf("worktrees count = %d, want 3", len(worktrees))
	}
	inference, ok := worktrees["amdgpu/inference"].(map[string]any)
	if !ok {
		t.Fatal("worktrees[amdgpu/inference] missing or wrong type")
	}
	assertField(t, inference, "branch", "feature/amdgpu-inference")
	assertField(t, inference, "description", "AMDGPU inference pipeline")

	// Directories should be omitted for git-backed projects.
	if _, exists := raw["directories"]; exists {
		t.Error("directories should be omitted for git-backed projects")
	}

	var decoded ProjectConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Repository != original.Repository {
		t.Errorf("Repository: got %q, want %q", decoded.Repository, original.Repository)
	}
	if decoded.WorkspacePath != original.WorkspacePath {
		t.Errorf("WorkspacePath: got %q, want %q", decoded.WorkspacePath, original.WorkspacePath)
	}
	if decoded.DefaultBranch != original.DefaultBranch {
		t.Errorf("DefaultBranch: got %q, want %q", decoded.DefaultBranch, original.DefaultBranch)
	}
	if len(decoded.Worktrees) != 3 {
		t.Fatalf("Worktrees count = %d, want 3", len(decoded.Worktrees))
	}
	inferenceConfig := decoded.Worktrees["amdgpu/inference"]
	if inferenceConfig.Branch != "feature/amdgpu-inference" {
		t.Errorf("Worktrees[amdgpu/inference].Branch: got %q, want %q",
			inferenceConfig.Branch, "feature/amdgpu-inference")
	}
	if inferenceConfig.Description != "AMDGPU inference pipeline" {
		t.Errorf("Worktrees[amdgpu/inference].Description: got %q, want %q",
			inferenceConfig.Description, "AMDGPU inference pipeline")
	}
}

func TestProjectConfigNonGitRoundTrip(t *testing.T) {
	original := ProjectConfig{
		WorkspacePath: "lore",
		Directories: map[string]DirectoryConfig{
			"novel4": {Description: "Fourth novel workspace"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "workspace_path", "lore")

	// Git-specific fields should be omitted.
	for _, field := range []string{"repository", "default_branch", "worktrees"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted for non-git projects", field)
		}
	}

	directories, ok := raw["directories"].(map[string]any)
	if !ok {
		t.Fatal("directories field missing or wrong type")
	}
	novel4, ok := directories["novel4"].(map[string]any)
	if !ok {
		t.Fatal("directories[novel4] missing or wrong type")
	}
	assertField(t, novel4, "description", "Fourth novel workspace")

	var decoded ProjectConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.WorkspacePath != "lore" {
		t.Errorf("WorkspacePath: got %q, want %q", decoded.WorkspacePath, "lore")
	}
	if len(decoded.Directories) != 1 {
		t.Fatalf("Directories count = %d, want 1", len(decoded.Directories))
	}
	if decoded.Directories["novel4"].Description != "Fourth novel workspace" {
		t.Errorf("Directories[novel4].Description: got %q, want %q",
			decoded.Directories["novel4"].Description, "Fourth novel workspace")
	}
}

func TestProjectConfigOmitsEmptyFields(t *testing.T) {
	// Minimal ProjectConfig with only required field.
	config := ProjectConfig{
		WorkspacePath: "scratch",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"repository", "default_branch", "worktrees", "directories"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
	assertField(t, raw, "workspace_path", "scratch")
}

func TestWorktreeConfigOmitsEmptyDescription(t *testing.T) {
	config := WorktreeConfig{Branch: "main"}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "branch", "main")
	if _, exists := raw["description"]; exists {
		t.Error("description should be omitted when empty")
	}
}

func TestWorkspaceStateRoundTrip(t *testing.T) {
	original := WorkspaceState{
		Status:        WorkspaceStatusActive,
		Project:       "iree",
		Machine:       "workstation",
		WorkspacePath: "/var/bureau/workspace/iree",
		UpdatedAt:     "2026-02-10T12:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "active")
	assertField(t, raw, "project", "iree")
	assertField(t, raw, "machine", "workstation")
	assertField(t, raw, "workspace_path", "/var/bureau/workspace/iree")
	assertField(t, raw, "updated_at", "2026-02-10T12:00:00Z")
	if _, exists := raw["archive_path"]; exists {
		t.Error("archive_path should be omitted when empty")
	}

	var decoded WorkspaceState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestWorkspaceStatePendingOmitsWorkspacePath(t *testing.T) {
	original := WorkspaceState{
		Status:    WorkspaceStatusPending,
		Project:   "iree",
		Machine:   "workstation",
		UpdatedAt: "2026-02-10T12:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["workspace_path"]; exists {
		t.Error("workspace_path should be omitted when empty (pending status)")
	}
}

func TestWorkspaceStateArchived(t *testing.T) {
	original := WorkspaceState{
		Status:      WorkspaceStatusArchived,
		Project:     "iree",
		Machine:     "workstation",
		UpdatedAt:   "2026-02-10T14:30:00Z",
		ArchivePath: "/workspace/.archive/iree-20260210T143000",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "archived")
	assertField(t, raw, "archive_path", "/workspace/.archive/iree-20260210T143000")

	var decoded WorkspaceState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestWorktreeStateRoundTrip(t *testing.T) {
	original := WorktreeState{
		Status:       WorktreeStatusActive,
		Project:      "iree",
		WorktreePath: "feature/amdgpu",
		Branch:       "feature/amdgpu-inference",
		Machine:      "workstation",
		UpdatedAt:    "2026-02-12T00:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "status", "active")
	assertField(t, raw, "project", "iree")
	assertField(t, raw, "worktree_path", "feature/amdgpu")
	assertField(t, raw, "branch", "feature/amdgpu-inference")
	assertField(t, raw, "machine", "workstation")
	assertField(t, raw, "updated_at", "2026-02-12T00:00:00Z")

	var decoded WorktreeState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestWorktreeStateOmitsEmptyBranch(t *testing.T) {
	state := WorktreeState{
		Status:       WorktreeStatusCreating,
		Project:      "iree",
		WorktreePath: "detached-work",
		Machine:      "workstation",
		UpdatedAt:    "2026-02-12T00:00:00Z",
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["branch"]; exists {
		t.Error("branch should be omitted when empty")
	}
	assertField(t, raw, "status", "creating")
	assertField(t, raw, "worktree_path", "detached-work")
}
