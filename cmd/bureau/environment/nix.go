// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/lib/nix"
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
	args := []string{"flake", "show", flakeRef, "--json"}
	args = append(args, options.overrideArgs()...)

	output, err := nix.Run(args...)
	if err != nil {
		return nil, err
	}

	var show flakeShowJSON
	if err := json.Unmarshal([]byte(output), &show); err != nil {
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
	Name           string `json:"name"                    desc:"profile name"`
	DerivationName string `json:"derivation_name"         desc:"Nix derivation name"`
	Description    string `json:"description,omitempty"   desc:"profile description"`
}

// buildProfile builds a profile from a flake and places the result
// symlink at outLink. Returns the store path of the built derivation.
func buildProfile(flakeRef, profile, outLink string, options *nixOptions) (string, error) {
	installable := flakeRef + "#" + profile

	args := []string{"build", installable, "--out-link", outLink, "--print-out-paths"}
	args = append(args, options.overrideArgs()...)

	output, err := nix.Run(args...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(output), nil
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
