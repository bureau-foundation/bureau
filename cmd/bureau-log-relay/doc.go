// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-log-relay sits between tmux and a sandboxed process. It operates in
// two modes depending on whether the --relay flag is provided.
//
// # Passthrough mode (default)
//
// Runs the child with inherited stdin/stdout/stderr (the outer PTY slave fds
// allocated by tmux). The relay holds these fds open until the child exits,
// eliminating a race condition in tmux 3.4+ where PTY EOF is detected
// (pane_dead=1) before SIGCHLD processing populates the exit status fields
// (pane_dead_status), causing exit code 0 to be reported for non-zero exits.
//
// # Capture mode (--relay)
//
// Allocates an inner PTY pair, runs the child on the slave, and tees output
// to both the outer tmux PTY and the telemetry relay as CBOR output delta
// messages. Stdin from tmux is passed through to the child. Window resize
// signals (SIGWINCH) are propagated to the inner PTY.
//
// Output is buffered and flushed to the telemetry relay periodically (1s) or
// when the buffer reaches 64 KB, whichever comes first. Each flush produces
// a SubmitRequest containing an OutputDelta with a monotonically increasing
// sequence number, enabling downstream consumers to reconstruct the full
// output stream and detect gaps.
//
// Capture mode requires identity flags (--fleet, --machine, --source,
// --session-id) and telemetry relay connection flags (--relay, --token).
// These are passed by the launcher when the template opts in to output
// capture.
//
// # Process tree
//
//	tmux pane → bureau-log-relay → sandbox.sh → [exec] bwrap → agent
//
// Signal forwarding: SIGINT, SIGTERM, SIGHUP, and SIGQUIT received by the
// relay are forwarded to the child process. This preserves the graceful drain
// path where the daemon sends SIGTERM through tmux's pane PID.
package main
