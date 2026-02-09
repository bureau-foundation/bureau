// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package config provides configuration loading for Bureau components.
//
// Configuration is loaded from a single file specified by:
//   - BUREAU_CONFIG environment variable, or
//   - --config flag passed to the command
//
// There are no fallbacks or automatic discovery. This ensures deterministic,
// auditable configuration with no hidden overrides.
//
// The config file may contain environment-specific sections (development,
// staging, production) that override base values when the environment matches.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Environment represents the deployment environment.
type Environment string

const (
	// Development is for local development machines.
	Development Environment = "development"
	// Staging is for pre-production testing.
	Staging Environment = "staging"
	// Production is for production deployments.
	Production Environment = "production"
)

// Config is the master configuration for Bureau.
type Config struct {
	// Environment identifies the deployment type (development, staging, production).
	Environment Environment `yaml:"environment"`

	// Paths configures directory locations.
	Paths PathsConfig `yaml:"paths"`

	// Proxy configures the credential proxy.
	Proxy ProxyConfig `yaml:"proxy"`

	// Sandbox configures the execution sandbox.
	Sandbox SandboxConfig `yaml:"sandbox"`

	// Launcher configures the agent launcher.
	Launcher LauncherConfig `yaml:"launcher"`

	// EnvironmentOverrides contains per-environment overrides.
	// These are applied after the base config is loaded.
	Development *ConfigOverrides `yaml:"development,omitempty"`
	Staging     *ConfigOverrides `yaml:"staging,omitempty"`
	Production  *ConfigOverrides `yaml:"production,omitempty"`
}

// ConfigOverrides contains fields that can be overridden per environment.
type ConfigOverrides struct {
	Paths    *PathsConfig    `yaml:"paths,omitempty"`
	Proxy    *ProxyConfig    `yaml:"proxy,omitempty"`
	Sandbox  *SandboxConfig  `yaml:"sandbox,omitempty"`
	Launcher *LauncherConfig `yaml:"launcher,omitempty"`
}

// PathsConfig configures directory locations.
type PathsConfig struct {
	// Root is the base directory for Bureau data.
	Root string `yaml:"root"`

	// Bin is where Bureau binaries are installed.
	// This provides hermetic binary paths independent of user PATH.
	// Contains: bureau-sandbox, bureau-bridge, etc.
	Bin string `yaml:"bin"`

	// Worktrees is where agent worktrees are created.
	Worktrees string `yaml:"worktrees"`

	// Archives is where completed worktrees are archived.
	Archives string `yaml:"archives"`

	// State is where runtime state is stored.
	State string `yaml:"state"`

	// AgentTypes is the directory containing agent type manifests.
	AgentTypes string `yaml:"agent_types"`
}

// ProxyConfig configures the credential proxy.
type ProxyConfig struct {
	// SocketPath is the Unix socket path for the proxy.
	// Default: /run/bureau/proxy.sock
	SocketPath string `yaml:"socket_path"`

	// ConfigFile is the path to the proxy configuration file.
	// Default: /etc/bureau/proxy.yaml
	ConfigFile string `yaml:"config_file"`

	// AutoStart enables automatic proxy startup if not running.
	// Default: true (development), false (production)
	AutoStart bool `yaml:"auto_start"`

	// CredentialPrefix is the environment variable prefix for credentials.
	// Default: BUREAU_
	CredentialPrefix string `yaml:"credential_prefix"`
}

// SandboxConfig configures the execution sandbox.
type SandboxConfig struct {
	// DefaultProfile is the sandbox profile used when none is specified.
	// Default: developer
	DefaultProfile string `yaml:"default_profile"`

	// ProfilesFile is the path to sandbox profiles configuration.
	// Default: /etc/bureau/sandbox-profiles.yaml (or embedded defaults)
	ProfilesFile string `yaml:"profiles_file"`

	// Fallback configures behavior when sandbox capabilities are unavailable.
	Fallback FallbackConfig `yaml:"fallback"`
}

// FallbackConfig configures graceful degradation when capabilities are missing.
type FallbackConfig struct {
	// NoUserns specifies behavior when user namespaces are unavailable.
	// Values: "skip" (continue without), "warn" (warn and continue), "error" (fail)
	// Default: skip (development), error (production)
	NoUserns string `yaml:"no_userns"`

	// NoBwrap specifies behavior when bubblewrap is unavailable.
	// Values: "skip", "warn", "error"
	// Default: error (all environments)
	NoBwrap string `yaml:"no_bwrap"`

	// NoSystemd specifies behavior when systemd is unavailable.
	// Values: "skip", "warn", "error"
	// Default: skip (all environments)
	NoSystemd string `yaml:"no_systemd"`
}

// LauncherConfig configures the agent launcher.
type LauncherConfig struct {
	// DefaultShell is the shell to launch in the sandbox.
	// Default: /bin/bash
	DefaultShell string `yaml:"default_shell"`

	// StartupTimeout is how long to wait for services to start.
	// Default: 30s
	StartupTimeout string `yaml:"startup_timeout"`

	// WorktreeScript is the path to bureau-worktree-init.
	// Default: bureau-worktree-init (found in PATH)
	WorktreeScript string `yaml:"worktree_script"`
}

// Default returns the default configuration.
// These defaults are used as a base before loading the config file.
// They exist primarily to ensure all fields have sensible zero-values,
// not as a fallback - the config file is required.
func Default() *Config {
	homeDir, _ := os.UserHomeDir()
	defaultRoot := filepath.Join(homeDir, ".cache", "bureau")

	return &Config{
		Environment: Development,
		Paths: PathsConfig{
			Root:       defaultRoot,
			Bin:        filepath.Join(defaultRoot, "bin"),
			Worktrees:  filepath.Join(defaultRoot, "worktrees"),
			Archives:   filepath.Join(defaultRoot, "archives"),
			State:      filepath.Join(defaultRoot, "state"),
			AgentTypes: filepath.Join(defaultRoot, "agent-types"),
		},
		Proxy: ProxyConfig{
			SocketPath:       "/run/bureau/proxy.sock",
			ConfigFile:       "/etc/bureau/proxy.yaml",
			AutoStart:        true,
			CredentialPrefix: "BUREAU_",
		},
		Sandbox: SandboxConfig{
			DefaultProfile: "developer",
			ProfilesFile:   "",
			Fallback: FallbackConfig{
				NoUserns:  "skip",
				NoBwrap:   "error",
				NoSystemd: "skip",
			},
		},
		Launcher: LauncherConfig{
			DefaultShell:   "/bin/bash",
			StartupTimeout: "30s",
			WorktreeScript: "bureau-worktree-init",
		},
	}
}

// Load loads configuration from BUREAU_CONFIG environment variable.
//
// This is the only way to load configuration without an explicit path.
// There are no fallbacks or defaults - if BUREAU_CONFIG is not set, this fails.
// This ensures deterministic, auditable configuration with no hidden overrides.
func Load() (*Config, error) {
	configPath := os.Getenv("BUREAU_CONFIG")
	if configPath == "" {
		return nil, fmt.Errorf("BUREAU_CONFIG environment variable not set; " +
			"set it to the path of your bureau.yaml config file, or use --config flag")
	}

	return LoadFile(configPath)
}

// LoadFile loads configuration from a specific file path.
//
// The config file is the single source of truth. Environment variables do not
// override config values - this ensures deterministic, auditable configuration.
// The only expansion performed is ${HOME} and similar path variables for portability.
func LoadFile(path string) (*Config, error) {
	cfg := Default()

	if err := cfg.loadFile(path); err != nil {
		return nil, err
	}

	// Apply environment-specific overrides (development/staging/production sections in the file).
	cfg.applyEnvironmentOverrides()

	// Expand ${HOME} and similar variables in paths for portability.
	cfg.expandVariables()

	return cfg, nil
}

// loadFile loads a single configuration file, merging into the current config.
func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return yaml.Unmarshal(data, c)
}

// applyEnvironmentOverrides applies the environment-specific overrides.
func (c *Config) applyEnvironmentOverrides() {
	var overrides *ConfigOverrides

	switch c.Environment {
	case Development:
		overrides = c.Development
	case Staging:
		overrides = c.Staging
	case Production:
		overrides = c.Production
		// Production defaults: stricter fallback behavior.
		if overrides == nil {
			overrides = &ConfigOverrides{
				Proxy: &ProxyConfig{
					AutoStart: false,
				},
				Sandbox: &SandboxConfig{
					DefaultProfile: "assistant",
					Fallback: FallbackConfig{
						NoUserns: "error",
					},
				},
			}
		}
	}

	if overrides == nil {
		return
	}

	if overrides.Paths != nil {
		if overrides.Paths.Root != "" {
			c.Paths.Root = overrides.Paths.Root
		}
		if overrides.Paths.Bin != "" {
			c.Paths.Bin = overrides.Paths.Bin
		}
		if overrides.Paths.Worktrees != "" {
			c.Paths.Worktrees = overrides.Paths.Worktrees
		}
		if overrides.Paths.Archives != "" {
			c.Paths.Archives = overrides.Paths.Archives
		}
		if overrides.Paths.State != "" {
			c.Paths.State = overrides.Paths.State
		}
	}

	if overrides.Proxy != nil {
		if overrides.Proxy.SocketPath != "" {
			c.Proxy.SocketPath = overrides.Proxy.SocketPath
		}
		if overrides.Proxy.ConfigFile != "" {
			c.Proxy.ConfigFile = overrides.Proxy.ConfigFile
		}
		// AutoStart is a bool, so we always apply it from overrides.
		c.Proxy.AutoStart = overrides.Proxy.AutoStart
		if overrides.Proxy.CredentialPrefix != "" {
			c.Proxy.CredentialPrefix = overrides.Proxy.CredentialPrefix
		}
	}

	if overrides.Sandbox != nil {
		if overrides.Sandbox.DefaultProfile != "" {
			c.Sandbox.DefaultProfile = overrides.Sandbox.DefaultProfile
		}
		if overrides.Sandbox.ProfilesFile != "" {
			c.Sandbox.ProfilesFile = overrides.Sandbox.ProfilesFile
		}
		if overrides.Sandbox.Fallback.NoUserns != "" {
			c.Sandbox.Fallback.NoUserns = overrides.Sandbox.Fallback.NoUserns
		}
		if overrides.Sandbox.Fallback.NoBwrap != "" {
			c.Sandbox.Fallback.NoBwrap = overrides.Sandbox.Fallback.NoBwrap
		}
		if overrides.Sandbox.Fallback.NoSystemd != "" {
			c.Sandbox.Fallback.NoSystemd = overrides.Sandbox.Fallback.NoSystemd
		}
	}

	if overrides.Launcher != nil {
		if overrides.Launcher.DefaultShell != "" {
			c.Launcher.DefaultShell = overrides.Launcher.DefaultShell
		}
		if overrides.Launcher.StartupTimeout != "" {
			c.Launcher.StartupTimeout = overrides.Launcher.StartupTimeout
		}
		if overrides.Launcher.WorktreeScript != "" {
			c.Launcher.WorktreeScript = overrides.Launcher.WorktreeScript
		}
	}
}

// expandVariables expands ${VAR} and ${VAR:-default} patterns in paths.
func (c *Config) expandVariables() {
	vars := map[string]string{
		"BUREAU_ROOT": c.Paths.Root,
		"HOME":        os.Getenv("HOME"),
	}

	c.Paths.Root = expandVars(c.Paths.Root, vars)
	vars["BUREAU_ROOT"] = c.Paths.Root // Update for dependent paths.

	c.Paths.Bin = expandVars(c.Paths.Bin, vars)
	c.Paths.Worktrees = expandVars(c.Paths.Worktrees, vars)
	c.Paths.Archives = expandVars(c.Paths.Archives, vars)
	c.Paths.State = expandVars(c.Paths.State, vars)
	c.Paths.AgentTypes = expandVars(c.Paths.AgentTypes, vars)
	c.Proxy.SocketPath = expandVars(c.Proxy.SocketPath, vars)
	c.Proxy.ConfigFile = expandVars(c.Proxy.ConfigFile, vars)
	c.Sandbox.ProfilesFile = expandVars(c.Sandbox.ProfilesFile, vars)
}

// expandVars expands ${VAR} and ${VAR:-default} patterns.
var varPattern = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

func expandVars(s string, vars map[string]string) string {
	return varPattern.ReplaceAllStringFunc(s, func(match string) string {
		parts := varPattern.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}

		name := parts[1]
		defaultValue := ""
		if len(parts) >= 3 {
			defaultValue = parts[2]
		}

		// Check provided vars first, then environment.
		if value, ok := vars[name]; ok && value != "" {
			return value
		}
		if value := os.Getenv(name); value != "" {
			return value
		}
		return defaultValue
	})
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error

	if c.Environment != Development && c.Environment != Staging && c.Environment != Production {
		errs = append(errs, fmt.Errorf("invalid environment: %s", c.Environment))
	}

	if c.Paths.Root == "" {
		errs = append(errs, fmt.Errorf("paths.root is required"))
	}

	if c.Proxy.SocketPath == "" {
		errs = append(errs, fmt.Errorf("proxy.socket_path is required"))
	}

	if c.Sandbox.DefaultProfile == "" {
		errs = append(errs, fmt.Errorf("sandbox.default_profile is required"))
	}

	fallbackValues := []string{"skip", "warn", "error"}
	if !contains(fallbackValues, c.Sandbox.Fallback.NoUserns) {
		errs = append(errs, fmt.Errorf("sandbox.fallback.no_userns must be one of: %v", fallbackValues))
	}
	if !contains(fallbackValues, c.Sandbox.Fallback.NoBwrap) {
		errs = append(errs, fmt.Errorf("sandbox.fallback.no_bwrap must be one of: %v", fallbackValues))
	}
	if !contains(fallbackValues, c.Sandbox.Fallback.NoSystemd) {
		errs = append(errs, fmt.Errorf("sandbox.fallback.no_systemd must be one of: %v", fallbackValues))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// HasSystemd returns true if systemd is available on this system.
func (c *Config) HasSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// EnsurePaths creates all configured directories if they don't exist.
func (c *Config) EnsurePaths() error {
	paths := []string{
		c.Paths.Root,
		c.Paths.Bin,
		c.Paths.Worktrees,
		c.Paths.Archives,
		c.Paths.State,
	}

	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
	}

	return nil
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// BinaryPath returns the full path to a Bureau binary.
// It looks in Paths.Bin first, then falls back to exec.LookPath.
// This provides hermetic binary resolution when Bin is configured.
func (c *Config) BinaryPath(name string) (string, error) {
	// If Bin is configured, look there first.
	if c.Paths.Bin != "" {
		binPath := filepath.Join(c.Paths.Bin, name)
		if _, err := os.Stat(binPath); err == nil {
			return binPath, nil
		}
	}

	// Fall back to PATH lookup.
	path, err := exec.LookPath(name)
	if err != nil {
		if c.Paths.Bin != "" {
			return "", fmt.Errorf("%s not found in %s or PATH", name, c.Paths.Bin)
		}
		return "", fmt.Errorf("%s not found in PATH", name)
	}
	return path, nil
}
