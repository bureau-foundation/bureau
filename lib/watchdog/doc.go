// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package watchdog provides atomic state file operations for tracking
// risky process transitions like exec(). A process writes a watchdog
// [State] before the transition; on startup, any process can read the
// state to determine whether the transition succeeded or failed.
//
// The intended workflow for self-updating binaries:
//
//  1. Before exec(): call [Write] with the previous and new binary
//     paths.
//  2. exec() the new binary.
//  3. The new binary starts, calls [Check], finds its own executable
//     path matches State.NewBinary -- the update succeeded. Call
//     [Clear] to remove the watchdog file.
//  4. If the new binary crashes: systemd restarts the old binary,
//     which calls [Check], finds its own path matches
//     State.PreviousBinary -- the new version failed. Report the
//     failure and [Clear] the watchdog.
//
// The watchdog file is written atomically (write to temporary file,
// fsync, rename into place, fsync parent directory) so readers never
// see a partial or corrupt state. [Check] includes staleness detection:
// it ignores watchdog files older than a configurable maximum age to
// prevent acting on ancient files left behind by unrelated restarts.
//
// The [State] struct records the component name, previous and new
// binary paths, and a timestamp. It is serialized as JSON.
//
// This package has no dependencies on other Bureau packages.
package watchdog
