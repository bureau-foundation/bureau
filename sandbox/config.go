// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Profile defines the sandbox configuration for a particular role.
type Profile struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Inherit     string            `yaml:"inherit,omitempty"`
	Filesystem  []Mount           `yaml:"filesystem,omitempty"`
	Namespaces  NamespaceConfig   `yaml:"namespaces,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Resources   ResourceConfig    `yaml:"resources,omitempty"`
	Security    SecurityConfig    `yaml:"security,omitempty"`
	CreateDirs  []string          `yaml:"create_dirs,omitempty"`
}

// Mount defines a filesystem mount in the sandbox.
type Mount struct {
	Source   string `yaml:"source,omitempty"`
	Dest     string `yaml:"dest"`
	Mode     string `yaml:"mode,omitempty"`
	Type     string `yaml:"type,omitempty"`
	Options  string `yaml:"options,omitempty"`
	Optional bool   `yaml:"optional,omitempty"`
	Glob     bool   `yaml:"glob,omitempty"`
	// Upper specifies the upper layer for overlay mounts.
	// Must be "tmpfs" (default) or a path inside the worktree.
	// Only used when Type is "overlay".
	Upper string `yaml:"upper,omitempty"`
}

// MountType constants for the Type field.
const (
	MountTypeBind    = ""         // Default: bind mount
	MountTypeTmpfs   = "tmpfs"    // tmpfs mount
	MountTypeProc    = "proc"     // /proc
	MountTypeDev     = "dev"      // /dev (minimal)
	MountTypeDevBind = "dev-bind" // Device node bind
	MountTypeOverlay = "overlay"  // Overlay mount (fuse-overlayfs)
)

// OverlayUpperTmpfs is the special value for overlay upper layer that uses tmpfs.
const OverlayUpperTmpfs = "tmpfs"

// MountMode constants for the Mode field.
const (
	MountModeRO = "ro" // Read-only
	MountModeRW = "rw" // Read-write
)

// NamespaceConfig defines which namespaces to unshare.
type NamespaceConfig struct {
	PID    bool `yaml:"pid"`
	Net    bool `yaml:"net"`
	IPC    bool `yaml:"ipc"`
	UTS    bool `yaml:"uts"`
	Cgroup bool `yaml:"cgroup"`
	User   bool `yaml:"user"`
}

// ResourceConfig defines resource limits via systemd scopes.
type ResourceConfig struct {
	TasksMax  int    `yaml:"tasks_max,omitempty"`
	MemoryMax string `yaml:"memory_max,omitempty"`
	CPUQuota  string `yaml:"cpu_quota,omitempty"`

	// CPUWeight is the cgroup v2 cpu.weight value (1–10000, default 100).
	// This controls relative CPU time under contention via the systemd
	// CPUWeight property. Zero means no limit (use cgroup default).
	CPUWeight int `yaml:"cpu_weight,omitempty"`
}

// HasLimits returns true if any resource limits are configured.
func (r ResourceConfig) HasLimits() bool {
	return r.TasksMax > 0 || r.MemoryMax != "" || r.CPUQuota != "" || r.CPUWeight > 0
}

// SecurityConfig defines security settings for the sandbox.
type SecurityConfig struct {
	NewSession    bool `yaml:"new_session"`
	DieWithParent bool `yaml:"die_with_parent"`
	NoNewPrivs    bool `yaml:"no_new_privs"`
}

// Clone creates a deep copy of the profile.
func (p *Profile) Clone() *Profile {
	clone := &Profile{
		Name:        p.Name,
		Description: p.Description,
		Inherit:     p.Inherit,
		Namespaces:  p.Namespaces,
		Resources:   p.Resources,
		Security:    p.Security,
	}

	// Deep copy slices.
	if p.Filesystem != nil {
		clone.Filesystem = make([]Mount, len(p.Filesystem))
		copy(clone.Filesystem, p.Filesystem)
	}
	if p.CreateDirs != nil {
		clone.CreateDirs = make([]string, len(p.CreateDirs))
		copy(clone.CreateDirs, p.CreateDirs)
	}

	// Deep copy maps.
	if p.Environment != nil {
		clone.Environment = make(map[string]string)
		for k, v := range p.Environment {
			clone.Environment[k] = v
		}
	}

	return clone
}

// MergeProfiles merges child profile settings into parent.
// Child settings override parent settings.
func MergeProfiles(parent, child *Profile) *Profile {
	result := parent.Clone()
	result.Name = child.Name
	result.Inherit = ""

	if child.Description != "" {
		result.Description = child.Description
	}

	// Filesystem: child completely replaces matching dest paths, adds new ones.
	if len(child.Filesystem) > 0 {
		destMap := make(map[string]Mount)
		for _, m := range result.Filesystem {
			destMap[m.Dest] = m
		}
		for _, m := range child.Filesystem {
			destMap[m.Dest] = m
		}
		result.Filesystem = make([]Mount, 0, len(destMap))
		for _, m := range destMap {
			result.Filesystem = append(result.Filesystem, m)
		}
	}

	// Namespaces: child overrides if any are set.
	if child.Namespaces != (NamespaceConfig{}) {
		result.Namespaces = child.Namespaces
	}

	// Environment: merge maps.
	if len(child.Environment) > 0 {
		if result.Environment == nil {
			result.Environment = make(map[string]string)
		}
		for k, v := range child.Environment {
			result.Environment[k] = v
		}
	}

	// Resources: merge individual fields (non-zero values from child override).
	if child.Resources.TasksMax != 0 {
		result.Resources.TasksMax = child.Resources.TasksMax
	}
	if child.Resources.MemoryMax != "" {
		result.Resources.MemoryMax = child.Resources.MemoryMax
	}
	if child.Resources.CPUQuota != "" {
		result.Resources.CPUQuota = child.Resources.CPUQuota
	}

	// Security: child overrides if any are set.
	if child.Security != (SecurityConfig{}) {
		result.Security = child.Security
	}

	// CreateDirs: merge and deduplicate.
	if len(child.CreateDirs) > 0 {
		dirSet := make(map[string]bool)
		for _, d := range result.CreateDirs {
			dirSet[d] = true
		}
		for _, d := range child.CreateDirs {
			dirSet[d] = true
		}
		result.CreateDirs = make([]string, 0, len(dirSet))
		for d := range dirSet {
			result.CreateDirs = append(result.CreateDirs, d)
		}
	}

	return result
}

// Variables holds the variable values for expansion in profiles.
type Variables map[string]string

// Expand expands variables in a string using ${VAR} syntax.
// Falls back to environment variables if not in the Variables map.
func (v Variables) Expand(s string) string {
	// Match ${VAR} patterns.
	re := regexp.MustCompile(`\$\{([^}]+)\}`)
	return re.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name.
		varName := match[2 : len(match)-1]

		// Check Variables map first.
		if val, ok := v[varName]; ok {
			return val
		}

		// Fall back to environment.
		if val := os.Getenv(varName); val != "" {
			return val
		}

		// Return original if not found.
		return match
	})
}

// ExpandProfile expands all variables in a profile.
func (v Variables) ExpandProfile(p *Profile) *Profile {
	result := p.Clone()

	// Expand filesystem sources.
	for i := range result.Filesystem {
		result.Filesystem[i].Source = v.Expand(result.Filesystem[i].Source)
		result.Filesystem[i].Dest = v.Expand(result.Filesystem[i].Dest)
	}

	// Expand environment values.
	for key, val := range result.Environment {
		result.Environment[key] = v.Expand(val)
	}

	// Expand create_dirs.
	for i := range result.CreateDirs {
		result.CreateDirs[i] = v.Expand(result.CreateDirs[i])
	}

	return result
}

// DefaultVariables returns the default variable set with common Bureau paths.
func DefaultVariables() Variables {
	bureauRoot := os.Getenv("BUREAU_ROOT")
	if bureauRoot == "" {
		bureauRoot = os.ExpandEnv("$HOME/.cache/bureau")
	}

	proxySocket := os.Getenv("BUREAU_PROXY_SOCKET")
	if proxySocket == "" {
		proxySocket = "/run/bureau/proxy.sock"
	}

	return Variables{
		"BUREAU_ROOT":  bureauRoot,
		"PROXY_SOCKET": proxySocket,
		"TERM":         os.Getenv("TERM"),
	}
}

// Validate checks that a profile is valid.
func (p *Profile) Validate() error {
	var errors []string

	// Check filesystem mounts.
	for i, m := range p.Filesystem {
		if m.Dest == "" {
			errors = append(errors, fmt.Sprintf("filesystem[%d]: dest is required", i))
		}
		if m.Type == "" && m.Source == "" {
			errors = append(errors, fmt.Sprintf("filesystem[%d]: source is required for bind mounts", i))
		}
		if m.Mode != "" && m.Mode != MountModeRO && m.Mode != MountModeRW {
			errors = append(errors, fmt.Sprintf("filesystem[%d]: invalid mode %q (must be ro or rw)", i, m.Mode))
		}
		// Overlay mount validation.
		if m.Type == MountTypeOverlay {
			if m.Source == "" {
				errors = append(errors, fmt.Sprintf("filesystem[%d]: source (lower layer) is required for overlay mounts", i))
			}
			// Upper validation happens after variable expansion in ValidateOverlayUpper.
		}
		// Upper field only valid for overlay mounts.
		if m.Upper != "" && m.Type != MountTypeOverlay {
			errors = append(errors, fmt.Sprintf("filesystem[%d]: upper is only valid for overlay mounts", i))
		}
	}

	// Check resource limits.
	if p.Resources.TasksMax < 0 {
		errors = append(errors, "resources.tasks_max must be >= 0")
	}

	if len(errors) > 0 {
		return fmt.Errorf("profile %q validation failed:\n  %s", p.Name, strings.Join(errors, "\n  "))
	}

	return nil
}

// ValidateOverlayUpper validates that an overlay upper path is safe.
// The upper path must be either "tmpfs" or inside the worktree.
// This MUST be called after variable expansion.
//
// Security invariant: upper layer writes must never escape the sandbox.
// If upper is a host path outside worktree, an agent could write malicious
// code that gets executed with the host user's permissions.
//
// This function resolves symlinks to prevent bypass attacks where an attacker
// creates a symlink inside the worktree pointing outside (e.g., /workspace/cache -> /etc).
func ValidateOverlayUpper(upper string, worktree string) error {
	// Empty or "tmpfs" is always safe.
	if upper == "" || upper == OverlayUpperTmpfs {
		return nil
	}

	// Resolve worktree symlinks first (worktree must exist).
	worktreeResolved, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return fmt.Errorf("cannot resolve worktree path %q: %w", worktree, err)
	}
	worktreeResolved = filepath.Clean(worktreeResolved)

	// Try to resolve upper path symlinks.
	// If the path doesn't exist yet, resolve its parent directory.
	upperResolved, err := filepath.EvalSymlinks(upper)
	if err != nil {
		// Path doesn't exist - resolve the parent directory instead.
		parentDir := filepath.Dir(upper)
		parentResolved, parentErr := filepath.EvalSymlinks(parentDir)
		if parentErr != nil {
			return fmt.Errorf("cannot resolve overlay upper parent path %q: %w", parentDir, parentErr)
		}
		upperResolved = filepath.Join(parentResolved, filepath.Base(upper))
	}
	upperResolved = filepath.Clean(upperResolved)

	// Upper must be inside worktree (comparing resolved paths).
	if !strings.HasPrefix(upperResolved, worktreeResolved+string(filepath.Separator)) && upperResolved != worktreeResolved {
		return fmt.Errorf("overlay upper path %q resolves to %q which is outside worktree %q: "+
			"upper must be \"tmpfs\" or inside the worktree to prevent privilege escalation",
			upper, upperResolved, worktreeResolved)
	}

	return nil
}
