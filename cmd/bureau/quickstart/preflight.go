// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// checkResult describes the outcome of a single preflight check.
type checkResult struct {
	// Name identifies the check (e.g., "homeserver", "launcher").
	Name string

	// Passed is true if the check succeeded.
	Passed bool

	// Message describes the outcome — either a success note or an error
	// with actionable guidance.
	Message string
}

// preflightConfig describes what the preflight checks need to verify.
type preflightConfig struct {
	HomeserverURL  string
	RunDir         string
	CredentialFile string
	Agent          string
}

// runPreflight executes all preflight checks and returns the results. All
// checks run regardless of earlier failures so the user gets a complete
// picture of what needs fixing.
func runPreflight(ctx context.Context, config preflightConfig) []checkResult {
	var results []checkResult

	results = append(results, checkHomeserver(ctx, config.HomeserverURL))
	results = append(results, checkLauncher(config.RunDir))
	results = append(results, checkDaemon(config.RunDir))
	results = append(results, checkCredentialFile(config.CredentialFile))
	results = append(results, checkAgentBinary(config.Agent))

	return results
}

// preflightPassed returns true if all checks passed.
func preflightPassed(results []checkResult) bool {
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return true
}

// checkHomeserver verifies that the Continuwuity homeserver is reachable by
// hitting the /_matrix/client/versions endpoint.
func checkHomeserver(ctx context.Context, homeserverURL string) checkResult {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet,
		homeserverURL+"/_matrix/client/versions", nil)
	if err != nil {
		return checkResult{
			Name:    "homeserver",
			Message: fmt.Sprintf("Continuwuity not reachable at %s: %v", homeserverURL, err),
		}
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return checkResult{
			Name: "homeserver",
			Message: fmt.Sprintf("Continuwuity not reachable at %s: %v\n"+
				"  Start the homeserver or pass --homeserver with the correct URL.", homeserverURL, err),
		}
	}
	response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return checkResult{
			Name: "homeserver",
			Message: fmt.Sprintf("Continuwuity returned HTTP %d at %s/_matrix/client/versions",
				response.StatusCode, homeserverURL),
		}
	}

	return checkResult{
		Name:    "homeserver",
		Passed:  true,
		Message: fmt.Sprintf("Continuwuity reachable at %s", homeserverURL),
	}
}

// checkLauncher verifies that the launcher is running by checking for its
// Unix socket.
func checkLauncher(runDir string) checkResult {
	socketPath := principal.LauncherSocketPath(runDir)

	// Check socket exists.
	info, err := os.Stat(socketPath)
	if err != nil {
		return checkResult{
			Name: "launcher",
			Message: fmt.Sprintf("Launcher not running (socket not found at %s).\n"+
				"  Start the launcher: bureau-launcher --run-dir %s ...", socketPath, runDir),
		}
	}

	// Verify it's actually a socket.
	if info.Mode()&os.ModeSocket == 0 {
		return checkResult{
			Name:    "launcher",
			Message: fmt.Sprintf("Expected Unix socket at %s but found a regular file", socketPath),
		}
	}

	// Try to connect to verify the process is alive.
	connection, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return checkResult{
			Name: "launcher",
			Message: fmt.Sprintf("Launcher socket exists at %s but is not accepting connections: %v\n"+
				"  The launcher may have crashed. Check its logs and restart.", socketPath, err),
		}
	}
	connection.Close()

	return checkResult{
		Name:    "launcher",
		Passed:  true,
		Message: "Launcher running",
	}
}

// checkDaemon verifies that the daemon is running by checking for its
// observe socket.
func checkDaemon(runDir string) checkResult {
	socketPath := principal.ObserveSocketPath(runDir)

	info, err := os.Stat(socketPath)
	if err != nil {
		return checkResult{
			Name: "daemon",
			Message: fmt.Sprintf("Daemon not running (observe socket not found at %s).\n"+
				"  Start the daemon: bureau-daemon --run-dir %s ...", socketPath, runDir),
		}
	}

	if info.Mode()&os.ModeSocket == 0 {
		return checkResult{
			Name:    "daemon",
			Message: fmt.Sprintf("Expected Unix socket at %s but found a regular file", socketPath),
		}
	}

	return checkResult{
		Name:    "daemon",
		Passed:  true,
		Message: "Daemon running",
	}
}

// checkCredentialFile verifies that the Bureau credential file exists and
// is readable.
func checkCredentialFile(path string) checkResult {
	if path == "" {
		return checkResult{
			Name: "credentials",
			Message: "No credential file specified.\n" +
				"  Pass --credential-file or run 'bureau matrix setup' first.",
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return checkResult{
			Name: "credentials",
			Message: fmt.Sprintf("Credential file not found at %s.\n"+
				"  Run 'bureau matrix setup --credential-file %s' to create it.", path, path),
		}
	}

	if info.IsDir() {
		return checkResult{
			Name:    "credentials",
			Message: fmt.Sprintf("Credential file path %s is a directory, not a file", path),
		}
	}

	return checkResult{
		Name:    "credentials",
		Passed:  true,
		Message: fmt.Sprintf("Credential file found: %s", path),
	}
}

// checkAgentBinary verifies that the agent binary is available in PATH.
// For --agent=test, checks bureau-test-agent. For --agent=claude, checks
// the claude CLI.
func checkAgentBinary(agent string) checkResult {
	binaryName := agentBinary(agent)
	if binaryName == "" {
		return checkResult{
			Name:    "agent-binary",
			Message: fmt.Sprintf("Unknown agent type %q. Supported: test, claude", agent),
		}
	}

	path, err := exec.LookPath(binaryName)
	if err != nil {
		return checkResult{
			Name: "agent-binary",
			Message: fmt.Sprintf("Agent binary %q not found in PATH.\n"+
				"  Ensure %s is installed and in your PATH.", binaryName, binaryName),
		}
	}

	return checkResult{
		Name:    "agent-binary",
		Passed:  true,
		Message: fmt.Sprintf("Agent binary found: %s", path),
	}
}
