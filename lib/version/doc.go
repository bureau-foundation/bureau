// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package version provides build version information for Bureau
// binaries.
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
// This package has no dependencies on other Bureau packages.
package version
