// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package observe implements Bureau's observation primitive: live bidirectional
// terminal access to sandboxed principals across the fleet.
//
// Observation connects a local terminal to a remote tmux session through the
// daemon's transport layer, providing full-fidelity interactive access with
// scrollback history. The tmux process runs outside the sandbox; only the PTY
// slave file descriptor crosses the namespace boundary via inheritance.
//
// The wire protocol uses a framed binary format: each message has a 5-byte
// header (1 byte type, 4 bytes big-endian payload length) followed by the
// payload. Four message types exist: Data (terminal I/O bytes), Resize
// (terminal dimensions as JSON), History (ring buffer replay on connect),
// and Metadata (principal name and mode as JSON). See [MessageTypeData],
// [WriteMessage], and [ReadMessage] for the protocol surface.
//
// On the remote side, [Relay] allocates a PTY pair, attaches to the
// principal's tmux session, sends metadata and history from the
// [RingBuffer], then bridges the PTY and network connection bidirectionally.
// The ring buffer (default 1 megabyte) is a circular buffer with sequence
// tracking, enabling reconnecting clients to receive only the terminal
// output they missed.
//
// On the local side, [Connect] dials the daemon's observation socket,
// performs a JSON handshake ([ObserveRequest]/[ObserveResponse]), reads the
// initial metadata message, and returns a [Session]. Session.Run relays
// stdin/stdout between the local terminal and the remote PTY, handling
// SIGWINCH for terminal resize propagation.
//
// Layout management converts between live tmux state and Matrix state events.
// [ReadTmuxLayout] captures the current tmux session structure,
// [ApplyLayout] creates or modifies sessions to match a target layout, and
// [LayoutToSchema]/[SchemaToLayout] convert between the observe and schema
// package representations. [Dashboard] builds composite local tmux sessions
// from layouts, resolving abstract pane types (Observe, Command, Role) into
// concrete tmux commands. [ControlClient] monitors a tmux server via control
// mode for layout change detection with debouncing.
//
// [ListTargets] queries the daemon for observable principals and machines.
// [QueryLayout] fetches expanded channel layouts. [QueryMachineLayout]
// requests auto-generated layouts for machine-level observation.
// [ExpandMembers] resolves ObserveMembers panes into concrete Observe panes
// by filtering room membership against label patterns.
package observe
