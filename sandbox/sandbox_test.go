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

// testSandboxProfile returns a minimal profile suitable for sandbox tests.
// This constructs the profile directly rather than loading from any external
// source — sandbox configuration comes from Matrix templates at runtime,
// not compiled-in YAML.
func testSandboxProfile() *Profile {
	return &Profile{
		Name:        "test",
		Description: "Minimal profile for sandbox tests",
		Filesystem: []Mount{
			{Source: "${WORKING_DIRECTORY}", Dest: "/workspace", Mode: "rw"},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
			{Source: "/lib", Dest: "/lib", Mode: "ro"},
			{Source: "/lib64", Dest: "/lib64", Mode: "ro", Optional: true},
			{Source: "/etc/passwd", Dest: "/etc/passwd", Mode: "ro"},
			{Source: "/etc/group", Dest: "/etc/group", Mode: "ro"},
			{Source: "/nix", Dest: "/nix", Mode: "ro", Optional: true},
		},
		Namespaces: NamespaceConfig{
			PID: true,
			Net: true,
			IPC: true,
			UTS: true,
		},
		Environment: map[string]string{
			"PATH":                "/workspace/bin:/usr/local/bin:/usr/bin:/bin",
			"HOME":                "/workspace",
			"TERM":                "${TERM}",
			"BUREAU_SANDBOX":      "1",
			"BUREAU_PROXY_SOCKET": "/run/bureau/proxy.sock",
		},
		Security: SecurityConfig{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
	}
}

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
	profile := testSandboxProfile()

	workingDirectory := t.TempDir()

	sb, err := New(Config{
		Profile:          profile,
		WorkingDirectory: workingDirectory,
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
	profile := testSandboxProfile()

	workingDirectory := t.TempDir()

	sb, err := New(Config{
		Profile:          profile,
		WorkingDirectory: workingDirectory,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Validate should produce output.
	var buffer bytes.Buffer
	err = sb.Validate(&buffer)

	output := buffer.String()
	t.Logf("Validation output:\n%s", output)

	// Should mention the profile.
	if !strings.Contains(output, "test") {
		t.Errorf("expected profile name in output")
	}

	// Should mention the working directory.
	if !strings.Contains(output, workingDirectory) {
		t.Errorf("expected working directory in output")
	}
}

func TestSandboxRunSimple(t *testing.T) {
	skipIfNoSandbox(t)

	profile := testSandboxProfile()

	workingDirectory := t.TempDir()

	testFile := filepath.Join(workingDirectory, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	sb, err := New(Config{
		Profile:          profile,
		WorkingDirectory: workingDirectory,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	ctx := context.Background()

	// Run a simple command that reads from the working directory.
	err = sb.Run(ctx, []string{"/bin/cat", "/workspace/test.txt"})
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}
}

func TestSandboxRunWriteWorkingDirectory(t *testing.T) {
	skipIfNoSandbox(t)

	profile := testSandboxProfile()

	workingDirectory := t.TempDir()

	sb, err := New(Config{
		Profile:          profile,
		WorkingDirectory: workingDirectory,
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

	// Verify file was written to host working directory.
	outputFile := filepath.Join(workingDirectory, "output.txt")
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

	profile := testSandboxProfile()

	workingDirectory := t.TempDir()

	sb, err := New(Config{
		Profile:          profile,
		WorkingDirectory: workingDirectory,
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
