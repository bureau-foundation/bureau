// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Sandbox manages isolated command execution.
type Sandbox struct {
	profile        *Profile
	worktree       string
	proxySocket    string
	scopeName      string
	gpu            bool
	bazelCache     string
	extraBinds     []string
	extraEnv       map[string]string
	logger         *slog.Logger
	overlayManager *OverlayManager
	overlayMerged  map[string]string // dest -> merged path for overlay mounts
}

// Config holds configuration for creating a new Sandbox.
type Config struct {
	// Profile is the resolved profile to use.
	Profile *Profile

	// Worktree is the path to the agent's worktree.
	Worktree string

	// ProxySocket is the path to the bureau-proxy Unix socket.
	ProxySocket string

	// ScopeName is the systemd scope name for resource tracking.
	ScopeName string

	// GPU enables GPU passthrough.
	GPU bool

	// BazelCache is the path to a shared Bazel cache directory.
	BazelCache string

	// ExtraBinds are additional bind mounts (source:dest[:mode]).
	ExtraBinds []string

	// ExtraEnv are additional environment variables.
	ExtraEnv map[string]string

	// Logger for sandbox operations.
	Logger *slog.Logger
}

// New creates a new Sandbox.
func New(config Config) (*Sandbox, error) {
	if config.Profile == nil {
		return nil, fmt.Errorf("profile is required")
	}
	if config.Worktree == "" {
		return nil, fmt.Errorf("worktree is required")
	}

	// Resolve worktree to absolute path.
	worktree, err := filepath.Abs(config.Worktree)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve worktree path: %w", err)
	}

	// Default proxy socket.
	proxySocket := config.ProxySocket
	if proxySocket == "" {
		proxySocket = "/run/bureau/proxy.sock"
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Sandbox{
		profile:     config.Profile,
		worktree:    worktree,
		proxySocket: proxySocket,
		scopeName:   config.ScopeName,
		gpu:         config.GPU,
		bazelCache:  config.BazelCache,
		extraBinds:  config.ExtraBinds,
		extraEnv:    config.ExtraEnv,
		logger:      logger,
	}, nil
}

// Run executes a command in the sandbox.
func (s *Sandbox) Run(ctx context.Context, command []string) error {
	// Set up overlay mounts if any are configured.
	if HasOverlayMounts(s.profile) {
		if err := s.setupOverlays(); err != nil {
			return fmt.Errorf("failed to set up overlay mounts: %w", err)
		}
		defer s.cleanupOverlays()
	}

	cmd, err := s.Command(ctx, command)
	if err != nil {
		return err
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	s.logger.Info("running sandboxed command",
		"profile", s.profile.Name,
		"worktree", s.worktree,
		"command", command,
	)

	if err := cmd.Run(); err != nil {
		// Extract exit code if available.
		if exitErr, ok := err.(*exec.ExitError); ok {
			return &ExitError{Code: exitErr.ExitCode()}
		}
		return fmt.Errorf("sandbox command failed: %w", err)
	}

	return nil
}

// setupOverlays creates overlay mounts for the sandbox.
func (s *Sandbox) setupOverlays() error {
	var err error
	s.overlayManager, err = NewOverlayManager(s.worktree)
	if err != nil {
		return err
	}

	s.overlayMerged = make(map[string]string)

	// Expand variables for overlay mounts.
	vars := Variables{
		"WORKTREE":     s.worktree,
		"PROXY_SOCKET": s.proxySocket,
		"TERM":         os.Getenv("TERM"),
		"HOME":         os.Getenv("HOME"),
	}

	for _, mount := range s.profile.Filesystem {
		if mount.Type != MountTypeOverlay {
			continue
		}

		// Expand variables in mount paths.
		expandedMount := Mount{
			Source:   vars.Expand(mount.Source),
			Dest:     vars.Expand(mount.Dest),
			Type:     mount.Type,
			Upper:    vars.Expand(mount.Upper),
			Options:  mount.Options,
			Optional: mount.Optional,
		}

		s.logger.Info("setting up overlay mount",
			"source", expandedMount.Source,
			"dest", expandedMount.Dest,
			"upper", expandedMount.Upper,
		)

		mergedPath, err := s.overlayManager.SetupMount(expandedMount)
		if err != nil {
			s.overlayManager.Cleanup()
			return fmt.Errorf("failed to set up overlay for %s: %w", mount.Dest, err)
		}

		if mergedPath != "" {
			s.overlayMerged[expandedMount.Dest] = mergedPath
		}
	}

	return nil
}

// cleanupOverlays unmounts all overlay mounts.
func (s *Sandbox) cleanupOverlays() {
	if s.overlayManager != nil {
		s.overlayManager.Cleanup()
		s.overlayManager = nil
		s.overlayMerged = nil
	}
}

// Command creates an exec.Cmd for running in the sandbox.
// Useful for custom I/O handling or testing.
func (s *Sandbox) Command(ctx context.Context, command []string) (*exec.Cmd, error) {
	// Expand profile variables.
	vars := Variables{
		"WORKTREE":     s.worktree,
		"PROXY_SOCKET": s.proxySocket,
		"TERM":         os.Getenv("TERM"),
	}
	if s.bazelCache != "" {
		vars["BAZEL_CACHE"] = s.bazelCache
	}
	profile := vars.ExpandProfile(s.profile)

	// Build bwrap command.
	builder := NewBwrapBuilder()
	bwrapArgs, err := builder.Build(&BwrapOptions{
		Profile:       profile,
		Worktree:      s.worktree,
		ExtraBinds:    s.extraBinds,
		ExtraEnv:      s.extraEnv,
		BazelCache:    s.bazelCache,
		GPU:           s.gpu,
		Command:       command,
		ClearEnv:      true,
		OverlayMerged: s.overlayMerged,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build bwrap command: %w", err)
	}

	// Get bwrap path.
	bwrapPath, err := BwrapPath()
	if err != nil {
		return nil, err
	}

	// Full command: bwrap [args...]
	fullCmd := append([]string{bwrapPath}, bwrapArgs...)

	// Wrap with systemd scope if resource limits are configured.
	if profile.Resources.HasLimits() {
		scope := NewSystemdScope(s.scopeName, profile.Resources)
		if scope.Available() {
			fullCmd = scope.WrapCommand(fullCmd)
		} else {
			s.logger.Warn("systemd-run not available, resource limits will not be enforced")
		}
	}

	// Create command.
	cmd := exec.CommandContext(ctx, fullCmd[0], fullCmd[1:]...)

	// CRITICAL: Explicitly set a minimal environment for the bwrap process.
	// If cmd.Env is nil, Go inherits the parent's full environment. Even though
	// bwrap uses --clearenv internally, the bwrap process itself would have the
	// parent's env in /proc/<pid>/environ, creating a sandbox escape where the
	// sandboxed process can read /proc/1/environ to extract secrets.
	//
	// We only need PATH for bwrap to find libraries, and TERM for proper
	// terminal handling. Everything else is passed via bwrap's --setenv.
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"TERM=" + os.Getenv("TERM"),
	}

	// Set process group for clean shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return cmd, nil
}

// DryRun returns the command that would be executed without running it.
// Note: For profiles with overlay mounts, this returns the command as if
// overlays were set up. Call Run() to actually execute with overlay setup.
func (s *Sandbox) DryRun(command []string) ([]string, error) {
	// Expand profile variables.
	vars := Variables{
		"WORKTREE":     s.worktree,
		"PROXY_SOCKET": s.proxySocket,
		"TERM":         os.Getenv("TERM"),
	}
	if s.bazelCache != "" {
		vars["BAZEL_CACHE"] = s.bazelCache
	}
	profile := vars.ExpandProfile(s.profile)

	// Build bwrap command.
	// Note: overlayMerged will be nil for DryRun, so overlay mounts will
	// show up as errors or be skipped. This is intentional - DryRun shows
	// the template, not the actual runtime configuration.
	builder := NewBwrapBuilder()
	bwrapArgs, err := builder.Build(&BwrapOptions{
		Profile:       profile,
		Worktree:      s.worktree,
		ExtraBinds:    s.extraBinds,
		ExtraEnv:      s.extraEnv,
		BazelCache:    s.bazelCache,
		GPU:           s.gpu,
		Command:       command,
		ClearEnv:      true,
		OverlayMerged: s.overlayMerged,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build bwrap command: %w", err)
	}

	// Get bwrap path.
	bwrapPath, err := BwrapPath()
	if err != nil {
		return nil, err
	}

	// Full command: bwrap [args...]
	fullCmd := append([]string{bwrapPath}, bwrapArgs...)

	// Wrap with systemd scope if resource limits are configured.
	if profile.Resources.HasLimits() {
		scope := NewSystemdScope(s.scopeName, profile.Resources)
		fullCmd = scope.WrapCommand(fullCmd)
	}

	return fullCmd, nil
}

// Validate runs pre-flight validation checks.
func (s *Sandbox) Validate(w io.Writer) error {
	validator := NewValidator()
	validator.ValidateAll(s.profile, s.worktree, s.proxySocket)

	if s.gpu {
		validator.ValidateGPU()
	}

	validator.PrintResults(w)

	if validator.HasErrors() {
		return fmt.Errorf("validation failed")
	}
	return nil
}

// Profile returns the sandbox's profile.
func (s *Sandbox) Profile() *Profile {
	return s.profile
}

// Worktree returns the sandbox's worktree path.
func (s *Sandbox) Worktree() string {
	return s.worktree
}

// ExitError represents a non-zero exit from the sandboxed command.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited with code %d", e.Code)
}

// IsExitError checks if an error is an ExitError and returns the code.
func IsExitError(err error) (int, bool) {
	if exitErr, ok := err.(*ExitError); ok {
		return exitErr.Code, true
	}
	return 0, false
}
