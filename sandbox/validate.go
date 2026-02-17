// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ValidationResult holds the result of a validation check.
type ValidationResult struct {
	Name    string
	Passed  bool
	Message string
	Warning bool // True if this is a warning, not an error.
}

// Validator performs pre-flight validation for sandbox execution.
type Validator struct {
	results []ValidationResult
	errors  int
}

// NewValidator creates a new validator.
func NewValidator() *Validator {
	return &Validator{
		results: make([]ValidationResult, 0),
	}
}

// Results returns all validation results.
func (v *Validator) Results() []ValidationResult {
	return v.results
}

// HasErrors returns true if any validation failed.
func (v *Validator) HasErrors() bool {
	return v.errors > 0
}

// pass records a successful validation.
func (v *Validator) pass(name, message string) {
	v.results = append(v.results, ValidationResult{
		Name:    name,
		Passed:  true,
		Message: message,
	})
}

// warn records a warning (not a failure).
func (v *Validator) warn(name, message string) {
	v.results = append(v.results, ValidationResult{
		Name:    name,
		Passed:  true,
		Message: message,
		Warning: true,
	})
}

// fail records a validation failure.
func (v *Validator) fail(name, message string) {
	v.results = append(v.results, ValidationResult{
		Name:    name,
		Passed:  false,
		Message: message,
	})
	v.errors++
}

// ValidateAll runs all validation checks for a sandbox configuration.
func (v *Validator) ValidateAll(profile *Profile, workingDirectory string, proxySocket string) {
	v.ValidateBwrap()
	v.ValidateSystemd()
	v.ValidateUserNamespaces()
	v.ValidateWorkingDirectory(workingDirectory)
	v.ValidateProxySocket(proxySocket)
	v.ValidateProfile(profile)
	v.ValidateProfileSources(profile, workingDirectory, proxySocket)
}

// ValidateBwrap checks that bubblewrap is available.
func (v *Validator) ValidateBwrap() {
	path, err := BwrapPath()
	if err != nil {
		v.fail("bwrap", "bubblewrap not found in standard locations")
		return
	}

	// Check it's executable.
	info, err := os.Stat(path)
	if err != nil {
		v.fail("bwrap", fmt.Sprintf("cannot stat %s: %v", path, err))
		return
	}

	if info.Mode()&0111 == 0 {
		v.fail("bwrap", fmt.Sprintf("%s is not executable", path))
		return
	}

	// Try to get version.
	cmd := exec.Command(path, "--version")
	output, err := cmd.Output()
	if err != nil {
		v.warn("bwrap", fmt.Sprintf("found at %s but --version failed", path))
		return
	}

	version := strings.TrimSpace(string(output))
	v.pass("bwrap", fmt.Sprintf("available: %s (%s)", path, version))
}

// ValidateSystemd checks that systemd-run is available for resource limits.
func (v *Validator) ValidateSystemd() {
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		v.warn("systemd", "systemd-run not found (resource limits will not be enforced)")
		return
	}

	// Check if we can use systemd-run (requires user session or root).
	cmd := exec.Command(path, "--user", "--scope", "--", "true")
	if err := cmd.Run(); err != nil {
		v.warn("systemd", "systemd-run available but cannot create user scopes")
		return
	}

	v.pass("systemd", fmt.Sprintf("available: %s (user scopes supported)", path))
}

// ValidateUserNamespaces checks that user namespaces are enabled.
func (v *Validator) ValidateUserNamespaces() {
	// Check /proc/sys/kernel/unprivileged_userns_clone.
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err != nil {
		// File might not exist on some kernels - that's usually fine.
		if os.IsNotExist(err) {
			v.pass("userns", "user namespaces supported (no clone restriction)")
			return
		}
		v.warn("userns", fmt.Sprintf("cannot check user namespace support: %v", err))
		return
	}

	value := strings.TrimSpace(string(data))
	if value == "0" {
		v.fail("userns", "unprivileged user namespaces are disabled (set kernel.unprivileged_userns_clone=1)")
		return
	}

	v.pass("userns", "user namespaces enabled")
}

// ValidateWorkingDirectory checks that the working directory exists.
func (v *Validator) ValidateWorkingDirectory(workingDirectory string) {
	if workingDirectory == "" {
		v.fail("working_directory", "working directory path is required")
		return
	}

	// Resolve to absolute path.
	absPath, err := filepath.Abs(workingDirectory)
	if err != nil {
		v.fail("working_directory", fmt.Sprintf("cannot resolve path: %v", err))
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			v.fail("working_directory", fmt.Sprintf("does not exist: %s", absPath))
		} else {
			v.fail("working_directory", fmt.Sprintf("cannot access: %v", err))
		}
		return
	}

	if !info.IsDir() {
		v.fail("working_directory", fmt.Sprintf("not a directory: %s", absPath))
		return
	}

	v.pass("working_directory", fmt.Sprintf("exists: %s", absPath))
}

// ValidateProxySocket checks that the proxy socket exists.
func (v *Validator) ValidateProxySocket(socketPath string) {
	if socketPath == "" {
		socketPath = "/run/bureau/proxy.sock"
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			v.warn("proxy", fmt.Sprintf("socket not found: %s (proxy features will not work)", socketPath))
		} else {
			v.warn("proxy", fmt.Sprintf("cannot access socket: %v", err))
		}
		return
	}

	// Check it's a socket.
	if info.Mode()&os.ModeSocket == 0 {
		v.warn("proxy", fmt.Sprintf("not a socket: %s", socketPath))
		return
	}

	v.pass("proxy", fmt.Sprintf("socket exists: %s", socketPath))
}

// ValidateProfile checks that the profile is valid.
func (v *Validator) ValidateProfile(profile *Profile) {
	if profile == nil {
		v.fail("profile", "profile is nil")
		return
	}

	if err := profile.Validate(); err != nil {
		v.fail("profile", err.Error())
		return
	}

	v.pass("profile", fmt.Sprintf("loaded: %s", profile.Name))
}

// ValidateProfileSources checks that all non-optional mount sources exist.
func (v *Validator) ValidateProfileSources(profile *Profile, workingDirectory, proxySocket string) {
	if profile == nil {
		return
	}

	vars := Variables{
		"WORKING_DIRECTORY": workingDirectory,
		"PROXY_SOCKET":      proxySocket,
	}

	for _, mount := range profile.Filesystem {
		// Skip special mount types.
		if mount.Type == MountTypeTmpfs || mount.Type == MountTypeProc || mount.Type == MountTypeDev {
			continue
		}

		source := vars.Expand(mount.Source)

		// Skip variable references that weren't expanded.
		if strings.Contains(source, "${") {
			if mount.Optional {
				continue
			}
			v.fail("mount", fmt.Sprintf("unresolved variable in source: %s", mount.Source))
			continue
		}

		// Check if source exists.
		_, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				if mount.Optional {
					v.warn("mount", fmt.Sprintf("optional source not found: %s -> %s", source, mount.Dest))
				} else {
					v.fail("mount", fmt.Sprintf("source not found: %s -> %s", source, mount.Dest))
				}
			} else {
				v.fail("mount", fmt.Sprintf("cannot access source %s: %v", source, err))
			}
			continue
		}
	}
}

// ValidateGPU checks GPU-specific requirements.
func (v *Validator) ValidateGPU() {
	// Check for DRI devices.
	driPath := "/dev/dri"
	if _, err := os.Stat(driPath); err == nil {
		entries, err := os.ReadDir(driPath)
		if err == nil && len(entries) > 0 {
			devices := make([]string, 0)
			for _, e := range entries {
				devices = append(devices, e.Name())
			}
			v.pass("gpu-dri", fmt.Sprintf("DRI devices found: %s", strings.Join(devices, ", ")))
		} else {
			v.warn("gpu-dri", "DRI directory exists but no devices found")
		}
	} else {
		v.warn("gpu-dri", "no DRI devices found (optional)")
	}

	// Check for NVIDIA devices.
	nvidiaFound := false
	nvidiaDevices := []string{"/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-uvm"}
	for _, dev := range nvidiaDevices {
		if _, err := os.Stat(dev); err == nil {
			nvidiaFound = true
			break
		}
	}
	if nvidiaFound {
		v.pass("gpu-nvidia", "NVIDIA devices found")
	} else {
		v.warn("gpu-nvidia", "no NVIDIA devices found (optional)")
	}

	// Check for Vulkan ICD.
	vulkanPaths := []string{
		"/usr/share/vulkan/icd.d",
		"/etc/vulkan/icd.d",
	}
	for _, path := range vulkanPaths {
		if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
			icds := make([]string, 0)
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					icds = append(icds, e.Name())
				}
			}
			if len(icds) > 0 {
				v.pass("gpu-vulkan", fmt.Sprintf("Vulkan ICDs found: %s", strings.Join(icds, ", ")))
				return
			}
		}
	}
	v.warn("gpu-vulkan", "no Vulkan ICDs found (optional)")
}

// PrintResults writes validation results to a writer.
func (v *Validator) PrintResults(w io.Writer) {
	for _, r := range v.results {
		var prefix string
		if r.Passed {
			if r.Warning {
				prefix = "⚠"
			} else {
				prefix = "✓"
			}
		} else {
			prefix = "✗"
		}
		fmt.Fprintf(w, "%s %s: %s\n", prefix, r.Name, r.Message)
	}

	fmt.Fprintln(w)
	if v.HasErrors() {
		fmt.Fprintf(w, "Validation failed with %d error(s)\n", v.errors)
	} else {
		fmt.Fprintln(w, "Ready to run sandbox")
	}
}
