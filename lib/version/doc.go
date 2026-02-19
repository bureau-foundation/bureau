// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package version provides build version information and binary
// comparison logic for Bureau's self-update system.
//
// # Build information
//
// Four package-level variables are injected at build time via
// -ldflags -X:
//
//   - [GitCommit] -- short git SHA of the build
//   - [GitDirty] -- "true" if there were uncommitted changes
//   - [BuildTime] -- UTC timestamp of the build
//   - [Version] -- semantic version string (set manually for releases)
//
// These default to "unknown" / "0.1.0-dev" when not injected, which
// occurs during development builds and test runs.
//
// Formatting functions produce human-readable version strings:
//
//   - [Info] -- "0.1.0-dev (abc1234, 2026-02-10T...)" for --version
//   - [Full] -- Info plus Go version and GOOS/GOARCH
//   - [Short] -- just the version number
//   - [Commit] -- just the git SHA
//
// # Binary comparison
//
// [Compare] compares desired BureauVersion store paths against currently
// running binary hashes to produce a [Diff] describing which components
// (daemon, launcher, proxy) need updating. [ComputeSelfHash] returns the
// SHA256 digest of the currently running binary for use as the "current"
// input to Compare.
package version
