// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// OverlayManager handles fuse-overlayfs mounts for sandbox overlay filesystems.
//
// Overlay mounts provide copy-on-write semantics: reads come from the lower
// (host) layer, writes go to the upper (sandbox-local) layer. This allows
// sharing host caches (pre-commit, bazel, npm) without allowing the sandbox
// to modify them.
//
// Security invariant: the lower layer is ALWAYS read-only from the host's
// perspective, and the upper layer is ALWAYS either tmpfs or inside the
// sandbox working directory. This prevents privilege escalation.
type OverlayManager struct {
	fuseBin          string
	fusermountBin    string
	workingDirectory string
	mounts           []*overlayMount
	tempDir          string
}

// overlayMount represents a single active overlay mount.
type overlayMount struct {
	lowerDir  string // Host path (source) - read-only
	upperDir  string // Where writes go
	workDir   string // Required by overlayfs
	mergedDir string // What gets bind-mounted into sandbox
	isTmpfs   bool   // True if upper is tmpfs
}

// validateOverlayPath checks that a path is safe for use in fuse-overlayfs options.
// fuse-overlayfs uses commas to separate options, so a path containing a comma
// could inject additional options (e.g., "lowerdir=/tmp,upperdir=/etc" would
// set upperdir to /etc instead of the validated upper directory).
func validateOverlayPath(path, fieldName string) error {
	if strings.Contains(path, ",") {
		return fmt.Errorf("%s path %q contains comma which would corrupt fuse-overlayfs options: "+
			"commas are used as option separators and cannot be safely escaped", fieldName, path)
	}
	// Reject null bytes and newlines which could cause other parsing issues.
	if strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("%s path %q contains invalid characters (null or newline)", fieldName, path)
	}
	return nil
}

// NewOverlayManager creates a new overlay manager.
//
// Returns an error if fuse-overlayfs is not available.
// This ensures we fail loudly rather than falling back to insecure mounts.
func NewOverlayManager(workingDirectory string) (*OverlayManager, error) {
	fuseBin, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return nil, fmt.Errorf("fuse-overlayfs not found: %w\n\n"+
			"Install with: sudo apt install fuse-overlayfs\n\n"+
			"Overlay mounts require fuse-overlayfs to provide copy-on-write\n"+
			"semantics for host caches. Without it, the sandbox cannot safely\n"+
			"share caches like pre-commit or bazel.", err)
	}

	fusermountBin, err := exec.LookPath("fusermount")
	if err != nil {
		// Try fusermount3
		fusermountBin, err = exec.LookPath("fusermount3")
		if err != nil {
			return nil, fmt.Errorf("fusermount/fusermount3 not found: %w\n\n"+
				"Install with: sudo apt install fuse3", err)
		}
	}

	return &OverlayManager{
		fuseBin:          fuseBin,
		fusermountBin:    fusermountBin,
		workingDirectory: workingDirectory,
		mounts:           make([]*overlayMount, 0),
	}, nil
}

// SetupMount creates an overlay mount for the given configuration.
//
// The mount is created immediately. Call Cleanup() to unmount when done.
//
// Returns the path to the merged directory that should be bind-mounted
// into the sandbox.
func (m *OverlayManager) SetupMount(mount Mount) (mergedPath string, err error) {
	// Validate this is an overlay mount.
	if mount.Type != MountTypeOverlay {
		return "", fmt.Errorf("SetupMount called with non-overlay mount type: %s", mount.Type)
	}

	// Validate source exists.
	if _, err := os.Stat(mount.Source); err != nil {
		if os.IsNotExist(err) && mount.Optional {
			return "", nil // Skip optional missing source
		}
		return "", fmt.Errorf("overlay lower path does not exist: %s", mount.Source)
	}

	// Validate upper path security.
	if err := ValidateOverlayUpper(mount.Upper, m.workingDirectory); err != nil {
		return "", err
	}

	// Validate paths don't contain characters that could corrupt fuse-overlayfs options.
	// This prevents argument injection attacks via paths like "/tmp,upperdir=/etc".
	if err := validateOverlayPath(mount.Source, "lower"); err != nil {
		return "", err
	}
	if mount.Upper != "" && mount.Upper != OverlayUpperTmpfs {
		if err := validateOverlayPath(mount.Upper, "upper"); err != nil {
			return "", err
		}
	}

	// Create temp directory for this overlay's working files.
	if m.tempDir == "" {
		m.tempDir, err = os.MkdirTemp("", "bureau-overlay-*")
		if err != nil {
			return "", fmt.Errorf("failed to create overlay temp dir: %w", err)
		}
	}

	// Generate unique name for this mount.
	mountName := filepath.Base(mount.Dest)
	if mountName == "" || mountName == "/" {
		mountName = "root"
	}

	overlay := &overlayMount{
		lowerDir: mount.Source,
		isTmpfs:  mount.Upper == "" || mount.Upper == OverlayUpperTmpfs,
	}

	// Set up upper directory.
	if overlay.isTmpfs {
		// Create upper in our temp directory (which could be tmpfs or regular fs).
		overlay.upperDir = filepath.Join(m.tempDir, mountName+"-upper")
	} else {
		// Use specified path (already validated to be in working directory).
		overlay.upperDir = mount.Upper
	}

	// Create directories.
	overlay.workDir = filepath.Join(m.tempDir, mountName+"-work")
	overlay.mergedDir = filepath.Join(m.tempDir, mountName+"-merged")

	// Create directories with restricted permissions (0700) to prevent other
	// local users from accessing sandbox artifacts. While the parent tempDir
	// is already 0700 for the tmpfs case, this protects non-tmpfs upper dirs.
	for _, dir := range []string{overlay.upperDir, overlay.workDir, overlay.mergedDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", fmt.Errorf("failed to create overlay directory %s: %w", dir, err)
		}
	}

	// Mount the overlay using fuse-overlayfs.
	// Options:
	//   -o lowerdir=X   : read-only lower layer
	//   -o upperdir=X   : read-write upper layer
	//   -o workdir=X    : required work directory
	args := []string{
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			overlay.lowerDir, overlay.upperDir, overlay.workDir),
		overlay.mergedDir,
	}

	cmd := exec.Command(m.fuseBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("fuse-overlayfs failed: %w\nOutput: %s\nCommand: %s %v",
			err, string(output), m.fuseBin, args)
	}

	// Wait for the FUSE mount to be ready before returning.
	// This prevents race conditions where bwrap tries to bind-mount
	// the directory before the FUSE filesystem is registered.
	if err := waitForMount(overlay.mergedDir); err != nil {
		// Attempt cleanup on failure.
		unmountCmd := exec.Command(m.fusermountBin, "-u", overlay.mergedDir)
		unmountCmd.Run() // Best effort
		return "", fmt.Errorf("overlay mount not ready after fuse-overlayfs: %w", err)
	}

	m.mounts = append(m.mounts, overlay)
	return overlay.mergedDir, nil
}

// Cleanup unmounts all overlay mounts and removes temporary directories.
//
// This should be called when the sandbox exits, even on error.
// Errors are logged but not returned to ensure all mounts are attempted.
func (m *OverlayManager) Cleanup() {
	for _, overlay := range m.mounts {
		// Unmount using fusermount.
		cmd := exec.Command(m.fusermountBin, "-u", overlay.mergedDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			// Try lazy unmount if regular unmount fails.
			cmd = exec.Command(m.fusermountBin, "-u", "-z", overlay.mergedDir)
			if output2, err2 := cmd.CombinedOutput(); err2 != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to unmount overlay %s: %v\n%s\n%s\n",
					overlay.mergedDir, err, string(output), string(output2))
			}
		}
	}

	// Remove temp directory.
	if m.tempDir != "" {
		if err := os.RemoveAll(m.tempDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove overlay temp dir %s: %v\n",
				m.tempDir, err)
		}
	}

	m.mounts = nil
	m.tempDir = ""
}

// HasOverlayMounts returns true if the profile has any overlay mounts.
func HasOverlayMounts(profile *Profile) bool {
	for _, m := range profile.Filesystem {
		if m.Type == MountTypeOverlay {
			return true
		}
	}
	return false
}

// FilterOverlayMounts separates overlay mounts from regular mounts.
// Returns (regular mounts, overlay mounts).
func FilterOverlayMounts(mounts []Mount) (regular []Mount, overlays []Mount) {
	for _, m := range mounts {
		if m.Type == MountTypeOverlay {
			overlays = append(overlays, m)
		} else {
			regular = append(regular, m)
		}
	}
	return regular, overlays
}

// CheckFuseOverlayfs checks if fuse-overlayfs is available.
// Returns nil if available, error with installation instructions if not.
func CheckFuseOverlayfs() error {
	_, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return fmt.Errorf("fuse-overlayfs is not installed.\n\n" +
			"Install with: sudo apt install fuse-overlayfs\n\n" +
			"This is required for overlay mounts which provide copy-on-write\n" +
			"semantics for sharing host caches safely with sandboxed agents.")
	}
	return nil
}

// waitForMount waits until a FUSE mount is ready by checking the filesystem type.
// Returns nil when the mount is ready, or an error after timeout.
func waitForMount(path string) error {
	const maxAttempts = 50 // 50 * 20ms = 1 second max wait
	const sleepInterval = 20 * time.Millisecond

	for i := 0; i < maxAttempts; i++ {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil {
			// Check if it's a FUSE filesystem (magic number 0x65735546).
			// This confirms fuse-overlayfs has registered the mount.
			if stat.Type == 0x65735546 {
				return nil
			}
		}
		time.Sleep(sleepInterval)
	}
	return fmt.Errorf("timeout waiting for FUSE mount at %s (waited %v)", path, maxAttempts*sleepInterval)
}
