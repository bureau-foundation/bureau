// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package command provides the async command framework for Bureau.
//
// Bureau commands are sent as Matrix messages (m.bureau.command) to
// workspace or config rooms. The daemon receives them via /sync,
// executes the operation, and posts a threaded reply
// (m.bureau.command_result) with the result.
//
// This package provides three levels of API:
//
//   - [Execute] sends a command and waits for a single result. Use
//     for synchronous commands like workspace.status and workspace.du.
//
//   - [ExecutePhased] sends a command and collects multiple results.
//     Use for async commands like workspace.worktree.add that return
//     an "accepted" acknowledgment followed by a pipeline result.
//
//   - [Send] sends a command and returns a [Future] for fine-grained
//     control. The caller decides when to wait, how many results to
//     collect, or whether to discard the future (--no-wait mode).
//     Multiple futures can run independently for fan-out.
//
// All waiting is event-driven via Matrix /sync long-polling. The
// server holds the connection until events arrive — there is no
// client-side polling interval. This matches Bureau's architecture
// where all coordination is event-driven.
//
// The [Send] function captures the /sync position BEFORE sending the
// command, preventing a race where the result arrives between send
// and watch-start. This is the same pattern used by the integration
// test helpers (watchRoom + action + WaitForEvent).
//
// Fan-out works because [messaging.Session.Sync] is stateless: each
// call is an HTTP GET with a ?since= query parameter. Multiple
// futures on the same session with different sync positions work
// correctly without coordination.
package command
