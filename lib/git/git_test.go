// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRepo creates a bare git repository in a temp directory and
// returns the path. The repository has an initial commit so that
// worktree operations work.
func initBareRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	bareDir := filepath.Join(dir, ".bare")

	// Initialize a bare repository.
	command := exec.Command("git", "init", "--bare", bareDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}

	// Create a worktree with an initial commit so operations like
	// worktree list have something to show.
	worktreeDir := filepath.Join(dir, "main")
	command = exec.Command("git", "-C", bareDir, "worktree", "add", worktreeDir, "--orphan", "-b", "main")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, output)
	}

	// Create an initial commit in the worktree.
	readmePath := filepath.Join(worktreeDir, "README")
	if err := os.WriteFile(readmePath, []byte("test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	command = exec.Command("git", "-C", worktreeDir, "add", "README")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	command = exec.Command("git", "-C", worktreeDir, "commit", "-m", "initial",
		"--author", "Test <test@test.local>")
	command.Env = append(os.Environ(),
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.local",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}

	return bareDir
}

func TestRepository_Run(t *testing.T) {
	t.Parallel()

	bareDir := initBareRepo(t)
	repo := NewRepository(bareDir)

	output, err := repo.Run(context.Background(), "worktree", "list")
	if err != nil {
		t.Fatalf("Run(worktree list): %v", err)
	}

	// Output should contain at least the main worktree and the bare dir.
	if !strings.Contains(output, "main") {
		t.Errorf("worktree list output = %q, want to contain 'main'", output)
	}
}

func TestRepository_Run_InvalidSubcommand(t *testing.T) {
	t.Parallel()

	bareDir := initBareRepo(t)
	repo := NewRepository(bareDir)

	_, err := repo.Run(context.Background(), "not-a-real-command")
	if err == nil {
		t.Fatal("expected error for invalid git subcommand")
	}
	if !strings.Contains(err.Error(), bareDir) {
		t.Errorf("error = %v, want to contain repository dir %q", err, bareDir)
	}
}

func TestRepository_Run_NonexistentDirectory(t *testing.T) {
	t.Parallel()

	repo := NewRepository("/tmp/nonexistent-git-repo-abcxyz")

	_, err := repo.Run(context.Background(), "status")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestRepository_Command(t *testing.T) {
	t.Parallel()

	repo := NewRepository("/some/dir")

	cmd := repo.Command(context.Background(), "status", "--porcelain")

	// Verify the command has the right arguments.
	// exec.Cmd.Args includes the program name as Args[0].
	expectedArgs := []string{"git", "-C", "/some/dir", "status", "--porcelain"}
	if len(cmd.Args) != len(expectedArgs) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, expectedArgs)
	}
	for i, want := range expectedArgs {
		if cmd.Args[i] != want {
			t.Errorf("cmd.Args[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}
}

func TestRepository_Dir(t *testing.T) {
	t.Parallel()

	repo := NewRepository("/path/to/repo/.bare")
	if repo.Dir() != "/path/to/repo/.bare" {
		t.Errorf("Dir() = %q, want %q", repo.Dir(), "/path/to/repo/.bare")
	}
}

func TestRepository_RunLocked(t *testing.T) {
	t.Parallel()

	// Verify flock is available.
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skipf("flock not available: %v", err)
	}

	bareDir := initBareRepo(t)
	repo := NewRepository(bareDir)
	lockPath := filepath.Join(bareDir, "test.lock")

	// RunLocked should work for a simple git command.
	output, err := repo.RunLocked(context.Background(), lockPath, "branch", "--list")
	if err != nil {
		t.Fatalf("RunLocked(branch --list): %v", err)
	}

	// The bare repo should have a 'main' branch.
	if !strings.Contains(output, "main") {
		t.Errorf("branch list output = %q, want to contain 'main'", output)
	}

	// Verify the lock file was created.
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}
