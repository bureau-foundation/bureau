// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import "io"

// Relay runs the remote side of an observation session. It attaches to a
// tmux session and relays terminal I/O between the tmux PTY and the
// provided connection (typically a unix socket inherited from the daemon).
//
// The relay:
//   - Sends a metadata message describing the session
//   - Sends the ring buffer contents as a history message
//   - Copies PTY output → connection as data messages
//   - Copies connection data messages → PTY input
//   - Handles resize messages by setting the PTY window size
//   - Feeds all PTY output through the ring buffer for future observers
//
// serverSocket is the tmux server socket path (the -S flag). All tmux
// commands use this to avoid interfering with the user's personal tmux
// or killing the agent's own session.
//
// sessionName is the tmux session to attach to (e.g., "bureau/iree/amdgpu/pm").
//
// readOnly controls whether input from the connection is relayed to the
// PTY. When true, the relay attaches with tmux attach -r and does not
// write to the PTY master.
//
// Relay blocks until the tmux session ends, the connection closes, or
// an unrecoverable error occurs. Returns nil on clean shutdown.
func Relay(connection io.ReadWriteCloser, serverSocket, sessionName string, readOnly bool) error {
	// Implementation in bd-304 (PTY relay logic bead).
	panic("observe.Relay not yet implemented")
}
