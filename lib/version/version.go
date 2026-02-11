// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"runtime"
)

// These variables are set via -ldflags at build time.
var (
	// GitCommit is the short git SHA of the build.
	GitCommit = "unknown"

	// GitDirty indicates whether there were uncommitted changes.
	GitDirty = "false"

	// BuildTime is the UTC timestamp of the build.
	BuildTime = "unknown"

	// Version is the semantic version. This is set manually for releases.
	Version = "0.1.0-dev"
)

// Info returns a formatted version string suitable for --version output.
func Info() string {
	dirty := ""
	if GitDirty == "true" {
		dirty = "-dirty"
	}
	return fmt.Sprintf("%s (%s%s, %s)", Version, GitCommit, dirty, BuildTime)
}

// Full returns detailed version information including Go version.
func Full() string {
	return fmt.Sprintf("%s\n  Go: %s\n  Platform: %s/%s",
		Info(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Short returns just the version number.
func Short() string {
	return Version
}

// Commit returns the git commit SHA.
func Commit() string {
	return GitCommit
}
