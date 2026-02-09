// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// ProfileLoader loads and resolves sandbox profiles.
type ProfileLoader struct {
	configs  []*ProfilesConfig
	resolved map[string]*Profile
	logger   *slog.Logger
}

// NewProfileLoader creates a new profile loader.
func NewProfileLoader() *ProfileLoader {
	return &ProfileLoader{
		configs:  make([]*ProfilesConfig, 0),
		resolved: make(map[string]*Profile),
	}
}

// SetLogger enables verbose logging during profile loading.
// When set, the loader logs details about which files are checked,
// which profiles are loaded, and inheritance resolution.
func (l *ProfileLoader) SetLogger(logger *slog.Logger) {
	l.logger = logger
}

// log is a helper that only logs if a logger is configured.
func (l *ProfileLoader) log(msg string, args ...any) {
	if l.logger != nil {
		l.logger.Info(msg, args...)
	}
}

// LoadDefaults loads the built-in default profiles.
func (l *ProfileLoader) LoadDefaults() error {
	l.log("loading built-in default profiles")
	config, err := ParseProfilesConfig([]byte(defaultProfilesYAML))
	if err != nil {
		return fmt.Errorf("failed to parse default profiles: %w", err)
	}
	l.configs = append(l.configs, config)
	l.log("loaded default profiles", "count", len(config.Profiles))
	return nil
}

// LoadFile loads profiles from a YAML file.
func (l *ProfileLoader) LoadFile(path string) error {
	l.log("loading profiles from file", "path", path)
	config, err := LoadProfilesConfig(path)
	if err != nil {
		l.log("failed to load profiles", "path", path, "error", err)
		return err
	}
	l.configs = append(l.configs, config)
	l.log("loaded profiles from file", "path", path, "count", len(config.Profiles))
	return nil
}

// LoadDirectory loads all YAML files from a directory.
func (l *ProfileLoader) LoadDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Directory doesn't exist - not an error.
		}
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml" {
			continue
		}
		path := filepath.Join(dir, name)
		if err := l.LoadFile(path); err != nil {
			return fmt.Errorf("failed to load %s: %w", path, err)
		}
	}

	return nil
}

// Resolve resolves a profile by name, applying inheritance.
// Later-loaded configs override earlier ones.
func (l *ProfileLoader) Resolve(name string) (*Profile, error) {
	l.log("resolving profile", "name", name)

	// Check cache.
	if profile, ok := l.resolved[name]; ok {
		l.log("profile found in cache", "name", name)
		return profile, nil
	}

	// Find profile in configs (last one wins).
	var baseProfile *Profile
	for _, config := range l.configs {
		if profile, ok := config.Profiles[name]; ok {
			baseProfile = profile
		}
	}

	if baseProfile == nil {
		l.log("profile not found", "name", name)
		return nil, fmt.Errorf("profile not found: %s", name)
	}
	l.log("found profile definition", "name", name)

	// Resolve inheritance.
	var profile *Profile
	if baseProfile.Inherit != "" {
		l.log("resolving parent profile", "child", name, "parent", baseProfile.Inherit)
		parent, err := l.Resolve(baseProfile.Inherit)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve parent profile %q: %w", baseProfile.Inherit, err)
		}
		l.log("merging parent into child", "child", name, "parent", baseProfile.Inherit)
		profile = mergeProfiles(parent, baseProfile)
	} else {
		profile = baseProfile.Clone()
	}

	// Cache resolved profile.
	l.resolved[name] = profile
	l.log("profile resolved",
		"name", name,
		"mounts", len(profile.Filesystem),
		"env_vars", len(profile.Environment),
	)
	return profile, nil
}

// List returns all available profile names.
func (l *ProfileLoader) List() []string {
	names := make(map[string]bool)
	for _, config := range l.configs {
		for name := range config.Profiles {
			names[name] = true
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// ConfigSearchPaths returns the paths to search for profile configs.
func ConfigSearchPaths() []string {
	paths := []string{}

	// User config directory.
	if configDir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(configDir, "bureau", "sandbox-profiles.yaml"))
	}

	// XDG config home.
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		paths = append(paths, filepath.Join(xdgConfig, "bureau", "sandbox-profiles.yaml"))
	}

	// Home directory.
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "bureau", "sandbox-profiles.yaml"))
	}

	// System config.
	paths = append(paths, "/etc/bureau/sandbox-profiles.yaml")

	// Bureau config directory (when running from repo).
	if cwd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(cwd, "config", "sandbox-profiles.yaml"))
	}

	return paths
}

// LoadFromSearchPaths creates a loader and loads profiles from standard locations.
func LoadFromSearchPaths() (*ProfileLoader, error) {
	return LoadFromSearchPathsWithLogger(nil)
}

// LoadFromSearchPathsWithLogger creates a loader with optional verbose logging.
func LoadFromSearchPathsWithLogger(logger *slog.Logger) (*ProfileLoader, error) {
	loader := NewProfileLoader()
	loader.SetLogger(logger)

	// Load defaults first.
	if err := loader.LoadDefaults(); err != nil {
		return nil, err
	}

	// Load from search paths (files that exist).
	for _, path := range ConfigSearchPaths() {
		if _, err := os.Stat(path); err == nil {
			if err := loader.LoadFile(path); err != nil {
				return nil, fmt.Errorf("failed to load %s: %w", path, err)
			}
		} else {
			loader.log("profile config not found", "path", path)
		}
	}

	return loader, nil
}

// defaultProfilesYAML contains the built-in profile definitions.
const defaultProfilesYAML = `
profiles:
  developer:
    description: "Full development access to worktree"

    filesystem:
      - source: ${WORKTREE}
        dest: /workspace
        mode: rw
      - type: tmpfs
        dest: /tmp
        options: "size=1G"
      - source: /usr
        dest: /usr
        mode: ro
      - source: /bin
        dest: /bin
        mode: ro
      - source: /lib
        dest: /lib
        mode: ro
      - source: /lib64
        dest: /lib64
        mode: ro
        optional: true
      - source: /etc/resolv.conf
        dest: /etc/resolv.conf
        mode: ro
        optional: true
      - source: /etc/ssl
        dest: /etc/ssl
        mode: ro
        optional: true
      - source: /etc/ca-certificates
        dest: /etc/ca-certificates
        mode: ro
        optional: true
      - source: /etc/passwd
        dest: /etc/passwd
        mode: ro
      - source: /etc/group
        dest: /etc/group
        mode: ro
      - source: /etc/alternatives
        dest: /etc/alternatives
        mode: ro
        optional: true
      # Nix store - allows nix-provided tools to work inside sandbox
      # Required when /workspace/bin contains symlinks to /nix/store paths
      - source: /nix
        dest: /nix
        mode: ro
        optional: true
      - source: ${PROXY_SOCKET}
        dest: /run/bureau/proxy.sock
        mode: rw
        optional: true
      # Pre-commit cache - share host's pre-built hook environments
      # Uses overlay mount: reads from host cache, writes to tmpfs
      # MUST mount at same path as host because pre-commit stores absolute paths in db.db
      - type: overlay
        source: ${HOME}/.cache/pre-commit
        dest: ${HOME}/.cache/pre-commit
        optional: true

    namespaces:
      pid: true
      net: true
      ipc: true
      uts: true
      cgroup: false

    environment:
      PATH: "/workspace/bin:/usr/local/bin:/usr/bin:/bin"
      HOME: "/workspace"
      TERM: "${TERM}"
      BUREAU_PROXY_SOCKET: "/run/bureau/proxy.sock"
      BUREAU_SANDBOX: "1"
      # Pre-commit cache is mounted at host path (not /workspace) because
      # its db.db contains hardcoded absolute paths. Tell pre-commit where.
      PRE_COMMIT_HOME: "${HOME}/.cache/pre-commit"

    resources:
      tasks_max: 0
      memory_max: ""
      cpu_quota: ""

    security:
      new_session: true
      die_with_parent: true
      no_new_privs: true

    create_dirs:
      - /tmp
      - /var/tmp
      - /run/bureau

  developer-gpu:
    description: "Development with GPU access for ML/graphics"
    inherit: developer

    filesystem:
      - source: /dev/dri
        dest: /dev/dri
        type: dev-bind
        optional: true
      - source: /usr/share/vulkan
        dest: /usr/share/vulkan
        mode: ro
        optional: true
      - source: /etc/vulkan
        dest: /etc/vulkan
        mode: ro
        optional: true
      - source: /usr/lib/x86_64-linux-gnu/dri
        dest: /usr/lib/x86_64-linux-gnu/dri
        mode: ro
        optional: true
      - source: /opt/rocm
        dest: /opt/rocm
        mode: ro
        optional: true

    environment:
      VK_ICD_FILENAMES: ""
      HIP_PATH: "/opt/rocm"

  developer-network:
    description: "Development with network access (credentials via proxy)"
    inherit: developer

    namespaces:
      pid: true
      net: false
      ipc: true
      uts: true
      cgroup: false

  assistant:
    description: "API-only access, minimal filesystem"
    inherit: developer

    filesystem:
      - source: ${WORKTREE}
        dest: /workspace
        mode: ro

    resources:
      tasks_max: 50
      memory_max: "2G"
      cpu_quota: "100%"

  readonly:
    description: "Read-only analysis and review"
    inherit: developer

    filesystem:
      - source: ${WORKTREE}
        dest: /workspace
        mode: ro

  unrestricted:
    description: "Trusted internal tools with full worktree access"
    inherit: developer

    resources:
      tasks_max: 0
      memory_max: ""
      cpu_quota: ""
`
