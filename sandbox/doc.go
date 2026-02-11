// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package sandbox creates isolated execution environments for untrusted agent
// processes using bubblewrap (bwrap) Linux namespaces.
//
// The central type is [Sandbox], which assembles a bwrap command from a
// [Profile] and executes it. Profiles are YAML-driven configurations that
// declare filesystem mounts, namespace isolation flags, environment variables,
// resource limits, and directories to create. Profiles support single
// inheritance via the Inherit field, and all string values undergo variable
// expansion ([Variables].ExpandProfile) before use.
//
// Filesystem isolation is the primary security boundary. Every mount is
// declared explicitly in the profile; there is no implicit host filesystem
// visibility. Mount types include bind (read-only or read-write), tmpfs,
// proc, dev, dev-bind (for GPU passthrough), and overlay. Overlay mounts
// use fuse-overlayfs ([OverlayManager]) to provide copy-on-write access to
// host directories (package caches, tool installations) with writes captured
// in either a tmpfs or a worktree-contained upper layer. The upper layer
// path is validated ([ValidateOverlayUpper]) with symlink resolution to
// prevent writes from escaping the worktree.
//
// Resource limits are enforced via systemd transient scopes ([SystemdScope]),
// setting cgroup v2 properties for task count, memory, CPU quota, and CPU
// weight. The scope wraps the bwrap command, so limits apply to the entire
// sandbox process tree.
//
// [BwrapBuilder] translates a Profile into bwrap command-line arguments.
// [Validator] performs pre-flight checks (bwrap availability, user namespace
// support, worktree existence, proxy socket reachability, mount source
// validity). [Capabilities] probes the host for available features.
// [EscapeTestRunner] verifies sandbox containment by running a battery of
// escape attempts (network, filesystem, process, privilege, terminal) and
// confirming they all fail.
//
// The sandbox intentionally does not manage the process running inside it.
// It creates the namespace and mounts, then exec's the command. Process
// lifecycle (tmux sessions, PTY allocation, observation) is handled by the
// launcher and observe packages.
package sandbox
