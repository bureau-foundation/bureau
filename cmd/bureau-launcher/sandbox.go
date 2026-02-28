// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/sandbox"
)

// specToProfile converts a schema.SandboxSpec (JSON wire format from the daemon)
// into a sandbox.Profile (the format that BwrapBuilder consumes). This is the
// translation layer between the Matrix-native template system and the bwrap
// execution engine.
//
// In addition to converting struct fields, specToProfile:
//   - Adds a /nix/store ro bind mount when EnvironmentPath is set (so Nix
//     store paths referenced by the environment are accessible inside the sandbox)
//   - Prepends EnvironmentPath/bin to PATH
//   - Adds a bind mount for the proxy socket at /run/bureau/proxy.sock
func specToProfile(spec *schema.SandboxSpec, proxySocketPath string) *sandbox.Profile {
	profile := &sandbox.Profile{}

	// Convert filesystem mounts.
	for _, mount := range spec.Filesystem {
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source:   mount.Source,
			Dest:     mount.Dest,
			Type:     mount.Type,
			Mode:     string(mount.Mode),
			Options:  mount.Options,
			Optional: mount.Optional,
		})
	}

	// Convert namespaces.
	if spec.Namespaces != nil {
		profile.Namespaces = sandbox.NamespaceConfig{
			PID: spec.Namespaces.PID,
			Net: spec.Namespaces.Net,
			IPC: spec.Namespaces.IPC,
			UTS: spec.Namespaces.UTS,
		}
	}

	// Convert resources. Schema types use explicit integer values;
	// sandbox.ResourceConfig uses strings for memory and quota.
	if spec.Resources != nil {
		if spec.Resources.MemoryLimitMB > 0 {
			profile.Resources.MemoryMax = fmt.Sprintf("%dM", spec.Resources.MemoryLimitMB)
		}
		if spec.Resources.PidsLimit > 0 {
			profile.Resources.TasksMax = spec.Resources.PidsLimit
		}
		if spec.Resources.CPUShares > 0 {
			// Convert cgroup v1 cpu.shares (default 1024) to cgroup v2
			// cpu.weight (default 100). The scaling is approximately:
			//   weight = shares * 100 / 1024
			// Clamped to the valid cpu.weight range [1, 10000].
			weight := spec.Resources.CPUShares * 100 / 1024
			if weight < 1 {
				weight = 1
			}
			if weight > 10000 {
				weight = 10000
			}
			profile.Resources.CPUWeight = weight
		}
	}

	// Convert security settings.
	if spec.Security != nil {
		profile.Security = sandbox.SecurityConfig{
			NewSession:    spec.Security.NewSession,
			DieWithParent: spec.Security.DieWithParent,
			NoNewPrivs:    spec.Security.NoNewPrivs,
		}
	}

	// Convert environment variables.
	if len(spec.EnvironmentVariables) > 0 {
		profile.Environment = make(map[string]string, len(spec.EnvironmentVariables))
		for key, value := range spec.EnvironmentVariables {
			profile.Environment[key] = value
		}
	}

	// Handle Nix environment path: when set, bind mount /nix/store (so the
	// Nix closure is accessible inside the sandbox) and prepend the
	// environment's bin directory to PATH.
	if spec.EnvironmentPath != "" {
		// Add /nix/store as a read-only bind mount if not already present.
		hasNixStore := false
		for _, mount := range profile.Filesystem {
			if mount.Dest == "/nix/store" || mount.Dest == "/nix" {
				hasNixStore = true
				break
			}
		}
		if !hasNixStore {
			profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
				Source: "/nix/store",
				Dest:   "/nix/store",
				Mode:   sandbox.MountModeRO,
			})
		}

		// Prepend the environment's bin directory to PATH.
		environmentBin := spec.EnvironmentPath + "/bin"
		if profile.Environment == nil {
			profile.Environment = make(map[string]string)
		}
		existingPath := profile.Environment["PATH"]
		if existingPath != "" {
			profile.Environment["PATH"] = environmentBin + ":" + existingPath
		} else {
			profile.Environment["PATH"] = environmentBin + ":/usr/local/bin:/usr/bin:/bin"
		}
	}

	// Add the proxy socket bind mount. The proxy runs outside the sandbox
	// and listens on a host socket; the bind mount makes it accessible at
	// /run/bureau/proxy.sock inside the sandbox.
	profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
		Source: proxySocketPath,
		Dest:   "/run/bureau/proxy.sock",
		Mode:   sandbox.MountModeRW,
	})

	profile.CreateDirs = spec.CreateDirs

	return profile
}

// writePayloadFile marshals the payload map to JSON and writes it to a
// file in the config directory. Returns the path to the written file.
// The caller should add a bind mount for this file to make it accessible
// inside the sandbox at /run/bureau/payload.json.
func writePayloadFile(configDir string, payload map[string]any) (string, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}

	payloadPath := filepath.Join(configDir, "payload.json")
	if err := os.WriteFile(payloadPath, payloadJSON, 0644); err != nil {
		return "", fmt.Errorf("writing payload file: %w", err)
	}

	return payloadPath, nil
}

// writeTriggerFile writes the trigger event content to trigger.json in the
// config directory. The content is pre-serialized JSON bytes passed through
// the IPC layer without parsing. Returns the path to the written file. The
// caller adds a bind mount for this file at /run/bureau/trigger.json inside
// the sandbox.
func writeTriggerFile(configDir string, content []byte) (string, error) {
	triggerPath := filepath.Join(configDir, "trigger.json")
	if err := os.WriteFile(triggerPath, content, 0644); err != nil {
		return "", fmt.Errorf("writing trigger file: %w", err)
	}
	return triggerPath, nil
}

// sandboxCaptureConfig holds the parameters for bureau-log-relay's capture
// mode. When passed to writeSandboxScript, the relay interposes a PTY
// between the sandboxed process and tmux, tees all output to the
// telemetry relay as OutputDelta CBOR messages, and passes stdin through
// transparently. When nil, the relay runs in passthrough mode.
type sandboxCaptureConfig struct {
	relaySocketPath string // host path to the telemetry relay Unix socket
	tokenPath       string // host path to the service token file for relay auth
	fleet           string // MarshalText format (e.g., "#bureau/fleet/prod:bureau.local")
	machine         string // MarshalText format (e.g., "@bureau/fleet/prod/machine/workstation:bureau.local")
	source          string // MarshalText format (e.g., "@bureau/fleet/prod/agent/code-reviewer:bureau.local")
	sessionID       string // unique per sandbox invocation
}

// writeSandboxScript writes a shell script that exec's bureau-log-relay
// wrapping the bwrap command. Using a script avoids shell escaping issues
// when passing bwrap args through tmux's new-session command parser.
//
// The exec replaces the shell with bureau-log-relay, making it the tmux
// pane process (visible via #{pane_pid}). The log relay runs bwrap as its
// child, holding the outer PTY file descriptors open until it collects
// the child's exit code via waitpid. This eliminates the tmux 3.4+ race
// between PTY EOF detection and SIGCHLD processing that causes exit codes
// to be lost when bwrap is exec'd directly.
//
// When exitCodeFilePath is non-empty, the relay writes the child's exit
// code to that path (atomic temp+rename) after child.Wait() but before
// os.Exit(). The launcher's session watcher reads this via inotify,
// bypassing tmux's #{pane_dead_status} race entirely.
//
// When capture is non-nil, the relay runs in capture mode: PTY
// interposition, output delta streaming to the telemetry relay. The
// capture flags are emitted before the exit-code-file flag in the
// generated script.
//
// Process tree (passthrough):
//
//	tmux pane → bureau-log-relay (holds PTY fds) → bwrap → sandboxed process
//
// Process tree (capture):
//
//	tmux pane → bureau-log-relay (PTY interposition) → bwrap → sandboxed process
//
// Signal delivery: SIGTERM sent to the pane PID reaches bureau-log-relay,
// which forwards it to bwrap, which forwards it to the sandboxed process
// through its PID-1 signal helper. This is the signal path for graceful drain.
//
// Returns the script path.
func writeSandboxScript(configDir string, logRelayPath string, exitCodeFilePath string, capture *sandboxCaptureConfig, bwrapPath string, bwrapArgs []string) (string, error) {
	scriptPath := filepath.Join(configDir, "sandbox.sh")

	var script strings.Builder
	script.WriteString("#!/bin/sh\nexec ")
	script.WriteString(shellQuote(logRelayPath))
	if capture != nil {
		script.WriteString(" --relay=")
		script.WriteString(shellQuote(capture.relaySocketPath))
		script.WriteString(" --token=")
		script.WriteString(shellQuote(capture.tokenPath))
		script.WriteString(" --fleet=")
		script.WriteString(shellQuote(capture.fleet))
		script.WriteString(" --machine=")
		script.WriteString(shellQuote(capture.machine))
		script.WriteString(" --source=")
		script.WriteString(shellQuote(capture.source))
		script.WriteString(" --session-id=")
		script.WriteString(shellQuote(capture.sessionID))
	}
	if exitCodeFilePath != "" {
		script.WriteString(" --exit-code-file=")
		script.WriteString(shellQuote(exitCodeFilePath))
	}
	script.WriteString(" -- ")
	script.WriteString(shellQuote(bwrapPath))
	for _, arg := range bwrapArgs {
		script.WriteString(" ")
		script.WriteString(shellQuote(arg))
	}
	script.WriteString("\n")

	if err := os.WriteFile(scriptPath, []byte(script.String()), 0755); err != nil {
		return "", fmt.Errorf("writing sandbox script: %w", err)
	}

	return scriptPath, nil
}

// shellQuote returns a shell-safe quoted version of a string. If the string
// contains no shell metacharacters, it's returned as-is. Otherwise, it's
// wrapped in single quotes with internal single quotes escaped.
func shellQuote(s string) string {
	// Safe characters that don't need quoting.
	safe := true
	for _, char := range s {
		if !isShellSafe(char) {
			safe = false
			break
		}
	}
	if safe && s != "" {
		return s
	}

	// Single-quote the string, escaping any internal single quotes.
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// isShellSafe returns true if the character doesn't need shell quoting.
func isShellSafe(char rune) bool {
	if char >= 'a' && char <= 'z' {
		return true
	}
	if char >= 'A' && char <= 'Z' {
		return true
	}
	if char >= '0' && char <= '9' {
		return true
	}
	switch char {
	case '-', '_', '.', '/', ':', '=', '+', ',', '@':
		return true
	}
	return false
}
