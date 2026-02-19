// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// --- NewCLIService ---

func TestNewCLIService_MissingName(t *testing.T) {
	_, err := NewCLIService(CLIServiceConfig{
		Binary: "/usr/bin/echo",
	})
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "service name is required") {
		t.Errorf("error = %q, want it to contain %q", err, "service name is required")
	}
}

func TestNewCLIService_MissingBinary(t *testing.T) {
	_, err := NewCLIService(CLIServiceConfig{
		Name: "test-service",
	})
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "binary path is required") {
		t.Errorf("error = %q, want it to contain %q", err, "binary path is required")
	}
}

func TestNewCLIService_Valid(t *testing.T) {
	service, err := NewCLIService(CLIServiceConfig{
		Name:   "test-service",
		Binary: "/usr/bin/echo",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.Name() != "test-service" {
		t.Errorf("Name() = %q, want %q", service.Name(), "test-service")
	}
}

// --- sanitizedEnvironment ---

func TestSanitizedEnvironment_IncludesSafeVars(t *testing.T) {
	// Set safe vars with known values.
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/test")
	t.Setenv("USER", "testuser")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TMPDIR", "/tmp")

	environment := sanitizedEnvironment()

	// Build a map for easier lookup.
	envMap := make(map[string]string, len(environment))
	for _, entry := range environment {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	expected := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/home/test",
		"USER":   "testuser",
		"LANG":   "en_US.UTF-8",
		"LC_ALL": "C",
		"TZ":     "UTC",
		"TERM":   "xterm-256color",
		"TMPDIR": "/tmp",
	}

	for key, wantValue := range expected {
		gotValue, ok := envMap[key]
		if !ok {
			t.Errorf("safe var %s missing from sanitized environment", key)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("%s = %q, want %q", key, gotValue, wantValue)
		}
	}
}

func TestSanitizedEnvironment_ExcludesUnsafeVars(t *testing.T) {
	t.Setenv("SECRET_KEY", "super-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("BUREAU_GITHUB_PAT", "ghp_dangerous")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-leak")

	environment := sanitizedEnvironment()

	unsafeVars := []string{"SECRET_KEY", "AWS_SECRET_ACCESS_KEY", "BUREAU_GITHUB_PAT", "ANTHROPIC_API_KEY"}
	for _, entry := range environment {
		for _, unsafe := range unsafeVars {
			if strings.HasPrefix(entry, unsafe+"=") {
				t.Errorf("unsafe var %s leaked into sanitized environment", unsafe)
			}
		}
	}
}

func TestSanitizedEnvironment_OmitsEmptyVars(t *testing.T) {
	// Unset a safe var to verify it is omitted (not included as "KEY=").
	t.Setenv("LC_ALL", "")

	environment := sanitizedEnvironment()
	for _, entry := range environment {
		if strings.HasPrefix(entry, "LC_ALL=") {
			t.Errorf("empty LC_ALL should be omitted, but found: %q", entry)
		}
	}
}

// --- checkAndPrepareCommand ---

func TestCheckAndPrepareCommand_FilterBlocks(t *testing.T) {
	echoBinary := requireBinary(t, "echo")
	service, err := NewCLIService(CLIServiceConfig{
		Name:   "filtered",
		Binary: echoBinary,
		Filter: &DenyAllFilter{Reason: "testing"},
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	_, _, err = service.checkAndPrepareCommand(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error from filter block, got nil")
	}
	if !strings.Contains(err.Error(), "request blocked") {
		t.Errorf("error = %q, want it to contain %q", err, "request blocked")
	}
}

func TestCheckAndPrepareCommand_MissingCredentials(t *testing.T) {
	echoBinary := requireBinary(t, "echo")
	// Credential source that has nothing in it.
	emptyCredentials := testCredentials(t, map[string]string{})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "needs-creds",
		Binary: echoBinary,
		EnvVars: map[string]EnvVarConfig{
			"GH_TOKEN": {Credential: "github-pat", Type: "value"},
		},
		Credential: emptyCredentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	_, _, err = service.checkAndPrepareCommand(context.Background(), []string{"pr", "list"})
	if err == nil {
		t.Fatal("expected error for missing credentials, got nil")
	}
	if !strings.Contains(err.Error(), "missing credentials") {
		t.Errorf("error = %q, want it to contain %q", err, "missing credentials")
	}
	if !strings.Contains(err.Error(), "github-pat") {
		t.Errorf("error = %q, want it to mention the missing credential name %q", err, "github-pat")
	}
}

func TestCheckAndPrepareCommand_NilCredentialSource(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "nil-creds",
		Binary: echoBinary,
		EnvVars: map[string]EnvVarConfig{
			"API_KEY": {Credential: "some-key", Type: "value"},
		},
		// Credential intentionally nil.
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	_, _, err = service.checkAndPrepareCommand(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected error for nil credential source, got nil")
	}
	if !strings.Contains(err.Error(), "missing credentials") {
		t.Errorf("error = %q, want it to contain %q", err, "missing credentials")
	}
}

func TestCheckAndPrepareCommand_HappyPath(t *testing.T) {
	echoBinary := requireBinary(t, "echo")
	credentials := testCredentials(t, map[string]string{
		"github-pat": "ghp_test123",
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "gh",
		Binary: echoBinary,
		EnvVars: map[string]EnvVarConfig{
			"GH_TOKEN": {Credential: "github-pat", Type: "value"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	command, cleanup, err := service.checkAndPrepareCommand(context.Background(), []string{"pr", "list"})
	if err != nil {
		t.Fatalf("checkAndPrepareCommand: %v", err)
	}
	defer cleanup()

	if command == nil {
		t.Fatal("expected non-nil command")
	}

	// Verify the credential is in the command's environment.
	var foundToken bool
	for _, entry := range command.Env {
		if entry == "GH_TOKEN=ghp_test123" {
			foundToken = true
			break
		}
	}
	if !foundToken {
		t.Error("GH_TOKEN not found in command environment")
	}
}

func TestCheckAndPrepareCommand_NoEnvVarsNoCredentials(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	// A service with no env vars should not require credentials at all.
	service, err := NewCLIService(CLIServiceConfig{
		Name:   "simple",
		Binary: echoBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	command, cleanup, err := service.checkAndPrepareCommand(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("checkAndPrepareCommand: %v", err)
	}
	defer cleanup()

	if command == nil {
		t.Fatal("expected non-nil command")
	}
}

// --- Execute ---

func TestExecute_EchoStdout(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "echo",
		Binary: echoBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), []string{"hello", "world"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello world" {
		t.Errorf("Stdout = %q, want %q", strings.TrimSpace(result.Stdout), "hello world")
	}
}

func TestExecute_NonZeroExitCode(t *testing.T) {
	falseBinary := requireBinary(t, "false")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "fail",
		Binary: falseBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("Execute returned error instead of non-zero exit code: %v", err)
	}

	if result.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero")
	}
}

func TestExecute_StdinInput(t *testing.T) {
	catBinary := requireBinary(t, "cat")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "cat",
		Binary: catBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), nil, "piped input data")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "piped input data" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "piped input data")
	}
}

func TestExecute_FilterBlocks(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "blocked",
		Binary: echoBinary,
		Filter: &DenyAllFilter{Reason: "denied"},
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	_, err = service.Execute(context.Background(), []string{"test"}, "")
	if err == nil {
		t.Fatal("expected error from filter block, got nil")
	}
	if !strings.Contains(err.Error(), "request blocked") {
		t.Errorf("error = %q, want it to contain %q", err, "request blocked")
	}
}

func TestExecute_Stderr(t *testing.T) {
	shBinary := requireBinary(t, "sh")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "stderr-test",
		Binary: shBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), []string{"-c", "echo error-output >&2"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stderr) != "error-output" {
		t.Errorf("Stderr = %q, want %q", strings.TrimSpace(result.Stderr), "error-output")
	}
}

func TestExecute_CredentialInEnvironment(t *testing.T) {
	// Use 'env' or 'sh -c printenv' to verify the credential appears in the
	// subprocess environment but unsafe vars from the test process do not.
	shBinary := requireBinary(t, "sh")

	t.Setenv("BUREAU_GITHUB_PAT", "should-not-leak")

	credentials := testCredentials(t, map[string]string{
		"github-pat": "ghp_injected_value",
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "env-check",
		Binary: shBinary,
		EnvVars: map[string]EnvVarConfig{
			"GH_TOKEN": {Credential: "github-pat", Type: "value"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), []string{"-c", "echo $GH_TOKEN"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.TrimSpace(result.Stdout) != "ghp_injected_value" {
		t.Errorf("GH_TOKEN = %q, want %q", strings.TrimSpace(result.Stdout), "ghp_injected_value")
	}

	// Verify the proxy's own env var does not leak.
	result, err = service.Execute(context.Background(), []string{"-c", "echo $BUREAU_GITHUB_PAT"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.TrimSpace(result.Stdout) != "" {
		t.Errorf("BUREAU_GITHUB_PAT leaked to subprocess: %q", strings.TrimSpace(result.Stdout))
	}
}

func TestExecute_ContextCancellation(t *testing.T) {
	// Use a command that sleeps, cancel the context, and verify it fails.
	sleepBinary := requireBinary(t, "sleep")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "cancel-test",
		Binary: sleepBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err = service.Execute(ctx, []string{"60"}, "")
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// --- ExecuteStream ---

func TestExecuteStream_EchoStdout(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "echo-stream",
		Binary: echoBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := service.ExecuteStream(context.Background(), []string{"streamed", "output"}, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if strings.TrimSpace(stdout.String()) != "streamed output" {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(stdout.String()), "streamed output")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestExecuteStream_NonZeroExitCode(t *testing.T) {
	falseBinary := requireBinary(t, "false")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "fail-stream",
		Binary: falseBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := service.ExecuteStream(context.Background(), nil, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteStream returned error instead of non-zero exit code: %v", err)
	}

	if exitCode == 0 {
		t.Error("exit code = 0, want non-zero")
	}
}

func TestExecuteStream_StdinInput(t *testing.T) {
	catBinary := requireBinary(t, "cat")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "cat-stream",
		Binary: catBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := service.ExecuteStream(context.Background(), nil, "stream input data", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if stdout.String() != "stream input data" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "stream input data")
	}
}

func TestExecuteStream_FilterBlocks(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "blocked-stream",
		Binary: echoBinary,
		Filter: &DenyAllFilter{Reason: "denied"},
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := service.ExecuteStream(context.Background(), []string{"test"}, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error from filter block, got nil")
	}
	if exitCode != -1 {
		t.Errorf("exit code = %d, want -1 for filter rejection", exitCode)
	}
}

func TestExecuteStream_Stderr(t *testing.T) {
	shBinary := requireBinary(t, "sh")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "stderr-stream",
		Binary: shBinary,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := service.ExecuteStream(context.Background(), []string{"-c", "echo out; echo err >&2"}, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if strings.TrimSpace(stdout.String()) != "out" {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(stdout.String()), "out")
	}
	if strings.TrimSpace(stderr.String()) != "err" {
		t.Errorf("stderr = %q, want %q", strings.TrimSpace(stderr.String()), "err")
	}
}

// --- Credential file injection ---

func TestExecute_FileTypeCredential(t *testing.T) {
	shBinary := requireBinary(t, "sh")

	credentials := testCredentials(t, map[string]string{
		"gcloud-sa": `{"type":"service_account","project_id":"test"}`,
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "gcloud",
		Binary: shBinary,
		EnvVars: map[string]EnvVarConfig{
			"GOOGLE_APPLICATION_CREDENTIALS": {Credential: "gcloud-sa", Type: "file"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	// Read the content of the file pointed to by the env var to verify it was
	// written correctly. Also verify the file is readable by the subprocess.
	result, err := service.Execute(context.Background(), []string{"-c", "cat $GOOGLE_APPLICATION_CREDENTIALS"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stderr: %s)", result.ExitCode, result.Stderr)
	}

	expectedContent := `{"type":"service_account","project_id":"test"}`
	if result.Stdout != expectedContent {
		t.Errorf("file content = %q, want %q", result.Stdout, expectedContent)
	}
}

func TestExecute_FileTypeCredentialCleanedUp(t *testing.T) {
	shBinary := requireBinary(t, "sh")

	credentials := testCredentials(t, map[string]string{
		"secret-file": "secret-content",
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "cleanup-test",
		Binary: shBinary,
		EnvVars: map[string]EnvVarConfig{
			"SECRET_FILE": {Credential: "secret-file", Type: "file"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	// Have the subprocess echo back the file path so we can check it was removed.
	result, err := service.Execute(context.Background(), []string{"-c", "echo $SECRET_FILE"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tempFilePath := strings.TrimSpace(result.Stdout)
	if tempFilePath == "" {
		t.Fatal("subprocess did not receive SECRET_FILE env var")
	}

	// After Execute returns, the cleanup function should have removed the temp file.
	if _, err := os.Stat(tempFilePath); !os.IsNotExist(err) {
		t.Errorf("temp file %s still exists after execution (should have been cleaned up)", tempFilePath)
	}
}

func TestExecute_FileTypeCredentialInShmOrTmp(t *testing.T) {
	shBinary := requireBinary(t, "sh")

	credentials := testCredentials(t, map[string]string{
		"location-check": "check-location",
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "location-test",
		Binary: shBinary,
		EnvVars: map[string]EnvVarConfig{
			"CRED_FILE": {Credential: "location-check", Type: "file"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), []string{"-c", "echo $CRED_FILE"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tempFilePath := strings.TrimSpace(result.Stdout)
	if tempFilePath == "" {
		t.Fatal("subprocess did not receive CRED_FILE env var")
	}

	// The file should be in /dev/shm (preferred) or the system temp dir.
	if !strings.HasPrefix(tempFilePath, "/dev/shm/") && !strings.HasPrefix(tempFilePath, os.TempDir()) {
		t.Errorf("temp file %s is not in /dev/shm or %s", tempFilePath, os.TempDir())
	}

	if !strings.Contains(tempFilePath, "bureau-cred-") {
		t.Errorf("temp file %s does not have expected bureau-cred- prefix", tempFilePath)
	}
}

func TestCheckAndPrepareCommand_MultipleFileCredentials(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	credentials := testCredentials(t, map[string]string{
		"cred-a": "value-a",
		"cred-b": "value-b",
	})

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "multi-file",
		Binary: echoBinary,
		EnvVars: map[string]EnvVarConfig{
			"FILE_A": {Credential: "cred-a", Type: "file"},
			"FILE_B": {Credential: "cred-b", Type: "file"},
		},
		Credential: credentials,
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	command, cleanup, err := service.checkAndPrepareCommand(context.Background(), nil)
	if err != nil {
		t.Fatalf("checkAndPrepareCommand: %v", err)
	}

	// Verify both file env vars are set to different paths.
	envMap := make(map[string]string)
	for _, entry := range command.Env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	filePathA, okA := envMap["FILE_A"]
	filePathB, okB := envMap["FILE_B"]
	if !okA || !okB {
		t.Fatalf("expected both FILE_A and FILE_B in env, got: %v", envMap)
	}
	if filePathA == filePathB {
		t.Error("FILE_A and FILE_B point to the same temp file")
	}

	// Verify both files exist before cleanup and contain the right values.
	for name, path := range map[string]string{"FILE_A": filePathA, "FILE_B": filePathB} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s (%s): %v", name, path, err)
			continue
		}
		expectedValue := fmt.Sprintf("value-%s", strings.ToLower(string(name[len(name)-1])))
		if string(content) != expectedValue {
			t.Errorf("%s content = %q, want %q", name, string(content), expectedValue)
		}
	}

	// After cleanup, both files should be gone.
	cleanup()

	for name, path := range map[string]string{"FILE_A": filePathA, "FILE_B": filePathB} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("temp file for %s (%s) still exists after cleanup", name, path)
		}
	}
}

// --- GlobFilter integration with CLIService ---

func TestExecute_GlobFilterAllowed(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "filtered-echo",
		Binary: echoBinary,
		Filter: &GlobFilter{
			Allowed: []string{"hello *"},
		},
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	result, err := service.Execute(context.Background(), []string{"hello", "world"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello world" {
		t.Errorf("Stdout = %q, want %q", strings.TrimSpace(result.Stdout), "hello world")
	}
}

func TestExecute_GlobFilterBlocked(t *testing.T) {
	echoBinary := requireBinary(t, "echo")

	service, err := NewCLIService(CLIServiceConfig{
		Name:   "filtered-echo",
		Binary: echoBinary,
		Filter: &GlobFilter{
			Allowed: []string{"hello *"},
		},
	})
	if err != nil {
		t.Fatalf("NewCLIService: %v", err)
	}

	_, err = service.Execute(context.Background(), []string{"goodbye", "world"}, "")
	if err == nil {
		t.Fatal("expected error from glob filter block, got nil")
	}
	if !strings.Contains(err.Error(), "request blocked") {
		t.Errorf("error = %q, want it to contain %q", err, "request blocked")
	}
}

// --- Helpers ---

// requireBinary calls exec.LookPath and skips the test if the binary is not found.
func requireBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("binary %q not found in PATH: %v", name, err)
	}
	return path
}
