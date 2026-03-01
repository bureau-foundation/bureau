// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// pluginExecFunc replaces the current process with a plugin binary.
// Defaults to syscall.Exec. Tests override this to capture the exec
// call instead of actually replacing the process.
var pluginExecFunc = syscall.Exec

// tryPluginDispatch searches for a bureau-<name> binary and execs it
// with Bureau context environment variables injected. Returns nil if
// no plugin binary is found (the caller should fall through to
// suggestion logic). Returns a non-nil error if the binary was found
// but exec failed.
//
// Search order:
//  1. The directory containing the current bureau binary (os.Executable).
//     This handles the production case where all Bureau binaries are
//     installed in the same bin/ directory (Nix, script/dev-env).
//  2. PATH (exec.LookPath). This handles third-party plugins installed
//     anywhere on the system.
//
// Plugin dispatch only fires at the root command level (c.parent == nil).
// This matches git's model: `git <name>` finds `git-<name>`, but
// `git remote <name>` does not look for `git-remote-<name>`.
//
// The plugin binary receives:
//   - argv[0]: absolute path to the plugin binary
//   - argv[1:]: remaining CLI arguments after the plugin name
//   - environment: caller's environment merged with Bureau context vars
//
// See [buildPluginEnv] for the environment variable contract.
func (c *Command) tryPluginDispatch(name string, args []string) error {
	if c.parent != nil {
		return nil
	}

	pluginBinary := "bureau-" + name
	pluginPath := findPlugin(pluginBinary)
	if pluginPath == "" {
		return nil
	}

	env := buildPluginEnv()
	argv := make([]string, 0, 1+len(args))
	argv = append(argv, pluginPath)
	argv = append(argv, args...)

	execErr := pluginExecFunc(pluginPath, argv, env)

	// If we reach here, exec failed. This is always an error — the
	// binary exists but couldn't be executed (permissions, missing
	// interpreter, etc.).
	return Transient("exec plugin %q at %s: %w", pluginBinary, pluginPath, execErr).
		WithHint("The plugin binary was found but could not be executed. " +
			"Check that it is executable and has a valid interpreter.")
}

// findPlugin searches for a plugin binary by name. Returns the absolute
// path if found, or empty string if not found.
//
// Search order: directory of current executable first (companion binary
// pattern used by the launcher), then PATH.
func findPlugin(name string) string {
	// Check next to the current binary. In production (Nix) and
	// script/dev-env, all Bureau binaries share a directory.
	if selfPath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(selfPath), name)
		if fileExists(candidate) {
			return candidate
		}
	}

	// Fall back to PATH search.
	if path, err := exec.LookPath(name); err == nil {
		return path
	}

	return ""
}

// buildPluginEnv constructs the environment for a plugin binary. Starts
// with the caller's full environment, then adds Bureau context variables
// that aren't already set. Existing variables take precedence — if
// someone exports BUREAU_SERVER_NAME before running bureau, the plugin
// sees their value, not the one from machine.conf.
//
// # Environment variable contract
//
// Always set:
//   - BUREAU_PLUGIN=1 — marker that this process was dispatched as a plugin
//
// Set from machine.conf (if available, not already in environment):
//   - BUREAU_SERVER_NAME — Matrix server name
//   - BUREAU_FLEET — fleet localpart
//   - BUREAU_HOMESERVER_URL — Matrix homeserver URL
//   - BUREAU_MACHINE_NAME — machine localpart
//   - BUREAU_MACHINE_CONF — path to machine.conf
//
// Set from session/config resolution (not already in environment):
//   - BUREAU_SESSION_FILE — resolved path to operator session.json
//   - BUREAU_CONFIG_DIR — Bureau config directory (~/.config/bureau)
//
// Not set by dispatch (plugins resolve these themselves if needed):
//   - BUREAU_PROXY_SOCKET — only meaningful inside sandboxes
//   - BUREAU_*_SOCKET / BUREAU_*_TOKEN — per-service, resolved by
//     [ServiceConnection] in Go plugins or read directly by others
func buildPluginEnv() []string {
	env := os.Environ()
	present := envSet(env)

	// Always set the plugin marker, even if already present.
	env = setEnv(env, "BUREAU_PLUGIN", "1")

	// Session file path — resolve the default if not already set.
	if !present["BUREAU_SESSION_FILE"] {
		env = setEnv(env, "BUREAU_SESSION_FILE", SessionFilePath())
	}

	// Config directory — the parent of the session file.
	if !present["BUREAU_CONFIG_DIR"] {
		configDirectory := os.Getenv("XDG_CONFIG_HOME")
		if configDirectory == "" {
			homeDirectory, err := os.UserHomeDir()
			if err == nil {
				configDirectory = filepath.Join(homeDirectory, ".config")
			}
		}
		if configDirectory != "" {
			env = setEnv(env, "BUREAU_CONFIG_DIR", filepath.Join(configDirectory, "bureau"))
		}
	}

	// Machine conf path.
	if !present["BUREAU_MACHINE_CONF"] {
		confPath := MachineConfPath()
		if confPath != DefaultMachineConfPath || fileExists(confPath) {
			env = setEnv(env, "BUREAU_MACHINE_CONF", confPath)
		}
	}

	// Machine context from machine.conf. LoadMachineConf returns
	// zero values if the file doesn't exist, which is fine — we
	// only inject non-empty values.
	conf := LoadMachineConf()
	if !present["BUREAU_SERVER_NAME"] && conf.ServerName != "" {
		env = setEnv(env, "BUREAU_SERVER_NAME", conf.ServerName)
	}
	if !present["BUREAU_FLEET"] && conf.Fleet != "" {
		env = setEnv(env, "BUREAU_FLEET", conf.Fleet)
	}
	if !present["BUREAU_HOMESERVER_URL"] && conf.HomeserverURL != "" {
		env = setEnv(env, "BUREAU_HOMESERVER_URL", conf.HomeserverURL)
	}
	if !present["BUREAU_MACHINE_NAME"] && conf.MachineName != "" {
		env = setEnv(env, "BUREAU_MACHINE_NAME", conf.MachineName)
	}

	return env
}

// envSet returns a set of environment variable names present in env.
func envSet(env []string) map[string]bool {
	result := make(map[string]bool, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if found {
			result[key] = true
		}
	}
	return result
}

// setEnv appends or replaces an environment variable in env.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for index, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[index] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
