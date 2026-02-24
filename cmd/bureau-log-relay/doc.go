// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-log-relay sits between tmux and a sandboxed process, holding the
// outer PTY file descriptors open until the child process exits. This
// eliminates a race condition in tmux 3.4+ where PTY EOF is detected
// (pane_dead=1) before SIGCHLD processing populates the exit status fields
// (pane_dead_status), causing exit code 0 to be reported for non-zero exits.
//
// The relay runs the child command with inherited stdin/stdout/stderr (the
// outer PTY slave fds allocated by tmux). Because the relay holds these fds
// open, tmux does not see PTY EOF until the relay exits — which only happens
// after it has collected the child's exit code via waitpid. This makes PTY
// EOF and SIGCHLD effectively simultaneous from tmux's perspective.
//
// Process tree:
//
//	tmux pane → bureau-log-relay → sandbox.sh → [exec] bwrap → agent
//
// Signal forwarding: SIGINT, SIGTERM, SIGHUP, and SIGQUIT received by the
// relay are forwarded to the child process. This preserves the graceful drain
// path where the daemon sends SIGTERM through tmux's pane PID.
//
// Future work: PTY interposition for output capture and CBOR telemetry
// streaming (see logging.md). The current implementation is a minimal
// wrapper that solves the exit code race without the telemetry plumbing.
package main
