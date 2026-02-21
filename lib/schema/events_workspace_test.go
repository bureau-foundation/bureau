// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestWorkspaceRoomPowerLevels(t *testing.T) {
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	machineUserID := ref.MustParseUserID("@machine/workstation:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}

	// Machine should have power level 50 (can invite principals into
	// workspace rooms, but cannot modify power levels or project config).
	if users[machineUserID.String()] != 50 {
		t.Errorf("machine power level = %v, want 50", users[machineUserID.String()])
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Workspace rooms are collaboration spaces: events_default should be 0
	// (agents can send messages freely), unlike config rooms where
	// events_default is 100 (admin-only).
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Event-specific power levels.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Admin-only events (PL 100).
	for _, eventType := range []string{EventTypeProject} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Admin-only power level management (PL 100). Continuwuity validates
	// all fields in m.room.power_levels against sender PL (not just changed
	// ones), so only admin can modify power levels.
	if events["m.room.power_levels"] != 100 {
		t.Errorf("m.room.power_levels power level = %v, want 100", events["m.room.power_levels"])
	}

	// Default-level events (PL 0): workspace state, worktree lifecycle, and
	// layout. Room membership is the authorization boundary — the room is
	// invite-only.
	for _, eventType := range []string{EventTypeWorkspace, EventTypeWorktree, EventTypeLayout} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []string{
		"m.room.encryption", "m.room.server_acl",
		"m.room.tombstone", "m.space.child",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// state_default is 0: room membership is the authorization boundary.
	// New Bureau state event types work without updating power levels.
	if levels["state_default"] != 0 {
		t.Errorf("state_default = %v, want 0", levels["state_default"])
	}

	// Moderation actions require power level 100.
	for _, field := range []string{"ban", "kick", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite should require PL 50 (machine can invite principals).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestWorkspaceRoomPowerLevelsSameUser(t *testing.T) {
	// When admin and machine are the same user, the users map should
	// have exactly one entry (no duplicate).
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for same user, got %d", len(users))
	}
	if users[adminUserID.String()] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID.String()])
	}
}

func TestWorkspaceRoomPowerLevelsEmptyMachine(t *testing.T) {
	// When machine user ID is zero, the users map should have only the admin.
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	levels := WorkspaceRoomPowerLevels(adminUserID, ref.UserID{})

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for empty machine, got %d", len(users))
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
		Status:        "active",
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
		Status:    "pending",
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
		Status:      "archived",
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
		Status:       "active",
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
		Status:       "creating",
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
