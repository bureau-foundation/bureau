// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

// ProjectConfig is the content of an EventTypeProject state event. It
// declares a workspace project: the top-level unit in
// /var/bureau/workspace/<project>/. For git-backed projects, it specifies
// the repository URL and worktree layout. For non-git projects (creative
// writing, ML training, etc.), it specifies directory structure.
//
// The daemon reads this during reconciliation and compares it to the host
// filesystem. If the workspace doesn't exist or is missing worktrees, the
// daemon spawns a setup principal to create them.
//
// The room alias maps mechanically to the filesystem path:
// #iree/amdgpu/inference:bureau.local → /var/bureau/workspace/iree/amdgpu/inference/
type ProjectConfig struct {
	// Repository is the git clone URL for git-backed projects (e.g.,
	// "https://github.com/iree-org/iree.git"). Empty for non-git
	// workspaces. When set, the setup principal clones this as a bare
	// repo into .bare/ under the project root.
	Repository string `json:"repository,omitempty"`

	// WorkspacePath is the project's directory name under
	// /var/bureau/workspace/ (e.g., "iree", "lore", "bureau"). This is
	// always the first segment of the room alias and is redundant with
	// the state key, but included explicitly for clarity and to
	// decouple the struct from its storage context.
	WorkspacePath string `json:"workspace_path"`

	// DefaultBranch is the primary branch name for git-backed projects
	// (e.g., "main", "master"). The setup principal creates a worktree
	// named "main/" tracking this branch. Empty for non-git workspaces.
	DefaultBranch string `json:"default_branch,omitempty"`

	// Worktrees maps worktree path suffixes to their configuration for
	// git-backed projects. The key is the path relative to the project
	// root (e.g., "amdgpu/inference", "remoting"). The setup principal
	// creates each worktree via git worktree add.
	//
	// Empty or nil for non-git workspaces.
	Worktrees map[string]WorktreeConfig `json:"worktrees,omitempty"`

	// Directories maps directory path suffixes to their configuration
	// for non-git workspaces. The key is the path relative to the
	// project root (e.g., "novel4", "training-data"). The setup
	// principal creates each directory.
	//
	// Empty or nil for git-backed workspaces.
	Directories map[string]DirectoryConfig `json:"directories,omitempty"`
}

// WorktreeConfig describes a single git worktree within a project.
type WorktreeConfig struct {
	// Branch is the git branch to check out in this worktree (e.g.,
	// "feature/amdgpu-inference", "main"). The setup principal runs
	// git worktree add with this branch.
	Branch string `json:"branch"`

	// Description is a human-readable description of what this worktree
	// is for (e.g., "AMDGPU inference pipeline development").
	Description string `json:"description,omitempty"`
}

// DirectoryConfig describes a single directory within a non-git workspace.
type DirectoryConfig struct {
	// Description is a human-readable description of what this directory
	// is for (e.g., "Fourth novel workspace", "Training dataset").
	Description string `json:"description,omitempty"`
}

// WorkspaceState is the content of an EventTypeWorkspace state event.
// It tracks the full lifecycle of a workspace as a status field that
// progresses one-directionally:
//
//	pending → active → teardown → archived | removed
//
// The teardown status triggers continuous enforcement: agent principals
// gated on "active" stop, and the teardown principal gated on "teardown"
// starts. The teardown principal performs cleanup (archive or delete)
// and publishes the final status ("archived" or "removed").
type WorkspaceState struct {
	// Status is the current lifecycle state. Valid values:
	//   - "pending": room created, setup not yet started or in progress.
	//   - "active": setup complete, workspace is usable.
	//   - "teardown": destroy requested, agents stopping, teardown running.
	//   - "archived": teardown completed in archive mode.
	//   - "removed": teardown completed in delete mode.
	Status string `json:"status"`

	// Project is the project name (first path segment of the workspace
	// alias). Matches the directory under /var/bureau/workspace/.
	Project string `json:"project"`

	// Machine is the machine localpart identifying which host the
	// workspace data lives on.
	Machine string `json:"machine"`

	// WorkspacePath is the absolute filesystem path of the workspace
	// on the host machine (e.g., "/var/bureau/workspace/iree"). Present
	// when the workspace is "active" — the setup pipeline sets it after
	// creating the project directory. Empty for "pending" (workspace not
	// yet created), "teardown", "archived", and "removed" (workspace no
	// longer exists at this path).
	WorkspacePath string `json:"workspace_path,omitempty"`

	// TeardownMode specifies how the teardown principal should handle
	// the workspace data. Set by "bureau workspace destroy --mode".
	// Valid values: "archive" (move to .archive/), "delete" (remove).
	// Empty for all statuses other than "teardown".
	TeardownMode string `json:"teardown_mode,omitempty"`

	// UpdatedAt is an ISO 8601 timestamp of the last status transition.
	UpdatedAt string `json:"updated_at"`

	// ArchivePath is the path under /workspace/.archive/ where the
	// project was moved when status is "archived". Empty for all other
	// statuses.
	ArchivePath string `json:"archive_path,omitempty"`
}

// WorktreeState is the content of an EventTypeWorktree state event.
// It tracks the lifecycle of an individual git worktree within a workspace.
// Each worktree has its own state event in the workspace room, keyed by
// the worktree path relative to the project root.
//
// The lifecycle parallels WorkspaceState but is simpler: worktrees don't
// have a "pending" state (the daemon publishes "creating" immediately
// when it accepts the add request) and don't have a "teardown" stage
// (removal is a single pipeline, not a multi-phase process).
type WorktreeState struct {
	// Status is the current lifecycle state. Valid values:
	//   - "creating": daemon accepted the add request, init pipeline running.
	//   - "active": init pipeline completed, worktree is ready for use.
	//   - "removing": daemon accepted the remove request, deinit pipeline running.
	//   - "archived": deinit completed in archive mode.
	//   - "removed": deinit completed in delete mode.
	//   - "failed": init or deinit pipeline failed.
	Status string `json:"status"`

	// Project is the project name (first path segment of the workspace alias).
	Project string `json:"project"`

	// WorktreePath is the worktree path relative to the project root
	// (e.g., "feature/amdgpu", "main"). Matches the state key of the event.
	WorktreePath string `json:"worktree_path"`

	// Branch is the git branch checked out in this worktree. Empty when
	// the worktree was created in detached HEAD mode.
	Branch string `json:"branch,omitempty"`

	// Machine is the machine localpart identifying which host the
	// worktree lives on.
	Machine string `json:"machine"`

	// UpdatedAt is an ISO 8601 timestamp of the last status transition.
	UpdatedAt string `json:"updated_at"`
}
