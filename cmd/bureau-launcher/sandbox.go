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
			Mode:     mount.Mode,
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

// writeSandboxScript writes a shell script that exec's the bwrap command.
// Using a script avoids shell escaping issues when passing bwrap args
// through tmux's new-session command parser. Returns the script path.
func writeSandboxScript(configDir string, bwrapPath string, bwrapArgs []string) (string, error) {
	scriptPath := filepath.Join(configDir, "sandbox.sh")

	var script strings.Builder
	script.WriteString("#!/bin/sh\nexec ")
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
