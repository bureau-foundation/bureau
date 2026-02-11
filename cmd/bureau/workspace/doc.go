// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package workspace implements the "bureau workspace" subcommands for
// managing project workspaces across the Bureau fleet.
//
// A workspace is a host-side directory structure under
// /var/bureau/workspace/ that gets mounted into sandboxes. The room
// alias IS the workspace path: #iree/amdgpu/inference maps
// mechanically to /var/bureau/workspace/iree/amdgpu/inference/ with
// no lookup table or configuration.
//
// The first segment of the alias is the project name. All worktrees
// within a project share a single bare git object store at
// /var/bureau/workspace/<project>/.bare/, enabling efficient
// multi-branch development.
//
// Subcommands are organized into lifecycle, status, and git groups:
//
//   - create: sets up the Matrix room, publishes ProjectConfig state,
//     builds PrincipalAssignment entries (one setup principal plus N
//     agent principals gated on workspace.ready), updates MachineConfig,
//     and invites the target machine's daemon. Routes directly to
//     Matrix via [cli.SessionConfig].
//   - destroy, list, status, du, worktree, fetch: declared but not
//     yet implemented (return [cli.ErrNotImplemented]).
//
// The create command supports --machine=local, which reads the
// launcher's session file via [cli.ResolveLocalMachine] to discover
// the local machine's identity.
package workspace
