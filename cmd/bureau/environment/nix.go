// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const defaultFlakeRef = "github:bureau-foundation/environment"

// nixOptions holds configuration passed to all nix commands.
type nixOptions struct {
	// OverrideInputs maps flake input names to replacement references.
	// For example, {"bureau": "path:/home/user/src/bureau"} replaces
	// the bureau input with a local checkout.
	OverrideInputs map[string]string
}

// parseOverrideInputs parses --override-input flag values (format:
// "name=flakeref") into a nixOptions struct. Returns nil if no
// overrides were specified.
func parseOverrideInputs(overrides []string) (*nixOptions, error) {
	if len(overrides) == 0 {
		return nil, nil
	}

	options := &nixOptions{
		OverrideInputs: make(map[string]string, len(overrides)),
	}

	for _, override := range overrides {
		parts := strings.SplitN(override, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid --override-input %q (expected name=flakeref)", override)
		}
		options.OverrideInputs[parts[0]] = parts[1]
	}

	return options, nil
}

// nixPath returns the path to the nix binary, checking the standard
// Determinate Nix installation location if it's not on PATH.
func nixPath() (string, error) {
	// Check PATH first (works inside nix develop and NixOS).
	if path, err := exec.LookPath("nix"); err == nil {
		return path, nil
	}

	// Determinate Nix installs to a profile outside PATH by default.
	const determinatePath = "/nix/var/nix/profiles/default/bin/nix"
	if _, err := os.Stat(determinatePath); err == nil {
		return determinatePath, nil
	}

	return "", fmt.Errorf("nix not found on PATH or at %s — install Nix first (see script/setup-nix)", determinatePath)
}

// overrideArgs returns the --override-input flags for a nix command.
func (options *nixOptions) overrideArgs() []string {
	if options == nil {
		return nil
	}
	var args []string
	for name, ref := range options.OverrideInputs {
		args = append(args, "--override-input", name, ref)
	}
	return args
}

// flakeShowJSON holds the structure returned by nix flake show --json.
// We only care about the packages.<system> subtree.
type flakeShowJSON struct {
	Packages map[string]map[string]flakePackage `json:"packages"`
}

// flakePackage is a single package entry from nix flake show --json.
type flakePackage struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// listProfiles queries a flake for its package outputs on the current
// system. Returns profile names and their derivation names.
func listProfiles(flakeRef string, options *nixOptions) ([]profileInfo, error) {
	nix, err := nixPath()
	if err != nil {
		return nil, err
	}

	args := []string{"flake", "show", flakeRef, "--json"}
	args = append(args, options.overrideArgs()...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(nix, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, nixError("nix flake show", &stderr, err)
	}

	var show flakeShowJSON
	if err := json.Unmarshal(stdout.Bytes(), &show); err != nil {
		return nil, fmt.Errorf("parsing nix flake show output: %w", err)
	}

	system := currentSystem()
	systemPackages, ok := show.Packages[system]
	if !ok {
		return nil, fmt.Errorf("no packages for system %q in %s", system, flakeRef)
	}

	var profiles []profileInfo
	for name, pkg := range systemPackages {
		profiles = append(profiles, profileInfo{
			Name:           name,
			DerivationName: pkg.Name,
			Description:    pkg.Description,
		})
	}

	return profiles, nil
}

// profileInfo describes an available environment profile.
type profileInfo struct {
	Name           string
	DerivationName string
	Description    string
}

// buildProfile builds a profile from a flake and places the result
// symlink at outLink. Returns the store path of the built derivation.
func buildProfile(flakeRef, profile, outLink string, options *nixOptions) (string, error) {
	nix, err := nixPath()
	if err != nil {
		return "", err
	}

	installable := flakeRef + "#" + profile

	args := []string{"build", installable, "--out-link", outLink, "--print-out-paths"}
	args = append(args, options.overrideArgs()...)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(nix, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", nixError("nix build "+installable, &stderr, err)
	}

	storePath := strings.TrimSpace(stdout.String())
	return storePath, nil
}

// nixError formats a nix command error, preferring stderr output over
// the generic exec error.
func nixError(command string, stderr *bytes.Buffer, err error) error {
	stderrText := strings.TrimSpace(stderr.String())
	if stderrText != "" {
		return fmt.Errorf("%s: %s", command, stderrText)
	}
	return fmt.Errorf("%s: %w", command, err)
}

// currentSystem returns the Nix system identifier for the current
// machine (e.g., "x86_64-linux", "aarch64-darwin").
func currentSystem() string {
	// Nix system strings use uname-style arch + OS.
	// Go's GOARCH/GOOS use different names, so we translate.
	arch := goArchToNix()
	operatingSystem := goOSToNix()
	return arch + "-" + operatingSystem
}

func goArchToNix() string {
	switch a := goarch(); a {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	default:
		return a
	}
}

func goOSToNix() string {
	switch o := goos(); o {
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return o
	}
}
