// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

func TestNewValidator(t *testing.T) {
	t.Parallel()

	validator := NewValidator()

	if validator.HasErrors() {
		t.Error("new validator should have no errors")
	}
	if length := len(validator.Results()); length != 0 {
		t.Errorf("new validator should have no results, got %d", length)
	}
}

func TestValidatorAccumulation(t *testing.T) {
	t.Parallel()

	validator := NewValidator()

	// Add a pass.
	validator.pass("check-a", "all good")
	if validator.HasErrors() {
		t.Error("should have no errors after a pass")
	}
	if length := len(validator.Results()); length != 1 {
		t.Errorf("expected 1 result, got %d", length)
	}

	// Add a warning (not an error).
	validator.warn("check-b", "something is off")
	if validator.HasErrors() {
		t.Error("warnings should not count as errors")
	}
	if length := len(validator.Results()); length != 2 {
		t.Errorf("expected 2 results, got %d", length)
	}

	// Verify the warning result.
	warningResult := validator.Results()[1]
	if !warningResult.Passed {
		t.Error("warning result should have Passed=true")
	}
	if !warningResult.Warning {
		t.Error("warning result should have Warning=true")
	}

	// Add a failure.
	validator.fail("check-c", "broken")
	if !validator.HasErrors() {
		t.Error("should have errors after a fail")
	}
	if length := len(validator.Results()); length != 3 {
		t.Errorf("expected 3 results, got %d", length)
	}

	// Verify the failure result.
	failureResult := validator.Results()[2]
	if failureResult.Passed {
		t.Error("failure result should have Passed=false")
	}
	if failureResult.Warning {
		t.Error("failure result should not be a warning")
	}

	// Add a second failure.
	validator.fail("check-d", "also broken")
	if length := len(validator.Results()); length != 4 {
		t.Errorf("expected 4 results, got %d", length)
	}
}

func TestValidateWorkingDirectory(t *testing.T) {
	t.Parallel()

	t.Run("empty string fails", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateWorkingDirectory("")

		if !validator.HasErrors() {
			t.Fatal("expected error for empty working directory")
		}
		result := validator.Results()[0]
		if result.Name != "working_directory" {
			t.Errorf("expected name 'working_directory', got %q", result.Name)
		}
		if !strings.Contains(result.Message, "required") {
			t.Errorf("expected message about required path, got %q", result.Message)
		}
	})

	t.Run("non-existent path fails", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateWorkingDirectory("/nonexistent/path/that/does/not/exist")

		if !validator.HasErrors() {
			t.Fatal("expected error for non-existent directory")
		}
		result := validator.Results()[0]
		if !strings.Contains(result.Message, "does not exist") {
			t.Errorf("expected 'does not exist' message, got %q", result.Message)
		}
	})

	t.Run("file instead of directory fails", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		filePath := filepath.Join(directory, "not-a-dir")
		if err := os.WriteFile(filePath, []byte("content"), 0644); err != nil {
			t.Fatalf("creating test file: %v", err)
		}

		validator := NewValidator()
		validator.ValidateWorkingDirectory(filePath)

		if !validator.HasErrors() {
			t.Fatal("expected error when path is a file, not a directory")
		}
		result := validator.Results()[0]
		if !strings.Contains(result.Message, "not a directory") {
			t.Errorf("expected 'not a directory' message, got %q", result.Message)
		}
	})

	t.Run("valid directory passes", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()

		validator := NewValidator()
		validator.ValidateWorkingDirectory(directory)

		if validator.HasErrors() {
			t.Fatalf("unexpected error for valid directory: %v", validator.Results())
		}
		result := validator.Results()[0]
		if !result.Passed {
			t.Error("expected pass for valid directory")
		}
		if !strings.Contains(result.Message, "exists") {
			t.Errorf("expected 'exists' in message, got %q", result.Message)
		}
	})
}

func TestValidateProxySocket(t *testing.T) {
	t.Parallel()

	t.Run("empty string defaults and warns about missing socket", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateProxySocket("")

		// The default path /run/bureau/proxy.sock almost certainly does not exist
		// in a test environment, so we expect a warning.
		if validator.HasErrors() {
			t.Fatal("missing proxy socket should warn, not fail")
		}
		results := validator.Results()
		if length := len(results); length != 1 {
			t.Fatalf("expected 1 result, got %d", length)
		}
		result := results[0]
		if !result.Warning {
			t.Error("expected a warning for missing default proxy socket")
		}
		if !strings.Contains(result.Message, "/run/bureau/proxy.sock") {
			t.Errorf("expected default path in message, got %q", result.Message)
		}
	})

	t.Run("non-existent explicit path warns", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateProxySocket("/nonexistent/proxy.sock")

		if validator.HasErrors() {
			t.Fatal("missing proxy socket should warn, not fail")
		}
		result := validator.Results()[0]
		if !result.Warning {
			t.Error("expected a warning for non-existent socket path")
		}
		if !strings.Contains(result.Message, "not found") {
			t.Errorf("expected 'not found' message, got %q", result.Message)
		}
	})

	t.Run("regular file warns about not being a socket", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		filePath := filepath.Join(directory, "not-a-socket")
		if err := os.WriteFile(filePath, []byte("content"), 0644); err != nil {
			t.Fatalf("creating test file: %v", err)
		}

		validator := NewValidator()
		validator.ValidateProxySocket(filePath)

		if validator.HasErrors() {
			t.Fatal("non-socket file should warn, not fail")
		}
		result := validator.Results()[0]
		if !result.Warning {
			t.Error("expected a warning for regular file")
		}
		if !strings.Contains(result.Message, "not a socket") {
			t.Errorf("expected 'not a socket' message, got %q", result.Message)
		}
	})

	t.Run("actual unix socket passes", func(t *testing.T) {
		t.Parallel()
		socketDirectory := testutil.SocketDir(t)
		socketPath := filepath.Join(socketDirectory, "test.sock")

		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("creating unix socket: %v", err)
		}
		t.Cleanup(func() { listener.Close() })

		validator := NewValidator()
		validator.ValidateProxySocket(socketPath)

		if validator.HasErrors() {
			t.Fatalf("unexpected error for valid socket: %v", validator.Results())
		}
		result := validator.Results()[0]
		if !result.Passed {
			t.Error("expected pass for valid socket")
		}
		if result.Warning {
			t.Error("valid socket should not produce a warning")
		}
		if !strings.Contains(result.Message, "socket exists") {
			t.Errorf("expected 'socket exists' message, got %q", result.Message)
		}
	})
}

func TestValidateProfile(t *testing.T) {
	t.Parallel()

	t.Run("nil profile fails", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateProfile(nil)

		if !validator.HasErrors() {
			t.Fatal("expected error for nil profile")
		}
		result := validator.Results()[0]
		if !strings.Contains(result.Message, "nil") {
			t.Errorf("expected 'nil' in message, got %q", result.Message)
		}
	})

	t.Run("valid profile passes", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test-profile",
			Filesystem: []Mount{
				{Source: "/usr", Dest: "/usr", Mode: "ro"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfile(profile)

		if validator.HasErrors() {
			t.Fatalf("unexpected error for valid profile: %v", validator.Results())
		}
		result := validator.Results()[0]
		if !result.Passed {
			t.Error("expected pass for valid profile")
		}
		if !strings.Contains(result.Message, "test-profile") {
			t.Errorf("expected profile name in message, got %q", result.Message)
		}
	})

	t.Run("invalid profile fails", func(t *testing.T) {
		t.Parallel()
		// A bind mount with no source is invalid.
		profile := &Profile{
			Name: "bad-profile",
			Filesystem: []Mount{
				{Dest: "/test", Mode: "ro"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfile(profile)

		if !validator.HasErrors() {
			t.Fatal("expected error for invalid profile")
		}
		result := validator.Results()[0]
		if result.Passed {
			t.Error("expected failure for invalid profile")
		}
		if !strings.Contains(result.Message, "source is required") {
			t.Errorf("expected validation error about missing source, got %q", result.Message)
		}
	})

	t.Run("profile with invalid mount mode fails", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "bad-mode-profile",
			Filesystem: []Mount{
				{Source: "/tmp", Dest: "/test", Mode: "invalid"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfile(profile)

		if !validator.HasErrors() {
			t.Fatal("expected error for profile with invalid mount mode")
		}
	})
}

func TestValidateProfileSources(t *testing.T) {
	t.Parallel()

	t.Run("nil profile produces no results", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.ValidateProfileSources(nil, "/workspace", "/run/proxy.sock")

		if validator.HasErrors() {
			t.Fatal("nil profile should produce no errors")
		}
		if length := len(validator.Results()); length != 0 {
			t.Errorf("nil profile should produce no results, got %d", length)
		}
	})

	t.Run("existing mount source produces no failure", func(t *testing.T) {
		t.Parallel()
		sourceDirectory := t.TempDir()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: sourceDirectory, Dest: "/mounted", Mode: "ro"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatalf("unexpected error for existing source: %v", validator.Results())
		}
	})

	t.Run("non-existent required source fails", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: "/nonexistent/required/source", Dest: "/mounted", Mode: "ro"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if !validator.HasErrors() {
			t.Fatal("expected error for non-existent required mount source")
		}
		result := validator.Results()[0]
		if !strings.Contains(result.Message, "source not found") {
			t.Errorf("expected 'source not found' message, got %q", result.Message)
		}
	})

	t.Run("non-existent optional source warns", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: "/nonexistent/optional/source", Dest: "/mounted", Mode: "ro", Optional: true},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatal("optional missing source should not produce an error")
		}
		results := validator.Results()
		if length := len(results); length != 1 {
			t.Fatalf("expected 1 result (warning), got %d", length)
		}
		result := results[0]
		if !result.Warning {
			t.Error("expected a warning for optional missing source")
		}
		if !strings.Contains(result.Message, "optional source not found") {
			t.Errorf("expected 'optional source not found' message, got %q", result.Message)
		}
	})

	t.Run("tmpfs mounts are skipped", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Dest: "/tmp", Type: MountTypeTmpfs},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatal("tmpfs mounts should be skipped during source validation")
		}
		if length := len(validator.Results()); length != 0 {
			t.Errorf("expected no results for tmpfs mount, got %d", length)
		}
	})

	t.Run("proc mounts are skipped", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Dest: "/proc", Type: MountTypeProc},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatal("proc mounts should be skipped during source validation")
		}
		if length := len(validator.Results()); length != 0 {
			t.Errorf("expected no results for proc mount, got %d", length)
		}
	})

	t.Run("dev mounts are skipped", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Dest: "/dev", Type: MountTypeDev},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatal("dev mounts should be skipped during source validation")
		}
		if length := len(validator.Results()); length != 0 {
			t.Errorf("expected no results for dev mount, got %d", length)
		}
	})

	t.Run("variable expansion in sources", func(t *testing.T) {
		t.Parallel()
		workingDirectory := t.TempDir()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: "${WORKING_DIRECTORY}", Dest: "/workspace", Mode: "rw"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, workingDirectory, "")

		if validator.HasErrors() {
			t.Fatalf("WORKING_DIRECTORY variable should expand to existing path: %v", validator.Results())
		}
	})

	t.Run("unresolved variable in required mount fails", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: "${UNKNOWN_VARIABLE}", Dest: "/mounted", Mode: "ro"},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if !validator.HasErrors() {
			t.Fatal("expected error for unresolved variable in required mount")
		}
		result := validator.Results()[0]
		if !strings.Contains(result.Message, "unresolved variable") {
			t.Errorf("expected 'unresolved variable' message, got %q", result.Message)
		}
	})

	t.Run("unresolved variable in optional mount is silently skipped", func(t *testing.T) {
		t.Parallel()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Source: "${UNKNOWN_VARIABLE}", Dest: "/mounted", Mode: "ro", Optional: true},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		if validator.HasErrors() {
			t.Fatal("unresolved variable in optional mount should not produce an error")
		}
		if length := len(validator.Results()); length != 0 {
			t.Errorf("expected no results for skipped optional unresolved mount, got %d", length)
		}
	})

	t.Run("mixed mounts with multiple outcomes", func(t *testing.T) {
		t.Parallel()
		existingDirectory := t.TempDir()
		profile := &Profile{
			Name: "test",
			Filesystem: []Mount{
				{Dest: "/tmp", Type: MountTypeTmpfs},
				{Dest: "/proc", Type: MountTypeProc},
				{Source: existingDirectory, Dest: "/existing", Mode: "ro"},
				{Source: "/nonexistent/required", Dest: "/missing-required", Mode: "ro"},
				{Source: "/nonexistent/optional", Dest: "/missing-optional", Mode: "ro", Optional: true},
			},
		}

		validator := NewValidator()
		validator.ValidateProfileSources(profile, "/workspace", "")

		// tmpfs and proc are skipped (no results).
		// existingDirectory passes (no results since it exists and no error/warning).
		// /nonexistent/required fails.
		// /nonexistent/optional warns.
		if !validator.HasErrors() {
			t.Fatal("expected at least one error from non-existent required source")
		}

		results := validator.Results()
		// Should have exactly 2 results: one fail for required, one warn for optional.
		if length := len(results); length != 2 {
			t.Fatalf("expected 2 results, got %d: %+v", length, results)
		}

		// Find the failure and warning.
		var foundFailure, foundWarning bool
		for _, result := range results {
			if !result.Passed && strings.Contains(result.Message, "source not found") {
				foundFailure = true
			}
			if result.Warning && strings.Contains(result.Message, "optional source not found") {
				foundWarning = true
			}
		}
		if !foundFailure {
			t.Error("expected failure for non-existent required source")
		}
		if !foundWarning {
			t.Error("expected warning for non-existent optional source")
		}
	})
}

func TestPrintResults(t *testing.T) {
	t.Parallel()

	t.Run("pass and warn and fail formatting", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.pass("check-a", "looks good")
		validator.warn("check-b", "might be a problem")
		validator.fail("check-c", "definitely broken")

		var buffer bytes.Buffer
		validator.PrintResults(&buffer)
		output := buffer.String()

		// Verify pass line has check mark prefix.
		if !strings.Contains(output, "\u2713 check-a: looks good") {
			t.Errorf("expected pass line with check mark, got:\n%s", output)
		}

		// Verify warning line has warning prefix.
		if !strings.Contains(output, "\u26a0 check-b: might be a problem") {
			t.Errorf("expected warning line with warning symbol, got:\n%s", output)
		}

		// Verify failure line has cross prefix.
		if !strings.Contains(output, "\u2717 check-c: definitely broken") {
			t.Errorf("expected failure line with cross mark, got:\n%s", output)
		}

		// Verify error summary.
		if !strings.Contains(output, "Validation failed with 1 error(s)") {
			t.Errorf("expected failure summary, got:\n%s", output)
		}
	})

	t.Run("all passing shows ready message", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.pass("check-a", "fine")
		validator.warn("check-b", "just a warning")

		var buffer bytes.Buffer
		validator.PrintResults(&buffer)
		output := buffer.String()

		if !strings.Contains(output, "Ready to run sandbox") {
			t.Errorf("expected ready message when no errors, got:\n%s", output)
		}
	})

	t.Run("no results prints ready message", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()

		var buffer bytes.Buffer
		validator.PrintResults(&buffer)
		output := buffer.String()

		if !strings.Contains(output, "Ready to run sandbox") {
			t.Errorf("expected ready message for empty validator, got:\n%s", output)
		}
	})

	t.Run("multiple failures counted correctly", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.fail("check-a", "broken")
		validator.fail("check-b", "also broken")
		validator.fail("check-c", "really broken")

		var buffer bytes.Buffer
		validator.PrintResults(&buffer)
		output := buffer.String()

		if !strings.Contains(output, "Validation failed with 3 error(s)") {
			t.Errorf("expected '3 error(s)' in summary, got:\n%s", output)
		}
	})
}

func TestHasErrorsAndResults(t *testing.T) {
	t.Parallel()

	t.Run("empty validator has no errors", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		if validator.HasErrors() {
			t.Error("empty validator should not have errors")
		}
	})

	t.Run("passes do not create errors", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.pass("a", "ok")
		validator.pass("b", "ok")
		if validator.HasErrors() {
			t.Error("only passes should not create errors")
		}
	})

	t.Run("warnings do not create errors", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.warn("a", "watch out")
		if validator.HasErrors() {
			t.Error("warnings should not count as errors")
		}
	})

	t.Run("failures create errors", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.fail("a", "bad")
		if !validator.HasErrors() {
			t.Error("failures should create errors")
		}
	})

	t.Run("results returns all accumulated entries", func(t *testing.T) {
		t.Parallel()
		validator := NewValidator()
		validator.pass("a", "ok")
		validator.warn("b", "hmm")
		validator.fail("c", "bad")

		results := validator.Results()
		if length := len(results); length != 3 {
			t.Fatalf("expected 3 results, got %d", length)
		}

		// Verify ordering matches insertion order.
		if results[0].Name != "a" || results[1].Name != "b" || results[2].Name != "c" {
			t.Errorf("results out of order: %v, %v, %v", results[0].Name, results[1].Name, results[2].Name)
		}
	})
}
