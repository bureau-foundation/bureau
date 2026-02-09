// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCapabilities caches capability detection across tests.
var testCapabilities *Capabilities

func getTestCapabilities(t *testing.T) *Capabilities {
	if testCapabilities == nil {
		testCapabilities = DetectCapabilities()
		t.Logf("Sandbox capabilities: bwrap=%v userns=%v systemd=%v",
			testCapabilities.BwrapAvailable,
			testCapabilities.UserNamespacesEnabled,
			testCapabilities.SystemdRunAvailable)
	}
	return testCapabilities
}

func skipIfNoSandbox(t *testing.T) {
	caps := getTestCapabilities(t)
	if reason := caps.SkipReason(); reason != "" {
		t.Skipf("Skipping sandbox test: %s", reason)
	}
}

func TestSandboxDryRun(t *testing.T) {
	// This test doesn't require actual sandbox execution.
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	profile, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	// Create temp worktree.
	worktree := t.TempDir()

	sb, err := New(Config{
		Profile:  profile,
		Worktree: worktree,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Dry run should work even without bwrap.
	cmd, err := sb.DryRun([]string{"/bin/echo", "hello"})
	if err != nil {
		// Dry run may fail if bwrap path can't be determined.
		caps := getTestCapabilities(t)
		if !caps.BwrapAvailable {
			t.Skipf("Skipping: %s", caps.SkipReason())
		}
		t.Fatalf("DryRun failed: %v", err)
	}

	// Should contain bwrap.
	cmdStr := strings.Join(cmd, " ")
	if !strings.Contains(cmdStr, "bwrap") {
		t.Errorf("expected bwrap in command, got: %s", cmdStr)
	}

	// Should contain --unshare-pid.
	if !strings.Contains(cmdStr, "--unshare-pid") {
		t.Errorf("expected --unshare-pid in command")
	}

	// Should contain the command.
	if !strings.Contains(cmdStr, "/bin/echo") {
		t.Errorf("expected /bin/echo in command")
	}
}

func TestSandboxValidate(t *testing.T) {
	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	profile, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	worktree := t.TempDir()

	sb, err := New(Config{
		Profile:  profile,
		Worktree: worktree,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Validate should produce output.
	var buf bytes.Buffer
	err = sb.Validate(&buf)

	output := buf.String()
	t.Logf("Validation output:\n%s", output)

	// Should mention the profile.
	if !strings.Contains(output, "developer") {
		t.Errorf("expected profile name in output")
	}

	// Should mention the worktree.
	if !strings.Contains(output, worktree) {
		t.Errorf("expected worktree in output")
	}
}

func TestSandboxRunSimple(t *testing.T) {
	skipIfNoSandbox(t)

	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	profile, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	worktree := t.TempDir()

	// Create a test file in worktree.
	testFile := filepath.Join(worktree, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	sb, err := New(Config{
		Profile:  profile,
		Worktree: worktree,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx := context.Background()

	// Run a simple command that reads from worktree.
	err = sb.Run(ctx, []string{"/bin/cat", "/workspace/test.txt"})
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}
}

func TestSandboxRunWriteWorktree(t *testing.T) {
	skipIfNoSandbox(t)

	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	profile, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	worktree := t.TempDir()

	sb, err := New(Config{
		Profile:  profile,
		Worktree: worktree,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx := context.Background()

	// Write a file inside the sandbox.
	err = sb.Run(ctx, []string{"/bin/sh", "-c", "echo 'sandbox wrote this' > /workspace/output.txt"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Verify file was written to host worktree.
	outputFile := filepath.Join(worktree, "output.txt")
	content, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if !strings.Contains(string(content), "sandbox wrote this") {
		t.Errorf("expected 'sandbox wrote this', got: %s", string(content))
	}
}

func TestSandboxExitCode(t *testing.T) {
	skipIfNoSandbox(t)

	loader := NewProfileLoader()
	if err := loader.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults failed: %v", err)
	}

	profile, err := loader.Resolve("developer")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	worktree := t.TempDir()

	sb, err := New(Config{
		Profile:  profile,
		Worktree: worktree,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx := context.Background()

	// Run a command that exits with code 42.
	err = sb.Run(ctx, []string{"/bin/sh", "-c", "exit 42"})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}

	code, ok := IsExitError(err)
	if !ok {
		t.Fatalf("expected ExitError, got: %v", err)
	}

	if code != 42 {
		t.Errorf("expected exit code 42, got %d", code)
	}
}

func TestCapabilities(t *testing.T) {
	caps := DetectCapabilities()

	t.Logf("BwrapAvailable: %v", caps.BwrapAvailable)
	t.Logf("BwrapPath: %s", caps.BwrapPath)
	t.Logf("BwrapVersion: %s", caps.BwrapVersion)
	t.Logf("UserNamespacesEnabled: %v", caps.UserNamespacesEnabled)
	t.Logf("SystemdRunAvailable: %v", caps.SystemdRunAvailable)
	t.Logf("SystemdUserScopesWork: %v", caps.SystemdUserScopesWork)
	t.Logf("CanRunSandbox: %v", caps.CanRunSandbox())
	t.Logf("SkipReason: %q", caps.SkipReason())
}
