// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package git provides typed access to the git CLI for repository
// operations. Bureau uses git for workspace management: listing
// worktrees and fetching from remotes. All commands target a specific
// repository directory via the -C flag, which is automatically injected
// by all Repository methods — analogous to how lib/tmux injects -S for
// its server socket.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Repository represents a git repository at a specific directory. All
// operations target this directory via "git -C <dir>". There is no
// default directory — callers must always specify which repository
// they mean.
type Repository struct {
	dir string
}

// NewRepository returns a Repository targeting the given directory.
// The directory should be a bare repository (.bare/) or a working tree.
func NewRepository(dir string) *Repository {
	return &Repository{dir: dir}
}

// Dir returns the repository directory.
func (r *Repository) Dir() string {
	return r.dir
}

// Run executes a git command targeting this repository and returns
// stdout. Stderr is captured separately and included in error messages
// on failure.
func (r *Repository) Run(ctx context.Context, args ...string) (string, error) {
	fullArgs := append([]string{"-C", r.dir}, args...)
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "git", fullArgs...)
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w (stderr: %s)",
			strings.Join(args, " "), r.dir, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunLocked executes a git command with flock(1) serialization.
// The lock file at lockPath is held for the duration of the command,
// preventing concurrent git operations on the same repository (e.g.,
// parallel fetch requests from multiple agents).
//
// Returns combined stdout and stderr output because git writes
// progress information to stderr (e.g., "Fetching origin...",
// "* branch main -> FETCH_HEAD").
func (r *Repository) RunLocked(ctx context.Context, lockPath string, args ...string) (string, error) {
	gitArgs := append([]string{"-C", r.dir}, args...)
	flockArgs := append([]string{lockPath, "git"}, gitArgs...)

	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "flock", flockArgs...)
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w (stderr: %s)",
			strings.Join(args, " "), r.dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String() + stderr.String()), nil
}

// Command returns an *exec.Cmd for a git command without running it.
// The caller gets full control over Stdin, Stdout, Stderr, and
// SysProcAttr before starting the process. The -C flag targeting
// this repository is automatically prepended.
func (r *Repository) Command(ctx context.Context, args ...string) *exec.Cmd {
	fullArgs := append([]string{"-C", r.dir}, args...)
	return exec.CommandContext(ctx, "git", fullArgs...)
}
