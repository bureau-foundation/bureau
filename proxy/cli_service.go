// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CLIService proxies requests to a command-line tool.
// Examples: gh, gcloud, az, kubectl, docker, etc.
type CLIService struct {
	name       string
	binary     string
	envVars    map[string]EnvVarConfig // Maps env var name -> credential config
	filter     Filter
	credential CredentialSource
}

// CLIServiceConfig holds configuration for creating a CLIService.
type CLIServiceConfig struct {
	// Name is the service name used in requests and logging.
	Name string

	// Binary is the path to the executable.
	Binary string

	// EnvVars maps environment variable names to credential configurations.
	// Example: {"GH_TOKEN": {Credential: "github-pat", Type: "value"}}
	EnvVars map[string]EnvVarConfig

	// Filter validates requests before execution.
	// If nil, all requests are allowed.
	Filter Filter

	// Credential provides credential values.
	Credential CredentialSource
}

// NewCLIService creates a new CLI service.
func NewCLIService(config CLIServiceConfig) (*CLIService, error) {
	if config.Name == "" {
		return nil, fmt.Errorf("service name is required")
	}
	if config.Binary == "" {
		return nil, fmt.Errorf("binary path is required")
	}

	return &CLIService{
		name:       config.Name,
		binary:     config.Binary,
		envVars:    config.EnvVars,
		filter:     config.Filter,
		credential: config.Credential,
	}, nil
}

// Name returns the service name.
func (s *CLIService) Name() string {
	return s.name
}

// checkAndPrepareCommand validates the request and creates a prepared command.
// Returns the command and a cleanup function that must be called after execution.
func (s *CLIService) checkAndPrepareCommand(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	// Check filter first
	if s.filter != nil {
		if err := s.filter.Check(args); err != nil {
			return nil, nil, fmt.Errorf("request blocked: %w", err)
		}
	}

	// Fail fast if any configured credentials are missing.
	// This prevents confusing "unauthorized" errors from the CLI tool
	// when the real problem is proxy misconfiguration.
	var missingCredentials []string
	for _, envConfig := range s.envVars {
		if s.credential == nil || s.credential.Get(envConfig.Credential) == nil {
			missingCredentials = append(missingCredentials, envConfig.Credential)
		}
	}
	if len(missingCredentials) > 0 {
		return nil, nil, fmt.Errorf("missing credentials: %v", missingCredentials)
	}

	cmd := exec.CommandContext(ctx, s.binary, args...)

	// Build a sanitized environment with only safe variables.
	// This prevents the proxy's own credentials (e.g., BUREAU_GITHUB_PAT in dev mode)
	// from leaking to the subprocess where the agent could read them.
	env := sanitizedEnvironment()

	// Track temp files for cleanup
	var tempFiles []string

	// Inject credentials from the configured source
	for envVar, envConfig := range s.envVars {
		value := s.credential.Get(envConfig.Credential)

		switch envConfig.Type {
		case "file":
			// Write credential to temp file and inject the path.
			// Use /dev/shm for RAM-backed storage when available (no disk write).
			tempDir := "/dev/shm"
			if _, err := os.Stat(tempDir); err != nil {
				tempDir = "" // Fall back to system default
			}
			tempFile, err := os.CreateTemp(tempDir, "bureau-cred-*")
			if err != nil {
				// Clean up any temp files already created
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("failed to create temp file for %s: %w", envVar, err)
			}
			// Write credential directly from mmap to file (no heap copy).
			if _, err := value.WriteTo(tempFile); err != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("failed to write temp file for %s: %w", envVar, err)
			}
			tempFile.Close()
			// Restrict permissions to owner-read-only
			os.Chmod(tempFile.Name(), 0400)
			tempFiles = append(tempFiles, tempFile.Name())
			env = append(env, fmt.Sprintf("%s=%s", envVar, tempFile.Name()))

		default: // "value"
			env = append(env, fmt.Sprintf("%s=%s", envVar, value.String()))
		}
	}
	cmd.Env = env

	// Return cleanup function to remove temp files
	cleanup := func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}

	return cmd, cleanup, nil
}

// sanitizedEnvironment returns a minimal set of safe environment variables.
// This prevents credential leakage from the proxy's environment to subprocesses.
func sanitizedEnvironment() []string {
	safeVars := []string{
		"PATH",
		"HOME",
		"USER",
		"LANG",
		"LC_ALL",
		"TZ",
		"TERM",
		"TMPDIR",
	}

	var env []string
	for _, name := range safeVars {
		if value := os.Getenv(name); value != "" {
			env = append(env, fmt.Sprintf("%s=%s", name, value))
		}
	}
	return env
}

// Execute runs the CLI command with injected credentials.
// Output is buffered and returned when the command completes.
func (s *CLIService) Execute(ctx context.Context, args []string, input string) (*ExecutionResult, error) {
	cmd, cleanup, err := s.checkAndPrepareCommand(ctx, args)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Provide stdin if input was given
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}

	// Capture output
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run command
	err = cmd.Run()

	result := &ExecutionResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("failed to execute command: %w", err)
		}
	}

	return result, nil
}

// ExecuteStream runs the CLI command and streams output to the provided writers.
// Returns the exit code when complete.
func (s *CLIService) ExecuteStream(ctx context.Context, args []string, input string, stdout, stderr io.Writer) (int, error) {
	cmd, cleanup, err := s.checkAndPrepareCommand(ctx, args)
	if err != nil {
		return -1, err
	}
	defer cleanup()

	// Provide stdin if input was given
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Run command
	err = cmd.Run()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("failed to execute command: %w", err)
	}

	return 0, nil
}

// Verify CLIService implements Service and StreamingService interfaces.
var (
	_ Service          = (*CLIService)(nil)
	_ StreamingService = (*CLIService)(nil)
)
