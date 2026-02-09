// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BwrapOptions holds options for building a bwrap command.
type BwrapOptions struct {
	// Profile is the resolved and expanded profile to use.
	Profile *Profile

	// Worktree is the path to the agent's worktree (mounted at /workspace).
	Worktree string

	// ExtraBinds are additional bind mounts specified via CLI.
	// Format: "source:dest:mode" where mode is "ro" or "rw".
	ExtraBinds []string

	// ExtraEnv are additional environment variables.
	ExtraEnv map[string]string

	// BazelCache is the path to a shared Bazel cache directory.
	BazelCache string

	// GPU enables GPU passthrough (binds /dev/dri and related devices).
	GPU bool

	// Command is the command to run inside the sandbox.
	Command []string

	// ClearEnv clears all environment variables before setting new ones.
	// Default is true for security.
	ClearEnv bool

	// OverlayMerged maps mount destinations to their fuse-overlayfs merged paths.
	// For overlay mounts, the merged path is bind-mounted instead of the source.
	// This is populated by Sandbox.setupOverlays().
	OverlayMerged map[string]string
}

// BwrapBuilder builds bubblewrap command-line arguments.
type BwrapBuilder struct {
	args []string
	env  map[string]string
}

// NewBwrapBuilder creates a new builder.
func NewBwrapBuilder() *BwrapBuilder {
	return &BwrapBuilder{
		args: []string{},
		env:  make(map[string]string),
	}
}

// Build constructs the bwrap arguments from options.
func (b *BwrapBuilder) Build(opts *BwrapOptions) ([]string, error) {
	if opts.Profile == nil {
		return nil, fmt.Errorf("profile is required")
	}
	if opts.Worktree == "" {
		return nil, fmt.Errorf("worktree is required")
	}
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}

	b.args = []string{}
	b.env = make(map[string]string)

	// Add namespace options.
	b.addNamespaces(opts.Profile.Namespaces)

	// Add security options.
	b.addSecurity(opts.Profile.Security)

	// Add /proc and /dev.
	b.addBaseMounts()

	// Add filesystem mounts from profile.
	if err := b.addProfileMounts(opts.Profile, opts.Worktree, opts.OverlayMerged); err != nil {
		return nil, err
	}

	// Add extra bind mounts from CLI.
	if err := b.addExtraBinds(opts.ExtraBinds); err != nil {
		return nil, err
	}

	// Add Bazel cache if specified.
	if opts.BazelCache != "" {
		b.addBazelCache(opts.BazelCache)
	}

	// Add GPU mounts if enabled.
	if opts.GPU {
		b.addGPUMounts()
	}

	// Create directories.
	for _, dir := range opts.Profile.CreateDirs {
		b.args = append(b.args, "--dir", dir)
	}

	// Environment handling.
	clearEnv := opts.ClearEnv
	// Default to clearing env for security if not explicitly set.
	if opts.Profile != nil {
		clearEnv = true
	}

	if clearEnv {
		b.args = append(b.args, "--clearenv")
	}

	// Add profile environment variables.
	for key, value := range opts.Profile.Environment {
		b.env[key] = value
	}

	// Add extra environment variables (override profile).
	for key, value := range opts.ExtraEnv {
		b.env[key] = value
	}

	// Add Bazel cache env if specified.
	if opts.BazelCache != "" {
		b.env["BAZEL_DISK_CACHE"] = "/var/cache/bazel"
	}

	// Set environment variables.
	// Sort keys for deterministic output.
	envKeys := make([]string, 0, len(b.env))
	for key := range b.env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		b.args = append(b.args, "--setenv", key, b.env[key])
	}

	// Add command separator and command.
	b.args = append(b.args, "--")
	b.args = append(b.args, opts.Command...)

	return b.args, nil
}

// addNamespaces adds namespace unsharing options.
func (b *BwrapBuilder) addNamespaces(ns NamespaceConfig) {
	if ns.PID {
		b.args = append(b.args, "--unshare-pid")
	}
	if ns.Net {
		b.args = append(b.args, "--unshare-net")
	}
	if ns.IPC {
		b.args = append(b.args, "--unshare-ipc")
	}
	if ns.UTS {
		b.args = append(b.args, "--unshare-uts")
	}
	if ns.Cgroup {
		b.args = append(b.args, "--unshare-cgroup")
	}
	if ns.User {
		b.args = append(b.args, "--unshare-user")
	}
}

// addSecurity adds security options.
func (b *BwrapBuilder) addSecurity(sec SecurityConfig) {
	if sec.NewSession {
		b.args = append(b.args, "--new-session")
	}
	if sec.DieWithParent {
		b.args = append(b.args, "--die-with-parent")
	}
	// Note: --cap-drop ALL and PR_SET_NO_NEW_PRIVS are always set by bwrap.
}

// addBaseMounts adds standard /proc and /dev mounts.
func (b *BwrapBuilder) addBaseMounts() {
	// /proc is needed for most programs.
	b.args = append(b.args, "--proc", "/proc")

	// Minimal /dev with only safe devices.
	b.args = append(b.args, "--dev", "/dev")
}

// addProfileMounts adds mounts from the profile configuration.
// overlayMerged maps dest paths to their fuse-overlayfs merged directories.
func (b *BwrapBuilder) addProfileMounts(profile *Profile, worktree string, overlayMerged map[string]string) error {
	for _, mount := range profile.Filesystem {
		source := mount.Source

		// Substitute ${WORKTREE} placeholder.
		if source == "${WORKTREE}" {
			source = worktree
		}

		switch mount.Type {
		case MountTypeTmpfs:
			args := []string{"--tmpfs", mount.Dest}
			b.args = append(b.args, args...)

		case MountTypeProc:
			b.args = append(b.args, "--proc", mount.Dest)

		case MountTypeDev:
			b.args = append(b.args, "--dev", mount.Dest)

		case MountTypeDevBind:
			// Device node bind - use --dev-bind.
			if mount.Optional {
				if _, err := os.Stat(source); os.IsNotExist(err) {
					continue
				}
			}
			b.args = append(b.args, "--dev-bind", source, mount.Dest)

		case MountTypeOverlay:
			// Overlay mounts are handled via fuse-overlayfs.
			// The merged directory is already set up and passed in overlayMerged.
			mergedPath, ok := overlayMerged[mount.Dest]
			if !ok {
				if mount.Optional {
					continue // Optional overlay wasn't set up (source missing)
				}
				return fmt.Errorf("overlay mount for %s not found in overlayMerged map", mount.Dest)
			}
			// Create all parent directories in the path. bwrap's --dir only creates
			// a single directory component, so we need to create the full hierarchy.
			// For example, /home/ben/.cache/pre-commit requires creating /home,
			// /home/ben, /home/ben/.cache, then /home/ben/.cache/pre-commit.
			for _, dir := range pathHierarchy(mount.Dest) {
				b.args = append(b.args, "--dir", dir)
			}
			// Bind-mount the merged fuse-overlayfs directory (read-write).
			b.args = append(b.args, "--bind", mergedPath, mount.Dest)

		default:
			// Regular bind mount (MountTypeBind or empty).
			if mount.Optional {
				if _, err := os.Stat(source); os.IsNotExist(err) {
					continue
				}
			}

			// Handle glob patterns.
			if mount.Glob {
				matches, err := filepath.Glob(source)
				if err != nil {
					return fmt.Errorf("invalid glob pattern %q: %w", source, err)
				}
				for _, match := range matches {
					dest := filepath.Join(mount.Dest, filepath.Base(match))
					if mount.Mode == MountModeRO {
						b.args = append(b.args, "--ro-bind", match, dest)
					} else {
						b.args = append(b.args, "--bind", match, dest)
					}
				}
				continue
			}

			if mount.Mode == MountModeRO {
				b.args = append(b.args, "--ro-bind", source, mount.Dest)
			} else {
				b.args = append(b.args, "--bind", source, mount.Dest)
			}
		}
	}

	return nil
}

// addExtraBinds adds CLI-specified bind mounts.
func (b *BwrapBuilder) addExtraBinds(binds []string) error {
	for _, bind := range binds {
		source, dest, mode, err := parseBindSpec(bind)
		if err != nil {
			return err
		}

		if mode == MountModeRO {
			b.args = append(b.args, "--ro-bind", source, dest)
		} else {
			b.args = append(b.args, "--bind", source, dest)
		}
	}
	return nil
}

// parseBindSpec parses a bind specification in format "source:dest[:mode]".
func parseBindSpec(spec string) (source, dest, mode string, err error) {
	parts := splitBindSpec(spec)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("invalid bind spec %q: must be source:dest[:mode]", spec)
	}

	source = parts[0]
	dest = parts[1]
	mode = MountModeRW // Default

	if len(parts) >= 3 {
		modeStr := parts[2]
		if modeStr != MountModeRO && modeStr != MountModeRW {
			return "", "", "", fmt.Errorf("invalid bind mode %q: must be ro or rw", modeStr)
		}
		mode = modeStr
	}

	return source, dest, mode, nil
}

// splitBindSpec splits a bind spec in format "source:dest[:mode]".
// For simplicity, we assume paths don't contain colons (common on Linux).
// Returns [source, dest] or [source, dest, mode].
func splitBindSpec(spec string) []string {
	// Split by colon.
	parts := strings.Split(spec, ":")

	switch len(parts) {
	case 2:
		// source:dest
		return parts
	case 3:
		// source:dest:mode
		return parts
	default:
		// Too few or too many colons.
		return []string{spec}
	}
}

// addBazelCache adds a Bazel cache bind mount.
func (b *BwrapBuilder) addBazelCache(cachePath string) {
	b.args = append(b.args, "--bind", cachePath, "/var/cache/bazel")
}

// addGPUMounts adds GPU device mounts for graphics/ML workloads.
func (b *BwrapBuilder) addGPUMounts() {
	// AMD/Intel GPU (DRI).
	if _, err := os.Stat("/dev/dri"); err == nil {
		b.args = append(b.args, "--dev-bind", "/dev/dri", "/dev/dri")
	}

	// NVIDIA GPU devices.
	nvidiaDevices := []string{
		"/dev/nvidia0",
		"/dev/nvidia1",
		"/dev/nvidia2",
		"/dev/nvidia3",
		"/dev/nvidiactl",
		"/dev/nvidia-uvm",
		"/dev/nvidia-uvm-tools",
	}
	for _, dev := range nvidiaDevices {
		if _, err := os.Stat(dev); err == nil {
			b.args = append(b.args, "--dev-bind", dev, dev)
		}
	}

	// Vulkan ICD files.
	vulkanPaths := []string{
		"/usr/share/vulkan",
		"/etc/vulkan",
	}
	for _, path := range vulkanPaths {
		if _, err := os.Stat(path); err == nil {
			b.args = append(b.args, "--ro-bind", path, path)
		}
	}

	// Common driver library paths.
	driverPaths := []string{
		"/usr/lib/x86_64-linux-gnu/dri",
		"/usr/lib/dri",
		"/opt/rocm",
	}
	for _, path := range driverPaths {
		if _, err := os.Stat(path); err == nil {
			b.args = append(b.args, "--ro-bind", path, path)
		}
	}
}

// BwrapPath returns the path to the bwrap executable.
func BwrapPath() (string, error) {
	// Check common locations.
	paths := []string{
		"/usr/bin/bwrap",
		"/usr/local/bin/bwrap",
		"/bin/bwrap",
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("bwrap not found in standard locations")
}

// pathHierarchy returns all directories in a path from root to the full path.
// For example, "/home/ben/.cache/pre-commit" returns:
// ["/home", "/home/ben", "/home/ben/.cache", "/home/ben/.cache/pre-commit"]
func pathHierarchy(path string) []string {
	path = filepath.Clean(path)
	if path == "/" || path == "." {
		return nil
	}

	var result []string
	current := path

	// Walk up the path collecting all directories.
	var components []string
	for current != "/" && current != "." {
		components = append(components, current)
		current = filepath.Dir(current)
	}

	// Reverse to get root-to-leaf order.
	for i := len(components) - 1; i >= 0; i-- {
		result = append(result, components[i])
	}

	return result
}
