// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package environment implements the "bureau environment" subcommands
// for managing Nix-based fleet environment profiles.
//
// An environment profile is a Nix flake output that defines the
// complete package set available on a class of machine. Profiles are
// defined in the bureau-foundation/environment repo (or a custom flake
// specified by --flake-ref) and consumed by Buildbarn runners, sandbox
// agents, and remote execution actions.
//
// Subcommands:
//
//   - list: query a Nix flake for available profile names using
//     "nix flake show --json".
//   - build: build a profile with "nix build" and optionally create
//     an --out-link symlink for deployment (e.g., into
//     deploy/buildbarn/runner-env).
//   - status: show which profiles are currently deployed by checking
//     existing out-link symlinks and their Nix store targets.
//
// Helper functions in nix.go locate the Nix binary (checking both
// PATH and /nix/var/nix/profiles/default/bin/nix), construct flake
// attribute paths for the current system, and handle --override-input
// for local development against unpushed Bureau changes.
package environment
